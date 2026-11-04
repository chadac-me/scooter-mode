package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"regexp"
	"strings"
	"time"
)

// maxImageBytes caps an image either way: a prompt's, or an asset fetched for replay.
const maxImageBytes = 5 << 20

// uploadTypes are the image types Scooter accepts in a prompt.
var uploadTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// assetPath is a conversation's uploaded or tool-made image, readable with the session's auth.
var assetPath = regexp.MustCompile(`^/conversations/[^/]+/assets/[^/]+$`)

// assetKey marks an image block whose bytes the bridge still has to fetch.
const assetKey = "_asset"

// promptImages returns the prompt's image blocks as /agui content parts.
func promptImages(blocks []json.RawMessage) ([]any, error) {
	var out []any
	for _, raw := range blocks {
		var b struct {
			Type     string `json:"type"`
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		}
		if json.Unmarshal(raw, &b) != nil || b.Type != "image" {
			continue
		}
		if b.Data == "" {
			return nil, fmt.Errorf("image block without base64 data")
		}
		if !uploadTypes[b.MimeType] {
			return nil, fmt.Errorf("scooter accepts png, jpeg, gif and webp images, not %s", b.MimeType)
		}
		if n := base64.StdEncoding.DecodedLen(len(b.Data)); n > maxImageBytes {
			return nil, fmt.Errorf("image is about %d bytes, over the %d-byte limit", n, maxImageBytes)
		}
		out = append(out, map[string]any{"type": "image", "data": b.Data, "mimeType": b.MimeType})
	}
	return out, nil
}

// promptContent is /agui's message content: plain text, or parts with images.
func promptContent(text string, images []any) any {
	if len(images) == 0 {
		return text
	}
	var parts []any
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	return append(parts, images...)
}

// imageRef is how Scooter's events point at an image.
type imageRef struct {
	Type     string `json:"type"`
	URL      string `json:"url"`
	MimeType string `json:"mimeType"`
	Omitted  string `json:"omitted"`
}

func omittedImage(mimeType, why string) map[string]any {
	if mimeType == "" {
		mimeType = "image"
	}
	return map[string]any{"type": "text", "text": fmt.Sprintf("\n[%s not shown: %s]\n", mimeType, why)}
}

// imageBlock is an ACP block for ref: one to fetch, or a note saying why not.
func imageBlock(ref imageRef) map[string]any {
	if assetPath.MatchString(ref.URL) {
		return map[string]any{"type": "image", "mimeType": ref.MimeType, assetKey: ref.URL}
	}
	why := ref.Omitted
	if why == "" {
		why = "unavailable"
	}
	return omittedImage(ref.MimeType, why)
}

// splitResultImages pulls image refs out of a JSON tool result, returning
// the rest as text; ok is false when the result has no images.
func splitResultImages(raw string) (text string, images []map[string]any, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.Contains(raw, `"image"`) || (!strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
		return "", nil, false
	}
	var parsed any
	if json.Unmarshal([]byte(raw), &parsed) != nil {
		return "", nil, false
	}
	rest, keep := stripImages(parsed, 0, &images)
	if len(images) == 0 {
		return "", nil, false
	}
	if !keep {
		return "", images, true
	}
	if m, isMap := rest.(map[string]any); isMap && len(m) == 0 {
		return "", images, true
	}
	if t, plain := textOnly(rest); plain {
		return t, images, true
	}
	b, _ := json.Marshal(rest)
	return string(b), images, true
}

// stripImages removes image refs from v into images; keep is false when nothing is left.
func stripImages(v any, depth int, images *[]map[string]any) (rest any, keep bool) {
	if depth > 8 {
		return v, true
	}
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "image" {
			url, hasURL := x["url"].(string)
			omitted, hasOmitted := x["omitted"].(string)
			if hasURL || hasOmitted {
				mt, _ := x["mimeType"].(string)
				*images = append(*images, imageBlock(imageRef{URL: url, MimeType: mt, Omitted: omitted}))
				return nil, false
			}
		}
		if x["type"] == "content" {
			if inner, has := x["content"]; has {
				r, k := stripImages(inner, depth+1, images)
				if !k {
					return nil, false
				}
				out := map[string]any{}
				for key, val := range x {
					out[key] = val
				}
				out["content"] = r
				return out, true
			}
		}
		out := map[string]any{}
		for key, val := range x {
			if r, k := stripImages(val, depth+1, images); k {
				out[key] = r
			}
		}
		return out, true
	case []any:
		if len(x) == 0 {
			return x, true
		}
		var out []any
		for _, val := range x {
			if r, k := stripImages(val, depth+1, images); k {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	return v, true
}

// textOnly joins a result made only of text blocks, as MCP tool results are.
func textOnly(v any) (string, bool) {
	blocks, isList := v.([]any)
	if m, isMap := v.(map[string]any); isMap && len(m) == 1 {
		blocks, isList = m["content"].([]any)
	}
	if !isList || len(blocks) == 0 {
		return "", false
	}
	var texts []string
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		if m["type"] == "content" {
			m, _ = m["content"].(map[string]any)
		}
		t, isText := m["text"].(string)
		if m["type"] != "text" || !isText {
			return "", false
		}
		texts = append(texts, t)
	}
	return strings.Join(texts, "\n"), true
}

// fetchAsset reads an image asset as base64, capped at maxImageBytes.
func (c *client) fetchAsset(ctx context.Context, path string) (mimeType, data string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.doWith(ctx, "GET", path, nil, map[string]string{"Accept": "image/*"})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("%s", resp.Status)
	}
	mimeType, _, _ = mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if !strings.HasPrefix(mimeType, "image/") {
		return "", "", fmt.Errorf("not an image (%s)", mimeType)
	}
	if resp.ContentLength > maxImageBytes {
		return "", "", fmt.Errorf("too large (%d bytes, limit %d)", resp.ContentLength, maxImageBytes)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", "", err
	}
	if len(b) > maxImageBytes {
		return "", "", fmt.Errorf("too large (over %d bytes)", maxImageBytes)
	}
	return mimeType, base64.StdEncoding.EncodeToString(b), nil
}

// resolveImages fetches the asset behind each pending image block in u.
func (s *session) resolveImages(ctx context.Context, u update) {
	resolve := func(block map[string]any) map[string]any {
		path, pending := block[assetKey].(string)
		if !pending {
			return block
		}
		mt, data, err := s.c.fetchAsset(ctx, path)
		if err != nil {
			log.Printf("session %s: image %s: %v", s.id, path, err)
			want, _ := block["mimeType"].(string)
			return omittedImage(want, err.Error())
		}
		return map[string]any{"type": "image", "mimeType": mt, "data": data}
	}
	switch c := u["content"].(type) {
	case map[string]any:
		u["content"] = resolve(c)
	case []any:
		for _, item := range c {
			if wrapper, isMap := item.(map[string]any); isMap {
				if inner, isBlock := wrapper["content"].(map[string]any); isBlock {
					wrapper["content"] = resolve(inner)
				}
			}
		}
	}
}
