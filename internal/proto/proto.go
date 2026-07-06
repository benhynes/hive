// Package proto defines the hive wire types: the message envelope and
// agent addressing. See docs/PROTOCOL.md for the wire spec.
package proto

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const (
	KindMsg    = "msg"
	KindAsk    = "ask"
	KindAnswer = "answer"

	// MaxBody is the maximum envelope body size in bytes.
	MaxBody = 8 * 1024

	// Broadcast is the special "to" address delivered to every agent
	// in the network.
	Broadcast = "@all"
)

// Envelope is a single hive message. From is always stamped by the hub
// from the authenticated token — clients never supply it.
type Envelope struct {
	ID     string `json:"id"`
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Body   string `json:"body"`
	CorrID string `json:"corr_id,omitempty"`
	TS     int64  `json:"ts"` // unix milliseconds, set by the origin hub
}

// NewID builds a unique message id. It is generated once at the origin
// hub and carried through forwards and retries unchanged, which is what
// makes dedup-on-read work.
func NewID(from, to, kind, body, corrID string, ts int64) string {
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%s|%d|", from, to, kind, corrID, ts)
	h.Write([]byte(body))
	h.Write(nonce[:])
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ValidName reports whether s is a legal agent, host, or network name.
func ValidName(s string) bool { return nameRe.MatchString(s) }

// SplitAgent splits "name@host" into its parts.
func SplitAgent(id string) (name, host string, err error) {
	name, host, ok := strings.Cut(id, "@")
	if !ok || !ValidName(name) || !ValidName(host) {
		return "", "", fmt.Errorf("bad agent id %q (want name@host)", id)
	}
	return name, host, nil
}

// Validate checks an envelope built from client input.
func (e *Envelope) Validate() error {
	switch e.Kind {
	case KindMsg, KindAsk, KindAnswer:
	default:
		return fmt.Errorf("bad kind %q", e.Kind)
	}
	if len(e.Body) > MaxBody {
		return fmt.Errorf("body %d bytes exceeds %d", len(e.Body), MaxBody)
	}
	if e.To != Broadcast {
		if _, _, err := SplitAgent(e.To); err != nil {
			return err
		}
	}
	if e.ID == "" {
		return errors.New("missing id")
	}
	return nil
}

// NewToken returns a fresh random bearer token (hex, 256 bits).
func NewToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return hex.EncodeToString(b[:])
}

// HashToken is the storage form of a token: hex(sha256(token)).
func HashToken(tok string) string {
	s := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(s[:])
}
