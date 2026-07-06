package tmux

// These tests drive a real tmux server on a dedicated socket so the
// user's tmux is never touched. They are the ground truth for the
// quoting rules: payloads travel send-keys/paste/env → pane → file,
// and the file must match byte-for-byte.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := fmt.Sprintf("hivetest-%d", os.Getpid())
	t.Setenv("HIVE_TMUX_SOCKET", sock)
	t.Cleanup(func() {
		exec.Command("tmux", "-L", sock, "kill-server").Run()
	})
}

func waitFile(t *testing.T, path string, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			last = b
			if string(b) == want {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("file %s:\n got %q\nwant %q", path, last, want)
}

var nasty = []struct{ name, payload string }{
	{"plain", "hello world"},
	{"singlequote", `it's a 'test'`},
	{"doublequote", `say "hi" to $USER`},
	{"subshell", `$(rm -rf /) and ` + "`echo pwned`"},
	{"leadingdash", `-rf --no-preserve-root`},
	{"unicode", `héllo wörld — 日本語 🐝`},
	{"semicolons", `a; b && c | d > e < f`},
	{"backslashes", `C:\path\to\thing \n not-a-newline`},
}

func TestSendKeysLiteralQuoting(t *testing.T) {
	setup(t)
	dir := t.TempDir()
	for i, tc := range nasty {
		out := filepath.Join(dir, fmt.Sprintf("out%d", i))
		sess := fmt.Sprintf("q%d", i)
		if _, err := NewSession(sess, "", nil, []string{"sh", "-c", "cat > " + out}); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		pane, err := FirstPane(sess)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(150 * time.Millisecond) // let cat start
		if err := SendKeysLiteral(pane, tc.payload); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if err := Enter(pane); err != nil {
			t.Fatal(err)
		}
		if _, err := run("send-keys", "-t", pane, "C-d"); err != nil {
			t.Fatal(err)
		}
		waitFile(t, out, tc.payload+"\n")
		KillSession(sess)
	}
}

func TestPasteMultiline(t *testing.T) {
	setup(t)
	out := filepath.Join(t.TempDir(), "out")
	if _, err := NewSession("paste", "", nil, []string{"sh", "-c", "cat > " + out}); err != nil {
		t.Fatal(err)
	}
	pane, _ := FirstPane("paste")
	time.Sleep(150 * time.Millisecond)
	payload := "line one\nline 'two' with $(stuff)\nline three"
	if err := Paste(pane, payload); err != nil {
		t.Fatal(err)
	}
	Enter(pane)
	run("send-keys", "-t", pane, "C-d")
	waitFile(t, out, payload+"\n")
}

func TestEnvInjection(t *testing.T) {
	setup(t)
	out := filepath.Join(t.TempDir(), "out")
	env := map[string]string{
		"HIVE_TOKEN": `s3cr3t with spaces 'quotes' "and" $dollars`,
		"HIVE_NET":   "dev",
	}
	cmd := []string{"sh", "-c", `printf '%s|%s' "$HIVE_TOKEN" "$HIVE_NET" > ` + out + `; sleep 60`}
	if _, err := NewSession("envt", "", env, cmd); err != nil {
		t.Fatal(err)
	}
	waitFile(t, out, env["HIVE_TOKEN"]+"|dev")
}

func TestPaneLifecycle(t *testing.T) {
	setup(t)
	pane, err := NewSession("life", "", nil, []string{"sleep", "60"})
	if err != nil {
		t.Fatal(err)
	}
	if !PaneExists(pane) {
		t.Fatal("pane should exist")
	}
	pid, err := PanePID(pane)
	if err != nil || pid <= 0 {
		t.Fatalf("pid=%d err=%v", pid, err)
	}
	epoch, err := ProcStartEpoch(pid)
	if err != nil || epoch == "" {
		t.Fatalf("epoch=%q err=%v", epoch, err)
	}
	// Same pid re-probed gives the same epoch (identity is stable).
	epoch2, _ := ProcStartEpoch(pid)
	if epoch != epoch2 {
		t.Fatalf("epoch changed: %q -> %q", epoch, epoch2)
	}
	if !Quiescent(pane, 100*time.Millisecond) {
		t.Fatal("sleeping pane should be quiescent")
	}
	if err := KillSession("life"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for PaneExists(pane) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if PaneExists(pane) {
		t.Fatal("pane survived kill")
	}
}

// A dead pane must fail WaitQuiescent promptly instead of hot-spinning
// tmux forks until the deadline.
func TestWaitQuiescentDeadPane(t *testing.T) {
	setup(t)
	pane, err := NewSession("wq-dead", "", nil, []string{"cat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := KillSession("wq-dead"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if WaitQuiescent(pane, 100*time.Millisecond, 10*time.Second) {
		t.Fatal("dead pane reported quiescent")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("dead pane held WaitQuiescent for %v (want fast bail-out)", d)
	}
}

func TestVersionCheck(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	if err := Available(); err != nil {
		t.Fatal(err)
	}
}
