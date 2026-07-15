package hub

import (
	"strings"
	"testing"
)

func TestPreviewIsSingleLineAndCapped(t *testing.T) {
	cases := []struct {
		name, in string
		want     string // exact, when short enough to state
		check    func(string) bool
	}{
		{name: "plain", in: "the build is green", want: "the build is green"},
		{
			name: "newlines folded",
			in:   "line one\nline two\r\n\tline three",
			want: "line one line two line three",
		},
		{
			name:  "long body is truncated with an ellipsis",
			in:    strings.Repeat("x", 400),
			check: func(s string) bool { return len(s) <= nudgePreviewMax+4 && strings.HasSuffix(s, "…") },
		},
		{
			name:  "multibyte not split mid-rune",
			in:    strings.Repeat("é", 400), // 2 bytes each
			check: func(s string) bool { return strings.HasSuffix(s, "…") && utf8Valid(s) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := preview(c.in)
			// The one invariant that actually matters for pane safety: never a
			// raw newline, or the injected text would submit early.
			if strings.ContainsAny(got, "\n\r") {
				t.Fatalf("preview leaked a line break: %q", got)
			}
			if c.want != "" && got != c.want {
				t.Fatalf("preview(%q) = %q, want %q", c.in, got, c.want)
			}
			if c.check != nil && !c.check(got) {
				t.Fatalf("preview(%q) = %q failed its check", c.in, got)
			}
		})
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
