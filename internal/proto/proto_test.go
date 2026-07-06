package proto

import (
	"strings"
	"testing"
)

func TestSplitAgent(t *testing.T) {
	name, host, err := SplitAgent("bob@vm1")
	if err != nil || name != "bob" || host != "vm1" {
		t.Fatalf("got %q %q %v", name, host, err)
	}
	for _, bad := range []string{"bob", "@vm1", "bob@", "Bob@vm1", "bob@vm 1", "bob@@vm1", "-x@vm1"} {
		if _, _, err := SplitAgent(bad); err == nil {
			t.Fatalf("%q should be rejected", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	e := Envelope{ID: "x", To: "a@b", Kind: KindMsg, Body: "hi"}
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
	e.Kind = "weird"
	if e.Validate() == nil {
		t.Fatal("bad kind accepted")
	}
	e.Kind = KindMsg
	e.Body = strings.Repeat("x", MaxBody+1)
	if e.Validate() == nil {
		t.Fatal("oversize body accepted")
	}
	e.Body = "ok"
	e.To = Broadcast
	if err := e.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestIDsUnique(t *testing.T) {
	a := NewID("f", "t", KindMsg, "b", "", 1)
	b := NewID("f", "t", KindMsg, "b", "", 1)
	if a == b {
		t.Fatal("ids collide for identical content — nonce missing")
	}
	if len(a) != 16 {
		t.Fatalf("id len %d", len(a))
	}
}

func TestTokens(t *testing.T) {
	tok := NewToken()
	if len(tok) != 64 || HashToken(tok) == HashToken(tok+"x") {
		t.Fatal("token gen/hash broken")
	}
}
