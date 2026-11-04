package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// hubContact is one company in the payload sent to the Hub (see the contract
// in the Hub repository, docs INT-xx "gmaps-scraper-sso-contatos").
type hubContact struct {
	Position int      `json:"posicao"`
	Name     string   `json:"nome"`
	Category string   `json:"categoria,omitempty"`
	Address  string   `json:"endereco,omitempty"`
	Phone    string   `json:"telefone,omitempty"`
	Website  string   `json:"site,omitempty"`
	Emails   []string `json:"emails,omitempty"`
	Link     string   `json:"link,omitempty"`
}

type hubBatch struct {
	ID        string `json:"id"`
	Title     string `json:"titulo"`
	CreatedAt string `json:"gerado_em"`
}

type hubSendRequest struct {
	Username string       `json:"username"`
	Batch    hubBatch     `json:"lote"`
	Contacts []hubContact `json:"contatos"`
}

type hubSendResponse struct {
	Created  int `json:"criadas"`
	Existing int `json:"existentes"`
	Ignored  []struct {
		Position int    `json:"posicao"`
		Reason   string `json:"motivo"`
	} `json:"ignoradas"`
}

// hubSendResult is what the person sees after sending a job to the Hub.
type hubSendResult struct {
	Message string `json:"message"`
	// TasksURL opens "Minhas tarefas" in the Hub (empty when HUB_URL is unset).
	TasksURL string `json:"url,omitempty"`
}

func truncate(text string, limit int) string {
	text = strings.TrimSpace(text)

	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit-1]) + "…"
}

// cleanURL keeps a URL only when the Hub would accept it: http(s), no
// whitespace and within the limit. A value over the limit is dropped, never cut:
// a truncated URL points somewhere else.
func cleanURL(raw string, limit int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len([]rune(raw)) > limit || strings.ContainsAny(raw, " \t\r\n") || !isHTTPURL(raw) {
		return ""
	}

	return raw
}

// cleanEmail keeps an e-mail only when it fits the Hub's rules (no whitespace,
// within the limit); like URLs it is dropped rather than cut.
func cleanEmail(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len([]rune(raw)) > hubMaxEmail || strings.ContainsAny(raw, " \t\r\n") || !strings.Contains(raw, "@") {
		return ""
	}

	return raw
}

// buildHubContacts converts places to Hub contacts. The position is the place's
// index in the job's result list (1-based) and does not change when places are
// filtered out, so sending the same job again is idempotent on the Hub side.
// Places with no usable phone, website or e-mail are skipped and counted.
// Every value is shaped to the Hub's limits first: it refuses a whole batch for
// one bad value.
func buildHubContacts(places []Place) ([]hubContact, int) {
	contacts := make([]hubContact, 0, len(places))
	skipped := 0

	for i := range places {
		place := &places[i]

		contact := hubContact{
			Position: i + 1,
			Name:     truncate(place.Title, hubMaxName),
			Category: truncate(place.Category, hubMaxCategory),
			Address:  truncate(place.Address, hubMaxAddress),
			Phone:    truncate(place.Phone, hubMaxPhone),
			Website:  cleanURL(place.Website, hubMaxSite),
			Link:     cleanURL(place.Link, hubMaxLink),
		}

		for _, email := range place.Emails {
			if cleaned := cleanEmail(email); cleaned != "" && len(contact.Emails) < hubMaxEmails {
				contact.Emails = append(contact.Emails, cleaned)
			}
		}

		if contact.Name == "" || contact.Position > hubMaxPosition ||
			(contact.Phone == "" && contact.Website == "" && len(contact.Emails) == 0) {
			skipped++

			continue
		}

		contacts = append(contacts, contact)
	}

	return contacts, skipped
}

// errHubUnavailable and errHubRefused carry friendly messages for the person.
var (
	errHubUnavailable = formError("O Hub não respondeu agora. Tente de novo em instantes.")
	errHubRefused     = formError("O Hub recusou o envio: sua conta não tem acesso a este recurso.")
	errHubConfig      = formError("A integração com o Hub não está autorizada (token do app). Avise a TI.")
	errHubRejected    = formError("O Hub recusou o formato dos dados desta pesquisa. Avise a TI.")
)

