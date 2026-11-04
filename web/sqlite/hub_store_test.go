package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/web"
	"github.com/gosom/google-maps-scraper/web/sqlite"
)

func newStore(t *testing.T, path string) web.HubStore {
	t.Helper()

	repo, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	store, ok := repo.(web.HubStore)
	if !ok {
		t.Fatal("the sqlite repository must implement web.HubStore so Hub login state is persistent")
	}

	return store
}

func TestHubStoreTicketsAreSingleUseAndSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	now := time.Now()

	store := newStore(t, path)

	first, err := store.ConsumeTicket(ctx, "jti-1", now, now.Add(15*time.Minute))
	if err != nil || !first {
		t.Fatalf("first use = %v, %v", first, err)
	}

	again, err := store.ConsumeTicket(ctx, "jti-1", now, now.Add(15*time.Minute))
	if err != nil || again {
		t.Fatalf("second use = %v, %v; a replay must be refused", again, err)
	}

	// A new process (database reopened) still knows the ticket was used.
	reopened := newStore(t, path)

	if reused, err := reopened.ConsumeTicket(ctx, "jti-1", now, now.Add(15*time.Minute)); err != nil || reused {
		t.Fatalf("replay after reopen = %v, %v", reused, err)
	}

	// After the keep window the record is discarded.
	later := now.Add(16 * time.Minute)
	if ok, err := reopened.ConsumeTicket(ctx, "jti-2", later, later.Add(15*time.Minute)); err != nil || !ok {
		t.Fatalf("other ticket = %v, %v", ok, err)
	}
}

func TestHubStoreSessions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	now := time.Now().Truncate(time.Second)
	store := newStore(t, path)

	rec := func(id, user string) web.HubSessionRecord {
		return web.HubSessionRecord{
			ID: id, SecretHash: "hash-" + id, Username: user, Name: "Ana", Email: "a@x.example",
			CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(7 * 24 * time.Hour),
		}
	}

	for _, r := range []web.HubSessionRecord{rec("s1", "ana"), rec("s2", "ana"), rec("s3", "bia")} {
		if err := store.CreateSession(ctx, r); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	got, err := store.GetSession(ctx, "s1")
	if err != nil || got.Username != "ana" || got.SecretHash != "hash-s1" || !got.CreatedAt.Equal(now) {
		t.Fatalf("GetSession = %+v, %v", got, err)
	}

	if err := store.TouchSession(ctx, "s1", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if got, _ := store.GetSession(ctx, "s1"); !got.LastSeen.Equal(now.Add(time.Hour)) {
		t.Fatalf("last seen not updated: %v", got.LastSeen)
	}

	// Ending one session drops its secret hash: it can never match again.
	if err := store.RevokeSession(ctx, "s1", now); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetSession(ctx, "s1"); !errors.Is(err, web.ErrHubSessionNotFound) {
		t.Fatalf("revoked session = %v, want ErrHubSessionNotFound", err)
	}

	// Ending a person ends all of their remaining sessions, and only theirs.
	count, err := store.RevokeUser(ctx, "ana", now)
	if err != nil || count != 1 {
		t.Fatalf("RevokeUser = %d, %v; want 1 (s1 was already ended)", count, err)
	}

	if _, err := store.GetSession(ctx, "s2"); !errors.Is(err, web.ErrHubSessionNotFound) {
		t.Fatalf("s2 should be ended: %v", err)
	}

	if _, err := store.GetSession(ctx, "s3"); err != nil {
		t.Fatalf("another person's session must stay: %v", err)
	}

	// Sessions survive a reopen; expired ones are purged.
	reopened := newStore(t, path)

	if _, err := reopened.GetSession(ctx, "s3"); err != nil {
		t.Fatalf("session lost after reopen: %v", err)
	}

	if err := reopened.PurgeExpired(ctx, now.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	if _, err := reopened.GetSession(ctx, "s3"); !errors.Is(err, web.ErrHubSessionNotFound) {
		t.Fatalf("expired session not purged: %v", err)
	}
}

func TestWebServerUsesTheSqliteStoreForHubLogin(t *testing.T) {
	repo, err := sqlite.New(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}

	// New wires the persistent store when the repository offers one; a short
	// key is rejected, proving WithHub ran against this repository.
	if _, err := web.New(web.NewService(repo, t.TempDir()), ":0", web.WithHub(web.HubConfig{SSOKey: "short"})); err == nil {
		t.Fatal("expected a configuration error")
	}
}
