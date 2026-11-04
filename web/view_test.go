package web

import (
	"testing"
	"time"
)

func TestElapsedSince(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		from time.Time
		want string
	}{
		{"seconds", now.Add(-42 * time.Second), "42 s"},
		{"minutes", now.Add(-5*time.Minute - 10*time.Second), "5 min"},
		{"hours with minutes", now.Add(-2*time.Hour - 10*time.Minute), "2 h 10 min"},
		{"exact hours", now.Add(-3 * time.Hour), "3 h"},
		{"days", now.Add(-50 * time.Hour), "2 d"},
		{"future", now.Add(time.Minute), ""},
		{"zero", time.Time{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := elapsedSince(tt.from, now); got != tt.want {
				t.Fatalf("elapsedSince = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewJobRowViewCollapsesKeywordsAndLabelsStatus(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	job := Job{
		ID:     "abcd1234-0000-0000-0000-000000000000",
		Name:   "Clínicas",
		Date:   now.Add(-time.Minute),
		Status: StatusWorking,
		Data:   JobData{Keywords: []string{"a", "b", "c", "d", "e"}, Lang: "pt", Depth: 3},
	}

	row := newJobRowView(&job, now)

	if row.ShortID != "abcd1234" {
		t.Fatalf("ShortID = %q", row.ShortID)
	}

	if len(row.Keywords) != maxKeywordChips || row.MoreKeywords != 2 {
		t.Fatalf("keywords = %v (+%d), want %d shown and +2", row.Keywords, row.MoreKeywords, maxKeywordChips)
	}

	if row.StatusLabel != "Executando" || !row.Active || row.Queued {
		t.Fatalf("unexpected status view: %+v", row)
	}

	if row.DateISO != "2026-11-04T11:59:00Z" {
		t.Fatalf("DateISO = %q", row.DateISO)
	}
}

func TestNewJobsViewSummarisesActiveJobs(t *testing.T) {
	now := time.Now()
	page := JobPage{Jobs: []Job{{ID: "1", Status: StatusOK}}, CurrentPage: 1, TotalPages: 1, Total: 1}

	idle := newJobsView(page, nil, now)
	if idle.ActiveCount != 0 || idle.PollEvery != pollIdle || idle.RunSummary() != "Nenhuma pesquisa em execução" {
		t.Fatalf("idle view = %+v / %q", idle, idle.RunSummary())
	}

	active := []Job{
		{ID: "2", Status: StatusWorking, Date: now},
		{ID: "3", Status: StatusWorking, Date: now},
		{ID: "4", Status: StatusPending, Date: now},
	}

	busy := newJobsView(page, active, now)
	if busy.ActiveCount != 3 || busy.PollEvery != pollActive {
		t.Fatalf("busy view = %+v", busy)
	}

	if got, want := busy.RunSummary(), "2 em execução · 1 na fila"; got != want {
		t.Fatalf("RunSummary = %q, want %q", got, want)
	}
}

func TestDefaultJobName(t *testing.T) {
	if got := defaultJobName(nil); got != "" {
		t.Fatalf("empty keywords: %q", got)
	}

	if got := defaultJobName([]string{"padarias em Curitiba"}); got != "padarias em Curitiba" {
		t.Fatalf("single keyword: %q", got)
	}

	if got := defaultJobName([]string{"padarias", "cafés", "bares"}); got != "padarias (+2)" {
		t.Fatalf("many keywords: %q", got)
	}
}
