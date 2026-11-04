//nolint:testpackage // tests unexported Hub login and send handlers directly
package web

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testSSOKey   = "0123456789abcdef0123456789abcdef-test-key"
	testJobID    = "11111111-1111-1111-1111-111111111111"
	testUsername = "ana.souza"
)

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

// mintTicket signs a ticket like the Hub does. Override claims through mutate.
func mintTicket(t *testing.T, key string, mutate func(map[string]any)) string {
	t.Helper()

	now := time.Now()
	claims := map[string]any{
		"kind": hubTicketKind, "iss": hubTicketIssuer, "aud": hubTicketAudience,
		"jti": fmt.Sprintf("jti-%d", now.UnixNano()), "iat": now.Unix(), "exp": now.Add(60 * time.Second).Unix(),
		"username": testUsername, "email": "ana@example.com", "first_name": "Ana", "last_name": "Souza",
	}

	if mutate != nil {
		mutate(claims)
	}

	header := b64([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	signing := header + "." + b64(payload)

	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(signing))

	return signing + "." + b64(mac.Sum(nil))
}

func newHubServer(t *testing.T, repo JobRepository, cfg HubConfig) *Server {
	t.Helper()

	if cfg.SSOKey == "" {
		cfg.SSOKey = testSSOKey
	}

	srv, err := New(NewService(repo, t.TempDir()), ":0", WithHub(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return srv
}

func serve(srv *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)

	return rec
}

func loginCookie(t *testing.T, srv *Server) *http.Cookie {
	t.Helper()

	rec := serve(srv, httptest.NewRequest(http.MethodGet, "/sso?ticket="+url.QueryEscape(mintTicket(t, testSSOKey, nil)), http.NoBody))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("sso status = %d: %s", rec.Code, rec.Body.String())
	}

	for _, c := range rec.Result().Cookies() {
		if c.Name == hubSessionCookie {
			return c
		}
	}

	t.Fatal("no session cookie")

	return nil
}

func TestHubConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     HubConfig
		wantErr string
	}{
		{"disabled needs nothing", HubConfig{}, ""},
		{"short key", HubConfig{SSOKey: "short"}, "at least 32"},
		{"bad api url", HubConfig{SSOKey: testSSOKey, APIURL: "ftp://x"}, "HUB_API_URL"},
		{"bad hub url", HubConfig{SSOKey: testSSOKey, URL: "javascript:alert(1)"}, "HUB_URL"},
		{"bad token", HubConfig{SSOKey: testSSOKey, AppToken: "nosecret"}, "HUB_APP_TOKEN"},
		{"ok", HubConfig{SSOKey: testSSOKey, URL: "https://hub.example", APIURL: "http://hub:8000", AppToken: "gmaps:secret"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.validate()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyTicketRejectsBadTickets(t *testing.T) {
	h, err := newHub(HubConfig{SSOKey: testSSOKey}, nil)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()

	tests := []struct {
		name   string
		ticket string
	}{
		{"valid is checked separately", ""},
		{"wrong key", mintTicket(t, "another-key-another-key-another-key-xx", nil)},
		{"wrong kind", mintTicket(t, testSSOKey, func(c map[string]any) { c["kind"] = "other-sso" })},
		{"wrong issuer", mintTicket(t, testSSOKey, func(c map[string]any) { c["iss"] = "evil" })},
		{"wrong audience", mintTicket(t, testSSOKey, func(c map[string]any) { c["aud"] = "dash-tv" })},
		{"expired", mintTicket(t, testSSOKey, func(c map[string]any) {
			c["iat"] = now.Add(-10 * time.Minute).Unix()
			c["exp"] = now.Add(-9 * time.Minute).Unix()
		})},
		{"issued in the future", mintTicket(t, testSSOKey, func(c map[string]any) {
			c["iat"] = now.Add(10 * time.Minute).Unix()
			c["exp"] = now.Add(11 * time.Minute).Unix()
		})},
		{"lifetime too long", mintTicket(t, testSSOKey, func(c map[string]any) { c["exp"] = now.Add(time.Hour).Unix() })},
		{"no username", mintTicket(t, testSSOKey, func(c map[string]any) { c["username"] = "" })},
		{"no jti", mintTicket(t, testSSOKey, func(c map[string]any) { c["jti"] = "" })},
		{"garbage", "not-a-jwt"},
		{"empty", ""},
	}

	for _, tt := range tests {
		if tt.name == "valid is checked separately" {
			continue
		}

		t.Run(tt.name, func(t *testing.T) {
			if _, err := h.verifyTicket(tt.ticket); err == nil {
				t.Fatal("ticket must be rejected")
			}
		})
	}

	if _, err := h.verifyTicket(mintTicket(t, testSSOKey, nil)); err != nil {
		t.Fatalf("valid ticket rejected: %v", err)
	}
}

func TestVerifyTicketRejectsAlgNoneAndTamperedPayload(t *testing.T) {
	h, _ := newHub(HubConfig{SSOKey: testSSOKey}, nil)

	payload, _ := json.Marshal(map[string]any{
		"kind": hubTicketKind, "iss": hubTicketIssuer, "aud": hubTicketAudience, "jti": "x",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "username": "root",
	})

	none := b64([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + b64(payload) + "."
	if _, err := h.verifyTicket(none); err == nil {
		t.Fatal("alg=none must be rejected")
	}

	// Valid signature for one payload, swapped payload.
	good := strings.Split(mintTicket(t, testSSOKey, nil), ".")
	forged := good[0] + "." + b64(payload) + "." + good[2]

	if _, err := h.verifyTicket(forged); err == nil {
		t.Fatal("tampered payload must be rejected")
	}
}

func TestSSOLoginFlow(t *testing.T) {
	srv := newHubServer(t, &statefulJobRepo{}, HubConfig{URL: "https://hub.example"})

	// No session: browsers get the login page, htmx/API get 401.
	page := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	page.Header.Set("Accept", "text/html")

	rec := serve(srv, page)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "https://hub.example") {
		t.Fatalf("login page: %d %s", rec.Code, rec.Body.String())
	}

	htmxReq := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	htmxReq.Header.Set("HX-Request", "true")

	if rec := serve(srv, htmxReq); rec.Code != http.StatusUnauthorized {
		t.Fatalf("htmx without session = %d", rec.Code)
	}

	if rec := serve(srv, httptest.NewRequest(http.MethodGet, "/api/v1/jobs", http.NoBody)); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("api without session = %d", rec.Code)
	}

	// Static assets stay public (the login page needs its CSS).
	if rec := serve(srv, httptest.NewRequest(http.MethodGet, "/static/css/main.css", http.NoBody)); rec.Code != http.StatusOK {
		t.Fatalf("static = %d", rec.Code)
	}

	// SSO: cookie is HttpOnly + SameSite, redirect has no ticket, no Referer leak.
	ticket := mintTicket(t, testSSOKey, nil)
	rec = serve(srv, httptest.NewRequest(http.MethodGet, "/sso?ticket="+url.QueryEscape(ticket), http.NoBody))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("sso: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("sso response must set Referrer-Policy: no-referrer")
	}

	var cookie *http.Cookie

	for _, c := range rec.Result().Cookies() {
		if c.Name == hubSessionCookie {
			cookie = c
		}
	}

	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie not hardened: %+v", cookie)
	}

	if strings.Contains(cookie.Value, "Ana") && !strings.Contains(cookie.Value, ".") {
		t.Fatal("cookie must be signed")
	}

	// With the session the app works and greets the person.
	req := httptest.NewRequest(http.MethodGet, "/", http.NoBody)
	req.AddCookie(cookie)

	rec = serve(srv, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Ana Souza") || !strings.Contains(rec.Body.String(), `action="/logout"`) {
		t.Fatalf("index with session: %d", rec.Code)
	}

	// The same ticket cannot be used twice.
	again := serve(srv, httptest.NewRequest(http.MethodGet, "/sso?ticket="+url.QueryEscape(ticket), http.NoBody))
	if again.Code != http.StatusUnauthorized {
		t.Fatalf("replayed ticket = %d, want 401", again.Code)
	}

	// Every refusal shows the same message: the reason is only in the log.
	bad := httptest.NewRequest(http.MethodGet, "/sso?ticket=garbage", http.NoBody)
	bad.Header.Set("Accept", "text/html")

	replay := httptest.NewRequest(http.MethodGet, "/sso?ticket="+url.QueryEscape(ticket), http.NoBody)
	replay.Header.Set("Accept", "text/html")

	badBody, replayBody := serve(srv, bad).Body.String(), serve(srv, replay).Body.String()
	if !strings.Contains(badBody, hubLoginRefused) || !strings.Contains(replayBody, hubLoginRefused) {
		t.Fatal("bad and replayed tickets must show the same generic message")
	}

	// Logout clears the cookie and goes back to the Hub.
	out := httptest.NewRequest(http.MethodPost, "/logout", http.NoBody)
	out.AddCookie(cookie)

	rec = serve(srv, out)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://hub.example" {
		t.Fatalf("logout: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}

	// Logout ends the session on the server: a copy of the old cookie is dead.
	stolen := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	stolen.AddCookie(cookie)

	if rec := serve(srv, stolen); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cookie still works after logout: %d", rec.Code)
	}
}

func TestSessionCookieCannotBeForged(t *testing.T) {
	srv := newHubServer(t, &statefulJobRepo{}, HubConfig{})
	good := loginCookie(t, srv)

	id, secret, _ := strings.Cut(good.Value, ".")

	for name, value := range map[string]string{
		"right id, wrong secret": id + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"unknown id":             "ffffffffffffffffffffff." + secret,
		"no secret":              id,
		"only dot":               ".",
		"empty":                  "",
	} {
		req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
		req.AddCookie(&http.Cookie{Name: hubSessionCookie, Value: value})

		if rec := serve(srv, req); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s accepted: %d", name, rec.Code)
		}
	}

	// The store keeps only a hash of the secret, never the secret itself.
	rec, err := srv.hub.store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	if rec.SecretHash == secret || strings.Contains(rec.SecretHash, secret) {
		t.Fatal("the store must hold a hash of the secret, not the secret")
	}
}

func TestSessionIdleAndAbsoluteLimits(t *testing.T) {
	srv := newHubServer(t, &statefulJobRepo{}, HubConfig{})
	cookie := loginCookie(t, srv)
	start := time.Now()

	get := func(at time.Time) int {
		srv.hub.now = func() time.Time { return at }

		req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
		req.AddCookie(cookie)

		return serve(srv, req).Code
	}

	if code := get(start.Add(time.Minute)); code != http.StatusOK {
		t.Fatalf("fresh session = %d", code)
	}

	// Idle: too long without use ends it (the clock restarts at each use).
	if code := get(start.Add(time.Minute + hubSessionIdle - time.Second)); code != http.StatusOK {
		t.Fatalf("just under the idle limit = %d", code)
	}

	if code := get(start.Add(time.Minute + 2*hubSessionIdle)); code != http.StatusUnauthorized {
		t.Fatalf("idle session = %d, want 401", code)
	}

	// Absolute: constant use still ends after the maximum age.
	srv2 := newHubServer(t, &statefulJobRepo{}, HubConfig{})
	cookie2 := loginCookie(t, srv2)
	now := time.Now()

	for elapsed := time.Duration(0); elapsed < hubSessionMax; elapsed += hubSessionIdle / 2 {
		srv2.hub.now = func() time.Time { return now.Add(elapsed) }

		req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
		req.AddCookie(cookie2)

		if rec := serve(srv2, req); rec.Code != http.StatusOK {
			t.Fatalf("active session ended early at %s: %d", elapsed, rec.Code)
		}
	}

	srv2.hub.now = func() time.Time { return now.Add(hubSessionMax + time.Minute) }

	req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	req.AddCookie(cookie2)

	if rec := serve(srv2, req); rec.Code != http.StatusUnauthorized {
		t.Fatalf("session past absolute limit = %d, want 401", rec.Code)
	}
}

func TestWithoutHubKeyAppStaysOpen(t *testing.T) {
	srv, err := New(NewService(&statefulJobRepo{}, t.TempDir()), ":0", WithHub(HubConfig{}))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Fatalf("no key = login off, got %d", rec.Code)
	}

	if strings.Contains(rec.Body.String(), "Enviar ao Hub") {
		t.Fatal("Hub actions must be hidden without the integration")
	}
}

