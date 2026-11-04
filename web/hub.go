package web

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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
	hubTicketKind     = "gmaps-sso"
	hubTicketIssuer   = "koria-hub"
	hubTicketAudience = "gmaps-scraper"

	// hubMinKeyBytes is the minimum size of the shared signing key.
	hubMinKeyBytes = 32

	// hubTicketMaxTTL is the longest ticket lifetime accepted (exp - iat).
	hubTicketMaxTTL = 120 * time.Second
	// hubClockSkew tolerates small clock differences between Hub and app.
	hubClockSkew = 30 * time.Second

	hubSessionCookie = "gmaps_session"
	hubSessionTTL    = 8 * time.Hour

	// hubAccessRecheck is how long a "still has access" answer is trusted.
	hubAccessRecheck = 5 * time.Minute
	hubAPITimeout    = 5 * time.Second
	hubSendTimeout   = 30 * time.Second

	// hubMaxContacts is the most contacts the Hub accepts per request.
	hubMaxContacts = 200

	hubContactsPath = "/api/minhas-tarefas/apps/gmaps/contatos/"
	hubAccessPath   = "/api/apps/module-access/"

	// Field limits, so one odd listing cannot make the whole batch invalid.
	hubMaxName     = 200
	hubMaxText     = 300
	hubMaxLink     = 1000
	hubMaxEmails   = 10
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
	// SessionKey signs the session cookie; defaults to a key derived from SSOKey.
	SessionKey string
}

// HubConfigFromEnv reads the integration settings from the environment:
// HUB_SSO_SHARED_KEY, HUB_URL, HUB_API_URL, HUB_APP_TOKEN and HUB_SESSION_KEY.
func HubConfigFromEnv() HubConfig {
	return HubConfig{
		SSOKey:     strings.TrimSpace(os.Getenv("HUB_SSO_SHARED_KEY")),
		URL:        strings.TrimRight(strings.TrimSpace(os.Getenv("HUB_URL")), "/"),
		APIURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("HUB_API_URL")), "/"),
		AppToken:   strings.TrimSpace(os.Getenv("HUB_APP_TOKEN")),
		SessionKey: strings.TrimSpace(os.Getenv("HUB_SESSION_KEY")),
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

	if c.SessionKey != "" && len(c.SessionKey) < hubMinKeyBytes {
		return fmt.Errorf("HUB_SESSION_KEY must have at least %d bytes", hubMinKeyBytes)
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

// hubSession is the identity kept in the session cookie.
type hubSession struct {
	Username string `json:"u"`
	Name     string `json:"n"`
	Email    string `json:"e"`
	Expires  int64  `json:"x"`
}

type hubCtxKey struct{}

// hub implements the Hub login, session and API calls.
type hub struct {
	cfg        HubConfig
	sessionKey []byte
	client     *http.Client
	now        func() time.Time

	mu     sync.Mutex
	usedID map[string]time.Time
	access map[string]accessEntry
}

type accessEntry struct {
	granted bool
	checked time.Time
}

func newHub(cfg HubConfig) (*hub, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	sessionKey := []byte(cfg.SessionKey)
	if len(sessionKey) == 0 {
		mac := hmac.New(sha256.New, []byte(cfg.SSOKey))
		mac.Write([]byte("gmaps-session-v1"))
		sessionKey = mac.Sum(nil)
	}

	return &hub{
		cfg:        cfg,
		sessionKey: sessionKey,
		client:     &http.Client{Timeout: hubSendTimeout},
		now:        time.Now,
		usedID:     make(map[string]time.Time),
		access:     make(map[string]accessEntry),
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

	if now.After(expires.Add(hubClockSkew)) {
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

// consumeTicketID records a ticket id; it returns false if it was used before.
func (h *hub) consumeTicketID(id string, expires time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.now()
	for used, until := range h.usedID {
		if now.After(until) {
			delete(h.usedID, used)
		}
	}

	if _, seen := h.usedID[id]; seen {
		return false
	}

	// Keep the id past the ticket's own expiry (plus skew) so it cannot be
	// replayed while it would still validate.
	h.usedID[id] = expires.Add(2 * hubClockSkew)

	return true
}

func (h *hub) sign(payload []byte) string {
	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write(payload)

	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// readSession validates a session cookie value.
func (h *hub) readSession(value string) (hubSession, bool) {
	var session hubSession

	payloadPart, sigPart, found := strings.Cut(value, ".")
	if !found {
		return session, false
	}

	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return session, false
	}

	signature, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return session, false
	}

	mac := hmac.New(sha256.New, h.sessionKey)
	mac.Write(payload)

	if subtle.ConstantTimeCompare(signature, mac.Sum(nil)) != 1 {
		return session, false
	}

	if err := json.Unmarshal(payload, &session); err != nil {
		return session, false
	}

	if session.Username == "" || h.now().Unix() >= session.Expires {
		return hubSession{}, false
	}

	return session, true
}

func (h *hub) sessionFromRequest(r *http.Request) (hubSession, bool) {
	cookie, err := r.Cookie(hubSessionCookie)
	if err != nil {
		return hubSession{}, false
	}

	return h.readSession(cookie.Value)
}

func (h *hub) setSessionCookie(w http.ResponseWriter, r *http.Request, claims ticketClaims) {
	name := strings.TrimSpace(claims.FirstName + " " + claims.LastName)
	if name == "" {
		name = claims.Username
	}

	payload, _ := json.Marshal(hubSession{
		Username: claims.Username,
		Name:     name,
		Email:    claims.Email,
		Expires:  h.now().Add(hubSessionTTL).Unix(),
	})

	http.SetCookie(w, &http.Cookie{
		Name:     hubSessionCookie,
		Value:    h.sign(payload),
		Path:     "/",
		MaxAge:   int(hubSessionTTL.Seconds()),
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
			s.hub.clearSessionCookie(w, r)

			ok = false
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

// hubSSO handles GET /sso?ticket=...: it validates the ticket, opens the
// session and redirects to a URL without the ticket.
func (s *Server) hubSSO(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	// The ticket is in the URL: never let it leak through the Referer header.
	w.Header().Set("Referrer-Policy", "no-referrer")

	claims, err := s.hub.verifyTicket(r.URL.Query().Get("ticket"))
	if err != nil {
		log.Printf("sso rejected: %v", err)
		s.renderHubLogin(w, r, "O link de acesso é inválido ou expirou. Volte ao Hub e abra o Google Maps Scraper de novo.")

		return
	}

	if !s.hub.consumeTicketID(claims.ID, time.Unix(int64(claims.ExpiresAt), 0)) {
		log.Printf("sso rejected: ticket id already used")
		s.renderHubLogin(w, r, "Este link de acesso já foi usado. Volte ao Hub e abra o Google Maps Scraper de novo.")

		return
	}

	s.hub.setSessionCookie(w, r, claims)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// hubLogout handles POST /logout: it drops the session and goes back to the Hub.
func (s *Server) hubLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	s.hub.clearSessionCookie(w, r)

	target := "/"
	if s.hub.cfg.URL != "" {
		target = s.hub.cfg.URL
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}
