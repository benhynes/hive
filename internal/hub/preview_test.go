package hub

import (
	"strings"
	"testing"
)

func TestNudgeLineIsFixedAndShellInert(t *testing.T) {
	got := nudgeLine()
	if got != nudgeNotice {
		t.Fatalf("nudgeLine() = %q, want fixed notice %q", got, nudgeNotice)
	}
	if !strings.HasPrefix(got, "# ") {
		t.Fatalf("nudge notice is not shell-inert: %q", got)
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("nudge notice contains a line break: %q", got)
	}
}

func TestEmptyPanePromptRejectsDraftsAndUnknownUIs(t *testing.T) {
	for _, screen := range []string{
		"$",
		"history\n  #  \n\n",
		"rendered output\n❯",
		"rendered output\n›\n",
	} {
		if !emptyPanePrompt(screen) {
			t.Errorf("emptyPanePrompt(%q) = false, want true", screen)
		}
	}

	for _, screen := range []string{
		"",
		"ordinary output",
		">", // POSIX shells commonly use this as the continuation (PS2) prompt.
		"$ existing draft",
		"❯ please keep this text",
		"$\nstatus bar",
		"unknown-ui>",
	} {
		if emptyPanePrompt(screen) {
			t.Errorf("emptyPanePrompt(%q) = true; draft/unknown UI must be rejected", screen)
		}
	}
}
