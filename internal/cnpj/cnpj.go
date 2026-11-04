// Package cnpj validates and formats Brazilian company registration numbers
// (CNPJ) and looks company data up in a public API (BrasilAPI by default, or any
// service with the same response shape, such as a self-hosted Minha Receita).
//
// Only company-level data is kept. Partners (the "qsa" list) and registered
// e-mail addresses are deliberately dropped: they can identify people.
package cnpj

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultAPIURL is the public BrasilAPI endpoint; the CNPJ is appended.
	DefaultAPIURL = "https://brasilapi.com.br/api/cnpj/v1"

	digits = 14

	// minGap spaces requests so a burst never trips the public API's limits.
	minGap         = 500 * time.Millisecond
	requestTimeout = 15 * time.Second
	maxBody        = 1 << 20
	maxRetryAfter  = 5 * time.Second
	userAgent      = "google-maps-scraper-koria-hub/1.0 (+https://github.com/koriabr/google-maps-scraper)"
	maxRepeatedRun = 8
	cnaeCodeLength = 7
)

var (
	// ErrInvalid means the number is not a valid CNPJ (length, check digits).
	ErrInvalid = errors.New("invalid cnpj")
	// ErrNotFound means the registry has no company with that number.
	ErrNotFound = errors.New("cnpj not found")
	// ErrRateLimited means the API refused the request for being too frequent.
	ErrRateLimited = errors.New("cnpj api rate limited")
	// ErrUnavailable means the API could not be reached or answered with an error.
	ErrUnavailable = errors.New("cnpj api unavailable")

	nonDigits = regexp.MustCompile(`\D+`)
)

// looksLikePlaceholder reports numbers that are not companies: every digit
// equal (11.111.111/1111-11), or a long run of one non-zero digit
// (49.999.999/9999-94). Runs of zeros are legitimate: the root of banks such as
// Banco do Brasil (00.000.000/0001-91) is made of them.
func looksLikePlaceholder(number string) bool {
	run := 1

	for i := 1; i < len(number); i++ {
		if number[i] != number[i-1] {
			run = 1

			continue
		}

		run++

		if run == len(number) || (run >= maxRepeatedRun && number[i] != '0') {
			return true
		}
	}

	return false
}

// Normalize strips punctuation and returns the 14 digits of a valid CNPJ. It
// rejects wrong lengths, wrong check digits and obvious placeholders such as
// 11.111.111/1111-11 or 49.999.999/9999-94.
func Normalize(raw string) (string, error) {
	number := nonDigits.ReplaceAllString(raw, "")
	if len(number) != digits {
		return "", fmt.Errorf("%w: expected %d digits", ErrInvalid, digits)
	}

	if looksLikePlaceholder(number) {
		return "", fmt.Errorf("%w: placeholder", ErrInvalid)
	}

	if number[12:] != checkDigits(number[:12]) {
		return "", fmt.Errorf("%w: check digits", ErrInvalid)
	}

	return number, nil
}

func checkDigits(base string) string {
	first := checkDigit(base, []int{5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2})
	second := checkDigit(base+first, []int{6, 5, 4, 3, 2, 9, 8, 7, 6, 5, 4, 3, 2})

	return first + second
}

func checkDigit(base string, weights []int) string {
	sum := 0

	for i, weight := range weights {
		sum += int(base[i]-'0') * weight
	}

	if rest := sum % 11; rest >= 2 {
		return strconv.Itoa(11 - rest)
	}

	return "0"
}

// Format renders 14 digits as 00.000.000/0000-00; anything else is returned as is.
func Format(number string) string {
	if len(number) != digits {
		return number
	}

	return number[0:2] + "." + number[2:5] + "." + number[5:8] + "/" + number[8:12] + "-" + number[12:]
}

// Company is the company-level data kept from a lookup.
type Company struct {
	CNPJ          string  `json:"cnpj"`
	LegalName     string  `json:"razao_social"`
	TradeName     string  `json:"nome_fantasia"`
	Status        string  `json:"situacao"`
	StatusDate    string  `json:"data_situacao"`
	OpeningDate   string  `json:"data_abertura"`
	CNAECode      string  `json:"cnae_codigo"`
	CNAEText      string  `json:"cnae_descricao"`
	Size          string  `json:"porte"`
	LegalNature   string  `json:"natureza_juridica"`
	Capital       float64 `json:"capital_social"`
	City          string  `json:"municipio"`
	State         string  `json:"uf"`
	MEI           bool    `json:"mei"`
	RegisteredTel string  `json:"telefone"`
}

