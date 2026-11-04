package sqlite

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/gosom/google-maps-scraper/internal/cnpj"
	"github.com/gosom/google-maps-scraper/web"
)

// The sqlite repo also keeps CNPJ lookups and the CNPJ linked to each place of a
// job (web.CompanyStore), so a number is never queried twice within its TTL and
// the links survive a restart.
var _ web.CompanyStore = (*repo)(nil)

func (repo *repo) GetLookups(ctx context.Context, numbers []string, now time.Time) (map[string]web.CompanyLookup, error) {
	found := make(map[string]web.CompanyLookup, len(numbers))
	if len(numbers) == 0 {
		return found, nil
	}

	args := make([]any, 0, len(numbers)+1)
	args = append(args, now.Unix())

	for _, number := range numbers {
		args = append(args, number)
	}

	query := `SELECT cnpj, status, payload, fetched_at, expires_at FROM cnpj_lookups
		WHERE expires_at > ? AND cnpj IN (?` + strings.Repeat(",?", len(numbers)-1) + `)`

	rows, err := repo.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	for rows.Next() {
		var (
			lookup           web.CompanyLookup
			payload          string
			fetched, expires int64
		)

		if err := rows.Scan(&lookup.CNPJ, &lookup.Status, &payload, &fetched, &expires); err != nil {
			return nil, err
		}

		if payload != "" {
			var company cnpj.Company
			if err := json.Unmarshal([]byte(payload), &company); err != nil {
				continue // an unreadable row is just a cache miss
			}

			lookup.Company = &company
		}

		lookup.FetchedAt = time.Unix(fetched, 0)
		lookup.ExpiresAt = time.Unix(expires, 0)
		found[lookup.CNPJ] = lookup
	}

	return found, rows.Err()
}

func (repo *repo) SaveLookup(ctx context.Context, lookup web.CompanyLookup) error {
	payload := ""

	if lookup.Company != nil {
		raw, err := json.Marshal(lookup.Company)
		if err != nil {
			return err
		}

		payload = string(raw)
	}

	// Housekeeping: expired lookups are never read again, so drop them instead
	// of letting the table grow. A failure here never blocks the save.
	_, _ = repo.db.ExecContext(ctx, `DELETE FROM cnpj_lookups WHERE expires_at < ?`, lookup.FetchedAt.Unix())

	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO cnpj_lookups (cnpj, status, payload, fetched_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(cnpj) DO UPDATE SET
			status = excluded.status, payload = excluded.payload,
			fetched_at = excluded.fetched_at, expires_at = excluded.expires_at`,
		lookup.CNPJ, lookup.Status, payload, lookup.FetchedAt.Unix(), lookup.ExpiresAt.Unix())

	return err
}

func (repo *repo) SetPlaceCNPJ(ctx context.Context, jobID string, position int, number, source string, now time.Time) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO place_cnpj (job_id, position, cnpj, source, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(job_id, position) DO UPDATE SET
			cnpj = excluded.cnpj, source = excluded.source, created_at = excluded.created_at`,
		jobID, position, number, source, now.Unix())

	return err
}

func (repo *repo) ClearPlaceCNPJ(ctx context.Context, jobID string, position int) error {
	_, err := repo.db.ExecContext(ctx, `DELETE FROM place_cnpj WHERE job_id = ? AND position = ?`, jobID, position)

	return err
}

func (repo *repo) PlaceCNPJs(ctx context.Context, jobID string) (map[int]string, error) {
	rows, err := repo.db.QueryContext(ctx, `SELECT position, cnpj FROM place_cnpj WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}

	defer rows.Close()

	out := make(map[int]string)

	for rows.Next() {
		var (
			position int
			number   string
		)

		if err := rows.Scan(&position, &number); err != nil {
			return nil, err
		}

		out[position] = number
	}

	return out, rows.Err()
}
