package hub

import (
	"reflect"
	"testing"

	"github.com/benhynes/hive/internal/config"
)

func TestWrapSandboxCommand(t *testing.T) {
	t.Parallel()
	got, err := wrapSandboxCommand(config.SandboxRunner{
		Command: "/usr/local/bin/ff", Profiles: "/etc/forcefield/runner.yaml", Profile: "codex-worker",
	}, "worker-2", "/srv/agents/worker-2", []string{"/usr/local/bin/codex", "--quiet"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/local/bin/ff", "run", "--profiles", "/etc/forcefield/runner.yaml",
		"--profile", "codex-worker", "--agent", "worker-2",
		"--workspace", "/srv/agents/worker-2", "--",
		"/usr/local/bin/codex", "--quiet",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wrapped command = %#v, want %#v", got, want)
	}
}

func TestWrapSandboxCommandRejectsUntrustedSelection(t *testing.T) {
	t.Parallel()
	for _, sandbox := range []config.SandboxRunner{
		{Command: "ff", Profiles: "/etc/forcefield/runner.yaml", Profile: "worker"},
		{Command: "/usr/local/bin/ff", Profiles: "runner.yaml", Profile: "worker"},
		{Command: "/usr/local/bin/ff", Profiles: "/etc/forcefield/runner.yaml", Profile: "../worker"},
	} {
		if _, err := wrapSandboxCommand(sandbox, "worker", "/srv/worker", []string{"/bin/true"}); err == nil {
			t.Fatalf("sandbox was accepted: %#v", sandbox)
		}
	}
	if _, err := wrapSandboxCommand(config.SandboxRunner{
		Command: "/usr/local/bin/ff", Profiles: "/etc/forcefield/runner.yaml", Profile: "worker",
	}, "worker", "", []string{"/bin/true"}); err == nil {
		t.Fatal("sandbox without workspace was accepted")
	}
}
