package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Koria Hub integration.
//
// When HUB_SSO_SHARED_KEY is set the app requires a login that comes from the
// Hub: the Hub issues a short-lived signed ticket (HS256 JWT), the browser is
// redirected to /sso?ticket=..., and the app opens its own session cookie. The
// ticket only asserts identity; the Hub decides who may enter (a permission
// module) and the app keeps asking the Hub whether the person still has it.
// Without the key the app behaves as before, with no login.

const (
	hubTicketKind     = "gmaps_sso"
	hubTicketIssuer   = "koria-hub"
	hubTicketAudience = "gmaps"

	// hubMinKeyBytes is the minimum size of the shared signing key.
	hubMinKeyBytes = 32

	// hubTicketMaxTTL is the longest ticket lifetime accepted (exp - iat).
	hubTicketMaxTTL = 120 * time.Second
	// hubClockSkew tolerates small clock differences between Hub and app.
	hubClockSkew = 30 * time.Second

	hubSessionCookie = "gmaps_session"
	// A session ends after hubSessionIdle without use, or hubSessionMax after
	// it opened, whichever comes first (same limits as the Engenharia module).
	hubSessionIdle = 8 * time.Hour
	hubSessionMax  = 7 * 24 * time.Hour
	// hubTouchEvery limits how often "last seen" is written.
	hubTouchEvery = time.Minute

	// hubTicketKeep is how long a used ticket id is remembered. The real replay
	// window is hubTicketMaxTTL + hubClockSkew; this leaves a wide margin.
	hubTicketKeep = 15 * time.Minute

	// hubAccessRecheck is how long a "still has access" answer is trusted.
	hubAccessRecheck = 5 * time.Minute
	hubAPITimeout    = 5 * time.Second
	hubSendTimeout   = 30 * time.Second

	// hubMaxContacts is the most contacts the Hub accepts per request.
	hubMaxContacts = 200

	hubContactsPath = "/api/minhas-tarefas/apps/gmaps/contatos/"
	hubAccessPath   = "/api/apps/module-access/"

	// Field limits of the Hub contract. The Hub validates strictly: one value
	// over its limit refuses the whole batch, so we never send more than this.
	hubMaxName     = 200
	hubMaxCategory = 120
	hubMaxAddress  = 300
	hubMaxPhone    = 50
	hubMaxSite     = 500
	hubMaxLink     = 1000
	hubMaxEmail    = 254
	hubMaxEmails   = 5
	hubMaxPosition = 1_000_000
	hubMaxRespBody = 1 << 20
)

// HubConfig holds the Koria Hub integration settings.
type HubConfig struct {
	// SSOKey is the HS256 key shared with the Hub. Empty disables the login.
	SSOKey string
	// URL is the public address of the Hub (link back to it).
	URL string
	// APIURL is the Hub API base used for server-to-server calls.
	APIURL string
	// AppToken is the "<slug>:<secret>" token sent as X-App-Token.
	AppToken string
}

// HubConfigFromEnv reads the integration settings from the environment:
// HUB_SSO_SHARED_KEY, HUB_URL, HUB_API_URL and HUB_APP_TOKEN.
func HubConfigFromEnv() HubConfig {
	return HubConfig{
		SSOKey:   strings.TrimSpace(os.Getenv("HUB_SSO_SHARED_KEY")),
		URL:      strings.TrimRight(strings.TrimSpace(os.Getenv("HUB_URL")), "/"),
		APIURL:   strings.TrimRight(strings.TrimSpace(os.Getenv("HUB_API_URL")), "/"),
		AppToken: strings.TrimSpace(os.Getenv("HUB_APP_TOKEN")),
	}
}

// Enabled reports whether login through the Hub is required.
func (c HubConfig) Enabled() bool {
	return c.SSOKey != ""
}

// CanSend reports whether contacts can be pushed to the Hub.
func (c HubConfig) CanSend() bool {
	return c.Enabled() && c.APIURL != "" && c.AppToken != ""
}

func (c HubConfig) validate() error {
	if !c.Enabled() {
		return nil
	}

	if len(c.SSOKey) < hubMinKeyBytes {
		return fmt.Errorf("HUB_SSO_SHARED_KEY must have at least %d bytes", hubMinKeyBytes)
	}

	for name, value := range map[string]string{"HUB_URL": c.URL, "HUB_API_URL": c.APIURL} {
		if value != "" && !isHTTPURL(value) {
			return fmt.Errorf("%s must be an http(s) URL", name)
		}
	}

	if c.AppToken != "" && !strings.Contains(c.AppToken, ":") {
		return errors.New(`HUB_APP_TOKEN must look like "<slug>:<secret>"`)
	}

	return nil
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)

	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// hubSession is the logged-in person, as seen by the handlers.
