//nolint:testpackage // tests unexported CNPJ service helpers and handlers directly
package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/internal/cnpj"
)

const (
	testCNPJ      = "24.614.048/0001-58"
	testCNPJDigit = "24614048000158"
)

// fakeCNPJ answers lookups from a script and counts the calls.
type fakeCNPJ struct {
	calls   int
	err     error
	company cnpj.Company
}

func (f *fakeCNPJ) Lookup(_ context.Context, number string) (cnpj.Company, error) {
	f.calls++

	if f.err != nil {
		return cnpj.Company{}, f.err
	}

	company := f.company
	company.CNPJ = number

	return company, nil
}

func sampleCompany() cnpj.Company {
	return cnpj.Company{
		LegalName: "TRANSFER TRANSPORTES URGENTES LTDA", Status: "ATIVA", OpeningDate: "2016-04-18",
		CNAECode: "4930202", CNAEText: "Transporte rodoviário de carga", Size: "MICRO EMPRESA",
		City: "CAXIAS DO SUL", State: "RS", RegisteredTel: "5432127047",
	}
}

// companyService builds a Service over a temp folder holding the sample CSV.
func companyService(t *testing.T, fake *fakeCNPJ) *Service {
	t.Helper()

	svc := NewService(&statefulJobRepo{}, t.TempDir())
	svc.cnpj = fake
	writeContactsCSV(t, svc.dataFolder, 6)

	return svc
}

func TestSetPlaceCNPJValidatesBeforeAnyLookup(t *testing.T) {
	fake := &fakeCNPJ{company: sampleCompany()}
	svc := companyService(t, fake)

	if _, err := svc.SetPlaceCNPJ(context.Background(), testJobID, 2, "123"); !errors.Is(err, cnpj.ErrInvalid) {
		t.Fatalf("invalid number: %v", err)
	}

	if _, err := svc.SetPlaceCNPJ(context.Background(), testJobID, 99, testCNPJ); !errors.Is(err, ErrPlacesNotFound) {
		t.Fatalf("position out of range: %v", err)
	}

	if _, err := svc.SetPlaceCNPJ(context.Background(), testJobID, 0, testCNPJ); !errors.Is(err, ErrPlacesNotFound) {
		t.Fatalf("position 0: %v", err)
	}

	if fake.calls != 0 {
		t.Fatalf("the registry was queried %d times for requests that could not succeed", fake.calls)
	}
}

func TestSetPlaceCNPJCachesLookupsAcrossPlacesAndJobs(t *testing.T) {
	fake := &fakeCNPJ{company: sampleCompany()}
	svc := companyService(t, fake)

	view, err := svc.SetPlaceCNPJ(context.Background(), testJobID, 2, testCNPJ)
	if err != nil {
		t.Fatalf("SetPlaceCNPJ: %v", err)
	}

	if view.CNPJ != testCNPJDigit || view.CNPJFormatted != testCNPJ || !view.Active || view.Source != "manual" {
		t.Fatalf("view = %+v", view)
	}

	// Same CNPJ for another place: served from the cache table, no new query.
	if _, err := svc.SetPlaceCNPJ(context.Background(), testJobID, 3, "24614048000158"); err != nil {
		t.Fatalf("second place: %v", err)
	}

	if fake.calls != 1 {
		t.Fatalf("registry queried %d times, want 1 (the second must come from the cache)", fake.calls)
	}
}

