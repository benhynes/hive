package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPErrorPreservesStatusThroughWrapping(t *testing.T) {
	c := &Client{
		Addr: "http://test", Net: "dev", Token: "token",
		hc: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Status:     "401 Unauthorized",
				Body:       io.NopCloser(strings.NewReader(`{"error":"expired identity"}`)),
				Header:     make(http.Header),
			}, nil
		})},
	}
	_, err := c.Agents(true)
	if err == nil || err.Error() != "expired identity" {
		t.Fatalf("response error = %v", err)
	}
	if wrapped := fmt.Errorf("request failed: %w", err); !IsHTTPStatus(wrapped, http.StatusUnauthorized) {
		t.Fatalf("wrapped error lost HTTP status: %v", wrapped)
	}
	if IsHTTPStatus(err, http.StatusNotFound) {
		t.Fatal("401 response matched 404")
	}
}

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

func TestResolveHostLocalControl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_TOKEN", "")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	local := strings.Repeat("a", 64)
	msg := strings.Repeat("b", 64)
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"control_host":"vm1","hosts":{"vm1":"127.0.0.1:7777","mac":"127.0.0.1:9999"}}`, msg, local)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg || c.Control != local || c.ControlHost != "vm1" {
		t.Fatalf("resolved token=%q control=%q host=%q", c.Token, c.Control, c.ControlHost)
	}
	if _, err := c.controlToken("vm1"); err != nil {
		t.Fatalf("local control rejected: %v", err)
	}
	if _, err := c.controlToken("mac"); err == nil || !strings.Contains(err.Error(), "scoped") {
		t.Fatalf("remote control not rejected locally: %v", err)
	}
}

func TestResolveSharedControlUsesMessageToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	t.Setenv("HIVE_TOKEN", "")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"mac","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	shared := strings.Repeat("c", 64)
	msg := strings.Repeat("d", 64)
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"hosts":{"mac":"127.0.0.1:7777"}}`, msg, shared)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != msg || c.Control != shared || c.ControlHost != "" {
		t.Fatalf("resolved token=%q control=%q host=%q", c.Token, c.Control, c.ControlHost)
	}
}

func TestResolveAgentTokenDoesNotInheritControl(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HIVE_HOME", home)
	t.Setenv("HIVE_ADDR", "")
	t.Setenv("HIVE_NET", "dev")
	personal := strings.Repeat("e", 64)
	t.Setenv("HIVE_TOKEN", personal)
	t.Setenv("HIVE_AGENT", "worker@vm1")
	t.Setenv("HIVE_CONTROL_TOKEN", "")
	t.Setenv("HIVE_CONTROL_HOST", "")
	if err := os.WriteFile(home+"/config.json", []byte(`{"host_name":"vm1","bind":"127.0.0.1","port":7777}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home+"/nets/dev", 0o700); err != nil {
		t.Fatal(err)
	}
	local := strings.Repeat("a", 64)
	msg := strings.Repeat("b", 64)
	netJSON := fmt.Sprintf(`{"name":"dev","msg_token":%q,"control_token":%q,"control_host":"vm1","hosts":{"vm1":"127.0.0.1:7777"}}`, msg, local)
	if err := os.WriteFile(home+"/nets/dev/net.json", []byte(netJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != personal {
		t.Fatalf("resolved token=%q, want personal token", c.Token)
	}
	if c.HasControl() || c.ControlHost != "" {
		t.Fatalf("agent inherited control=%q host=%q", c.Control, c.ControlHost)
	}
	if _, err := c.controlToken("vm1"); err == nil {
		t.Fatal("agent-scoped client unexpectedly obtained control")
	}
}

func TestRegisterSelectsCredentialByBinding(t *testing.T) {
	var gotAuth string
	var gotNudge bool
	requests := 0
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/health" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"api":"hive","v":1,"features":["explicit_nudge"]}`)), Header: make(http.Header)}, nil
		}
		requests++
		gotAuth = r.Header.Get("Authorization")
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			return nil, err
		}
		gotNudge, _ = body["nudge"].(bool)
		response := `{"agent":"worker@testhost","token":"personal","nudge_policy":"explicit"}`
		if gotNudge {
			response = `{"agent":"worker@testhost","token":"personal","nudge":true,"nudge_policy":"explicit"}`
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(response)),
			Header:     make(http.Header),
		}, nil
	})}

	c := &Client{
		Addr: "http://test", Net: "dev", Token: "msg-token",
		Control: "control-token", ControlHost: "testhost", self: "testhost",
		hc: hc,
	}

	if _, err := c.Register("messageonly", "", 0); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer msg-token" {
		t.Fatalf("message-only registration used %q, want MSG token", gotAuth)
	}
	if _, err := c.Register("pidbound", "", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer msg-token" {
		t.Fatalf("PID-bound registration used %q, want MSG token", gotAuth)
	}
	if _, err := c.Register("panebound", "%1", 0); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer control-token" {
		t.Fatalf("pane-bound registration used %q, want local CONTROL token", gotAuth)
	}
	if _, err := c.RegisterWithNudge("nudged", "%3", 0, true); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer control-token" || !gotNudge {
		t.Fatalf("nudge registration used auth=%q nudge=%v, want local CONTROL + explicit opt-in", gotAuth, gotNudge)
	}
	before := requests
	if _, err := c.RegisterWithNudge("nopane", "", 0, true); err == nil || !strings.Contains(err.Error(), "explicitly bound pane") {
		t.Fatalf("nudge registration without pane returned %v", err)
	}
	if requests != before {
		t.Fatal("nudge registration without a pane reached the server")
	}

	c.Control = ""
	before = requests
	if _, err := c.Register("denied", "%2", 0); err == nil || !strings.Contains(err.Error(), "control token required") {
		t.Fatalf("pane registration without CONTROL returned %v", err)
	}
	if requests != before {
		t.Fatal("pane registration without CONTROL reached the server")
	}
}