type hubSession struct {
	ID       string
	Username string
	Name     string
	Email    string
}

type hubCtxKey struct{}

// hub implements the Hub login, session and API calls.
type hub struct {
	cfg    HubConfig
	store  HubStore
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	access map[string]accessEntry
}

type accessEntry struct {
	granted bool
	checked time.Time
}

func newHub(cfg HubConfig, store HubStore) (*hub, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	if store == nil {
		store = newMemoryHubStore()
	}

	return &hub{
		cfg:    cfg,
		store:  store,
		client: &http.Client{Timeout: hubSendTimeout},
		now:    time.Now,
		access: make(map[string]accessEntry),
	}, nil
}

// ticketClaims are the claims of an SSO ticket we use.
type ticketClaims struct {
	Kind      string          `json:"kind"`
	Issuer    string          `json:"iss"`
	Audience  json.RawMessage `json:"aud"`
	ID        string          `json:"jti"`
	IssuedAt  float64         `json:"iat"`
	ExpiresAt float64         `json:"exp"`
	Username  string          `json:"username"`
	Email     string          `json:"email"`
	FirstName string          `json:"first_name"`
	LastName  string          `json:"last_name"`
}

// errInvalidTicket is the single error shown for any bad ticket: the reason is
// logged, not revealed.
var errInvalidTicket = errors.New("invalid ticket")

// verifyTicket checks signature, algorithm, kind, issuer, audience and
// lifetime of an SSO ticket and returns its claims.
func (h *hub) verifyTicket(token string) (ticketClaims, error) {
	var claims ticketClaims

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return claims, fmt.Errorf("%w: malformed", errInvalidTicket)
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return claims, fmt.Errorf("%w: header encoding", errInvalidTicket)
	}

	var header struct {
		Alg string `json:"alg"`
	}

	if err := json.Unmarshal(headerJSON, &header); err != nil || header.Alg != "HS256" {
		return claims, fmt.Errorf("%w: algorithm %q", errInvalidTicket, header.Alg)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims, fmt.Errorf("%w: signature encoding", errInvalidTicket)
	}

	mac := hmac.New(sha256.New, []byte(h.cfg.SSOKey))
	mac.Write([]byte(parts[0] + "." + parts[1]))

	if !hmac.Equal(signature, mac.Sum(nil)) {
		return claims, fmt.Errorf("%w: bad signature", errInvalidTicket)
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return claims, fmt.Errorf("%w: payload encoding", errInvalidTicket)
	}

	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("%w: payload", errInvalidTicket)
	}

	if err := h.checkClaims(&claims); err != nil {
		return ticketClaims{}, err
	}

	return claims, nil
}

func (h *hub) checkClaims(claims *ticketClaims) error {
	if claims.Kind != hubTicketKind || claims.Issuer != hubTicketIssuer {
		return fmt.Errorf("%w: kind/issuer", errInvalidTicket)
	}

	if !audienceMatches(claims.Audience, hubTicketAudience) {
		return fmt.Errorf("%w: audience", errInvalidTicket)
	}

	if claims.ID == "" || claims.Username == "" || len(claims.Username) > hubMaxName {
		return fmt.Errorf("%w: missing jti or username", errInvalidTicket)
	}

	now := h.now()
	issued := time.Unix(int64(claims.IssuedAt), 0)
	expires := time.Unix(int64(claims.ExpiresAt), 0)

	if claims.IssuedAt <= 0 || claims.ExpiresAt <= 0 {
		return fmt.Errorf("%w: missing iat/exp", errInvalidTicket)
	}

	// No tolerance on exp: it would widen the replay window.
	if !now.Before(expires) {
		return fmt.Errorf("%w: expired", errInvalidTicket)
	}

	if issued.After(now.Add(hubClockSkew)) {
		return fmt.Errorf("%w: issued in the future", errInvalidTicket)
	}

	if expires.Sub(issued) > hubTicketMaxTTL {
		return fmt.Errorf("%w: lifetime too long", errInvalidTicket)
	}

	return nil
}

func audienceMatches(raw json.RawMessage, want string) bool {
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == want
	}

	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, aud := range many {
			if aud == want {
				return true
			}
		}
	}

	return false
}

