package web

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Service struct {
	repo       JobRepository
	dataFolder string
}

type jobCounter interface {
	Count(context.Context, SelectParams) (int, error)
}

func NewService(repo JobRepository, dataFolder string) *Service {
	return &Service{
		repo:       repo,
		dataFolder: dataFolder,
	}
}

func (s *Service) Create(ctx context.Context, job *Job) error {
	return s.repo.Create(ctx, job)
}

func (s *Service) All(ctx context.Context) ([]Job, error) {
	return s.repo.Select(ctx, SelectParams{})
}

// ListJobs returns a page of jobs, clamping invalid page and limit values.
func (s *Service) ListJobs(ctx context.Context, page, limit int) (JobPage, error) {
	var ans JobPage

	if page < 1 {
		page = 1
	}

	if limit < 1 {
		limit = 20
	}

	total, err := s.countJobs(ctx, SelectParams{})
	if err != nil {
		return ans, err
	}

	totalPages := (total + limit - 1) / limit
	if totalPages == 0 {
		totalPages = 1
	}

	if page > totalPages {
		page = totalPages
	}

	offset := (page - 1) * limit
	params := SelectParams{
		Limit:  limit,
		Offset: offset,
	}

	jobs, err := s.repo.Select(ctx, params)
	if err != nil {
		return ans, err
	}

	ans = JobPage{
		Jobs:        jobs,
		CurrentPage: page,
		TotalPages:  totalPages,
		Total:       total,
		HasPrev:     page > 1,
		HasNext:     page < totalPages,
		PrevPage:    page - 1,
		NextPage:    page + 1,
		HasPages:    totalPages > 1,
	}

	return ans, nil
}

// ActiveJobs returns every job that is waiting to run or running, newest
// first, regardless of pagination. It feeds the "running now" indicators.
func (s *Service) ActiveJobs(ctx context.Context) ([]Job, error) {
	var active []Job

	for _, status := range []string{StatusWorking, StatusPending} {
		jobs, err := s.repo.Select(ctx, SelectParams{Status: status})
		if err != nil {
			return nil, fmt.Errorf("select %s jobs: %w", status, err)
		}

		// Not every JobRepository honours the status filter, so keep only
		// the jobs that really match.
		for i := range jobs {
			if jobs[i].Status == status {
				active = append(active, jobs[i])
			}
		}
	}

	return active, nil
}

// FailInterruptedJobs marks every job still "working" as failed and returns
// how many it changed. Call it once at startup, before the runner picks up new
// work: a job left "working" by a previous process can never finish.
func (s *Service) FailInterruptedJobs(ctx context.Context) (int, error) {
	jobs, err := s.repo.Select(ctx, SelectParams{Status: StatusWorking})
	if err != nil {
		return 0, fmt.Errorf("select working jobs: %w", err)
	}

	count := 0

	for i := range jobs {
		if jobs[i].Status != StatusWorking {
			continue
		}

		jobs[i].Status = StatusFailed

		if err := s.repo.Update(ctx, &jobs[i]); err != nil {
			return count, fmt.Errorf("fail job %s: %w", jobs[i].ID, err)
		}

		count++
	}

	return count, nil
}

func (s *Service) countJobs(ctx context.Context, params SelectParams) (int, error) {
	if counter, ok := s.repo.(jobCounter); ok {
		return counter.Count(ctx, params)
	}

	jobs, err := s.repo.Select(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("select jobs for count: %w", err)
	}

	return len(jobs), nil
}

func (s *Service) Get(ctx context.Context, id string) (Job, error) {
	return s.repo.Get(ctx, id)
}

func (s *Service) Delete(ctx context.Context, id string) error {
	datapath, err := s.csvPath(id)
	if err != nil {
		return err
	}

	if _, err := os.Stat(datapath); err == nil {
		if err := os.Remove(datapath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	return s.repo.Delete(ctx, id)
}

func (s *Service) Update(ctx context.Context, job *Job) error {
	return s.repo.Update(ctx, job)
}

func (s *Service) SelectPending(ctx context.Context) ([]Job, error) {
	return s.repo.Select(ctx, SelectParams{Status: StatusPending, Limit: 1})
}

// csvPath returns the on-disk path of a job's CSV output, rejecting ids that
// could escape the data folder.
func (s *Service) csvPath(id string) (string, error) {
	if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
		return "", fmt.Errorf("invalid file name")
	}

	return filepath.Join(s.dataFolder, id+".csv"), nil
}

func (s *Service) GetCSV(_ context.Context, id string) (string, error) {
	datapath, err := s.csvPath(id)
	if err != nil {
		return "", err
	}

	if _, err := os.Stat(datapath); os.IsNotExist(err) {
		return "", fmt.Errorf("csv file not found for job %s", id)
	}

	return datapath, nil
}
