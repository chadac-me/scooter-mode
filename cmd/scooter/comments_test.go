package main

import "testing"

func filterAll(chunks ...string) string {
	var f commentFilter
	out := ""
	for _, c := range chunks {
		out += f.write(c)
	}
	return out + f.flush()
}

func TestCommentFilter(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"own line", []string{"Done.\n\n<!-- scooter:done -->"}, "Done.\n\n"},
		{"inline", []string{"Done. <!-- x --> ok"}, "Done.  ok"},
		{"split open", []string{"a <!", "-- x --> b"}, "a  b"},
		{"split close", []string{"a <!-- x -", "-> b"}, "a  b"},
		{"split every byte", splitBytes("a<!--x-->b"), "ab"},
		{"unterminated", []string{"a <!-- still streaming"}, "a "},
		{"plain angle", []string{"1 < 2 <b> <!"}, "1 < 2 <b> <!"},
		{"inline code", []string{"use `<!-- x -->` here"}, "use `<!-- x -->` here"},
		{"fenced code", []string{"```html\n<!-- x -->\n``", "`\n<!-- y -->z"}, "```html\n<!-- x -->\n```\nz"},
		{"split backticks", []string{"`", "`<!-- x -->`` <!-- y -->"}, "``<!-- x -->`` "},
		{"multiline", []string{"a<!--\nline\n-->b"}, "ab"},
	}
	for _, c := range cases {
		if got := filterAll(c.chunks...); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func splitBytes(s string) []string {
	out := make([]string, len(s))
	for i := range s {
		out[i] = s[i : i+1]
	}
	return out
}

func TestTranslatorHidesComments(t *testing.T) {
	tr := newTranslator()
	var got []string
	for _, ev := range []aguiEvent{
		{Type: "TEXT_MESSAGE_START", MessageID: "a", Role: "assistant"},
		{Type: "TEXT_MESSAGE_CONTENT", MessageID: "a", Delta: "Done. <!"},
		{Type: "REASONING_MESSAGE_CONTENT", MessageID: "r", Delta: "hm <!-- t -->"},
		{Type: "TEXT_MESSAGE_CONTENT", MessageID: "a", Delta: "-- scooter:done -->"},
		{Type: "REASONING_MESSAGE_END", MessageID: "r"},
		{Type: "TEXT_MESSAGE_END", MessageID: "a"},
	} {
		ups, _ := tr.apply(ev)
		for _, u := range ups {
			got = append(got, u["sessionUpdate"].(string)+":"+u["content"].(map[string]any)["text"].(string))
		}
	}
	want := []string{"agent_message_chunk:Done. ", "agent_thought_chunk:hm "}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(tr.comments) != 0 {
		t.Errorf("filters leaked: %v", tr.comments)
	}
}
