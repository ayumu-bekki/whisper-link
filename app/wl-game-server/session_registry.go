package main

import "sync"

// SessionRegistry は コールサイン（自分CS/相手CS）→ wsSession のマッピングを管理する。
// 1つの wsSession が自分CS と 相手CS 複数をキーに登録される。
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*wsSession
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*wsSession),
	}
}

func (r *SessionRegistry) Register(callsign string, s *wsSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[callsign] = s
}

func (r *SessionRegistry) Unregister(callsign string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, callsign)
}

func (r *SessionRegistry) Lookup(callsign string) (*wsSession, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sessions[callsign]
	return s, ok
}
