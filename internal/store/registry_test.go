package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryRenewLeasePersists(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := AgentRec{
		Name: "worker", TokenHash: "token-hash", Registered: 1,
		LeaseSeconds: 45, LastSeen: 1, LeaseExpires: 45_001,
	}
	if err := r.Put(rec); err != nil {
		t.Fatal(err)
	}

	now := time.UnixMilli(123_456)
	got, ok, err := r.RenewLease("worker", "token-hash", now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("renew reported existing agent missing")
	}
	if got.LastSeen != now.UnixMilli() {
		t.Fatalf("last_seen=%d, want %d", got.LastSeen, now.UnixMilli())
	}
	wantExpiry := now.Add(45 * time.Second).UnixMilli()
	if got.LeaseExpires != wantExpiry {
		t.Fatalf("lease_expires=%d, want %d", got.LeaseExpires, wantExpiry)
	}
	if byToken, ok := r.ByToken("token-hash"); !ok || byToken.LeaseExpires != wantExpiry {
		t.Fatalf("token index lost renewed record: ok=%v rec=%+v", ok, byToken)
	}

	reopened, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted, ok := reopened.Get("worker"); !ok || persisted.LastSeen != now.UnixMilli() || persisted.LeaseExpires != wantExpiry {
		t.Fatalf("renewal not persisted: ok=%v rec=%+v", ok, persisted)
	}
}

func TestRegistryRenewLeaseIsBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	// This is the pre-lease on-disk shape. Missing lease fields must retain
	// the old trusted-until-deregistered behavior.
	if err := os.WriteFile(dir+"/registry.json", []byte(`{
  "legacy": {"name":"legacy","token_hash":"old","registered":7}
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.RenewLease("legacy", "old", time.UnixMilli(999))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("legacy record disappeared")
	}
	if got.LeaseSeconds != 0 || got.LastSeen != 0 || got.LeaseExpires != 0 {
		t.Fatalf("legacy renewal should be a no-op: %+v", got)
	}
	if _, ok, err := r.RenewLease("missing", "old", time.Now()); err != nil || ok {
		t.Fatalf("missing renewal: ok=%v err=%v", ok, err)
	}
	if _, ok, err := r.RenewLease("legacy", "replaced-token", time.Now()); err != nil || ok {
		t.Fatalf("mismatched-token renewal: ok=%v err=%v", ok, err)
	}
}

func TestRegistryReleaseLeaseKeepsRetainedIdentityOffline(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(50_000)
	rec := AgentRec{
		Name: "worker", TokenHash: "token", LeaseSeconds: 60,
		LeaseExpires: now.Add(time.Minute).UnixMilli(),
	}
	if err := r.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := r.ReleaseLease(rec.Name, rec.TokenHash, now)
	if err != nil || !ok {
		t.Fatalf("release: ok=%v err=%v", ok, err)
	}
	if got.LeaseExpires != now.UnixMilli() {
		t.Fatalf("released expiry=%d, want %d", got.LeaseExpires, now.UnixMilli())
	}
	if _, ok := r.Get(rec.Name); !ok {
		t.Fatal("release deleted retained identity")
	}
	reopened, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if persisted, ok := reopened.Get(rec.Name); !ok || persisted.LeaseExpires != now.UnixMilli() {
		t.Fatalf("release not persisted: ok=%v rec=%+v", ok, persisted)
	}
	if _, ok, err := r.ReleaseLease(rec.Name, "stale", now); err != nil || ok {
		t.Fatalf("stale release: ok=%v err=%v", ok, err)
	}
	for _, unsupported := range []AgentRec{
		{Name: "legacy", TokenHash: "legacy"},
		{Name: "generated", TokenHash: "generated", Ephemeral: true, LeaseSeconds: 60},
	} {
		if err := r.Put(unsupported); err != nil {
			t.Fatal(err)
		}
		if _, ok, err := r.ReleaseLease(unsupported.Name, unsupported.TokenHash, now); !ok || err == nil {
			t.Fatalf("unsupported release %s: ok=%v err=%v", unsupported.Name, ok, err)
		}
	}
}

func TestRegistryPrunesOnlyExpiredEphemeral(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(100_000)
	records := []AgentRec{
		{Name: "expired-generated", TokenHash: "expired-token", Ephemeral: true, LeaseSeconds: 45, LeaseExpires: now.Add(-time.Millisecond).UnixMilli()},
		{Name: "live-generated", TokenHash: "live-token", Ephemeral: true, LeaseSeconds: 45, LeaseExpires: now.Add(time.Second).UnixMilli()},
		{Name: "expired-named", TokenHash: "named-token", LeaseSeconds: 45, LeaseExpires: now.Add(-time.Second).UnixMilli()},
		{Name: "unleased-generated", TokenHash: "unleased-token", Ephemeral: true},
	}
	for _, rec := range records {
		if err := r.Put(rec); err != nil {
			t.Fatal(err)
		}
	}

	pruned, err := r.PruneExpiredEphemeral(now, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != "expired-generated" {
		t.Fatalf("pruned = %v, want [expired-generated]", pruned)
	}
	if _, ok := r.Get("expired-generated"); ok {
		t.Fatal("expired ephemeral registration remains in registry")
	}
	if _, ok := r.ByToken("expired-token"); ok {
		t.Fatal("expired ephemeral token remains in token index")
	}
	for _, name := range []string{"live-generated", "expired-named", "unleased-generated"} {
		if _, ok := r.Get(name); !ok {
			t.Fatalf("prune removed retained registration %q", name)
		}
	}

	reopened, err := OpenRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Get("expired-generated"); ok {
		t.Fatal("pruned registration reappeared after registry reload")
	}
	if named, ok := reopened.Get("expired-named"); !ok || named.Ephemeral {
		t.Fatalf("named expired registration was not preserved: ok=%v rec=%+v", ok, named)
	}
}

func TestRegistryPruneExpiredEphemeralHonorsRecoveryGrace(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	grace := 24 * time.Hour
	withinGrace := AgentRec{
		Name: "recoverable", TokenHash: "recoverable-token", Ephemeral: true,
		LeaseSeconds: 60, LeaseExpires: now.Add(-grace + time.Second).UnixMilli(),
	}
	pastGrace := AgentRec{
		Name: "retired", TokenHash: "retired-token", Ephemeral: true,
		LeaseSeconds: 60, LeaseExpires: now.Add(-grace - time.Millisecond).UnixMilli(),
	}
	for _, rec := range []AgentRec{withinGrace, pastGrace} {
		if err := r.Put(rec); err != nil {
			t.Fatal(err)
		}
	}

	pruned, err := r.PruneExpiredEphemeral(now, grace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != pastGrace.Name {
		t.Fatalf("pruned = %v, want [%s]", pruned, pastGrace.Name)
	}
	if got, ok := r.ByToken(withinGrace.TokenHash); !ok || got.Name != withinGrace.Name {
		t.Fatalf("recovery-grace identity was not retained: ok=%v rec=%+v", ok, got)
	}
	if _, ok := r.ByToken(pastGrace.TokenHash); ok {
		t.Fatal("identity beyond recovery grace still resolves by token")
	}
}

func TestPruneKeepsNameWhenMailboxRetirementFails(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	rec := AgentRec{
		Name: "generated", TokenHash: "token", Ephemeral: true,
		LeaseSeconds: 45, LeaseExpires: now.Add(-time.Second).UnixMilli(),
	}
	if err := r.Put(rec); err != nil {
		t.Fatal(err)
	}
	cleanupErr := errors.New("disk busy")
	if _, err := r.PruneExpiredEphemeral(now, 0, func(string) error { return cleanupErr }); !errors.Is(err, cleanupErr) {
		t.Fatalf("prune error = %v, want %v", err, cleanupErr)
	}
	if got, ok := r.Get(rec.Name); !ok || got.TokenHash != rec.TokenHash {
		t.Fatalf("cleanup failure made name reusable: ok=%v rec=%+v", ok, got)
	}
}

func TestRegistryMutationsRollBackWhenSnapshotSaveFails(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registry, time.Time) error
	}{
		{
			name: "replace",
			mutate: func(r *Registry, _ time.Time) error {
				return r.Put(AgentRec{Name: "worker", TokenHash: "replacement-token", LeaseSeconds: 60})
			},
		},
		{
			name: "renew",
			mutate: func(r *Registry, now time.Time) error {
				_, _, err := r.RenewLease("worker", "original-token", now)
				return err
			},
		},
		{
			name: "release",
			mutate: func(r *Registry, now time.Time) error {
				_, _, err := r.ReleaseLease("worker", "original-token", now)
				return err
			},
		},
		{
			name:   "delete",
			mutate: func(r *Registry, _ time.Time) error { return r.Delete("worker") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := OpenRegistry(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			original := AgentRec{
				Name: "worker", TokenHash: "original-token", Registered: 11,
				LeaseSeconds: 60, LastSeen: 12, LeaseExpires: 72_000,
			}
			if err := r.Put(original); err != nil {
				t.Fatal(err)
			}
			// Point the snapshot at a file beneath a missing parent. Every
			// mutation reaches its persistence step and fails deterministically.
			r.path = filepath.Join(t.TempDir(), "missing", "registry.json")
			if err := tt.mutate(r, time.UnixMilli(123_456)); err == nil {
				t.Fatal("mutation unexpectedly succeeded with an unwritable snapshot path")
			}

			if got, ok := r.Get(original.Name); !ok || got != original {
				t.Fatalf("registry record changed after failed save: ok=%v got=%+v want=%+v", ok, got, original)
			}
			if got, ok := r.ByToken(original.TokenHash); !ok || got != original {
				t.Fatalf("token index changed after failed save: ok=%v got=%+v want=%+v", ok, got, original)
			}
			if _, ok := r.ByToken("replacement-token"); ok {
				t.Fatal("failed replacement token remained in token index")
			}
		})
	}
}

func TestRegistryNewPutRollsBackWhenSnapshotSaveFails(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r.path = filepath.Join(t.TempDir(), "missing", "registry.json")
	if err := r.Put(AgentRec{Name: "new", TokenHash: "new-token"}); err == nil {
		t.Fatal("put unexpectedly succeeded with an unwritable snapshot path")
	}
	if _, ok := r.Get("new"); ok {
		t.Fatal("failed put remained in registry")
	}
	if _, ok := r.ByToken("new-token"); ok {
		t.Fatal("failed put remained in token index")
	}
}

func TestRegistryFailedPutRestoresPreviousTokenOwner(t *testing.T) {
	r, err := OpenRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := AgentRec{Name: "worker", TokenHash: "worker-token"}
	other := AgentRec{Name: "other", TokenHash: "other-token"}
	for _, rec := range []AgentRec{original, other} {
		if err := r.Put(rec); err != nil {
			t.Fatal(err)
		}
	}
	r.path = filepath.Join(t.TempDir(), "missing", "registry.json")
	if err := r.Put(AgentRec{Name: original.Name, TokenHash: other.TokenHash}); err == nil {
		t.Fatal("put unexpectedly succeeded with an unwritable snapshot path")
	}
	if got, ok := r.ByToken(original.TokenHash); !ok || got.Name != original.Name {
		t.Fatalf("original token owner was not restored: ok=%v rec=%+v", ok, got)
	}
	if got, ok := r.ByToken(other.TokenHash); !ok || got.Name != other.Name {
		t.Fatalf("colliding token owner was not restored: ok=%v rec=%+v", ok, got)
	}
}
