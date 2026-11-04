package web

import (
	"context"
	"sync"
	"time"

	"github.com/gosom/google-maps-scraper/internal/cnpj"
)

const (
	// lookupStatusOK and lookupStatusNotFound are the outcomes worth caching.
	// Transient failures (rate limit, outage) are never cached.
	lookupStatusOK       = "ok"
	lookupStatusNotFound = "not_found"

	// lookupTTLFound keeps a found company for a month: registry data changes
	// slowly, and the point of the cache is not to repeat the query.
	lookupTTLFound = 30 * 24 * time.Hour
	// lookupTTLNotFound is shorter: a number that did not exist may be
	// registered soon after.
	lookupTTLNotFound = 7 * 24 * time.Hour

	// Where a place's CNPJ came from.
	cnpjSourceManual = "manual"
)

// CompanyLookup is a cached answer of the CNPJ API.
type CompanyLookup struct {
	CNPJ      string
	Status    string
	Company   *cnpj.Company
	FetchedAt time.Time
	ExpiresAt time.Time
}

// CompanyStore persists CNPJ lookups (so the same number is never queried
// twice within its TTL) and which CNPJ was linked to each place of a job.
type CompanyStore interface {
	// GetLookups returns the unexpired cached lookups among the given numbers.
	GetLookups(ctx context.Context, numbers []string, now time.Time) (map[string]CompanyLookup, error)
	SaveLookup(ctx context.Context, lookup CompanyLookup) error
	// SetPlaceCNPJ links a CNPJ to the place at position (1-based) of a job.
	SetPlaceCNPJ(ctx context.Context, jobID string, position int, number, source string, now time.Time) error
	ClearPlaceCNPJ(ctx context.Context, jobID string, position int) error
	// PlaceCNPJs returns position -> CNPJ for a job.
	PlaceCNPJs(ctx context.Context, jobID string) (map[int]string, error)
}

// memoryCompanyStore is the in-process CompanyStore used when the job
// repository does not offer a persistent one (tests, other repositories).
type memoryCompanyStore struct {
	mu      sync.Mutex
	lookups map[string]CompanyLookup
	places  map[string]map[int]string
}

func newMemoryCompanyStore() *memoryCompanyStore {
	return &memoryCompanyStore{
		lookups: make(map[string]CompanyLookup),
		places:  make(map[string]map[int]string),
	}
}

func (m *memoryCompanyStore) GetLookups(_ context.Context, numbers []string, now time.Time) (map[string]CompanyLookup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	found := make(map[string]CompanyLookup, len(numbers))

	for _, number := range numbers {
		if lookup, ok := m.lookups[number]; ok && now.Before(lookup.ExpiresAt) {
			found[number] = lookup
		}
	}

	return found, nil
}

func (m *memoryCompanyStore) SaveLookup(_ context.Context, lookup CompanyLookup) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lookups[lookup.CNPJ] = lookup

	return nil
}

func (m *memoryCompanyStore) SetPlaceCNPJ(_ context.Context, jobID string, position int, number, _ string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.places[jobID] == nil {
		m.places[jobID] = make(map[int]string)
	}

	m.places[jobID][position] = number

	return nil
}

func (m *memoryCompanyStore) ClearPlaceCNPJ(_ context.Context, jobID string, position int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.places[jobID], position)

	return nil
}

func (m *memoryCompanyStore) PlaceCNPJs(_ context.Context, jobID string) (map[int]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[int]string, len(m.places[jobID]))
	for position, number := range m.places[jobID] {
		out[position] = number
	}

	return out, nil
}