func TestSetPlaceCNPJCachesNotFoundButNotTransientErrors(t *testing.T) {
	fake := &fakeCNPJ{err: cnpj.ErrNotFound}
	svc := companyService(t, fake)

	for range 2 {
		if _, err := svc.SetPlaceCNPJ(context.Background(), testJobID, 2, testCNPJ); !errors.Is(err, cnpj.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	}

	if fake.calls != 1 {
		t.Fatalf("a number that does not exist must be remembered: %d queries", fake.calls)
	}

	// Nothing was linked for a CNPJ that does not exist.
	links, _ := svc.companies.PlaceCNPJs(context.Background(), testJobID)
	if len(links) != 0 {
		t.Fatalf("links = %v", links)
	}

	// A rate limit or outage is never cached: the next try must ask again.
	transient := &fakeCNPJ{err: cnpj.ErrRateLimited}
	svc2 := companyService(t, transient)

	for range 2 {
		_, _ = svc2.SetPlaceCNPJ(context.Background(), testJobID, 2, testCNPJ)
	}

	if transient.calls != 2 {
		t.Fatalf("transient errors must not be cached: %d queries", transient.calls)
	}
}

func TestPlacesCarryTheirCompanyAndClearRemovesIt(t *testing.T) {
	svc := companyService(t, &fakeCNPJ{company: sampleCompany()})
	ctx := context.Background()

	if _, err := svc.SetPlaceCNPJ(ctx, testJobID, 3, testCNPJ); err != nil {
		t.Fatal(err)
	}

	places, err := svc.GetPlacesWithCompanies(ctx, testJobID)
	if err != nil {
		t.Fatal(err)
	}

	for i := range places {
		if places[i].Position != i+1 {
			t.Fatalf("place %d has position %d", i, places[i].Position)
		}

		if (places[i].Position == 3) != (places[i].Company != nil) {
			t.Fatalf("only position 3 must carry a company, place %d: %+v", places[i].Position, places[i].Company)
		}
	}

	if err := svc.ClearPlaceCNPJ(ctx, testJobID, 3); err != nil {
		t.Fatal(err)
	}

	places, _ = svc.GetPlacesWithCompanies(ctx, testJobID)
	if places[2].Company != nil {
		t.Fatal("the company must be gone after clearing")
	}
}

func TestCompanyViewAgreesOrDisagreesWithMaps(t *testing.T) {
	company := sampleCompany()
	company.CNPJ = testCNPJDigit

	matching := &Place{Phone: "(54) 3212-7047", Address: "R. José Francisco Goularte, 136 - Desvio Rizzo, Caxias do Sul - RS"}
	view := newCompanyView(&company, "manual", matching, mustDate(t, "2026-11-04"))

	if view.PhoneMatch == nil || !*view.PhoneMatch || view.CityMatch == nil || !*view.CityMatch {
		t.Fatalf("phone and city must match: %+v", view)
	}

	if view.AgeYears != 10 || view.CNAE != "4930-2/02 — Transporte rodoviário de carga" || view.Location != "CAXIAS DO SUL/RS" {
		t.Fatalf("view = %+v", view)
	}

	other := &Place{Phone: "(11) 99999-0000", Address: "Av. Paulista, 1000 - São Paulo - SP"}
	view = newCompanyView(&company, "manual", other, mustDate(t, "2026-11-04"))

	if view.PhoneMatch == nil || *view.PhoneMatch || view.CityMatch == nil || *view.CityMatch {
		t.Fatalf("a different company must be flagged: %+v", view)
	}

	// Nothing to compare: no verdict either way.
	view = newCompanyView(&company, "manual", &Place{}, mustDate(t, "2026-11-04"))
	if view.PhoneMatch != nil || view.CityMatch != nil {
		t.Fatalf("no data must give no verdict: %+v", view)
	}
}

func TestCityMatchingIgnoresAccentsAndCase(t *testing.T) {
	for address, want := range map[string]bool{
		"Av. Brasil, 10 - São José dos Pinhais - PR": true,
		"AV BRASIL 10, SAO JOSE DOS PINHAIS":         true,
		"R. Sete, 1 - José, Curitiba":                false, // "jose" alone is not the city
	} {
		got := cityMatches("SÃO JOSÉ DOS PINHAIS", address)
		if got == nil || *got != want {
			t.Errorf("cityMatches(%q) = %v, want %v", address, got, want)
		}
	}
}

// ---- HTTP ----

func cnpjPost(srv *Server, path string, form url.Values, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")

	if mutate != nil {
		mutate(req)
	}

	req = requestWithID(req)
	rec := httptest.NewRecorder()

	switch {
	case strings.HasPrefix(path, "/cnpj/set"):
		srv.setCNPJ(rec, req)
	default:
		srv.clearCNPJ(rec, req)
	}

	return rec
}

func companyServer(t *testing.T, fake *fakeCNPJ) *Server {
	t.Helper()

	srv := newTestServerWithRepo(t, &statefulJobRepo{})
	srv.svc.cnpj = fake
	writeContactsCSV(t, srv.svc.dataFolder, 6)

	return srv
}

func TestCNPJHandlers(t *testing.T) {
	setURL := "/cnpj/set?id=" + testJobID + "&pos=2"
	form := url.Values{"cnpj": {testCNPJ}}

	tests := []struct {
		name     string
		path     string
		form     url.Values
		mutate   func(*http.Request)
		fake     *fakeCNPJ
		wantCode int
		wantBody string
	}{
		{"ok", setURL, form, nil, &fakeCNPJ{company: sampleCompany()}, http.StatusOK, `"razao_social":"TRANSFER TRANSPORTES URGENTES LTDA"`},
		{"missing HX-Request", setURL, form, func(r *http.Request) { r.Header.Del("HX-Request") }, &fakeCNPJ{}, http.StatusBadRequest, "Requisição inválida"},
		{"wrong method", setURL, form, func(r *http.Request) { r.Method = http.MethodGet }, &fakeCNPJ{}, http.StatusMethodNotAllowed, ""},
		{"bad job id", "/cnpj/set?id=nope&pos=2", form, nil, &fakeCNPJ{}, http.StatusUnprocessableEntity, "Pesquisa inválida"},
		{"bad position", "/cnpj/set?id=" + testJobID + "&pos=abc", form, nil, &fakeCNPJ{}, http.StatusUnprocessableEntity, "Empresa inválida"},
		{"invalid cnpj", setURL, url.Values{"cnpj": {"123"}}, nil, &fakeCNPJ{}, http.StatusUnprocessableEntity, "CNPJ inválido"},
		{"not found", setURL, form, nil, &fakeCNPJ{err: cnpj.ErrNotFound}, http.StatusNotFound, "não foi encontrado na Receita"},
		{"rate limited", setURL, form, nil, &fakeCNPJ{err: cnpj.ErrRateLimited}, http.StatusTooManyRequests, "limitada"},
		{"api down", setURL, form, nil, &fakeCNPJ{err: cnpj.ErrUnavailable}, http.StatusBadGateway, "Não foi possível consultar"},
		{"unknown place", "/cnpj/set?id=" + testJobID + "&pos=99", form, nil, &fakeCNPJ{}, http.StatusNotFound, "Empresa não encontrada"},
		{"clear", "/cnpj/clear?id=" + testJobID + "&pos=2", nil, nil, &fakeCNPJ{}, http.StatusOK, `"ok":true`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := cnpjPost(companyServer(t, tt.fake), tt.path, tt.form, tt.mutate)

			if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("got %d %q, want %d containing %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantBody)
			}
		})
	}
}