// Active reports whether the registry marks the company as active ("ATIVA").
func (c *Company) Active() bool {
	return strings.EqualFold(strings.TrimSpace(c.Status), "ATIVA")
}

// FormatCNAE renders a 7-digit CNAE code as 0000-0/00.
func FormatCNAE(code string) string {
	code = nonDigits.ReplaceAllString(code, "")
	if len(code) != cnaeCodeLength {
		return code
	}

	return code[:4] + "-" + code[4:5] + "/" + code[5:]
}

// rawCompany is the subset of the API response we read.
type rawCompany struct {
	CNPJ        string          `json:"cnpj"`
	LegalName   string          `json:"razao_social"`
	TradeName   string          `json:"nome_fantasia"`
	Status      string          `json:"descricao_situacao_cadastral"`
	StatusDate  string          `json:"data_situacao_cadastral"`
	OpeningDate string          `json:"data_inicio_atividade"`
	CNAECode    json.RawMessage `json:"cnae_fiscal"`
	CNAEText    string          `json:"cnae_fiscal_descricao"`
	Size        string          `json:"porte"`
	LegalNature string          `json:"natureza_juridica"`
	Capital     float64         `json:"capital_social"`
	City        string          `json:"municipio"`
	State       string          `json:"uf"`
	MEI         *bool           `json:"opcao_pelo_mei"`
	Tel         string          `json:"ddd_telefone_1"`
}

func (r *rawCompany) company(number string) Company {
	return Company{
		CNPJ:          number,
		LegalName:     strings.TrimSpace(r.LegalName),
		TradeName:     strings.TrimSpace(r.TradeName),
		Status:        strings.ToUpper(strings.TrimSpace(r.Status)),
		StatusDate:    r.StatusDate,
		OpeningDate:   r.OpeningDate,
		CNAECode:      strings.Trim(string(r.CNAECode), `"`),
		CNAEText:      strings.TrimSpace(r.CNAEText),
		Size:          strings.TrimSpace(r.Size),
		LegalNature:   strings.TrimSpace(r.LegalNature),
		Capital:       r.Capital,
		City:          strings.TrimSpace(r.City),
		State:         strings.TrimSpace(r.State),
		MEI:           r.MEI != nil && *r.MEI,
		RegisteredTel: strings.TrimSpace(r.Tel),
	}
}

// Client looks companies up by CNPJ. It spaces its requests, so it is safe to
// share between goroutines.
type Client struct {
	baseURL string
	http    *http.Client

	mu   sync.Mutex
	last time.Time
	gap  time.Duration
}

// NewClient returns a client for the API at baseURL (the CNPJ is appended to
// it). An empty baseURL uses DefaultAPIURL.
func NewClient(baseURL string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = DefaultAPIURL
	}

	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: requestTimeout},
		gap:     minGap,
	}
}

// Lookup returns the company registered under the CNPJ (which must already be
// normalized). ErrNotFound, ErrRateLimited and ErrUnavailable are returned for
// the matching API outcomes.
func (c *Client) Lookup(ctx context.Context, number string) (Company, error) {
	if _, err := Normalize(number); err != nil {
		return Company{}, err
	}

	company, retryAfter, err := c.fetch(ctx, number)
	if errors.Is(err, ErrRateLimited) && retryAfter > 0 && retryAfter <= maxRetryAfter {
		select {
		case <-ctx.Done():
			return Company{}, ctx.Err()
		case <-time.After(retryAfter):
		}

		company, _, err = c.fetch(ctx, number)
	}

	return company, err
}

func (c *Client) waitTurn(ctx context.Context) error {
	c.mu.Lock()
	wait := c.gap - time.Since(c.last)
	if wait < 0 {
		wait = 0
	}

	c.last = time.Now().Add(wait)
	c.mu.Unlock()

	if wait == 0 {
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

func (c *Client) fetch(ctx context.Context, number string) (Company, time.Duration, error) {
	if err := c.waitTurn(ctx); err != nil {
		return Company{}, 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+url.PathEscape(number), http.NoBody)
	if err != nil {
		return Company{}, 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return Company{}, 0, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusOK:
		var raw rawCompany
		if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&raw); err != nil {
			return Company{}, 0, fmt.Errorf("%w: decode: %w", ErrUnavailable, err)
		}

		return raw.company(number), 0, nil
	case http.StatusNotFound:
		return Company{}, 0, ErrNotFound
	case http.StatusBadRequest:
		return Company{}, 0, ErrInvalid
	case http.StatusTooManyRequests:
		seconds, _ := strconv.Atoi(resp.Header.Get("Retry-After"))

		return Company{}, time.Duration(seconds) * time.Second, ErrRateLimited
	default:
		return Company{}, 0, fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
}
