package service

import (
	"context"
	"sync"
)

type testOpenAIOAuthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*OpenAIOAuthSession
	consumed map[string]bool
}

func newTestOpenAIOAuthSessionStore() *testOpenAIOAuthSessionStore {
	return &testOpenAIOAuthSessionStore{
		sessions: make(map[string]*OpenAIOAuthSession),
		consumed: make(map[string]bool),
	}
}

func (s *testOpenAIOAuthSessionStore) Create(_ context.Context, session *OpenAIOAuthSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[session.ID] = session
	return nil
}

func (s *testOpenAIOAuthSessionStore) Get(_ context.Context, sessionID string) (*OpenAIOAuthSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrPendingAuthSessionNotFound
	}
	if s.consumed[sessionID] {
		return nil, ErrPendingAuthSessionConsumed
	}
	return session, nil
}

func (s *testOpenAIOAuthSessionStore) Consume(_ context.Context, sessionID string) (*OpenAIOAuthSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrPendingAuthSessionNotFound
	}
	if s.consumed[sessionID] {
		return nil, ErrPendingAuthSessionConsumed
	}
	s.consumed[sessionID] = true
	return session, nil
}
