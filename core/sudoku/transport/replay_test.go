package sudoku

import (
	"testing"
	"time"
)

func TestHandshakeReplayProtectorBoundsUserSets(t *testing.T) {
	p := &handshakeReplayProtector{maxUsers: 8}
	now := time.Unix(100, 0)
	var nonce [kipHelloNonceSize]byte

	if !p.allow("user-a", nonce, now) {
		t.Fatal("first nonce was rejected")
	}
	nonce[0]++
	if !p.allow("user-b", nonce, now) {
		t.Fatal("second nonce was rejected")
	}
	nonce[0]++
	if !p.allow("user-c", nonce, now) {
		t.Fatal("third nonce was rejected")
	}
	if got := len(p.users); got > 8 {
		t.Fatalf("replay user sets = %d, want at most 8", got)
	}

	// Expired user sets are removed during the next prune pass, so a retry
	// storm with rotating hashes cannot retain the full TTL forever.
	now = now.Add(handshakeReplayTTL + time.Second)
	nonce[0]++
	if !p.allow("user-d", nonce, now) {
		t.Fatal("post-expiry nonce was rejected")
	}
	if _, ok := p.users["user-a"]; ok {
		t.Fatal("expired replay user set was retained")
	}
}

func TestNonceSetBoundsEntries(t *testing.T) {
	set := newNonceSet(2)
	now := time.Unix(200, 0)
	for i := 0; i < 10; i++ {
		var nonce [kipHelloNonceSize]byte
		nonce[0] = byte(i)
		if !set.allow(nonce, now, handshakeReplayTTL) {
			t.Fatalf("nonce %d was rejected", i)
		}
	}
	if got := len(set.m); got > 2 {
		t.Fatalf("nonce entries = %d, want at most 2", got)
	}
}