// openSession creates a server-side session for the person in a ticket and
// returns the cookie value ("<id>.<secret>").
func (h *hub) openSession(ctx context.Context, claims *ticketClaims) (string, error) {
	name := strings.TrimSpace(claims.FirstName + " " + claims.LastName)
	if name == "" {
		name = claims.Username
	}

	id, err := randomToken(16)
	if err != nil {
		return "", err
	}

	secret, err := randomToken(32)
	if err != nil {
		return "", err
	}

	now := h.now()

	err = h.store.CreateSession(ctx, HubSessionRecord{
		ID:         id,
		SecretHash: hashSecret(secret),
		Username:   claims.Username,
		Name:       name,
		Email:      claims.Email,
		CreatedAt:  now,
		LastSeen:   now,
		ExpiresAt:  now.Add(hubSessionMax),
	})
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}

	// Housekeeping: a failure here never blocks a login.
	if err := h.store.PurgeExpired(ctx, now); err != nil {
		log.Printf("purge expired sessions: %v", err)
	}

	return id + "." + secret, nil
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))

	return hex.EncodeToString(sum[:])
}

// lookupSession validates a cookie value against the store: the secret must
// match (constant time), and neither the idle nor the absolute limit may have
// passed. Any failure just means "no session".
func (h *hub) lookupSession(ctx context.Context, value string) (hubSession, bool) {
	id, secret, found := strings.Cut(value, ".")
	if !found || id == "" || secret == "" {
		return hubSession{}, false
	}

	rec, err := h.store.GetSession(ctx, id)
	if err != nil {
		if !errors.Is(err, ErrHubSessionNotFound) {
			log.Printf("hub session lookup: %v", err)
		}

		return hubSession{}, false
	}

	if subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(rec.SecretHash)) != 1 {
		return hubSession{}, false
	}

	now := h.now()
	if !now.Before(rec.ExpiresAt) || now.Sub(rec.LastSeen) >= hubSessionIdle {
		return hubSession{}, false
	}

	if now.Sub(rec.LastSeen) >= hubTouchEvery {
		if err := h.store.TouchSession(ctx, id, now); err != nil {
			log.Printf("hub session touch: %v", err)
		}
	}

	return hubSession{ID: rec.ID, Username: rec.Username, Name: rec.Name, Email: rec.Email}, true
}

func (h *hub) sessionFromRequest(r *http.Request) (hubSession, bool) {
	cookie, err := r.Cookie(hubSessionCookie)
	if err != nil {
		return hubSession{}, false
	}

	return h.lookupSession(r.Context(), cookie.Value)
}

