//nolint:testpackage // tests unexported geocoder helpers and handlers directly
package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const nominatimSample = `[
 {"display_name":"Caxias do Sul, Rio Grande do Sul, Região Sul, Brasil","lat":"-29.1685045","lon":"-51.1796385",
  "boundingbox":["-29.3570144","-28.7944879","-51.3115485","-50.7825689"]},
 {"display_name":"Sem coordenadas","lat":"abc","lon":"1"},
 {"display_name":"Fora da faixa","lat":"91","lon":"10"}
]`

func TestZoomForBoundingBox(t *testing.T) {
	tests := []struct {
		name string
		box  []string
		want int
	}{
		{"municipality", []string{"-29.35", "-28.79", "-51.31", "-50.78"}, 11},
		{"city", []string{"0", "0.2", "0", "0.2"}, 13},
		{"neighbourhood", []string{"0", "0.02", "0", "0.02"}, 16},
		{"tiny is capped", []string{"0", "0.0001", "0", "0.0001"}, geocodeMaxZoom},
		{"huge is capped", []string{"-10", "10", "-50", "50"}, geocodeMinZoom},
		{"missing box", nil, 13},
		{"garbage", []string{"a", "b", "c", "d"}, 13},
		{"inverted", []string{"0", "1", "5", "1"}, 13},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := zoomForBoundingBox(tt.box); got != tt.want {
				t.Fatalf("zoom = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNominatimSearchParsesCachesAndIdentifiesItself(t *testing.T) {
	var calls atomic.Int32

	var userAgent, query, lang string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		userAgent = r.Header.Get("User-Agent")
		query = r.URL.Query().Get("q")
		lang = r.URL.Query().Get("accept-language")

		_, _ = w.Write([]byte(nominatimSample))
	}))
	defer upstream.Close()

	geo := newNominatim(upstream.URL)

	results, err := geo.Search(context.Background(), "Caxias do Sul")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// Entries with unusable coordinates are dropped.
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(results), results)
	}

	got := results[0]
	if got.Lat != "-29.168505" || got.Lon != "-51.179639" || got.Zoom != 11 || !strings.HasPrefix(got.Name, "Caxias do Sul") {
		t.Fatalf("unexpected result: %+v", got)
	}

	if !strings.Contains(userAgent, "google-maps-scraper") {
		t.Fatalf("User-Agent must identify the app (Nominatim policy), got %q", userAgent)
	}

	if strings.Contains(userAgent, "@") {
		t.Fatalf("User-Agent must not carry personal data, got %q", userAgent)
	}

	if query != "Caxias do Sul" || !strings.HasPrefix(lang, "pt") {
		t.Fatalf("query=%q lang=%q", query, lang)
	}

	// Same text (any case/spacing) is served from the cache.
	if _, err := geo.Search(context.Background(), "  caxias   DO sul "); err != nil {
		t.Fatalf("cached Search: %v", err)
	}

	if calls.Load() != 1 {
		t.Fatalf("upstream called %d times, want 1 (second answer must be cached)", calls.Load())
	}
}

func TestNominatimSearchReportsUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	_, err := newNominatim(upstream.URL).Search(context.Background(), "Curitiba")
	if !errors.Is(err, ErrGeocodeUnavailable) {
		t.Fatalf("err = %v, want ErrGeocodeUnavailable", err)
	}
}

type fakeGeocoder struct {
	results []GeoResult
	err     error
	gotText string
}

func (f *fakeGeocoder) Search(_ context.Context, query string) ([]GeoResult, error) {
	f.gotText = query

	return f.results, f.err
}

