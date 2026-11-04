package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	nominatimURL = "https://nominatim.openstreetmap.org/search"

	// nominatimUserAgent identifies this app, as the Nominatim usage policy
	// requires. It carries no personal data.
	nominatimUserAgent = "google-maps-scraper-koria-hub/1.0 (+https://github.com/koriabr/google-maps-scraper)"

	geocodeMaxResults = 5
	geocodeMinQuery   = 3
	geocodeMaxQuery   = 200
	geocodeTimeout    = 8 * time.Second

	// nominatimMinInterval honours the public instance's limit of one
	// request per second.
	nominatimMinInterval = time.Second

	geocodeCacheTTL     = 24 * time.Hour
	geocodeCacheMaxSize = 500

	// Zoom bounds for the zoom suggested from a place's bounding box.
	geocodeMinZoom = 11
	geocodeMaxZoom = 17

	// viewportDegreesAtZoom0 is how many degrees of longitude a ~1440px wide
	// Google Maps viewport spans at zoom 0 (360 * 1440 / 256).
	viewportDegreesAtZoom0 = 2025.0
)

// ErrGeocodeUnavailable is returned when the geocoding service cannot be
// reached or answers with an error.
var ErrGeocodeUnavailable = errors.New("geocoding service unavailable")

// GeoResult is a place found by a geocoder, ready to anchor a search.
type GeoResult struct {
	Name string `json:"name"`
	Lat  string `json:"lat"`
	Lon  string `json:"lon"`
	// Zoom is a suggested Google Maps zoom level that frames the whole place.
	Zoom int `json:"zoom"`
}

// Geocoder turns free text (a city, a neighbourhood, an address) into
// coordinates.
type Geocoder interface {
	Search(ctx context.Context, query string) ([]GeoResult, error)
}

// nominatim is a Geocoder backed by an OpenStreetMap Nominatim instance. It
// spaces its requests and caches answers so it stays within the public
// instance's usage policy.
type nominatim struct {
	baseURL string
	client  *http.Client

	mu      sync.Mutex
	lastReq time.Time
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	results []GeoResult
	expires time.Time
}

func newNominatim(baseURL string) *nominatim {
	return &nominatim{
		baseURL: baseURL,
		client:  &http.Client{Timeout: geocodeTimeout},
		cache:   make(map[string]cacheEntry),
	}
}

type nominatimPlace struct {
	DisplayName string   `json:"display_name"`
	Lat         string   `json:"lat"`
	Lon         string   `json:"lon"`
	BoundingBox []string `json:"boundingbox"`
}

func (n *nominatim) Search(ctx context.Context, query string) ([]GeoResult, error) {
	key := strings.ToLower(strings.Join(strings.Fields(query), " "))

	if results, ok := n.cached(key); ok {
		return results, nil
	}

	if err := n.waitTurn(ctx); err != nil {
		return nil, err
	}

	params := url.Values{
		"q":               {query},
		"format":          {"jsonv2"},
		"limit":           {strconv.Itoa(geocodeMaxResults)},
		"accept-language": {"pt-BR,pt,en"},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.baseURL+"?"+params.Encode(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build geocode request: %w", err)
	}

	req.Header.Set("User-Agent", nominatimUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrGeocodeUnavailable, err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrGeocodeUnavailable, resp.StatusCode)
	}

	var places []nominatimPlace
	if err := json.NewDecoder(resp.Body).Decode(&places); err != nil {
		return nil, fmt.Errorf("%w: decode: %w", ErrGeocodeUnavailable, err)
	}

	results := make([]GeoResult, 0, len(places))

	for i := range places {
		if result, ok := toGeoResult(&places[i]); ok {
			results = append(results, result)
		}
	}

	n.store(key, results)

	return results, nil
}

func (n *nominatim) cached(key string) ([]GeoResult, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	entry, ok := n.cache[key]
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}

	return entry.results, true
}

func (n *nominatim) store(key string, results []GeoResult) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if len(n.cache) >= geocodeCacheMaxSize {
		clear(n.cache)
	}

	n.cache[key] = cacheEntry{results: results, expires: time.Now().Add(geocodeCacheTTL)}
}

// waitTurn blocks until at least nominatimMinInterval has passed since the
// previous request, or until ctx is done.
func (n *nominatim) waitTurn(ctx context.Context) error {
	n.mu.Lock()
	wait := nominatimMinInterval - time.Since(n.lastReq)
	if wait < 0 {
		wait = 0
	}

	// Reserve the slot now so concurrent callers queue behind each other.
	n.lastReq = time.Now().Add(wait)
	n.mu.Unlock()

	if wait == 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// toGeoResult validates a Nominatim place and converts it. Places with
// unusable coordinates are dropped.
func toGeoResult(place *nominatimPlace) (GeoResult, bool) {
	lat, errLat := strconv.ParseFloat(place.Lat, 64)
	lon, errLon := strconv.ParseFloat(place.Lon, 64)

	if errLat != nil || errLon != nil || !finite(lat) || !finite(lon) {
		return GeoResult{}, false
	}

	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return GeoResult{}, false
	}

	return GeoResult{
		Name: place.DisplayName,
		Lat:  strconv.FormatFloat(lat, 'f', 6, 64),
		Lon:  strconv.FormatFloat(lon, 'f', 6, 64),
		Zoom: zoomForBoundingBox(place.BoundingBox),
	}, true
}

// zoomForBoundingBox suggests the Google Maps zoom that fits a Nominatim
// bounding box ([south, north, west, east]) in a typical viewport: wide places
// (a municipality) get a lower zoom than small ones (a neighbourhood).
func zoomForBoundingBox(box []string) int {
	const defaultZoom = 13

	if len(box) != 4 {
		return defaultZoom
	}

	west, errW := strconv.ParseFloat(box[2], 64)
	east, errE := strconv.ParseFloat(box[3], 64)

	span := east - west
	if errW != nil || errE != nil || span <= 0 || !finite(span) {
		return defaultZoom
	}

	zoom := int(math.Floor(math.Log2(viewportDegreesAtZoom0 / span)))

	return min(max(zoom, geocodeMinZoom), geocodeMaxZoom)
}

// geocode handles GET /geocode?q=...: it looks up a place by name and answers
// with candidates the form can use to fill latitude, longitude and zoom.
func (s *Server) geocode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	query := strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")

	if len([]rune(query)) < geocodeMinQuery {
		http.Error(w, "Digite pelo menos 3 letras da cidade ou do endereço.", http.StatusUnprocessableEntity)

		return
	}

	if len([]rune(query)) > geocodeMaxQuery {
		http.Error(w, "Texto muito longo. Use o nome da cidade ou do endereço.", http.StatusUnprocessableEntity)

		return
	}

	results, err := s.geo.Search(r.Context(), query)
	if err != nil {
		if r.Context().Err() == nil {
			log.Printf("geocode: %v", err)
		}

		http.Error(w, "Não foi possível buscar a localização agora. Informe latitude e longitude manualmente ou tente de novo.", http.StatusBadGateway)

		return
	}

	renderJSON(w, http.StatusOK, results)
}