func TestPaneRegistrationPreflightsExplicitNudgePolicy(t *testing.T) {
	for _, nudge := range []bool{false, true} {
		t.Run(fmt.Sprintf("nudge=%v", nudge), func(t *testing.T) {
			var paths []string
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				body := `{"api":"hive","v":1}`
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			c := &Client{Addr: "http://test", Net: "dev", Token: "msg", Control: "control", self: "testhost", hc: hc}
			_, err := c.RegisterWithNudge("worker", "%1", 0, nudge)
			if err == nil || !strings.Contains(err.Error(), "explicit_nudge") {
				t.Fatalf("legacy pane registration result = %v", err)
			}
			if len(paths) != 1 || paths[0] != "/v1/health" {
				t.Fatalf("requests = %v, want read-only health preflight", paths)
			}
		})
	}
}

func TestSpawnPreflightsExplicitNudgePolicy(t *testing.T) {
	for _, nudge := range []bool{false, true} {
		t.Run(fmt.Sprintf("nudge=%v", nudge), func(t *testing.T) {
			var paths []string
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				body := `{"api":"hive","v":1}`
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			c := &Client{Addr: "http://test", Net: "dev", Token: "msg", Control: "control", self: "testhost", hc: hc}
			_, err := c.SpawnWithNudge("", "worker", []string{"sh"}, "", "", false, false, false, nudge, false)
			if err == nil || !strings.Contains(err.Error(), "explicit_nudge") {
				t.Fatalf("legacy spawn result = %v", err)
			}
			if len(paths) != 1 || paths[0] != "/v1/health" {
				t.Fatalf("requests = %v, want read-only health preflight", paths)
			}
		})
	}
}