func TestMapModalShowsTheLinkedCompany(t *testing.T) {
	srv := companyServer(t, &fakeCNPJ{company: sampleCompany()})

	if rec := cnpjPost(srv, "/cnpj/set?id="+testJobID+"&pos=2", url.Values{"cnpj": {testCNPJ}}, nil); rec.Code != http.StatusOK {
		t.Fatalf("set = %d: %s", rec.Code, rec.Body.String())
	}

	repo := srv.svc.repo.(*statefulJobRepo)
	repo.jobs = []Job{{ID: testJobID, Name: "x", Status: StatusOK}}

	rec := httptest.NewRecorder()
	req := requestWithID(httptest.NewRequest(http.MethodGet, "/view?id="+testJobID, http.NoBody))
	srv.viewJob(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"TRANSFER TRANSPORTES URGENTES LTDA", "cnpj_formatado", "/cnpj/set", "Informar CNPJ"} {
		if !strings.Contains(body, want) {
			t.Fatalf("modal missing %q", want)
		}
	}
}

func TestCNPJEndpointsNeedTheHubSession(t *testing.T) {
	srv := newHubServer(t, &statefulJobRepo{}, HubConfig{})

	req := httptest.NewRequest(http.MethodPost, "/cnpj/set?id="+testJobID+"&pos=1", strings.NewReader("cnpj="+testCNPJ))
	req.Header.Set("HX-Request", "true")

	if rec := serve(srv, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session = %d, want 401", rec.Code)
	}
}

func TestSQLiteStyleStoreContract(t *testing.T) {
	// The memory store must honour the same TTL contract as the sqlite one.
	store := newMemoryCompanyStore()
	ctx := context.Background()
	now := mustDate(t, "2026-11-04")

	company := sampleCompany()
	company.CNPJ = testCNPJDigit

	if err := store.SaveLookup(ctx, CompanyLookup{CNPJ: testCNPJDigit, Status: lookupStatusOK, Company: &company, FetchedAt: now, ExpiresAt: now.Add(lookupTTLFound)}); err != nil {
		t.Fatal(err)
	}

	hit, _ := store.GetLookups(ctx, []string{testCNPJDigit}, now.Add(lookupTTLFound-1))
	if _, ok := hit[testCNPJDigit]; !ok {
		t.Fatal("an unexpired lookup must be returned")
	}

	miss, _ := store.GetLookups(ctx, []string{testCNPJDigit}, now.Add(lookupTTLFound+1))
	if len(miss) != 0 {
		t.Fatal("an expired lookup must not be returned")
	}
}

func mustDate(t *testing.T, day string) time.Time {
	t.Helper()

	parsed, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}

	return parsed
}
