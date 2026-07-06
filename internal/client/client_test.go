package client

import (
	"fmt"
	"os"
	"testing"
)

// A daemon bound to a specific address doesn't listen on loopback, so
// the default HIVE_ADDR must follow the configured bind.
func TestResolveAddrFollowsBind(t *testing.T) {
	cases := []struct {
		bind, want string
	}{
		{"192.168.1.9", "http://192.168.1.9:7345"},
		{"0.0.0.0", "http://127.0.0.1:7345"},
		{"::", "http://127.0.0.1:7345"},
		{"fd07::fe", "http://[fd07::fe]:7345"},
	}
	for _, c := range cases {
		home := t.TempDir()
		t.Setenv("HIVE_HOME", home)
		t.Setenv("HIVE_ADDR", "")
		t.Setenv("HIVE_NET", "dev")
		t.Setenv("HIVE_TOKEN", "x")
		cfg := fmt.Sprintf(`{"host_name":"h","bind":%q,"port":7345}`, c.bind)
		if err := os.WriteFile(home+"/config.json", []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		cl, err := Resolve("")
		if err != nil {
			t.Fatalf("bind %q: %v", c.bind, err)
		}
		if cl.Addr != c.want {
			t.Errorf("bind %q: addr %q, want %q", c.bind, cl.Addr, c.want)
		}
	}
}