func TestGeocodeHandler(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		target   string
		geo      *fakeGeocoder
		wantCode int
		wantBody string
	}{
		{"ok", http.MethodGet, "/geocode?q=Caxias%20do%20Sul", &fakeGeocoder{results: []GeoResult{{Name: "Caxias", Lat: "1", Lon: "2", Zoom: 12}}}, http.StatusOK, `"zoom":12`},
		{"no results is an empty list", http.MethodGet, "/geocode?q=Nada", &fakeGeocoder{results: []GeoResult{}}, http.StatusOK, "[]"},
		{"too short", http.MethodGet, "/geocode?q=ab", &fakeGeocoder{}, http.StatusUnprocessableEntity, "pelo menos 3 letras"},
		{"missing", http.MethodGet, "/geocode", &fakeGeocoder{}, http.StatusUnprocessableEntity, "pelo menos 3 letras"},
		{"too long", http.MethodGet, "/geocode?q=" + strings.Repeat("a", geocodeMaxQuery+1), &fakeGeocoder{}, http.StatusUnprocessableEntity, "muito longo"},
		{"upstream down", http.MethodGet, "/geocode?q=Curitiba", &fakeGeocoder{err: ErrGeocodeUnavailable}, http.StatusBadGateway, "manualmente"},
		{"wrong method", http.MethodPost, "/geocode?q=Curitiba", &fakeGeocoder{}, http.StatusMethodNotAllowed, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t, t.TempDir())
			srv.geo = tt.geo

			rec := httptest.NewRecorder()
			srv.geocode(rec, httptest.NewRequest(tt.method, tt.target, http.NoBody))

			if rec.Code != tt.wantCode {
				t.Fatalf("code = %d, want %d (%s)", rec.Code, tt.wantCode, rec.Body.String())
			}

			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Fatalf("body %q does not contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestGeocodeHandlerNormalisesQuery(t *testing.T) {
	srv := newTestServer(t, t.TempDir())
	fake := &fakeGeocoder{results: []GeoResult{}}
	srv.geo = fake

	rec := httptest.NewRecorder()
	srv.geocode(rec, httptest.NewRequest(http.MethodGet, "/geocode?q=%20%20Caxias%20%20do%0ASul%20", http.NoBody))

	if fake.gotText != "Caxias do Sul" {
		t.Fatalf("geocoder got %q, want collapsed whitespace", fake.gotText)
	}
}

func TestIndexOffersCitySearchWithoutSubmittingTheForm(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	rec := httptest.NewRecorder()
	srv.index(rec, httptest.NewRequest(http.MethodGet, "/", http.NoBody))

	body := rec.Body.String()

	for _, want := range []string{`id="geo-query"`, `id="geo-search"`, "/geocode?q=", "OpenStreetMap"} {
		if !strings.Contains(body, want) {
			t.Fatalf("index missing %q", want)
		}
	}

	// The search field must not be posted with the job form.
	if strings.Contains(body, `id="geo-query" name=`) || strings.Contains(body, `name="geo-query"`) {
		t.Fatal("geo-query must not have a name: it is not part of the job")
	}
}

func TestParsePlacesReadsContactEmails(t *testing.T) {
	csv := "title,latitude,longitude,website,phone,emails\n" +
		"Com email,1.5,2.5,https://a.example,(54) 1111,\"a@a.example, b@a.example\"\n" +
		"Sem email,1.6,2.6,,,\n"

	places, err := parsePlaces(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parsePlaces: %v", err)
	}

	if len(places) != 2 {
		t.Fatalf("got %d places", len(places))
	}

	if got := places[0].Emails; len(got) != 2 || got[0] != "a@a.example" || got[1] != "b@a.example" {
		t.Fatalf("emails = %q", got)
	}

	// Must be [] (not null) so the embedded JSON is safe to iterate in the page.
	if places[1].Emails == nil || len(places[1].Emails) != 0 {
		t.Fatalf("empty emails must be a non-nil empty slice, got %#v", places[1].Emails)
	}

	if places[0].Phone != "(54) 1111" || places[0].Website != "https://a.example" {
		t.Fatalf("phone/website not read: %+v", places[0])
	}
}

func TestJobViewShowsContactsAndCopyButton(t *testing.T) {
	srv := newTestServer(t, t.TempDir())

	rec := httptest.NewRecorder()

	tmpl := srv.tmpl["static/templates/job_view.html"]
	if err := tmpl.Execute(rec, []Place{{Title: "X", Latitude: 1, Longitude: 2, Phone: "123", Emails: []string{}}}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{"place-copy", "Telefone: ", "mailto:", "tel:", "isSecureContext"} {
		if !strings.Contains(body, want) {
			t.Fatalf("job view missing %q", want)
		}
	}
}

func TestParsePlacesDropsPlaceholderEmails(t *testing.T) {
	csv := "title,latitude,longitude,emails\n" +
		"A,1.5,2.5,\"contato@empresa.com.br, seuemail@email.com\"\n" +
		"B,1.6,2.6,\"seuemail@email.com\"\n"

	places, err := parsePlaces(strings.NewReader(csv))
	if err != nil {
		t.Fatalf("parsePlaces: %v", err)
	}

	if got := places[0].Emails; len(got) != 1 || got[0] != "contato@empresa.com.br" {
		t.Fatalf("emails = %q, want only the real one", got)
	}

	if places[1].Emails == nil || len(places[1].Emails) != 0 {
		t.Fatalf("a place with only a placeholder must end with no emails, got %#v", places[1].Emails)
	}
}
