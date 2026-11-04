package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"uuid"
)

//go:embed static
var static embed.FS

const (
	jobsPageLimit           = 20
	methodNotAllowedMessage = "Method not allowed"
)

type Server struct {
	tmpl map[string]*template.Template
	srv  *http.Server
	svc  *Service
}

func New(svc *Service, addr string) (*Server, error) {
	ans := Server{
		svc:  svc,
		tmpl: make(map[string]*template.Template),
		srv: &http.Server{
			Addr:              addr,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      60 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    1 << 20,
		},
	}

	staticFS, err := fs.Sub(static, "static")
	if err != nil {
		return nil, err
	}

	fileServer := http.FileServer(http.FS(staticFS))
	mux := http.NewServeMux()

	mux.Handle("/static/", http.StripPrefix("/static/", fileServer))
	mux.HandleFunc("/scrape", ans.scrape)
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.download(w, r)
	})
	mux.HandleFunc("/delete", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.delete(w, r)
	})
	mux.HandleFunc("/jobs", ans.getJobs)
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		ans.viewJob(w, r)
	})
	mux.HandleFunc("/", ans.index)

	// api routes
	mux.HandleFunc("/api/docs", ans.redocHandler)
	mux.HandleFunc("/api/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			ans.apiScrape(w, r)
		case http.MethodGet:
			ans.apiGetJobs(w, r)
		default:
			ans := apiError{
				Code:    http.StatusMethodNotAllowed,
				Message: methodNotAllowedMessage,
			}

			renderJSON(w, http.StatusMethodNotAllowed, ans)
		}
	})

	mux.HandleFunc("/api/v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		switch r.Method {
		case http.MethodGet:
			ans.apiGetJob(w, r)
		case http.MethodDelete:
			ans.apiDeleteJob(w, r)
		default:
			ans := apiError{
				Code:    http.StatusMethodNotAllowed,
				Message: methodNotAllowedMessage,
			}

			renderJSON(w, http.StatusMethodNotAllowed, ans)
		}
	})

	mux.HandleFunc("/api/v1/jobs/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		r = requestWithID(r)

		if r.Method != http.MethodGet {
			ans := apiError{
				Code:    http.StatusMethodNotAllowed,
				Message: methodNotAllowedMessage,
			}

			renderJSON(w, http.StatusMethodNotAllowed, ans)

			return
		}

		ans.download(w, r)
	})

	handler := securityHeaders(mux)
	ans.srv.Handler = handler

	tmplsKeys := []string{
		"static/templates/index.html",
		"static/templates/job_rows.html",
		"static/templates/job_view.html",
		"static/templates/redoc.html",
	}

	for _, key := range tmplsKeys {
		tmp, err := template.ParseFS(static, key)
		if err != nil {
			return nil, err
		}

		ans.tmpl[key] = tmp
	}

	return &ans, nil
}

func (s *Server) Start(ctx context.Context) error {
	go func(shutdownSource context.Context) {
		<-shutdownSource.Done()

		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(shutdownSource), 10*time.Second)
		defer cancel()

		err := s.srv.Shutdown(shutdownCtx)
		if err != nil {
			log.Println(err)

			return
		}

		log.Println("server stopped")
	}(ctx)

	fmt.Fprintf(os.Stderr, "visit http://localhost%s\n", s.srv.Addr)

	err := s.srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		return err
	}

	return nil
}

type formData struct {
	Name     string
	MaxTime  string
	Keywords []string
	Language string
	Zoom     int
	FastMode bool
	Radius   int
	Lat      string
	Lon      string
	Depth    int
	Email    bool
	Proxies  []string
}

type ctxKey string

const idCtxKey ctxKey = "id"

