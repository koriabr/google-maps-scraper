//nolint:testpackage // tests unexported handlers (scrape, getJobs, redocHandler) directly
package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// statefulJobRepo is an in-memory JobRepository that honours status filters
// and persists updates.
type statefulJobRepo struct {
	jobs []Job
}

func (r *statefulJobRepo) Get(_ context.Context, id string) (Job, error) {
	for i := range r.jobs {
		if r.jobs[i].ID == id {
			return r.jobs[i], nil
		}
	}

	return Job{}, nil
}

func (r *statefulJobRepo) Create(_ context.Context, job *Job) error {
	r.jobs = append([]Job{*job}, r.jobs...)

	return nil
}

func (r *statefulJobRepo) Delete(_ context.Context, _ string) error {
	return nil
}

func (r *statefulJobRepo) Update(_ context.Context, job *Job) error {
	for i := range r.jobs {
		if r.jobs[i].ID == job.ID {
			r.jobs[i] = *job
		}
	}

	return nil
}

func (r *statefulJobRepo) Select(_ context.Context, params SelectParams) ([]Job, error) {
	var res []Job

	for i := range r.jobs {
		if params.Status == "" || r.jobs[i].Status == params.Status {
			res = append(res, r.jobs[i])
		}
	}

	return res, nil
}

func validForm() url.Values {
	return url.Values{
		"name":      {""},
		"maxtime":   {"10m"},
		"keywords":  {"padarias em Curitiba\n\n  cafés em Curitiba  \n"},
		"lang":      {"pt"},
		"zoom":      {"15"},
		"radius":    {"10000"},
		"latitude":  {""},
		"longitude": {""},
		"depth":     {"10"},
	}
}

func postScrape(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/scrape", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	srv.scrape(rec, req)

	return rec
}

func TestIndexDoesNotAnchorSearchesToOceanOrShowPromos(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	rec := httptest.NewRecorder()
	srv.index(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

	body := rec.Body.String()

	// Defaults of 0,0 sent every web search to the middle of the ocean.
	if strings.Contains(body, `name="latitude" value="0"`) || strings.Contains(body, `name="longitude" value="0"`) {
		t.Fatal("latitude/longitude must not default to 0")
	}

	lower := strings.ToLower(body)
	for _, banned := range []string{"sponsor", "star on github", "starring"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("index still mentions %q", banned)
		}
	}

	if !strings.Contains(body, "KORIA_Selo.png") {
		t.Fatal("index must use the KORIA seal as favicon")
	}

	if !strings.Contains(body, `href="/api/docs"`) {
		t.Fatal("index must keep the link to the API docs")
	}
}

func TestScrapeWithoutCoordinatesDoesNotSetThem(t *testing.T) {
	repo := &statefulJobRepo{}
	srv := newTestServerWithRepo(t, repo)

	rec := postScrape(t, srv, validForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(repo.jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(repo.jobs))
	}

	job := repo.jobs[0]
	if job.Data.Lat != "" || job.Data.Lon != "" {
		t.Fatalf("coordinates must stay empty, got %q,%q", job.Data.Lat, job.Data.Lon)
	}

	if job.Name != "padarias em Curitiba (+1)" {
		t.Fatalf("name should default from keywords, got %q", job.Name)
	}

	if len(job.Data.Keywords) != 2 || job.Data.Keywords[1] != "cafés em Curitiba" {
		t.Fatalf("keywords not trimmed: %q", job.Data.Keywords)
	}
}

func TestScrapeRejectsBadCoordinates(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon string
		want     string
	}{
		{"only latitude", "-25.4", "", "latitude e longitude juntas"},
		{"only longitude", "", "-49.2", "latitude e longitude juntas"},
		{"latitude out of range", "91", "10", "Latitude inválida"},
		{"longitude out of range", "10", "181", "Longitude inválida"},
		{"not a number", "abc", "10", "Latitude inválida"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &statefulJobRepo{}
			srv := newTestServerWithRepo(t, repo)

			form := validForm()
			form.Set("latitude", tt.lat)
			form.Set("longitude", tt.lon)

			rec := postScrape(t, srv, form)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d", rec.Code)
			}

			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body %q does not mention %q", rec.Body.String(), tt.want)
			}

			if len(repo.jobs) != 0 {
				t.Fatal("invalid form must not create a job")
			}
		})
	}
}

func TestScrapeAcceptsValidCoordinates(t *testing.T) {
	repo := &statefulJobRepo{}
	srv := newTestServerWithRepo(t, repo)

	form := validForm()
	form.Set("latitude", "-25.4284")
	form.Set("longitude", "-49.2733")

	if rec := postScrape(t, srv, form); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if repo.jobs[0].Data.Lat != "-25.4284" || repo.jobs[0].Data.Lon != "-49.2733" {
		t.Fatalf("coordinates not stored: %+v", repo.jobs[0].Data)
	}
}

func TestScrapeValidationMessagesAreInPortuguese(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(url.Values)
		want   string
	}{
		{"no keywords", func(f url.Values) { f.Set("keywords", "  \n ") }, "palavra-chave"},
		{"short max time", func(f url.Values) { f.Set("maxtime", "1m") }, "pelo menos 3m"},
		{"bad max time", func(f url.Values) { f.Set("maxtime", "soon") }, "Tempo máximo inválido"},
		{"bad lang", func(f url.Values) { f.Set("lang", "por") }, "Idioma inválido"},
		{"fast mode without coordinates", func(f url.Values) { f.Set("fastmode", "on") }, "modo rápido exige"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServerWithRepo(t, &statefulJobRepo{})

			form := validForm()
			tt.mutate(form)

			rec := postScrape(t, srv, form)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d", rec.Code)
			}

			if !strings.Contains(rec.Body.String(), tt.want) {
				t.Fatalf("body %q does not mention %q", rec.Body.String(), tt.want)
			}
		})
	}
}

