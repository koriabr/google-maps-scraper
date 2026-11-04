//nolint:testpackage // renders the unexported job view template directly
package web

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func renderJobView(t *testing.T, places []Place) string {
	t.Helper()

	srv := newTestServer(t, t.TempDir())
	rec := httptest.NewRecorder()

	if err := srv.tmpl["static/templates/job_view.html"].Execute(rec, jobViewData{JobID: "11111111-1111-1111-1111-111111111111", Places: places}); err != nil {
		t.Fatalf("execute: %v", err)
	}

	return rec.Body.String()
}

func samplePlaces() []Place {
	return []Place{{Title: "X", Latitude: 1, Longitude: 2, Emails: []string{}}}
}

func TestJobViewUsesPinnedMapLibreWithOpenFreeMap(t *testing.T) {
	body := renderJobView(t, samplePlaces())

	for _, want := range []string{
		"cdnjs.cloudflare.com/ajax/libs/maplibre-gl/5.24.0/maplibre-gl.js",
		"cdnjs.cloudflare.com/ajax/libs/maplibre-gl/5.24.0/maplibre-gl.css",
		"https://tiles.openfreemap.org/styles/positron",
		"https://tiles.openfreemap.org/styles/dark",
		"classList.contains('light')",
		"map.setStyle(",
		"attributionControl",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("job view missing %q", want)
		}
	}

	// MapLibre must never float to another version (6.x is not used).
	if strings.Contains(body, "maplibre-gl/6") || strings.Contains(body, "maplibre-gl@") {
		t.Fatal("MapLibre must stay pinned to 5.24.0")
	}
}

func TestJobViewKeepsLeafletOSMFallback(t *testing.T) {
	body := renderJobView(t, samplePlaces())

	for _, want := range []string{
		"hasWebGL()",
		"ensureLeaflet(",
		"leaflet/1.9.4/leaflet.min.js",
		"https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png",
		"OpenStreetMap contributors",
		"NOTE_NO_WEBGL",
		"NOTE_LOAD_FAILED",
		"closeJobMap",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("job view missing %q", want)
		}
	}
}

func TestJobViewPopupsAreBuiltWithDOMNodes(t *testing.T) {
	body := renderJobView(t, samplePlaces())

	for _, banned := range []string{".setHTML(", "bindPopup('<", "setPopupContent('<"} {
		if strings.Contains(body, banned) {
			t.Fatalf("job view must build popups with DOM nodes, found %q", banned)
		}
	}

	if !strings.Contains(body, ".setDOMContent(") {
		t.Fatal("MapLibre popup must be built with setDOMContent")
	}
}

func TestMapPinIsStyledFromBrandToken(t *testing.T) {
	css, err := static.ReadFile("static/css/main.css")
	if err != nil {
		t.Fatalf("read main.css: %v", err)
	}

	_, after, found := strings.Cut(string(css), ".map-pin-shape {")
	if !found {
		t.Fatal("missing .map-pin-shape rule")
	}

	rule, _, _ := strings.Cut(after, "}")
	if !strings.Contains(rule, "hsl(var(--primary))") || strings.Contains(rule, "#") {
		t.Fatalf("pin must use the --primary token, got:\n%s", rule)
	}
}
