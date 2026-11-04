package cnpj_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosom/google-maps-scraper/internal/cnpj"
)

func TestNormalize(t *testing.T) {
	valid := map[string]string{
		"24.614.048/0001-58":       "24614048000158",
		"24614048000158":           "24614048000158",
		" 00.000.000/0001-91 ":     "00000000000191",
		"11.222.333/0001-81":       "11222333000181",
		"CNPJ: 24.614.048/0001-58": "24614048000158",
	}

	for raw, want := range valid {
		got, err := cnpj.Normalize(raw)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}

	invalid := []string{
		"", "123", "24.614.048/0001-59", // wrong check digit
		"11.111.111/1111-11", "00.000.000/0000-00", // repeated digits
		"49.999.999/9999-94",                   // placeholder with a lucky check digit
		"24.614.048/0001-5", "246140480001580", // wrong length
	}

	for _, raw := range invalid {
		if _, err := cnpj.Normalize(raw); !errors.Is(err, cnpj.ErrInvalid) {
			t.Errorf("Normalize(%q) must fail with ErrInvalid, got %v", raw, err)
		}
	}
}

func TestFormat(t *testing.T) {
	if got := cnpj.Format("24614048000158"); got != "24.614.048/0001-58" {
		t.Fatalf("Format = %q", got)
	}

	if got := cnpj.Format("123"); got != "123" {
		t.Fatalf("Format of a non-CNPJ must be unchanged, got %q", got)
	}

	if got := cnpj.FormatCNAE("4930202"); got != "4930-2/02" {
		t.Fatalf("FormatCNAE = %q", got)
	}
}

const sample = `{
  "cnpj": "24614048000158",
  "razao_social": "TRANSFER TRANSPORTES URGENTES LTDA",
  "nome_fantasia": "TRANSFER TRANSPORTES URGENTES",
  "descricao_situacao_cadastral": "ATIVA",
  "data_situacao_cadastral": "2016-04-18",
  "data_inicio_atividade": "2016-04-18",
  "cnae_fiscal": 4930202,
  "cnae_fiscal_descricao": "Transporte rodoviário de carga",
  "porte": "MICRO EMPRESA",
  "natureza_juridica": "Sociedade Empresária Limitada",
  "capital_social": 70000,
  "municipio": "CAXIAS DO SUL",
  "uf": "RS",
  "opcao_pelo_mei": false,
  "ddd_telefone_1": "5432127047",
  "email": "pessoa@example.com",
  "qsa": [{"nome_socio": "FULANO DE TAL", "cnpj_cpf_do_socio": "***123456**"}]
}`

func newTestClient(t *testing.T, handler http.HandlerFunc) *cnpj.Client {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return cnpj.NewClient(srv.URL)
}

func TestLookupParsesCompanyAndDropsPersonalData(t *testing.T) {
	var gotPath, gotUA string

	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotUA = r.URL.Path, r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(sample))
	})

	company, err := client.Lookup(context.Background(), "24614048000158")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	if gotPath != "/24614048000158" || !strings.Contains(gotUA, "google-maps-scraper") {
		t.Fatalf("request: path=%q ua=%q", gotPath, gotUA)
	}

	if company.LegalName != "TRANSFER TRANSPORTES URGENTES LTDA" || !company.Active() || company.CNAECode != "4930202" ||
		company.Size != "MICRO EMPRESA" || company.City != "CAXIAS DO SUL" || company.State != "RS" ||
		company.Capital != 70000 || company.MEI || company.OpeningDate != "2016-04-18" || company.RegisteredTel != "5432127047" {
		t.Fatalf("company = %+v", company)
	}
}

func TestLookupErrors(t *testing.T) {
	tests := []struct {
		status int
		want   error
	}{
		{http.StatusNotFound, cnpj.ErrNotFound},
		{http.StatusBadRequest, cnpj.ErrInvalid},
		{http.StatusTooManyRequests, cnpj.ErrRateLimited},
		{http.StatusInternalServerError, cnpj.ErrUnavailable},
		{http.StatusBadGateway, cnpj.ErrUnavailable},
	}

	for _, tt := range tests {
		client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tt.status) })

		if _, err := client.Lookup(context.Background(), "24614048000158"); !errors.Is(err, tt.want) {
			t.Errorf("status %d: err = %v, want %v", tt.status, err, tt.want)
		}
	}

	// A bad number never reaches the network.
	var calls atomic.Int32

	client := newTestClient(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })

	if _, err := client.Lookup(context.Background(), "123"); !errors.Is(err, cnpj.ErrInvalid) || calls.Load() != 0 {
		t.Fatalf("invalid number: err=%v calls=%d", err, calls.Load())
	}
}

func TestLookupRetriesOnceAfterRetryAfter(t *testing.T) {
	var calls atomic.Int32

	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)

			return
		}

		_, _ = w.Write([]byte(sample))
	})

	start := time.Now()

	if _, err := client.Lookup(context.Background(), "24614048000158"); err != nil {
		t.Fatalf("Lookup: %v", err)
	}

	if calls.Load() != 2 || time.Since(start) < time.Second {
		t.Fatalf("calls=%d elapsed=%s; want one retry after waiting Retry-After", calls.Load(), time.Since(start))
	}
}

func TestLookupSpacesRequests(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sample)) })

	start := time.Now()

	for range 3 {
		if _, err := client.Lookup(context.Background(), "24614048000158"); err != nil {
			t.Fatal(err)
		}
	}

	// Three calls need at least two gaps of ~500ms.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("3 lookups took %s; requests must be spaced", elapsed)
	}
}

func TestLookupHonoursContextCancellation(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(sample)) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := client.Lookup(ctx, "24614048000158"); err == nil {
		t.Fatal("a cancelled context must fail the lookup")
	}
}