func TestNewRejectsShortHubKey(t *testing.T) {
	if _, err := New(NewService(&statefulJobRepo{}, t.TempDir()), ":0", WithHub(HubConfig{SSOKey: "short"})); err == nil {
		t.Fatal("short key must fail at startup")
	}
}

func TestRevokedAccessEndsSessionButHubOutageDoesNot(t *testing.T) {
	var granted atomic.Bool

	var fail atomic.Bool

	var gotToken, gotPath string

	hubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken, gotPath = r.Header.Get("X-App-Token"), r.URL.Path

		if fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)

			return
		}

		_, _ = fmt.Fprintf(w, `{"username":%q,"module":"gmaps","exists":true,"is_active":true,"granted":%t}`, testUsername, granted.Load())
	}))
	defer hubAPI.Close()

	srv := newHubServer(t, &statefulJobRepo{}, HubConfig{APIURL: hubAPI.URL, AppToken: "gmaps:s3cret"})
	cookie := loginCookie(t, srv)

	get := func() int {
		req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
		req.AddCookie(cookie)

		return serve(srv, req).Code
	}

	// stale makes the cached "still has access" answer old, so the next
	// request asks the Hub again.
	stale := func() { srv.hub.access = map[string]accessEntry{} }

	granted.Store(true)

	if code := get(); code != http.StatusOK {
		t.Fatalf("granted = %d", code)
	}

	if gotToken != "gmaps:s3cret" || gotPath != "/api/apps/module-access/"+testUsername+"/" {
		t.Fatalf("hub call: token=%q path=%q", gotToken, gotPath)
	}

	// Revoked in the Hub: the next check ends the session for good.
	granted.Store(false)
	stale()

	rec := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
		req.AddCookie(cookie)
		req.Header.Set("Accept", "text/html")
		req.URL.Path = "/"

		return serve(srv, req)
	}()

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "foi retirado no Hub") {
		t.Fatalf("revoked = %d %q, want 401 saying access was withdrawn", rec.Code, rec.Body.String())
	}

	// Even if the Hub answers "granted" again, that old cookie stays dead: the
	// session was ended on the server, not just hidden.
	granted.Store(true)
	stale()

	if code := get(); code != http.StatusUnauthorized {
		t.Fatalf("revoked session came back: %d", code)
	}

	// A new login (new ticket) once access is back works.
	cookie = loginCookie(t, srv)

	if code := get(); code != http.StatusOK {
		t.Fatalf("after new login = %d", code)
	}

	// Hub down: fail open, nobody is locked out by an outage.
	fail.Store(true)
	stale()

	if code := get(); code != http.StatusOK {
		t.Fatalf("hub outage = %d, want 200 (fail open)", code)
	}
}

