package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/internal/cnpj"
	"github.com/gosom/google-maps-scraper/web"
	"github.com/gosom/google-maps-scraper/web/sqlite"
)

func newCompanyStore(t *testing.T, path string) (web.CompanyStore, web.JobRepository) {
	t.Helper()

	repo, err := sqlite.New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	store, ok := repo.(web.CompanyStore)
	if !ok {
		t.Fatal("the sqlite repository must implement web.CompanyStore so cached lookups persist")
	}

	return store, repo
}

func TestCompanyStoreLookupsExpireAndSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	now := time.Now().Truncate(time.Second)

	store, _ := newCompanyStore(t, path)

	company := cnpj.Company{CNPJ: "24614048000158", LegalName: "TRANSFER LTDA", Status: "ATIVA", Capital: 70000, MEI: false}
	if err := store.SaveLookup(ctx, web.CompanyLookup{CNPJ: company.CNPJ, Status: "ok", Company: &company, FetchedAt: now, ExpiresAt: now.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if err := store.SaveLookup(ctx, web.CompanyLookup{CNPJ: "11222333000181", Status: "not_found", FetchedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// A new process (database reopened) still has the answers.
	reopened, _ := newCompanyStore(t, path)

	got, err := reopened.GetLookups(ctx, []string{company.CNPJ, "11222333000181", "99999999999999"}, now)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d lookups, want 2 (the unknown number is a miss)", len(got))
	}

	if hit := got[company.CNPJ]; hit.Status != "ok" || hit.Company == nil || hit.Company.LegalName != "TRANSFER LTDA" || hit.Company.Capital != 70000 {
		t.Fatalf("company did not round-trip: %+v", hit)
	}

	if hit := got["11222333000181"]; hit.Status != "not_found" || hit.Company != nil {
		t.Fatalf("not_found must be remembered without a company: %+v", hit)
	}

	// Past its expiry a lookup is a cache miss.
	late, _ := reopened.GetLookups(ctx, []string{company.CNPJ, "11222333000181"}, now.Add(2*time.Hour))
	if len(late) != 1 {
		t.Fatalf("after 2h only the 24h lookup must remain, got %d", len(late))
	}

	// Saving again replaces the row (refresh after expiry).
	company.Status = "BAIXADA"
	if err := reopened.SaveLookup(ctx, web.CompanyLookup{CNPJ: company.CNPJ, Status: "ok", Company: &company, FetchedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	if refreshed, _ := reopened.GetLookups(ctx, []string{company.CNPJ}, now); refreshed[company.CNPJ].Company.Status != "BAIXADA" {
		t.Fatal("a second save must update the cached company")
	}

	if empty, err := reopened.GetLookups(ctx, nil, now); err != nil || len(empty) != 0 {
		t.Fatalf("no numbers = no rows: %v %v", empty, err)
	}
}

func TestCompanyStorePlaceLinksAndJobDeletion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	now := time.Now()

	store, repo := newCompanyStore(t, path)

	job := web.Job{ID: "job-1", Name: "x", Date: now, Status: web.StatusOK, Data: web.JobData{Keywords: []string{"k"}, Lang: "pt", Depth: 1}}
	if err := repo.Create(ctx, &job); err != nil {
		t.Fatal(err)
	}

	for position, number := range map[int]string{1: "24614048000158", 3: "11222333000181"} {
		if err := store.SetPlaceCNPJ(ctx, "job-1", position, number, "manual", now); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.SetPlaceCNPJ(ctx, "other-job", 1, "24614048000158", "manual", now); err != nil {
		t.Fatal(err)
	}

	// Linking again replaces the CNPJ of that place.
	if err := store.SetPlaceCNPJ(ctx, "job-1", 1, "00000000000191", "manual", now); err != nil {
		t.Fatal(err)
	}

	links, err := store.PlaceCNPJs(ctx, "job-1")
	if err != nil || len(links) != 2 || links[1] != "00000000000191" || links[3] != "11222333000181" {
		t.Fatalf("links = %v, %v", links, err)
	}

	if err := store.ClearPlaceCNPJ(ctx, "job-1", 3); err != nil {
		t.Fatal(err)
	}

	if links, _ := store.PlaceCNPJs(ctx, "job-1"); len(links) != 1 {
		t.Fatalf("after clearing one: %v", links)
	}

	// Deleting a job takes its CNPJ links with it, and only its own.
	if err := repo.Delete(ctx, "job-1"); err != nil {
		t.Fatal(err)
	}

	if links, _ := store.PlaceCNPJs(ctx, "job-1"); len(links) != 0 {
		t.Fatalf("a deleted job must not keep CNPJ links: %v", links)
	}

	if links, _ := store.PlaceCNPJs(ctx, "other-job"); len(links) != 1 {
		t.Fatalf("another job's links must stay: %v", links)
	}
}

func TestCompanyStoreDropsExpiredLookupsOnSave(t *testing.T) {
	ctx := context.Background()
	store, _ := newCompanyStore(t, filepath.Join(t.TempDir(), "jobs.db"))
	now := time.Now().Truncate(time.Second)

	old := cnpj.Company{CNPJ: "11222333000181", LegalName: "OLD"}
	if err := store.SaveLookup(ctx, web.CompanyLookup{CNPJ: old.CNPJ, Status: "ok", Company: &old, FetchedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	fresh := cnpj.Company{CNPJ: "24614048000158", LegalName: "FRESH"}
	if err := store.SaveLookup(ctx, web.CompanyLookup{CNPJ: fresh.CNPJ, Status: "ok", Company: &fresh, FetchedAt: now, ExpiresAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	// Even asking for a time in the past, the expired row is gone from the table.
	got, _ := store.GetLookups(ctx, []string{old.CNPJ, fresh.CNPJ}, now.Add(-72*time.Hour))
	if _, kept := got[old.CNPJ]; kept {
		t.Fatal("an expired lookup must be removed when a new one is saved")
	}

	if _, ok := got[fresh.CNPJ]; !ok {
		t.Fatal("the fresh lookup must stay")
	}
}