func requestWithID(r *http.Request) *http.Request {
	id := r.PathValue("id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}

	parsed, err := uuid.Parse(id)
	if err == nil {
		r = r.WithContext(context.WithValue(r.Context(), idCtxKey, parsed))
	}

	return r
}

func getIDFromRequest(r *http.Request) (uuid.UUID, bool) {
	id, ok := r.Context().Value(idCtxKey).(uuid.UUID)

	return id, ok
}

//nolint:gocritic // this is used in template
func (f formData) ProxiesString() string {
	return strings.Join(f.Proxies, "\n")
}

//nolint:gocritic // this is used in template
func (f formData) KeywordsString() string {
	return strings.Join(f.Keywords, "\n")
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	tmpl, ok := s.tmpl["static/templates/index.html"]
	if !ok {
		http.Error(w, "missing tpl", http.StatusInternalServerError)

		return
	}

	data := formData{
		Name:     "",
		MaxTime:  "10m",
		Keywords: []string{},
		Language: "pt",
		Zoom:     15,
		FastMode: false,
		Radius:   10000,
		// Lat/Lon stay empty on purpose: when set, every search is anchored to
		// that point on the map, and 0,0 is open ocean (no results).
		Lat:   "",
		Lon:   "",
		Depth: 10,
		Email: false,
	}

	_ = tmpl.Execute(w, data)
}

func (s *Server) scrape(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	err := r.ParseForm()
	if err != nil {
		http.Error(w, "Não foi possível ler o formulário.", http.StatusBadRequest)

		return
	}

	newJob, err := jobFromForm(r.Form)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)

		return
	}

	err = s.svc.Create(r.Context(), &newJob)
	if err != nil {
		log.Printf("create job: %v", err)
		http.Error(w, "Não foi possível salvar a pesquisa. Tente novamente.", http.StatusInternalServerError)

		return
	}

	if err := s.renderJobsPage(r.Context(), w, 1); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// formError is a validation message meant to be shown to the person filling
// the web form (pt-BR, capitalized, with punctuation).
type formError string

func (e formError) Error() string { return string(e) }

// jobFromForm builds and validates a Job from the web form. Errors are
// user-facing messages (pt-BR).
func jobFromForm(form url.Values) (Job, error) {
	newJob := Job{
		ID:     uuid.NewV4().String(),
		Name:   strings.TrimSpace(form.Get("name")),
		Date:   time.Now().UTC(),
		Status: StatusPending,
		Data:   JobData{},
	}

	maxTime, err := time.ParseDuration(strings.TrimSpace(form.Get("maxtime")))
	if err != nil {
		return Job{}, formError("Tempo máximo inválido. Use um valor como 10m ou 1h.")
	}

	if maxTime < time.Minute*3 {
		return Job{}, formError("O tempo máximo deve ser de pelo menos 3m.")
	}

	newJob.Data.MaxTime = maxTime
	newJob.Data.Keywords = splitLines(form.Get("keywords"))

	if len(newJob.Data.Keywords) == 0 {
		return Job{}, formError("Informe ao menos uma palavra-chave, uma por linha.")
	}

	if newJob.Name == "" {
		newJob.Name = defaultJobName(newJob.Data.Keywords)
	}

	newJob.Data.Lang = strings.TrimSpace(form.Get("lang"))
	newJob.Data.FastMode = form.Get("fastmode") == "on"
	newJob.Data.Email = form.Get("email") == "on"
	newJob.Data.Proxies = splitLines(form.Get("proxies"))

	if err := parseFormNumbers(form, &newJob.Data); err != nil {
		return Job{}, err
	}

	if err := parseFormCoordinates(form, &newJob.Data); err != nil {
		return Job{}, err
	}

	if err := newJob.Validate(); err != nil {
		return Job{}, translateValidationError(err)
	}

	return newJob, nil
}

func parseFormNumbers(form url.Values, data *JobData) error {
	var err error

	data.Zoom, err = strconv.Atoi(strings.TrimSpace(form.Get("zoom")))
	if err != nil {
		return formError("Zoom inválido. Use um número inteiro.")
	}

	data.Radius, err = strconv.Atoi(strings.TrimSpace(form.Get("radius")))
	if err != nil {
		return formError("Raio inválido. Use um número inteiro, em metros.")
	}

	data.Depth, err = strconv.Atoi(strings.TrimSpace(form.Get("depth")))
	if err != nil {
		return formError("Profundidade inválida. Use um número inteiro.")
	}

	return nil
}

