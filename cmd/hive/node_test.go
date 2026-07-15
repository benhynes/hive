package main

import (
	"strings"
	"testing"

	"github.com/benhynes/hive/internal/config"
)

func TestSelectNodeControl(t *testing.T) {
	nc := config.NetConfig{MsgToken: strings.Repeat("a", 64), ControlToken: strings.Repeat("b", 64)}

	mode, tok, err := selectNodeControl(nc, false, false)
	if err != nil || mode != nodeControlShared || tok != nc.ControlToken {
		t.Fatalf("default = %v %q %v, want shared source token", mode, tok, err)
	}
	mode, tok, err = selectNodeControl(nc, true, false)
	if err != nil || mode != nodeControlNone || tok != "" {
		t.Fatalf("msg-only = %v %q %v", mode, tok, err)
	}
	if _, _, err := selectNodeControl(nc, true, true); err == nil {
		t.Fatal("--msg-only with --local-control was accepted")
	}

	mode, first, err := selectNodeControl(nc, false, true)
	if err != nil || mode != nodeControlLocal || len(first) != 64 {
		t.Fatalf("local-control = %v %q %v", mode, first, err)
	}
	_, second, _ := selectNodeControl(nc, false, true)
	if first == nc.ControlToken || first == nc.MsgToken || first == second {
		t.Fatal("local-control token was not fresh and independent")
	}
}

func TestSelectNodeControlDoesNotCopyLocalSource(t *testing.T) {
	nc := config.NetConfig{
		MsgToken: "msg", ControlToken: "host-a-token", ControlHost: "host-a",
	}
	mode, tok, err := selectNodeControl(nc, false, false)
	if err != nil || mode != nodeControlNone || tok != "" {
		t.Fatalf("local source default = %v %q %v, want msg-only", mode, tok, err)
	}
	mode, tok, err = selectNodeControl(nc, false, true)
	if err != nil || mode != nodeControlLocal || tok == "" || tok == nc.ControlToken {
		t.Fatalf("explicit local child = %v %q %v", mode, tok, err)
	}
}

func TestPlatformOf(t *testing.T) {
	cases := []struct {
		s, m, goos, goarch string
		wantErr            bool
	}{
		{"Linux", "x86_64", "linux", "amd64", false},
		{"Linux", "aarch64", "linux", "arm64", false},
		{"Darwin", "arm64", "darwin", "arm64", false},
		{"Darwin", "x86_64", "darwin", "amd64", false},
		{"FreeBSD", "amd64", "", "", true},
		{"Linux", "riscv64", "", "", true},
	}
	for _, c := range cases {
		goos, goarch, err := platformOf(c.s, c.m)
		if (err != nil) != c.wantErr || goos != c.goos || goarch != c.goarch {
			t.Errorf("platformOf(%q,%q) = %q,%q,%v", c.s, c.m, goos, goarch, err)
		}
	}
}

func TestSeedHosts(t *testing.T) {
	local := map[string]string{
		"mac": "127.0.0.1:7777", // our loopback self-entry
		"vm1": "100.1.2.3:7777",
	}
	got := seedHosts(local, "mac", "100.9.9.9:7777", "vm2", 7878)
	want := map[string]string{
		"mac": "100.9.9.9:7777", // replaced with the reachable addr
		"vm1": "100.1.2.3:7777", // carried over
		"vm2": "127.0.0.1:7878", // the node's own loopback entry
	}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("hosts[%s] = %q, want %q", k, got[k], v)
		}
	}
	if local["mac"] != "127.0.0.1:7777" {
		t.Error("seedHosts mutated its input")
	}
}

func TestParseWinProbe(t *testing.T) {
	c, arch, profile, err := parseWinProbe("Windows_NT DESKTOP-TFS0UOF AMD64 C:\\Users\\digiwin\r\n")
	if err != nil || c != "DESKTOP-TFS0UOF" || arch != "amd64" || profile != `C:\Users\digiwin` {
		t.Fatalf("got %q %q %q %v", c, arch, profile, err)
	}
	// Profiles may contain spaces; they're last so they survive the split.
	_, _, profile, err = parseWinProbe(`Windows_NT PC ARM64 C:\Users\John Smith`)
	if err != nil || profile != `C:\Users\John Smith` {
		t.Fatalf("spaced profile: %q %v", profile, err)
	}
	if _, _, _, err := parseWinProbe("Windows_NT PC x86 C:\\Users\\a"); err == nil {
		t.Fatal("32-bit x86 accepted")
	}
	if _, _, _, err := parseWinProbe("%OS% %COMPUTERNAME% %PROCESSOR_ARCHITECTURE% %USERPROFILE%"); err == nil {
		t.Fatal("unexpanded cmd variables accepted")
	}
}

func TestWinPath(t *testing.T) {
	if p, err := winPath(`C:\Users\digiwin\.hive\`, "x"); err != nil || p != `C:\Users\digiwin\.hive` {
		t.Fatalf("got %q %v", p, err)
	}
	for _, bad := range []string{`C:\Users\John Smith\.hive`, `C:\a'b`, `\\server\share`, `relative\path`, `C:\a"b`} {
		if _, err := winPath(bad, "x"); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestWinPS(t *testing.T) {
	// "hi" in UTF-16LE is 68 00 69 00 → aABpAA== in base64.
	if got := winPS("hi"); got != "powershell -NoProfile -NonInteractive -EncodedCommand aABpAA==" {
		t.Fatalf("got %q", got)
	}
}

func TestRemotePath(t *testing.T) {
	if p, err := remotePath("~/.local/bin/hive"); err != nil || p != "$HOME/.local/bin/hive" {
		t.Errorf("tilde expansion: %q %v", p, err)
	}
	if _, err := remotePath(`$HOME/bin"; rm -rf /; "`); err == nil {
		t.Error("quote injection accepted")
	}
	if _, err := remotePath("$HOME/with space/hive"); err == nil {
		t.Error("space accepted")
	}
	if p, err := remotePath("/usr/local/bin/hive"); err != nil || p != "/usr/local/bin/hive" {
		t.Errorf("absolute path: %q %v", p, err)
	}
}
