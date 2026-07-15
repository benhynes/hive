package hub

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequireHealthFeatureURL(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    bool
	}{
		{name: "advertised", body: `{"api":"hive","v":1,"features":["explicit_nudge"]}`},
		{name: "legacy", body: `{"api":"hive","v":1}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := requireHealthFeatureURL(srv.URL, "explicit_nudge")
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "explicit_nudge")) {
				t.Fatalf("missing feature error = %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}
}
