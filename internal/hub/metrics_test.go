package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/benhynes/hive/internal/config"
	"github.com/benhynes/hive/internal/proto"
)

func TestMetricsExposeAggregateStateWithoutAgentIdentity(t *testing.T) {
	t.Setenv("HIVE_HOME", t.TempDir())
	msgToken := proto.NewToken()
	controlToken := proto.NewToken()
	if err := config.SaveNet(config.NetConfig{
		Name: "dev", MsgToken: msgToken, ControlToken: controlToken,
		ControlHost: "testhost", Hosts: map[string]string{"testhost": "127.0.0.1:7777"},
	}); err != nil {
		t.Fatal(err)
	}
	h := New(config.Config{HostName: "testhost"})
	handler := h.Handler()

	req := httptest.NewRequest(http.MethodPost, "/v1/nets/dev/register", strings.NewReader(`{"name":"secret-agent"}`))
	req.Header.Set("Authorization", "Bearer "+msgToken)
	reg := httptest.NewRecorder()
	handler.ServeHTTP(reg, req)
	if reg.Code != http.StatusOK {
		t.Fatalf("register: %d %s", reg.Code, reg.Body.String())
	}

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("metrics: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{"secret-agent", msgToken, controlToken} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics leaked %q:\n%s", secret, body)
		}
	}
	for _, want := range []string{
		`hive_up{host="testhost"} 1`,
		`hive_agents{host="testhost",network="dev",state="alive"} 1`,
		`hive_inbox_lag_messages{host="testhost",network="dev"} 0`,
		`hive_persistent_sessions_ready{host="testhost",network="dev"} 0`,
		`hive_control_scope{host="testhost",network="dev",scope="host_local"} 1`,
		`hive_metrics_scrape_error{host="testhost",network="dev"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
}
