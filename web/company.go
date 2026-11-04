package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gosom/google-maps-scraper/internal/cnpj"
)

const (
	// cnpjLookupTimeout bounds one consultation, including the wait for the
	// client's request spacing.
	cnpjLookupTimeout = 20 * time.Second

	// phoneTailDigits is how many trailing digits are compared between the
	// registry phone and the Maps phone (ignores area code and country code).
	phoneTailDigits = 8

	dateLayout = "2006-01-02"
)

// CNPJLooker looks a company up in the registry by CNPJ.
type CNPJLooker interface {
	Lookup(ctx context.Context, number string) (cnpj.Company, error)
}

// CompanyView is what the results list shows about a place's company.
type CompanyView struct {
	CNPJ          string  `json:"cnpj"`
	CNPJFormatted string  `json:"cnpj_formatado"`
	LegalName     string  `json:"razao_social"`
	TradeName     string  `json:"nome_fantasia"`
	Status        string  `json:"situacao"`
	Active        bool    `json:"ativa"`
	StatusDate    string  `json:"data_situacao"`
	Opening       string  `json:"abertura"`
	AgeYears      int     `json:"anos"`
	CNAE          string  `json:"cnae"`
	Size          string  `json:"porte"`
	LegalNature   string  `json:"natureza_juridica"`
	Capital       float64 `json:"capital_social"`
	Location      string  `json:"local"`
	MEI           bool    `json:"mei"`
	Source        string  `json:"origem"`
	// PhoneMatch and CityMatch say whether the registry agrees with the Maps
	// listing; nil when there is nothing to compare. They catch a wrong CNPJ.
	PhoneMatch *bool `json:"telefone_confere"`
	CityMatch  *bool `json:"cidade_confere"`
}

func newCompanyView(company *cnpj.Company, source string, place *Place, now time.Time) *CompanyView {
	view := &CompanyView{
		CNPJ:          company.CNPJ,
		CNPJFormatted: cnpj.Format(company.CNPJ),
		LegalName:     company.LegalName,
		TradeName:     company.TradeName,
		Status:        company.Status,
		Active:        company.Active(),
		StatusDate:    company.StatusDate,
		Opening:       company.OpeningDate,
		Size:          company.Size,
		LegalNature:   company.LegalNature,
		Capital:       company.Capital,
		MEI:           company.MEI,
		Source:        source,
	}

	if opened, err := time.Parse(dateLayout, company.OpeningDate); err == nil && opened.Before(now) {
		view.AgeYears = int(now.Sub(opened).Hours() / (24 * 365.25))
	}

	if company.CNAECode != "" {
		view.CNAE = cnpj.FormatCNAE(company.CNAECode)
		if company.CNAEText != "" {
			view.CNAE += " — " + company.CNAEText
		}
	}

	if company.City != "" {
		view.Location = company.City
		if company.State != "" {
			view.Location += "/" + company.State
		}
	}

	if place != nil {
		view.PhoneMatch = phoneMatches(company.RegisteredTel, place.Phone)
		view.CityMatch = cityMatches(company.City, place.Address)
	}

	return view
}

func onlyDigits(text string) string {
	var b strings.Builder

	for _, r := range text {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}

	return b.String()
}

// phoneMatches compares the last digits of both phones; nil when either is too
// short to compare.
func phoneMatches(registry, maps string) *bool {
	a, b := onlyDigits(registry), onlyDigits(maps)
	if len(a) < phoneTailDigits || len(b) < phoneTailDigits {
		return nil
	}

	match := a[len(a)-phoneTailDigits:] == b[len(b)-phoneTailDigits:]

	return &match
}

var accentFolder = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "ä", "a", "é", "e", "è", "e", "ê", "e", "ë", "e",
	"í", "i", "ì", "i", "î", "i", "ï", "i", "ó", "o", "ò", "o", "ô", "o", "õ", "o", "ö", "o",
	"ú", "u", "ù", "u", "û", "u", "ü", "u", "ç", "c", "ñ", "n",
)

func foldText(text string) string {
	folded := accentFolder.Replace(strings.ToLower(text))

	var b strings.Builder

	for _, r := range folded {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}

	return " " + strings.Join(strings.Fields(b.String()), " ") + " "
}

// cityMatches reports whether the registry's city appears in the Maps address;
// nil when there is no city or address to compare.
func cityMatches(city, address string) *bool {
	if strings.TrimSpace(city) == "" || strings.TrimSpace(address) == "" {
		return nil
	}

	match := strings.Contains(foldText(address), foldText(city))

	return &match
}

// lookupCompany returns the company for a normalized CNPJ, from the cache when
// it has an unexpired answer and from the API otherwise (caching the outcome).
// Transient failures are returned without being cached.
func (s *Service) lookupCompany(ctx context.Context, number string) (cnpj.Company, error) {
	now := time.Now()

	cached, err := s.companies.GetLookups(ctx, []string{number}, now)
	if err != nil {
		return cnpj.Company{}, fmt.Errorf("read cnpj cache: %w", err)
	}

	if hit, ok := cached[number]; ok {
		if hit.Status == lookupStatusNotFound {
			return cnpj.Company{}, cnpj.ErrNotFound
		}

		if hit.Company != nil {
			return *hit.Company, nil
		}
	}

	ctx, cancel := context.WithTimeout(ctx, cnpjLookupTimeout)
	defer cancel()

	company, err := s.cnpj.Lookup(ctx, number)

	switch {
	case err == nil:
		s.saveLookup(ctx, CompanyLookup{
			CNPJ: number, Status: lookupStatusOK, Company: &company,
			FetchedAt: now, ExpiresAt: now.Add(lookupTTLFound),
		})
	case errors.Is(err, cnpj.ErrNotFound):
		s.saveLookup(ctx, CompanyLookup{
			CNPJ: number, Status: lookupStatusNotFound,
			FetchedAt: now, ExpiresAt: now.Add(lookupTTLNotFound),
		})
	}

	return company, err
}

