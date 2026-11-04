package web

import (
	"fmt"
	"strings"
	"time"
)

const (
	// maxKeywordChips is how many keywords a job row shows before collapsing
	// the rest into a "+N" chip.
	maxKeywordChips = 3

	// pollIdle is how often the job list refreshes when nothing is running.
	pollIdle = "10s"
	// pollActive is how often the job list refreshes while a job is pending
	// or working, so progress shows up quickly.
	pollActive = "4s"

	maxGeneratedNameLen = 60
)

// jobRowView is the presentation model of a job: everything the templates
// need already formatted, so the HTML stays free of logic.
type jobRowView struct {
	ID           string
	ShortID      string
	Name         string
	DateISO      string
	DateText     string
	Status       string
	StatusLabel  string
	Keywords     []string
	MoreKeywords int
	Lang         string
	Depth        int
	Email        bool
	FastMode     bool
	Active       bool
	Queued       bool
	Elapsed      string
}

// jobsView is the data rendered by job_rows.html: the current page of jobs
// plus the jobs running anywhere in the list (not just on this page).
type jobsView struct {
	JobPage

	Rows        []jobRowView
	Active      []jobRowView
	ActiveCount int
	WorkingCnt  int
	QueuedCnt   int
	PollEvery   string
	// HubSend shows the "Enviar ao Hub" action on finished jobs.
	HubSend bool
}

func newJobRowView(job *Job, now time.Time) jobRowView {
	keywords := job.Data.Keywords
	more := 0

	if len(keywords) > maxKeywordChips {
		more = len(keywords) - maxKeywordChips
		keywords = keywords[:maxKeywordChips]
	}

	shortID := job.ID
	if before, _, found := strings.Cut(job.ID, "-"); found {
		shortID = before
	}

	active := job.Status == StatusPending || job.Status == StatusWorking

	return jobRowView{
		ID:           job.ID,
		ShortID:      shortID,
		Name:         job.Name,
		DateISO:      job.Date.UTC().Format(time.RFC3339),
		DateText:     job.Date.UTC().Format("02/01/2006 15:04") + " UTC",
		Status:       job.Status,
		StatusLabel:  statusLabel(job.Status),
		Keywords:     keywords,
		MoreKeywords: more,
		Lang:         job.Data.Lang,
		Depth:        job.Data.Depth,
		Email:        job.Data.Email,
		FastMode:     job.Data.FastMode,
		Active:       active,
		Queued:       job.Status == StatusPending,
		Elapsed:      elapsedSince(job.Date, now),
	}
}

func newJobsView(page JobPage, active []Job, now time.Time) jobsView {
	view := jobsView{
		JobPage:   page,
		Rows:      make([]jobRowView, 0, len(page.Jobs)),
		Active:    make([]jobRowView, 0, len(active)),
		PollEvery: pollIdle,
	}

	for i := range page.Jobs {
		view.Rows = append(view.Rows, newJobRowView(&page.Jobs[i], now))
	}

	for i := range active {
		row := newJobRowView(&active[i], now)
		view.Active = append(view.Active, row)

		if row.Queued {
			view.QueuedCnt++
		} else {
			view.WorkingCnt++
		}
	}

	view.ActiveCount = len(view.Active)
	if view.ActiveCount > 0 {
		view.PollEvery = pollActive
	}

	return view
}

// RunSummary is the text of the header chip, e.g. "2 em execução · 1 na fila".
func (v jobsView) RunSummary() string {
	var parts []string

	if v.WorkingCnt > 0 {
		parts = append(parts, fmt.Sprintf("%d em execução", v.WorkingCnt))
	}

	if v.QueuedCnt > 0 {
		parts = append(parts, fmt.Sprintf("%d na fila", v.QueuedCnt))
	}

	if len(parts) == 0 {
		return "Nenhuma pesquisa em execução"
	}

	return strings.Join(parts, " · ")
}

func statusLabel(status string) string {
	switch status {
	case StatusPending:
		return "Na fila"
	case StatusWorking:
		return "Executando"
	case StatusOK:
		return "Concluída"
	case StatusFailed:
		return "Falhou"
	default:
		return status
	}
}

// elapsedSince renders how long ago t was, in short pt-BR ("12 s", "5 min",
// "2 h 10 min"). Future or zero times render as an empty string.
func elapsedSince(t, now time.Time) string {
	if t.IsZero() || now.Before(t) {
		return ""
	}

	d := now.Sub(t).Round(time.Second)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60

		if minutes == 0 {
			return fmt.Sprintf("%d h", hours)
		}

		return fmt.Sprintf("%d h %d min", hours, minutes)
	default:
		return fmt.Sprintf("%d d", int(d.Hours())/24)
	}
}

// defaultJobName builds a readable name from the first keyword when the user
// leaves the name empty.
func defaultJobName(keywords []string) string {
	if len(keywords) == 0 {
		return ""
	}

	name := keywords[0]

	runes := []rune(name)
	if len(runes) > maxGeneratedNameLen {
		name = string(runes[:maxGeneratedNameLen-1]) + "…"
	}

	if len(keywords) > 1 {
		name = fmt.Sprintf("%s (+%d)", name, len(keywords)-1)
	}

	return name
}

// newJobsView builds the view and adds the server-level switches.
func (s *Server) newJobsView(page JobPage, active []Job, now time.Time) jobsView {
	view := newJobsView(page, active, now)
	view.HubSend = s.hub != nil && s.hub.cfg.CanSend()

	return view
}
