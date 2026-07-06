// Package store persists hive state: per-agent JSONL inboxes with reader
// cursors, the agent registry snapshot, and the agent-token table.
package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/benhynes/hive/internal/proto"
)

// Cap is the retained-message window per inbox; older messages are
// dropped oldest-first and surfaced to the reader as a skipped count.
const Cap = 1000

// compactAt bounds file growth: once the on-disk line count reaches it,
// the file is rewritten with just the retained window.
const compactAt = 2 * Cap

// Rec is one stored message with its inbox sequence number (monotonic,
// 1-based, never reused within an inbox).
type Rec struct {
	Seq int64          `json:"seq"`
	Env proto.Envelope `json:"env"`
}

// Inbox is a durable, append-only mailbox for one agent. Reads are
// idempotent (cursor-based, not destructive); Ack advances the cursor.
type Inbox struct {
	mu        sync.Mutex
	path      string // <dir>/inbox/<name>.jsonl
	cursorP   string // <dir>/cursors/<name>
	recs      []Rec  // retained window, ascending seq
	nextSeq   int64
	cursor    int64 // last acked seq
	fileLines int
	seen      map[string]bool // envelope id -> present in retained window
	waiters   map[chan struct{}]bool
	pollers   int // live long-polls (nudge suppression)
}

// OpenInbox loads (or creates) the inbox for an agent under dir.
func OpenInbox(dir, name string) (*Inbox, error) {
	ib := &Inbox{
		path:    filepath.Join(dir, "inbox", name+".jsonl"),
		cursorP: filepath.Join(dir, "cursors", name),
		nextSeq: 1,
		seen:    map[string]bool{},
		waiters: map[chan struct{}]bool{},
	}
	for _, d := range []string{filepath.Dir(ib.path), filepath.Dir(ib.cursorP)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := ib.load(); err != nil {
		return nil, err
	}
	return ib, nil
}

func (ib *Inbox) load() error {
	f, err := os.Open(ib.path)
	if err != nil {
		if os.IsNotExist(err) {
			return ib.loadCursor()
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Rec
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // torn tail write from a crash: skip
		}
		ib.recs = append(ib.recs, r)
		ib.fileLines++
		if r.Seq >= ib.nextSeq {
			ib.nextSeq = r.Seq + 1
		}
	}
	if len(ib.recs) > Cap {
		ib.recs = ib.recs[len(ib.recs)-Cap:]
	}
	for _, r := range ib.recs {
		ib.seen[r.Env.ID] = true
	}
	return ib.loadCursor()
}

func (ib *Inbox) loadCursor() error {
	b, err := os.ReadFile(ib.cursorP)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err == nil {
		ib.cursor = n
	}
	return nil
}

// Append stores an envelope, deduplicating by envelope id within the
// retained window. It returns the assigned seq (or the existing one for
// a duplicate) and whether the message was new.
func (ib *Inbox) Append(env proto.Envelope) (int64, bool, error) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if ib.seen[env.ID] {
		for _, r := range ib.recs {
			if r.Env.ID == env.ID {
				return r.Seq, false, nil
			}
		}
	}
	r := Rec{Seq: ib.nextSeq, Env: env}
	line, err := json.Marshal(r)
	if err != nil {
		return 0, false, err
	}
	f, err := os.OpenFile(ib.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, false, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return 0, false, err
	}
	if err := f.Close(); err != nil {
		return 0, false, err
	}
	ib.nextSeq++
	ib.fileLines++
	ib.recs = append(ib.recs, r)
	ib.seen[env.ID] = true
	if len(ib.recs) > Cap {
		drop := ib.recs[:len(ib.recs)-Cap]
		for _, d := range drop {
			delete(ib.seen, d.Env.ID)
		}
		ib.recs = ib.recs[len(ib.recs)-Cap:]
	}
	if ib.fileLines >= compactAt {
		ib.compactLocked()
	}
	for w := range ib.waiters {
		close(w)
		delete(ib.waiters, w)
	}
	return r.Seq, true, nil
}

// compactLocked rewrites the file with only the retained window.
func (ib *Inbox) compactLocked() {
	tmp := ib.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return // compaction is best-effort; appends still work
	}
	w := bufio.NewWriter(f)
	for _, r := range ib.recs {
		b, _ := json.Marshal(r)
		w.Write(append(b, '\n'))
	}
	if w.Flush() != nil || f.Close() != nil {
		os.Remove(tmp)
		return
	}
	if os.Rename(tmp, ib.path) == nil {
		ib.fileLines = len(ib.recs)
	}
}

// ReadResult is one page of messages plus enough bookkeeping for the
// reader to detect drops.
type ReadResult struct {
	Msgs    []Rec `json:"msgs"`
	Cursor  int64 `json:"cursor"`  // reader's stored cursor
	Latest  int64 `json:"latest"`  // highest seq ever assigned
	Skipped int64 `json:"skipped"` // messages dropped before `after` could read them
}

// Read returns up to max records with seq > after. If after has fallen
// below the retained floor, the read clamps to the floor and reports how
// many messages were skipped — never a silent gap.
func (ib *Inbox) Read(after int64, max int) ReadResult {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.readLocked(after, max)
}

func (ib *Inbox) readLocked(after int64, max int) ReadResult {
	res := ReadResult{Cursor: ib.cursor, Latest: ib.nextSeq - 1}
	if max <= 0 {
		max = 100
	}
	if len(ib.recs) > 0 {
		floor := ib.recs[0].Seq
		if after < floor-1 {
			res.Skipped = (floor - 1) - after
			after = floor - 1
		}
	} else if after < ib.nextSeq-1 {
		// Everything was dropped.
		res.Skipped = (ib.nextSeq - 1) - after
		after = ib.nextSeq - 1
	}
	for _, r := range ib.recs {
		if r.Seq > after {
			res.Msgs = append(res.Msgs, r)
			if len(res.Msgs) >= max {
				break
			}
		}
	}
	return res
}

// Ack advances the durable cursor to seq (idempotent; never moves back).
func (ib *Inbox) Ack(seq int64) error {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	if seq <= ib.cursor {
		return nil
	}
	if seq > ib.nextSeq-1 {
		return fmt.Errorf("ack %d beyond latest %d", seq, ib.nextSeq-1)
	}
	ib.cursor = seq
	tmp := ib.cursorP + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(seq, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ib.cursorP)
}

// Cursor returns the reader's stored cursor.
func (ib *Inbox) Cursor() int64 {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.cursor
}

// Lag returns how many retained messages sit past the cursor.
func (ib *Inbox) Lag() int64 {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return (ib.nextSeq - 1) - ib.cursor
}

// Wait blocks until a record with seq > after exists or ctx is done,
// then returns the read. It counts as a live poller while blocked.
func (ib *Inbox) Wait(ctx context.Context, after int64, max int) ReadResult {
	for {
		ib.mu.Lock()
		res := ib.readLocked(after, max)
		if len(res.Msgs) > 0 || ctx.Err() != nil {
			ib.mu.Unlock()
			return res
		}
		ch := make(chan struct{})
		ib.waiters[ch] = true
		ib.pollers++
		ib.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
		}
		ib.mu.Lock()
		delete(ib.waiters, ch)
		ib.pollers--
		ib.mu.Unlock()
	}
}

// Pollers reports how many long-polls are currently blocked on this
// inbox (used to suppress nudges).
func (ib *Inbox) Pollers() int {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return ib.pollers
}