// ---- sending contacts ----

func writeContactsCSV(t *testing.T, dir string, rows int) {
	t.Helper()

	var b strings.Builder

	b.WriteString("title,link,category,address,latitude,longitude,website,phone,emails\n")

	for i := 1; i <= rows; i++ {
		switch {
		case i == 1: // no contact at all: must be skipped
			fmt.Fprintf(&b, "Sem contato %d,https://www.google.com/maps/place/x,Cat,End,%d.5,2.5,,,\n", i, i)
		case i == 2: // only e-mail, hostile website scheme
			fmt.Fprintf(&b, "Só email %d,https://www.google.com/maps/place/y,Cat,End,%d.5,2.5,javascript:alert(1),,\"a@x.example, b@x.example\"\n", i, i)
		default:
			fmt.Fprintf(&b, "Empresa %d,https://www.google.com/maps/place/z,Transportadora,Rua %d,%d.5,2.5,https://e%d.example,(54) 3000-%04d,\n", i, i, i%80, i, i)
		}
	}

	if err := os.WriteFile(filepath.Join(dir, testJobID+".csv"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func sendRequest(cookie *http.Cookie, id string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/hub/send?id="+id, http.NoBody)
	req.Header.Set("HX-Request", "true")
	req.AddCookie(cookie)

	return req
}

type sentCall struct {
	token string
	body  hubSendRequest
}

func TestSendToHub(t *testing.T) {
	var calls []sentCall

	hubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The per-request access check is a GET; only the POSTs are sends.
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"granted":true}`))

			return
		}

		var body hubSendRequest
		_ = json.NewDecoder(r.Body).Decode(&body)

		calls = append(calls, sentCall{token: r.Header.Get("X-App-Token"), body: body})

		if r.URL.Path != hubContactsPath {
			http.NotFound(w, r)

			return
		}

		// 1 already existed per call, the rest were created.
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"criadas":%d,"existentes":1,"ignoradas":[]}`, len(body.Contacts)-1)
	}))
	defer hubAPI.Close()

	repo := &statefulJobRepo{jobs: []Job{{ID: testJobID, Name: "Transportadoras Caxias", Status: StatusOK, Date: time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)}}}
	srv := newHubServer(t, repo, HubConfig{URL: "https://hub.example", APIURL: hubAPI.URL, AppToken: "gmaps:s3cret"})
	writeContactsCSV(t, srv.svc.dataFolder, 450)

	rec := serve(srv, sendRequest(loginCookie(t, srv), testJobID))
	if rec.Code != http.StatusOK {
		t.Fatalf("send = %d: %s", rec.Code, rec.Body.String())
	}

	// 449 companies with contact (the first has none) -> 3 requests of <= 200.
	if len(calls) != 3 {
		t.Fatalf("hub got %d calls, want 3", len(calls))
	}

	first := calls[0]
	if first.token != "gmaps:s3cret" || first.body.Username != testUsername {
		t.Fatalf("token/username: %+v", first)
	}

	if first.body.Batch.ID != testJobID || first.body.Batch.Title != "Transportadoras Caxias" || first.body.Batch.CreatedAt != "2026-11-04T15:00:00Z" {
		t.Fatalf("batch: %+v", first.body.Batch)
	}

	if len(first.body.Contacts) != hubMaxContacts {
		t.Fatalf("first batch has %d contacts", len(first.body.Contacts))
	}

	// Position 1 (no contact) is skipped, position 2 keeps its slot.
	c := first.body.Contacts[0]
	if c.Position != 2 || c.Name != "Só email 2" || len(c.Emails) != 2 {
		t.Fatalf("first contact: %+v", c)
	}

	if c.Website != "" {
		t.Fatalf("non-http website must be dropped, got %q", c.Website)
	}

	next := first.body.Contacts[1]
	if next.Position != 3 || next.Phone == "" || !strings.HasPrefix(next.Website, "https://") {
		t.Fatalf("second contact: %+v", next)
	}

	var answer hubSendResult
	if err := json.Unmarshal(rec.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}

	if answer.TasksURL != "https://hub.example/minhas-tarefas" {
		t.Fatalf("tasks url = %q", answer.TasksURL)
	}

	for _, want := range []string{"446 tarefas criadas", "(3 já existiam)", "1 empresas sem contato"} {
		if !strings.Contains(answer.Message, want) {
			t.Fatalf("message %q missing %q", answer.Message, want)
		}
	}
}

