package config

import "testing"

func TestControlForHost(t *testing.T) {
	shared := NetConfig{ControlToken: "shared"}
	if shared.ControlFor("a") != "shared" || shared.ControlFor("b") != "shared" {
		t.Fatal("legacy control token did not remain network-wide")
	}
	local := NetConfig{ControlToken: "local", ControlHost: "a"}
	if local.ControlFor("a") != "local" {
		t.Fatal("host-local token unavailable on its bound host")
	}
	if local.ControlFor("b") != "" {
		t.Fatal("host-local token escaped to a peer")
	}
}
