package sudoku

import (
	"sync"
	"time"
)

var handshakeReplayTTL = 60 * time.Second

type nonceSet struct {
	mu         sync.Mutex
	m          map[[kipHelloNonceSize]byte]time.Time
	maxEntries int
	lastPrune  time.Time
}

func newNonceSet(maxEntries int) *nonceSet {
	if maxEntries <= 0 {
		maxEntries = 4096
	}
	return &nonceSet{
		m:          make(map[[kipHelloNonceSize]byte]time.Time),
		maxEntries: maxEntries,
	}
}

func (s *nonceSet) allow(nonce [kipHelloNonceSize]byte, now time.Time, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if ttl <= 0 {
		ttl = 60 * time.Second
	}

	if now.Sub(s.lastPrune) > ttl/2 || len(s.m) >= s.maxEntries {
		for k, exp := range s.m {
			if !now.Before(exp) {
				delete(s.m, k)
			}
		}
		s.lastPrune = now
		for len(s.m) >= s.maxEntries {
			for k := range s.m {
				delete(s.m, k)
				break
			}
		}
	}

	if exp, ok := s.m[nonce]; ok && now.Before(exp) {
		return false
	}
	s.m[nonce] = now.Add(ttl)
	return true
}

type handshakeReplayProtector struct {
	mu        sync.Mutex
	users     map[string]*nonceSet
	lastPrune time.Time
	maxUsers  int
}

func (p *handshakeReplayProtector) allow(userHash string, nonce [kipHelloNonceSize]byte, now time.Time) bool {
	if p == nil {
		return false
	}
	if userHash == "" {
		userHash = "_"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.users == nil {
		p.users = make(map[string]*nonceSet)
	}
	if p.maxUsers <= 0 {
		p.maxUsers = 4096
	}
	if now.Sub(p.lastPrune) > handshakeReplayTTL/2 || len(p.users) >= p.maxUsers {
		for key, set := range p.users {
			if set.expired(now) {
				delete(p.users, key)
			}
		}
		p.lastPrune = now
		for len(p.users) > p.maxUsers {
			for key := range p.users {
				delete(p.users, key)
				break
			}
		}
	}
	set := p.users[userHash]
	if set == nil {
		for len(p.users) >= p.maxUsers {
			for key := range p.users {
				delete(p.users, key)
				break
			}
		}
		set = newNonceSet(4096)
		p.users[userHash] = set
	}
	return set.allow(nonce, now, handshakeReplayTTL)
}

func (s *nonceSet) expired(now time.Time) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, exp := range s.m {
		if now.Before(exp) {
			return false
		}
	}
	return true
}

var globalHandshakeReplay = &handshakeReplayProtector{}