// parseFormCoordinates reads the optional latitude/longitude pair. Both empty
// means "search without anchoring to a point"; a half-filled pair or an
// out-of-range value is rejected instead of silently searching somewhere else.
func parseFormCoordinates(form url.Values, data *JobData) error {
	lat := strings.TrimSpace(form.Get("latitude"))
	lon := strings.TrimSpace(form.Get("longitude"))

	if lat == "" && lon == "" {
		return nil
	}

	if lat == "" || lon == "" {
		return formError("Informe latitude e longitude juntas, ou deixe as duas em branco.")
	}

	latitude, err := strconv.ParseFloat(lat, 64)
	if err != nil || latitude < -90 || latitude > 90 {
		return formError("Latitude inválida. Use um valor entre -90 e 90.")
	}

	longitude, err := strconv.ParseFloat(lon, 64)
	if err != nil || longitude < -180 || longitude > 180 {
		return formError("Longitude inválida. Use um valor entre -180 e 180.")
	}

	data.Lat = lat
	data.Lon = lon

	return nil
}

// splitLines returns the trimmed, non-empty lines of text.
func splitLines(text string) []string {
	var lines []string

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		lines = append(lines, line)
	}

	return lines
}

// translateValidationError maps Job.Validate errors (shared with the JSON API,
// hence in English) to messages for the web form.
func translateValidationError(err error) error {
	switch err.Error() {
	case "missing keywords":
		return formError("Informe ao menos uma palavra-chave, uma por linha.")
	case "missing lang":
		return formError("Informe o idioma (2 letras, ex.: pt).")
	case "invalid lang":
		return formError("Idioma inválido. Use o código de 2 letras, ex.: pt ou en.")
	case "missing depth":
		return formError("Informe a profundidade (maior que zero).")
	case "missing max time":
		return formError("Informe o tempo máximo da pesquisa.")
	case "missing geo coordinates":
		return formError("O modo rápido exige latitude e longitude.")
	default:
		return err
	}
}

func (s *Server) renderJobsPage(ctx context.Context, w io.Writer, page int) error {
	tmpl, ok := s.tmpl["static/templates/job_rows.html"]
	if !ok {
		return errors.New("missing tpl")
	}

	jobPage, err := s.svc.ListJobs(ctx, page, jobsPageLimit)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	active, err := s.svc.ActiveJobs(ctx)
	if err != nil {
		return fmt.Errorf("list active jobs: %w", err)
	}

	var output bytes.Buffer
	if err := tmpl.Execute(&output, newJobsView(jobPage, active, time.Now())); err != nil {
		return fmt.Errorf("render jobs: %w", err)
	}

	if _, err := output.WriteTo(w); err != nil {
		return fmt.Errorf("write jobs: %w", err)
	}

	return nil
}

func pageFromRequest(r *http.Request) int {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil || page < 1 {
		return 1
	}

	return page
}

func (s *Server) getJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	if err := s.renderJobsPage(r.Context(), w, pageFromRequest(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	ctx := r.Context()

	id, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	filePath, err := s.svc.GetCSV(ctx, id.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	file, err := os.Open(filePath) //nolint:gosec // GetCSV returns a validated path rooted in the configured data directory.
	if err != nil {
		http.Error(w, "Failed to open file", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	fileName := filepath.Base(filePath)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
	w.Header().Set("Content-Type", "text/csv")

	_, err = io.Copy(w, file)
	if err != nil {
		http.Error(w, "Failed to send file", http.StatusInternalServerError)
		return
	}
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	deleteID, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	err := s.svc.Delete(r.Context(), deleteID.String())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if err := s.renderJobsPage(r.Context(), w, pageFromRequest(r)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type apiScrapeRequest struct {
	Name string
	JobData
}

type apiScrapeResponse struct {
	ID string `json:"id"`
}

func (s *Server) redocHandler(w http.ResponseWriter, _ *http.Request) {
	tmpl, ok := s.tmpl["static/templates/redoc.html"]
	if !ok {
		http.Error(w, "missing tpl", http.StatusInternalServerError)

		return
	}

	_ = tmpl.Execute(w, nil)
}

func (s *Server) apiScrape(w http.ResponseWriter, r *http.Request) {
	var req apiScrapeRequest

	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		ans := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusUnprocessableEntity, ans)

		return
	}

	newJob := Job{
		ID:     uuid.NewV4().String(),
		Name:   req.Name,
		Date:   time.Now().UTC(),
		Status: StatusPending,
		Data:   req.JobData,
	}

	// convert to seconds
	newJob.Data.MaxTime *= time.Second

	err = newJob.Validate()
	if err != nil {
		ans := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusUnprocessableEntity, ans)

		return
	}

	err = s.svc.Create(r.Context(), &newJob)
	if err != nil {
		ans := apiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusInternalServerError, ans)

		return
	}

	ans := apiScrapeResponse{
		ID: newJob.ID,
	}

	renderJSON(w, http.StatusCreated, ans)
}

func (s *Server) apiGetJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.svc.All(r.Context())
	if err != nil {
		apiError := apiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusInternalServerError, apiError)

		return
	}

	renderJSON(w, http.StatusOK, jobs)
}