func (h *hub) setSessionCookie(w http.ResponseWriter, r *http.Request, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     hubSessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   int(hubSessionMax.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func (h *hub) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     hubSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// stillGranted asks the Hub whether the person still has access to the app.
// It only answers false when the Hub says so explicitly: any error is treated
// as "keep going", so a Hub outage does not lock everyone out.
func (h *hub) stillGranted(ctx context.Context, username string) bool {
	if h.cfg.APIURL == "" || h.cfg.AppToken == "" {
		return true
	}

	h.mu.Lock()
	entry, ok := h.access[username]
	h.mu.Unlock()

	if ok && h.now().Sub(entry.checked) < hubAccessRecheck {
		return entry.granted
	}

	granted, err := h.queryAccess(ctx, username)
	if err != nil {
		log.Printf("hub access check failed (keeping session): %v", err)

		return true
	}

	h.mu.Lock()
	h.access[username] = accessEntry{granted: granted, checked: h.now()}
	h.mu.Unlock()

	return granted
}

func (h *hub) queryAccess(ctx context.Context, username string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, hubAPITimeout)
	defer cancel()

	endpoint := h.cfg.APIURL + hubAccessPath + url.PathEscape(username) + "/"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return false, err
	}

	req.Header.Set("X-App-Token", h.cfg.AppToken)
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return false, err
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("hub answered status %d", resp.StatusCode)
	}

	var answer struct {
		Granted *bool `json:"granted"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, hubMaxRespBody)).Decode(&answer); err != nil {
		return false, err
	}

	if answer.Granted == nil {
		return false, errors.New("hub answer has no granted field")
	}

	return *answer.Granted, nil
}

// ---- HTTP: middleware, /sso, /logout ----

// hubPublicPath reports paths that do not need a session.
func hubPublicPath(path string) bool {
	return path == "/sso" || strings.HasPrefix(path, "/static/")
}

// middleware requires a valid, still-authorised session on every route except
// the public ones.
func (s *Server) hubMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hubPublicPath(r.URL.Path) {
			next.ServeHTTP(w, r)

			return
		}

		session, ok := s.hub.sessionFromRequest(r)
		if ok && !s.hub.stillGranted(r.Context(), session.Username) {
			// The Hub withdrew access: end every session of the person.
			if n, err := s.hub.store.RevokeUser(r.Context(), session.Username, s.hub.now()); err != nil {
				log.Printf("hub revoke sessions: %v", err)
			} else {
				log.Printf("hub access withdrawn for %s: %d session(s) ended", session.Username, n)
			}

			s.hub.clearSessionCookie(w, r)
			s.renderHubLogin(w, r, "Seu acesso ao Google Maps Scraper foi retirado no Hub. Peça acesso à TI.")

			return
		}

		if !ok {
			s.renderHubLogin(w, r, "")

			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hubCtxKey{}, session)))
	})
}

// hubSessionFrom returns the logged-in person, if Hub login is on.
func hubSessionFrom(r *http.Request) (hubSession, bool) {
	session, ok := r.Context().Value(hubCtxKey{}).(hubSession)

	return session, ok
}

type loginView struct {
	Message string
	HubURL  string
}

// renderHubLogin answers an unauthenticated request. Browsers navigating get a
// page that points to the Hub; htmx and API calls get a plain 401.
func (s *Server) renderHubLogin(w http.ResponseWriter, r *http.Request, message string) {
	w.Header().Set("Cache-Control", "no-store")

	wantsPage := r.Method == http.MethodGet && r.Header.Get("HX-Request") == "" &&
		!strings.HasPrefix(r.URL.Path, "/api/") && strings.Contains(r.Header.Get("Accept"), "text/html")

	if !wantsPage {
		text := "Sessão expirada ou ausente. Entre pelo Koria Hub."
		if strings.HasPrefix(r.URL.Path, "/api/") {
			renderJSON(w, http.StatusUnauthorized, apiError{Code: http.StatusUnauthorized, Message: text})

			return
		}

		http.Error(w, text, http.StatusUnauthorized)

		return
	}

	tmpl, ok := s.tmpl["static/templates/entrar.html"]
	if !ok {
		http.Error(w, "missing tpl", http.StatusInternalServerError)

		return
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, loginView{Message: message, HubURL: s.hub.cfg.URL}); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = buf.WriteTo(w)
}

// hubLoginRefused is the one message shown for every refused ticket: the real
// reason goes to the log only, so nothing is revealed to someone probing.
const hubLoginRefused = "Não foi possível entrar. Volte ao Hub e abra o Google Maps Scraper de novo."

// hubSSO handles GET /sso?ticket=...: it validates the ticket, accepts its id
// once, opens the session and redirects to a URL without the ticket.
func (s *Server) hubSSO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	// The ticket is in the URL: never let it leak through the Referer header.
	w.Header().Set("Referrer-Policy", "no-referrer")

	claims, err := s.hub.verifyTicket(r.URL.Query().Get("ticket"))
	if err != nil {
		log.Printf("sso refused: %v", err)
		s.renderHubLogin(w, r, hubLoginRefused)

		return
	}

	now := s.hub.now()

	first, err := s.hub.store.ConsumeTicket(r.Context(), claims.ID, now, now.Add(hubTicketKeep))
	if err != nil {
		log.Printf("sso: ticket store: %v", err)
		http.Error(w, "Serviço indisponível. Tente de novo em instantes.", http.StatusServiceUnavailable)

		return
	}

	if !first {
		log.Printf("sso refused: ticket id already used")
		s.renderHubLogin(w, r, hubLoginRefused)

		return
	}

	cookie, err := s.hub.openSession(r.Context(), &claims)
	if err != nil {
		log.Printf("sso: %v", err)
		http.Error(w, "Serviço indisponível. Tente de novo em instantes.", http.StatusServiceUnavailable)

		return
	}

	log.Printf("sso login: %s", claims.Username)
	s.hub.setSessionCookie(w, r, cookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// hubLogout handles POST /logout: it drops the session and goes back to the Hub.
func (s *Server) hubLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if session, ok := hubSessionFrom(r); ok {
		if err := s.hub.store.RevokeSession(r.Context(), session.ID, s.hub.now()); err != nil {
			log.Printf("hub logout: %v", err)
		}
	}

	s.hub.clearSessionCookie(w, r)

	target := "/"
	if s.hub.cfg.URL != "" {
		target = s.hub.cfg.URL
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}