// sendContacts posts one batch to the Hub.
func (h *hub) sendContacts(ctx context.Context, req *hubSendRequest) (hubSendResponse, error) {
	var answer hubSendResponse

	body, err := json.Marshal(req)
	if err != nil {
		return answer, fmt.Errorf("encode hub request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, hubSendTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.cfg.APIURL+hubContactsPath, bytes.NewReader(body))
	if err != nil {
		return answer, fmt.Errorf("build hub request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-App-Token", h.cfg.AppToken)

	resp, err := h.client.Do(httpReq)
	if err != nil {
		log.Printf("hub send failed: %v", err)

		return answer, errHubUnavailable
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, hubMaxRespBody))

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		if err := json.Unmarshal(raw, &answer); err != nil {
			log.Printf("hub send: bad answer: %v", err)

			return answer, errHubUnavailable
		}

		return answer, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusServiceUnavailable:
		log.Printf("hub send refused: status %d", resp.StatusCode)

		return answer, errHubConfig
	case resp.StatusCode == http.StatusForbidden:
		return answer, errHubRefused
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusRequestEntityTooLarge:
		// A contract mismatch, not an outage: retrying will not help.
		log.Printf("hub send rejected: status %d", resp.StatusCode)

		return answer, errHubRejected
	default:
		// The body may echo contact data; log only the status.
		log.Printf("hub send failed: status %d", resp.StatusCode)

		return answer, errHubUnavailable
	}
}

// hubSend handles POST /hub/send?id=<job>: it sends the job's companies to the
// logged-in person's "Minhas tarefas" in the Hub.
func (s *Server) hubSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	// Cross-site forms cannot set this header, which (with SameSite=Lax on the
	// session cookie) keeps other sites from triggering a send.
	if r.Header.Get("HX-Request") != "true" {
		http.Error(w, "Requisição inválida.", http.StatusBadRequest)

		return
	}

	session, ok := hubSessionFrom(r)
	if !ok || !s.hub.cfg.CanSend() {
		http.Error(w, "A integração com o Hub não está configurada.", http.StatusNotFound)

		return
	}

	id, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Pesquisa inválida.", http.StatusUnprocessableEntity)

		return
	}

	job, err := s.svc.Get(r.Context(), id.String())
	if err != nil || job.ID == "" {
		http.Error(w, "Pesquisa não encontrada.", http.StatusNotFound)

		return
	}

	if job.Status != StatusOK {
		http.Error(w, "A pesquisa ainda não terminou. Envie depois que ela for concluída.", http.StatusConflict)

		return
	}

	places, err := s.svc.GetPlaces(r.Context(), id.String())
	if err != nil {
		if errors.Is(err, ErrPlacesNotFound) {
			http.Error(w, "Esta pesquisa não tem resultados para enviar.", http.StatusNotFound)

			return
		}

		log.Printf("hub send: places for %s: %v", id, err)
		http.Error(w, "Não foi possível ler os resultados.", http.StatusInternalServerError)

		return
	}

	result, err := s.pushToHub(r.Context(), &session, &job, places)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errHubRefused) {
			status = http.StatusForbidden
		}

		http.Error(w, err.Error(), status)

		return
	}

	renderJSON(w, http.StatusOK, result)
}

func (s *Server) pushToHub(ctx context.Context, session *hubSession, job *Job, places []Place) (hubSendResult, error) {
	contacts, skipped := buildHubContacts(places)
	if len(contacts) == 0 {
		return hubSendResult{}, formError("Nenhuma empresa desta pesquisa tem telefone, site ou e-mail para enviar.")
	}

	batch := hubBatch{
		ID:        job.ID,
		Title:     truncate(job.Name, hubMaxName),
		CreatedAt: job.Date.UTC().Format(time.RFC3339),
	}

	created, existing := 0, 0

	for start := 0; start < len(contacts); start += hubMaxContacts {
		end := min(start+hubMaxContacts, len(contacts))

		answer, err := s.hub.sendContacts(ctx, &hubSendRequest{
			Username: session.Username,
			Batch:    batch,
			Contacts: contacts[start:end],
		})
		if err != nil {
			if created+existing > 0 {
				return hubSendResult{}, fmt.Errorf("%d tarefas foram criadas antes de a conexão falhar; envie de novo para completar (não duplica). %w", created, err)
			}

			return hubSendResult{}, err
		}

		created += answer.Created
		existing += answer.Existing
	}

	message := fmt.Sprintf("%d tarefas criadas em Minhas tarefas", created)
	if existing > 0 {
		message += fmt.Sprintf(" (%d já existiam)", existing)
	}

	if skipped > 0 {
		message += fmt.Sprintf(". %d empresas sem contato não foram enviadas", skipped)
	}

	result := hubSendResult{Message: message + "."}
	if s.hub.cfg.URL != "" {
		result.TasksURL = s.hub.cfg.URL + "/minhas-tarefas"
	}

	return result, nil
}
