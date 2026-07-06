package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/benhynes/hive/internal/proto"
)

func env(id, body string) proto.Envelope {
	return proto.Envelope{ID: id, From: "a@h", To: "b@h", Kind: proto.KindMsg, Body: body, TS: 1}
}

func TestAppendReadAck(t *testing.T) {
	ib, err := OpenInbox(t.TempDir(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		seq, fresh, err := ib.Append(env(fmt.Sprintf("id%d", i), "hi"))
		if err != nil || !fresh || seq != int64(i) {
			t.Fatalf("append %d: seq=%d fresh=%v err=%v", i, seq, fresh, err)
		}
	}
	res := ib.Read(0, 10)
	if len(res.Msgs) != 3 || res.Latest != 3 || res.Skipped != 0 {
		t.Fatalf("read: %+v", res)
	}
	// Idempotent replay: same read again returns the same messages.
	res2 := ib.Read(0, 10)
	if len(res2.Msgs) != 3 || res2.Msgs[0].Env.ID != "id1" {
		t.Fatalf("replay differs: %+v", res2)
	}
	if err := ib.Ack(2); err != nil {
		t.Fatal(err)
	}
	if res := ib.Read(ib.Cursor(), 10); len(res.Msgs) != 1 || res.Msgs[0].Seq != 3 {
		t.Fatalf("after ack: %+v", res)
	}
	// Ack never regresses.
	if err := ib.Ack(1); err != nil {
		t.Fatal(err)
	}
	if ib.Cursor() != 2 {
		t.Fatalf("cursor regressed to %d", ib.Cursor())
	}
}

func TestDedup(t *testing.T) {
	ib, _ := OpenInbox(t.TempDir(), "bob")
	s1, fresh1, _ := ib.Append(env("dup", "x"))
	s2, fresh2, _ := ib.Append(env("dup", "x"))
	if !fresh1 || fresh2 || s1 != s2 {
		t.Fatalf("dedup broken: s1=%d s2=%d fresh2=%v", s1, s2, fresh2)
	}
	if res := ib.Read(0, 10); len(res.Msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(res.Msgs))
	}
}

func TestOverflowClampAndSkipped(t *testing.T) {
	ib, _ := OpenInbox(t.TempDir(), "bob")
	n := Cap + 250
	for i := 1; i <= n; i++ {
		ib.Append(env(fmt.Sprintf("id%d", i), "x"))
	}
	// Reader whose cursor predates the retained floor.
	res := ib.Read(0, 10)
	if res.Skipped != 250 {
		t.Fatalf("skipped=%d want 250", res.Skipped)
	}
	if res.Msgs[0].Seq != 251 {
		t.Fatalf("floor seq=%d want 251", res.Msgs[0].Seq)
	}
	if res.Latest != int64(n) {
		t.Fatalf("latest=%d want %d", res.Latest, n)
	}
}

func TestReloadFromDisk(t *testing.T) {
	dir := t.TempDir()
	ib, _ := OpenInbox(dir, "bob")
	for i := 1; i <= 5; i++ {
		ib.Append(env(fmt.Sprintf("id%d", i), "persist me"))
	}
	ib.Ack(3)

	ib2, err := OpenInbox(dir, "bob")
	if err != nil {
		t.Fatal(err)
	}
	if ib2.Cursor() != 3 {
		t.Fatalf("cursor=%d want 3", ib2.Cursor())
	}
	res := ib2.Read(ib2.Cursor(), 10)
	if len(res.Msgs) != 2 || res.Msgs[0].Env.Body != "persist me" {
		t.Fatalf("reload: %+v", res)
	}
	// Seq continues, no reuse.
	seq, _, _ := ib2.Append(env("id6", "x"))
	if seq != 6 {
		t.Fatalf("seq=%d want 6", seq)
	}
	// Dedup survives reload within the retained window.
	if _, fresh, _ := ib2.Append(env("id2", "persist me")); fresh {
		t.Fatal("dedup lost after reload")
	}
}

func TestCompaction(t *testing.T) {
	dir := t.TempDir()
	ib, _ := OpenInbox(dir, "bob")
	for i := 1; i <= compactAt+10; i++ {
		ib.Append(env(fmt.Sprintf("id%d", i), "x"))
	}
	ib2, err := OpenInbox(dir, "bob")
	if err != nil {
		t.Fatal(err)
	}
	res := ib2.Read(0, Cap+10)
	if len(res.Msgs) > Cap {
		t.Fatalf("retained %d > cap", len(res.Msgs))
	}
	if res.Latest != int64(compactAt+10) {
		t.Fatalf("latest=%d", res.Latest)
	}
}

func TestWaitWakesOnAppend(t *testing.T) {
	ib, _ := OpenInbox(t.TempDir(), "bob")
	done := make(chan ReadResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- ib.Wait(ctx, 0, 10)
	}()
	// Give the waiter time to block, confirming poller accounting too.
	deadline := time.Now().Add(2 * time.Second)
	for ib.Pollers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ib.Pollers() != 1 {
		t.Fatal("poller never blocked")
	}
	ib.Append(env("w1", "wake"))
	select {
	case res := <-done:
		if len(res.Msgs) != 1 || res.Msgs[0].Env.ID != "w1" {
			t.Fatalf("wait result: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait never woke")
	}
	if ib.Pollers() != 0 {
		t.Fatal("poller count leaked")
	}
}

func TestWaitTimeout(t *testing.T) {
	ib, _ := OpenInbox(t.TempDir(), "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res := ib.Wait(ctx, 0, 10)
	if len(res.Msgs) != 0 {
		t.Fatalf("expected empty, got %+v", res)
	}
}