func TestSendToHubGuards(t *testing.T) {
	hubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer hubAPI.Close()

	repo := &statefulJobRepo{jobs: []Job{
		{ID: testJobID, Name: "Pronta", Status: StatusOK},
		{ID: "22222222-2222-2222-2222-222222222222", Name: "Rodando", Status: StatusWorking},
	}}
	srv := newHubServer(t, repo, HubConfig{APIURL: hubAPI.URL, AppToken: "gmaps:s3cret"})
	cookie := loginCookie(t, srv)

	// Missing HX-Request header (cross-site form post) is refused.
	noHeader := httptest.NewRequest(http.MethodPost, "/hub/send?id="+testJobID, http.NoBody)
	noHeader.AddCookie(cookie)

	if rec := serve(srv, noHeader); rec.Code != http.StatusBadRequest {
		t.Fatalf("without HX-Request = %d", rec.Code)
	}

	// No session -> 401.
	anon := httptest.NewRequest(http.MethodPost, "/hub/send?id="+testJobID, http.NoBody)
	anon.Header.Set("HX-Request", "true")

	if rec := serve(srv, anon); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d", rec.Code)
	}

	// GET is not allowed.
	get := httptest.NewRequest(http.MethodGet, "/hub/send?id="+testJobID, http.NoBody)
	get.AddCookie(cookie)

	if rec := serve(srv, get); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d", rec.Code)
	}

	if rec := serve(srv, sendRequest(cookie, "not-a-uuid")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad id = %d", rec.Code)
	}

	if rec := serve(srv, sendRequest(cookie, "22222222-2222-2222-2222-222222222222")); rec.Code != http.StatusConflict {
		t.Fatalf("unfinished job = %d", rec.Code)
	}

	// Finished job without a CSV.
	if rec := serve(srv, sendRequest(cookie, testJobID)); rec.Code != http.StatusNotFound {
		t.Fatalf("no csv = %d", rec.Code)
	}

	// CSV where nobody has a contact.
	if err := os.WriteFile(filepath.Join(srv.svc.dataFolder, testJobID+".csv"),
		[]byte("title,latitude,longitude\nSó nome,1.5,2.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rec := serve(srv, sendRequest(cookie, testJobID))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "telefone, site ou e-mail") {
		t.Fatalf("nothing to send = %d %q", rec.Code, rec.Body.String())
	}
}

