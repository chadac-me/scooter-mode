package main

import "strings"

// commentFilter drops HTML comments from streamed markdown, as Scooter's UI does.
// Comments inside backtick code stay. Text that may start a comment or
// a code fence is held until the next chunk shows what it is.
type commentFilter struct {
	held      string
	inComment bool
	// Length of the backtick run that opened the current code span, or 0.
	fence int
}

const commentOpen, commentClose = "<!--", "-->"

// write returns the part of text that is safe to show now.
func (f *commentFilter) write(text string) string {
	buf := f.held + text
	f.held = ""
	var out strings.Builder
	for i := 0; i < len(buf); {
		if f.inComment {
			end := strings.Index(buf[i:], commentClose)
			if end < 0 {
				f.held = tail(buf[i:], len(commentClose)-1)
				break
			}
			i += end + len(commentClose)
			f.inComment = false
			continue
		}
		next := strings.IndexAny(buf[i:], "<`")
		if f.fence > 0 {
			next = strings.IndexByte(buf[i:], '`')
		}
		if next < 0 {
			out.WriteString(buf[i:])
			break
		}
		out.WriteString(buf[i : i+next])
		i += next
		if buf[i] == '`' {
			n := len(buf[i:]) - len(strings.TrimLeft(buf[i:], "`"))
			if i+n == len(buf) {
				f.held = buf[i:]
				break
			}
			out.WriteString(buf[i : i+n])
			i += n
			switch f.fence {
			case 0:
				f.fence = n
			case n:
				f.fence = 0
			}
			continue
		}
		switch rest := buf[i:]; {
		case strings.HasPrefix(rest, commentOpen):
			f.inComment = true
			i += len(commentOpen)
		case strings.HasPrefix(commentOpen, rest):
			f.held = rest
			i = len(buf)
		default:
			out.WriteByte('<')
			i++
		}
	}
	return out.String()
}

// flush returns held text at the end of a message; an unclosed comment stays hidden.
func (f *commentFilter) flush() string {
	held := f.held
	if f.inComment {
		held = ""
	}
	*f = commentFilter{}
	return held
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