func TestGetJobsShowsRunningJobsAndPollsFaster(t *testing.T) {
	repo := &statefulJobRepo{jobs: []Job{
		{ID: "11111111-1111-1111-1111-111111111111", Name: "Rodando", Status: StatusWorking, Date: time.Now().Add(-time.Minute), Data: JobData{Keywords: []string{"x"}}},
		{ID: "22222222-2222-2222-2222-222222222222", Name: "Esperando", Status: StatusPending, Date: time.Now(), Data: JobData{Keywords: []string{"y"}}},
		{ID: "33333333-3333-3333-3333-333333333333", Name: "Pronta", Status: StatusOK, Date: time.Now().Add(-time.Hour), Data: JobData{Keywords: []string{"z"}}},
	}}
	srv := newTestServerWithRepo(t, repo)

	rec := httptest.NewRecorder()
	srv.getJobs(rec, httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody))

	body := rec.Body.String()

	for _, want := range []string{
		`hx-trigger="every 4s"`,
		"1 em execução · 1 na fila",
		`data-active="2"`,
		"Em execução agora",
		"Executando",
		"Na fila",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %q:\n%s", want, body)
		}
	}

	// Out-of-band pieces must not follow a bare <tbody>: browsers move them
	// out of the table and htmx would lose them. They follow the region div.
	if strings.Index(body, `id="run-status"`) < strings.Index(body, `id="jobs-region"`) {
		t.Fatal("run-status must come after the jobs region")
	}
}

func TestGetJobsIdleHasNoActivePanel(t *testing.T) {
	srv := newTestServerWithRepo(t, &statefulJobRepo{jobs: []Job{
		{ID: "33333333-3333-3333-3333-333333333333", Name: "Pronta", Status: StatusOK, Date: time.Now()},
	}})

	rec := httptest.NewRecorder()
	srv.getJobs(rec, httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody))

	body := rec.Body.String()
	if strings.Contains(body, "Em execução agora") {
		t.Fatal("idle response must not render the active panel")
	}

	if !strings.Contains(body, "Nenhuma pesquisa em execução") || !strings.Contains(body, `data-active="0"`) {
		t.Fatalf("idle status missing:\n%s", body)
	}

	if !strings.Contains(body, `<div id="active-panel" hx-swap-oob="true"></div>`) {
		t.Fatal("idle response must clear the active panel")
	}
}

func TestActiveJobsIgnoresReposThatDropTheStatusFilter(t *testing.T) {
	repo := &legacyJobRepo{jobs: []Job{
		{ID: "1", Status: StatusOK},
		{ID: "2", Status: StatusWorking},
		{ID: "3", Status: StatusFailed},
		{ID: "4", Status: StatusPending},
	}}

	active, err := NewService(repo, t.TempDir()).ActiveJobs(context.Background())
	if err != nil {
		t.Fatalf("ActiveJobs: %v", err)
	}

	if len(active) != 2 || active[0].ID != "2" || active[1].ID != "4" {
		t.Fatalf("active = %+v, want jobs 2 (working) then 4 (pending)", active)
	}
}

func TestFailInterruptedJobs(t *testing.T) {
	repo := &statefulJobRepo{jobs: []Job{
		{ID: "1", Status: StatusWorking},
		{ID: "2", Status: StatusPending},
		{ID: "3", Status: StatusOK},
		{ID: "4", Status: StatusWorking},
	}}

	count, err := NewService(repo, t.TempDir()).FailInterruptedJobs(context.Background())
	if err != nil {
		t.Fatalf("FailInterruptedJobs: %v", err)
	}

	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	want := map[string]string{"1": StatusFailed, "2": StatusPending, "3": StatusOK, "4": StatusFailed}
	for i := range repo.jobs {
		if repo.jobs[i].Status != want[repo.jobs[i].ID] {
			t.Fatalf("job %s status = %q, want %q", repo.jobs[i].ID, repo.jobs[i].Status, want[repo.jobs[i].ID])
		}
	}
}

func TestDocsPageUsesPinnedLocalRedoc(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	rec := httptest.NewRecorder()
	srv.redocHandler(rec, httptest.NewRequest(http.MethodGet, "/api/docs", http.NoBody))

	body := rec.Body.String()

	// redoc@latest moved to a bundle that needs import.meta and rendered a
	// blank page; the docs must load a pinned copy served by this app.
	if strings.Contains(body, "cdn.redoc.ly/redoc/latest") {
		t.Fatal("docs must not load redoc@latest from the CDN")
	}

	const script = "/static/lib/redoc-2.5.1.standalone.js"

	if !strings.Contains(body, script) {
		t.Fatalf("docs page does not load %s", script)
	}
}

func TestAPISpecIsValidForRedoc(t *testing.T) {
	data, err := static.ReadFile("static/spec/spec.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}

	spec := string(data)

	// x-codeSamples must be a list of {lang, source}; a bare map made Redoc
	// fail to render. The deprecated x-code-samples spelling is not used.
	if strings.Contains(spec, "x-code-samples") {
		t.Fatal("spec uses deprecated x-code-samples")
	}

	if strings.Count(spec, "x-codeSamples:") != strings.Count(spec, "- lang: curl") {
		t.Fatal("every x-codeSamples block must hold a list with lang and source")
	}
}