func TestSendToHubMapsHubErrorsToFriendlyMessages(t *testing.T) {
	tests := []struct {
		status   int
		wantCode int
		wantText string
	}{
		{http.StatusUnauthorized, http.StatusBadGateway, "token do app"},
		{http.StatusServiceUnavailable, http.StatusBadGateway, "token do app"},
		{http.StatusForbidden, http.StatusForbidden, "não tem acesso"},
		{http.StatusInternalServerError, http.StatusBadGateway, "não respondeu"},
		{http.StatusBadRequest, http.StatusBadGateway, "não respondeu"},
	}

	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			hubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"detail":"contains Empresa 3 data"}`))
			}))
			defer hubAPI.Close()

			repo := &statefulJobRepo{jobs: []Job{{ID: testJobID, Name: "Pronta", Status: StatusOK}}}
			srv := newHubServer(t, repo, HubConfig{APIURL: hubAPI.URL, AppToken: "gmaps:s3cret"})
			writeContactsCSV(t, srv.svc.dataFolder, 5)

			rec := serve(srv, sendRequest(loginCookie(t, srv), testJobID))
			if rec.Code != tt.wantCode || !strings.Contains(rec.Body.String(), tt.wantText) {
				t.Fatalf("got %d %q, want %d containing %q", rec.Code, rec.Body.String(), tt.wantCode, tt.wantText)
			}

			// Never echo the Hub's body (it can repeat contact data).
			if strings.Contains(rec.Body.String(), "Empresa 3") {
				t.Fatal("Hub response body must not reach the person or the logs")
			}
		})
	}
}

func TestSendButtonOnlyWhenHubCanReceive(t *testing.T) {
	repo := &statefulJobRepo{jobs: []Job{{ID: testJobID, Name: "Pronta", Status: StatusOK, Date: time.Now()}}}

	withAPI := newHubServer(t, repo, HubConfig{APIURL: "http://hub.internal", AppToken: "gmaps:s"})
	rec := serve(withAPI, func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
		req.AddCookie(loginCookie(t, withAPI))

		return req
	}())

	if !strings.Contains(rec.Body.String(), `hx-post="/hub/send?id=`+testJobID) {
		t.Fatalf("send button missing:\n%s", rec.Body.String())
	}

	loginOnly := newHubServer(t, repo, HubConfig{})
	req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	req.AddCookie(loginCookie(t, loginOnly))

	if strings.Contains(serve(loginOnly, req).Body.String(), "hub/send") {
		t.Fatal("no API/token configured: send button must be hidden")
	}
}

