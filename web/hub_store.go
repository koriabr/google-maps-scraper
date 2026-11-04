package web

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrHubSessionNotFound is returned when a session does not exist or was ended.
var ErrHubSessionNotFound = errors.New("hub session not found")

// HubSessionRecord is a login session kept on the server. The browser only
// holds "<id>.<secret>"; the store keeps the SHA-256 of the secret, so a copy of
// the database cannot be turned back into a working cookie.
type HubSessionRecord struct {
	ID         string
	SecretHash string
	Username   string
	Name       string
	Email      string
	CreatedAt  time.Time
	LastSeen   time.Time
	ExpiresAt  time.Time
}

// HubStore persists the state of the Hub login: which ticket ids were already
// used (single use must survive restarts) and the server-side sessions (so that
// logout and revocation really end them).
type HubStore interface {
	// ConsumeTicket records a ticket id and reports whether it was the first
	// use. The record is kept until purgeAt and then discarded.
	ConsumeTicket(ctx context.Context, jti string, now, purgeAt time.Time) (bool, error)
	CreateSession(ctx context.Context, rec HubSessionRecord) error
	// GetSession returns an active session, or ErrHubSessionNotFound when it
	// does not exist or was ended.
	GetSession(ctx context.Context, id string) (HubSessionRecord, error)
	TouchSession(ctx context.Context, id string, now time.Time) error
	// RevokeSession ends one session: its secret hash is discarded, so no
	// cookie can ever match it again.
	RevokeSession(ctx context.Context, id string, now time.Time) error
	// RevokeUser ends every session of a person and returns how many.
	RevokeUser(ctx context.Context, username string, now time.Time) (int, error)
	// PurgeExpired removes sessions past their absolute expiry.
	PurgeExpired(ctx context.Context, now time.Time) error
}

// memoryHubStore is the in-process HubStore used when the job repository does
// not offer a persistent one (tests, other repositories). State is lost on
// restart.
type memoryHubStore struct {
	mu       sync.Mutex
	tickets  map[string]time.Time
	sessions map[string]*HubSessionRecord
}

func newMemoryHubStore() *memoryHubStore {
	return &memoryHubStore{
		tickets:  make(map[string]time.Time),
		sessions: make(map[string]*HubSessionRecord),
	}
}

func (m *memoryHubStore) ConsumeTicket(_ context.Context, jti string, now, purgeAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for used, until := range m.tickets {
		if now.After(until) {
			delete(m.tickets, used)
		}
	}

	if _, seen := m.tickets[jti]; seen {
		return false, nil
	}

	m.tickets[jti] = purgeAt

	return true, nil
}

func (m *memoryHubStore) CreateSession(_ context.Context, rec HubSessionRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.sessions[rec.ID] = &rec

	return nil
}

func (m *memoryHubStore) GetSession(_ context.Context, id string) (HubSessionRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	rec, ok := m.sessions[id]
	if !ok || rec.SecretHash == "" {
		return HubSessionRecord{}, ErrHubSessionNotFound
	}

	return *rec, nil
}

func (m *memoryHubStore) TouchSession(_ context.Context, id string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rec, ok := m.sessions[id]; ok {
		rec.LastSeen = now
	}

	return nil
}

func (m *memoryHubStore) RevokeSession(_ context.Context, id string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if rec, ok := m.sessions[id]; ok {
		rec.SecretHash = ""
	}

	return nil
}

func (m *memoryHubStore) RevokeUser(_ context.Context, username string, _ time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	count := 0

	for _, rec := range m.sessions {
		if rec.Username == username && rec.SecretHash != "" {
			rec.SecretHash = ""
			count++
		}
	}

	return count, nil
}

func (m *memoryHubStore) PurgeExpired(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, rec := range m.sessions {
		if now.After(rec.ExpiresAt) {
			delete(m.sessions, id)
		}
	}

	return nil
}
