//go:build !windows

package hub_test

// Live pane streaming against a real tmux server: spawn a pane, open
// /stream, check the snapshot arrives, type into the pane, and check
// the typed output shows up as a delta on the same stream.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestStreamSnapshotAndDeltas(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := fmt.Sprintf("hive-stream-test-%d", time.Now().UnixNano())
	t.Setenv("HIVE_TMUX_SOCKET", sock)
	t.Cleanup(func() { exec.Command("tmux", "-L", sock, "kill-server").Run() })
	srv, nc := newTestNet(t)

	code, out := call(t, srv, "POST", "/v1/nets/dev/spawn", nc.ControlToken,
		map[string]any{"name": "streamer", "cmd": []string{"cat"}})
	if code != 200 {
		t.Fatalf("spawn: %d %v", code, out)
	}

	// Put a marker on screen before the stream opens, so the snapshot
	// must contain it.
	code, _ = call(t, srv, "POST", "/v1/nets/dev/keys", nc.ControlToken,
		map[string]any{"agent": "streamer", "text": "snapshot-marker", "enter": true})
	if code != 200 {
		t.Fatalf("keys: %d", code)
	}
	time.Sleep(300 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/nets/dev/stream?agent=streamer", nil)
	req.Header.Set("Authorization", "Bearer "+nc.ControlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("stream: %d %s", resp.StatusCode, b)
	}
	if c := resp.Header.Get("X-Hive-Cols"); c == "" || c == "0" {
		t.Fatalf("missing pane geometry, cols=%q", c)
	}

	got := make(chan string, 64)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				got <- string(buf[:n])
			}
			if err != nil {
				close(got)
				return
			}
		}
	}()

	waitFor := func(what string) string {
		var all strings.Builder
		deadline := time.After(10 * time.Second)
		for {
			select {
			case chunk, ok := <-got:
				if !ok {
					t.Fatalf("stream closed before %q arrived; got: %q", what, all.String())
				}
				all.WriteString(chunk)
				if strings.Contains(all.String(), what) {
					return all.String()
				}
			case <-deadline:
				t.Fatalf("timeout waiting for %q; got: %q", what, all.String())
			}
		}
	}
	waitFor("snapshot-marker")

	// Raw keys typed after the snapshot must arrive as live deltas
	// (cat echoes them back to the pane).
	code, _ = call(t, srv, "POST", "/v1/nets/dev/keys", nc.ControlToken,
		map[string]any{"agent": "streamer", "text": "delta-marker\r", "raw": true})
	if code != 200 {
		t.Fatalf("raw keys: %d", code)
	}
	waitFor("delta-marker")
}