func (s *Server) apiGetJob(w http.ResponseWriter, r *http.Request) {
	id, ok := getIDFromRequest(r)
	if !ok {
		apiError := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: "Invalid ID",
		}

		renderJSON(w, http.StatusUnprocessableEntity, apiError)

		return
	}

	job, err := s.svc.Get(r.Context(), id.String())
	if err != nil {
		apiError := apiError{
			Code:    http.StatusNotFound,
			Message: http.StatusText(http.StatusNotFound),
		}

		renderJSON(w, http.StatusNotFound, apiError)

		return
	}

	renderJSON(w, http.StatusOK, job)
}

// viewJob renders the map modal fragment for a job, embedding the job's places
// directly so the client needs no separate data request.
func (s *Server) viewJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, methodNotAllowedMessage, http.StatusMethodNotAllowed)

		return
	}

	id, ok := getIDFromRequest(r)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusUnprocessableEntity)

		return
	}

	places, err := s.svc.GetPlaces(r.Context(), id.String())
	if err != nil {
		if !errors.Is(err, ErrPlacesNotFound) {
			log.Printf("view job %s: %v", id, err) //nolint:gosec // id is a parsed UUID and cannot contain log control characters.
			http.Error(w, "internal server error", http.StatusInternalServerError)

			return
		}

		// No CSV yet: render the modal with an empty state rather than an error.
		places = []Place{}
	}

	tmpl, ok := s.tmpl["static/templates/job_view.html"]
	if !ok {
		http.Error(w, "missing tpl", http.StatusInternalServerError)

		return
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, places); err != nil {
		log.Printf("view job %s: render: %v", id, err) //nolint:gosec // id is a parsed UUID and cannot contain log control characters.
		http.Error(w, "internal server error", http.StatusInternalServerError)

		return
	}

	_, _ = buf.WriteTo(w)
}

func (s *Server) apiDeleteJob(w http.ResponseWriter, r *http.Request) {
	id, ok := getIDFromRequest(r)
	if !ok {
		apiError := apiError{
			Code:    http.StatusUnprocessableEntity,
			Message: "Invalid ID",
		}

		renderJSON(w, http.StatusUnprocessableEntity, apiError)

		return
	}

	err := s.svc.Delete(r.Context(), id.String())
	if err != nil {
		apiError := apiError{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}

		renderJSON(w, http.StatusInternalServerError, apiError)

		return
	}

	w.WriteHeader(http.StatusOK)
}

func renderJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)

	_ = json.NewEncoder(w).Encode(data)
}

func formatDate(t time.Time) string {
	return t.Format("Jan 02, 2006 15:04:05")
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' cdnjs.cloudflare.com 'unsafe-inline' 'unsafe-eval'; "+
				"worker-src 'self' blob:; "+
				"style-src 'self' 'unsafe-inline' fonts.googleapis.com cdnjs.cloudflare.com; "+
				"img-src 'self' data: cdn.redoc.ly cdnjs.cloudflare.com *.tile.openstreetmap.org; "+
				"font-src 'self' fonts.gstatic.com; "+
				"connect-src 'self'")

		next.ServeHTTP(w, r)
	})
}