func TestSpawnWithOptionsSendsReplace(t *testing.T) {
	var spawnBody map[string]any
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{"api":"hive","v":1,"features":["explicit_nudge"]}`
		if strings.HasSuffix(r.URL.Path, "/spawn/v2") {
			if err := json.NewDecoder(r.Body).Decode(&spawnBody); err != nil {
				t.Fatal(err)
			}
			body = `{"agent":"worker@testhost","session":"s","pane":"%1","nudge_policy":"explicit"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	c := &Client{Addr: "http://test", Net: "dev", Token: "msg", Control: "control", self: "testhost", hc: hc}
	if _, err := c.SpawnWithOptions(SpawnOptions{Name: "worker", Profile: "codex", Replace: true}); err != nil {
		t.Fatal(err)
	}
	if got, ok := spawnBody["replace"].(bool); !ok || !got {
		t.Fatalf("spawn body replace = %#v", spawnBody["replace"])
	}
}

func TestAdvertisedNudgePolicyMismatchCleansUpOnlyMintedRegistration(t *testing.T) {
	for _, operation := range []string{"register", "spawn"} {
		t.Run(operation, func(t *testing.T) {
			var paths []string
			var deregisterAuth string
			hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				paths = append(paths, r.URL.Path)
				body := `{"api":"hive","v":1,"features":["explicit_nudge"]}`
				if strings.HasSuffix(r.URL.Path, "/register/v2") {
					body = `{"agent":"worker@testhost","token":"personal-token"}`
				}
				if strings.HasSuffix(r.URL.Path, "/spawn/v2") {
					// This may be an existing persistent worker, so a compatibility
					// failure must never try to kill or forget it.
					body = `{"agent":"worker@testhost","session":"s","pane":"%1","ready":true}`
				}
				if strings.HasSuffix(r.URL.Path, "/deregister") {
					deregisterAuth = r.Header.Get("Authorization")
					body = `{"ok":true}`
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			c := &Client{Addr: "http://test", Net: "dev", Token: "msg", Control: "control", self: "testhost", hc: hc}
			var err error
			if operation == "register" {
				_, err = c.Register("worker", "%1", 0)
			} else {
				_, err = c.Spawn("", "worker", []string{"sh"}, "", "", false, false, false, true)
			}
			if err == nil || !strings.Contains(err.Error(), "did not honor") {
				t.Fatalf("policy mismatch result = %v", err)
			}
			if operation == "register" {
				want := []string{"/v1/health", "/v1/nets/dev/register/v2", "/v1/nets/dev/deregister"}
				if !reflect.DeepEqual(paths, want) {
					t.Fatalf("requests = %v, want %v", paths, want)
				}
				if deregisterAuth != "Bearer personal-token" {
					t.Fatalf("registration cleanup auth = %q, want minted token", deregisterAuth)
				}
			} else {
				want := []string{"/v1/health", "/v1/nets/dev/spawn/v2"}
				if !reflect.DeepEqual(paths, want) {
					t.Fatalf("requests = %v, want %v and no destructive cleanup", paths, want)
				}
			}
		})
	}
}

func TestRegisterLeaseRejectsLegacyDaemonAndCleansUp(t *testing.T) {
	var paths, auth []string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		auth = append(auth, r.Header.Get("Authorization"))
		body := `{"agent":"worker@testhost","token":"personal-token"}`
		if strings.HasSuffix(r.URL.Path, "/deregister") {
			body = `{"ok":true}`
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
	c := &Client{Addr: "http://test", Net: "dev", Token: "msg-token", hc: hc}

	_, err := c.RegisterLease("worker", "", 0, DefaultLeaseSeconds)
	if err == nil || !strings.Contains(err.Error(), "upgrade/restart") {
		t.Fatalf("legacy daemon lease result = %v, want upgrade error", err)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/register") || !strings.HasSuffix(paths[1], "/deregister") {
		t.Fatalf("requests = %v, want register then cleanup deregister", paths)
	}
	if auth[0] != "Bearer msg-token" || auth[1] != "Bearer personal-token" {
		t.Fatalf("authorization = %v, want bootstrap then minted token", auth)
	}
}

func TestRegisterEphemeralLeaseRejectsPreEphemeralDaemonAndCleansUp(t *testing.T) {
	var paths []string
	var registerBody string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		body := fmt.Sprintf(`{"agent":"generated@testhost","token":"personal-token","lease_seconds":%d,"lease_expires":999999}`, DefaultLeaseSeconds)
		if strings.HasSuffix(r.URL.Path, "/deregister") {
			body = `{"ok":true}`
		} else {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			registerBody = string(b)
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})}
	c := &Client{Addr: "http://test", Net: "dev", Token: "msg-token", hc: hc}

	_, err := c.RegisterEphemeralLease("generated", DefaultLeaseSeconds)
	if err == nil || !strings.Contains(err.Error(), "ephemeral registration") || !strings.Contains(err.Error(), "upgrade/restart") {
		t.Fatalf("pre-ephemeral daemon result = %v, want upgrade error", err)
	}
	if !strings.Contains(registerBody, `"ephemeral":true`) {
		t.Fatalf("register body = %s, want ephemeral flag", registerBody)
	}
	if len(paths) != 2 || !strings.HasSuffix(paths[0], "/register") || !strings.HasSuffix(paths[1], "/deregister") {
		t.Fatalf("requests = %v, want register then cleanup deregister", paths)
	}
}
