package main

import "testing"

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