func TestTicketAndSessionStateSurvivesARestart(t *testing.T) {
	store := newMemoryHubStore()

	build := func() *Server {
		srv, err := New(NewService(&statefulJobRepo{}, t.TempDir()), ":0", WithHub(HubConfig{SSOKey: testSSOKey}))
		if err != nil {
			t.Fatal(err)
		}

		srv.hub.store = store // what a persistent repository provides

		return srv
	}

	first := build()
	ticket := mintTicket(t, testSSOKey, nil)

	rec := serve(first, httptest.NewRequest(http.MethodGet, "/sso?ticket="+url.QueryEscape(ticket), http.NoBody))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login = %d", rec.Code)
	}

	var cookie *http.Cookie

	for _, c := range rec.Result().Cookies() {
		if c.Name == hubSessionCookie {
			cookie = c
		}
	}

	// A new process with the same store: the ticket is still "used" and the
	// session still works.
	second := build()

	if rec := serve(second, httptest.NewRequest(http.MethodGet, "/sso?ticket="+url.QueryEscape(ticket), http.NoBody)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("ticket reused after restart = %d, want 401", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/jobs", http.NoBody)
	req.AddCookie(cookie)

	if rec := serve(second, req); rec.Code != http.StatusOK {
		t.Fatalf("session lost after restart = %d", rec.Code)
	}
}

func TestTicketExpiryHasNoClockTolerance(t *testing.T) {
	h, _ := newHub(HubConfig{SSOKey: testSSOKey}, nil)
	now := time.Now()

	// Expired one second ago: refused (a grace period would widen replay).
	ticket := mintTicket(t, testSSOKey, func(c map[string]any) {
		c["iat"] = now.Add(-60 * time.Second).Unix()
		c["exp"] = now.Add(-time.Second).Unix()
	})

	if _, err := h.verifyTicket(ticket); err == nil {
		t.Fatal("ticket expired 1s ago must be refused")
	}
}

func TestTicketKindAndAudienceFollowHubConvention(t *testing.T) {
	// Hub satellites use kind "<slug>_sso" and aud "<slug>" (engenharia_sso /
	// engenharia, expedicao_sso / expedicao); ours is "gmaps".
	if hubTicketKind != "gmaps_sso" || hubTicketAudience != "gmaps" || hubTicketIssuer != "koria-hub" {
		t.Fatalf("ticket contract changed: kind=%q aud=%q iss=%q", hubTicketKind, hubTicketAudience, hubTicketIssuer)
	}
}