// saveLookup caches an outcome; a failure to cache never fails the request.
func (s *Service) saveLookup(ctx context.Context, lookup CompanyLookup) {
	if err := s.companies.SaveLookup(ctx, lookup); err != nil {
		log.Printf("save cnpj lookup: %v", err)
	}
}

// GetPlacesWithCompanies is GetPlaces plus the company linked to each place.
func (s *Service) GetPlacesWithCompanies(ctx context.Context, id string) ([]Place, error) {
	places, err := s.GetPlaces(ctx, id)
	if err != nil {
		return nil, err
	}

	links, err := s.companies.PlaceCNPJs(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("read cnpj links: %w", err)
	}

	if len(links) == 0 {
		return places, nil
	}

	numbers := make([]string, 0, len(links))
	for _, number := range links {
		numbers = append(numbers, number)
	}

	now := time.Now()

	lookups, err := s.companies.GetLookups(ctx, numbers, now)
	if err != nil {
		return nil, fmt.Errorf("read cnpj cache: %w", err)
	}

	for i := range places {
		number, linked := links[places[i].Position]
		if !linked {
			continue
		}

		if lookup, ok := lookups[number]; ok && lookup.Company != nil {
			places[i].Company = newCompanyView(lookup.Company, cnpjSourceManual, &places[i], now)
		}
	}

	return places, nil
}

// SetPlaceCNPJ links a CNPJ typed by a person to the place at position of a
// job, looking the company up (cache first). The link is only saved when the
// company was found.
func (s *Service) SetPlaceCNPJ(ctx context.Context, jobID string, position int, raw string) (*CompanyView, error) {
	number, err := cnpj.Normalize(raw)
	if err != nil {
		return nil, err
	}

	places, err := s.GetPlaces(ctx, jobID)
	if err != nil {
		return nil, err
	}

	if position < 1 || position > len(places) {
		return nil, ErrPlacesNotFound
	}

	company, err := s.lookupCompany(ctx, number)
	if err != nil {
		return nil, err
	}

	if err := s.companies.SetPlaceCNPJ(ctx, jobID, position, number, cnpjSourceManual, time.Now()); err != nil {
		return nil, fmt.Errorf("save cnpj link: %w", err)
	}

	return newCompanyView(&company, cnpjSourceManual, &places[position-1], time.Now()), nil
}

// ClearPlaceCNPJ removes the CNPJ linked to a place.
func (s *Service) ClearPlaceCNPJ(ctx context.Context, jobID string, position int) error {
	return s.companies.ClearPlaceCNPJ(ctx, jobID, position)
}

// ---- HTTP ----

// cnpjRequest validates the common parts of /cnpj/set and /cnpj/clear: method,
// the HTMX header (a cross-site form cannot set it), the job id and position.
func (s *Server) cnpjRequest(w http.ResponseWriter, r *http.Request) (jobID string, position int, ok bool) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return "", 0, false
	}

	if r.Header.Get("HX-Request") != "true" {
		http.Error(w, "Requisição inválida.", http.StatusBadRequest)

		return "", 0, false
	}

	id, found := getIDFromRequest(r)
	if !found {
		http.Error(w, "Pesquisa inválida.", http.StatusUnprocessableEntity)

		return "", 0, false
	}

	position, err := strconv.Atoi(r.URL.Query().Get("pos"))
	if err != nil || position < 1 {
		http.Error(w, "Empresa inválida.", http.StatusUnprocessableEntity)

		return "", 0, false
	}

	return id.String(), position, true
}

// setCNPJ handles POST /cnpj/set?id=<job>&pos=<n> with the form field "cnpj".
func (s *Server) setCNPJ(w http.ResponseWriter, r *http.Request) {
	jobID, position, ok := s.cnpjRequest(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Não foi possível ler o formulário.", http.StatusBadRequest)

		return
	}

	view, err := s.svc.SetPlaceCNPJ(r.Context(), jobID, position, r.Form.Get("cnpj"))
	if err != nil {
		status, message := cnpjError(err)
		http.Error(w, message, status)

		return
	}

	renderJSON(w, http.StatusOK, view)
}

// clearCNPJ handles POST /cnpj/clear?id=<job>&pos=<n>.
func (s *Server) clearCNPJ(w http.ResponseWriter, r *http.Request) {
	jobID, position, ok := s.cnpjRequest(w, r)
	if !ok {
		return
	}

	if err := s.svc.ClearPlaceCNPJ(r.Context(), jobID, position); err != nil {
		log.Printf("clear cnpj: %v", err)
		http.Error(w, "Não foi possível remover o CNPJ.", http.StatusInternalServerError)

		return
	}

	renderJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// cnpjError maps a lookup error to an HTTP status and a message for the person.
func cnpjError(err error) (int, string) {
	switch {
	case errors.Is(err, cnpj.ErrInvalid):
		return http.StatusUnprocessableEntity, "CNPJ inválido. Confira os 14 dígitos."
	case errors.Is(err, cnpj.ErrNotFound):
		return http.StatusNotFound, "Este CNPJ não foi encontrado na Receita Federal."
	case errors.Is(err, cnpj.ErrRateLimited):
		return http.StatusTooManyRequests, "A consulta de CNPJ está limitada no momento. Tente de novo em instantes."
	case errors.Is(err, ErrPlacesNotFound):
		return http.StatusNotFound, "Empresa não encontrada nesta pesquisa."
	default:
		log.Printf("cnpj lookup: %v", err)

		return http.StatusBadGateway, "Não foi possível consultar o CNPJ agora. Tente de novo em instantes."
	}
}
