package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/gosom/google-maps-scraper/web"
)

// The sqlite repo also keeps the Koria Hub login state (web.HubStore), so used
// ticket ids and sessions survive restarts.
var _ web.HubStore = (*repo)(nil)

func (repo *repo) ConsumeTicket(ctx context.Context, jti string, now, purgeAt time.Time) (bool, error) {
	if _, err := repo.db.ExecContext(ctx, `DELETE FROM hub_tickets WHERE purge_at < ?`, now.Unix()); err != nil {
		return false, err
	}

	// The primary key makes the insert itself the replay detector: two
	// simultaneous uses of the same ticket cannot both succeed.
	res, err := repo.db.ExecContext(ctx,
		`INSERT INTO hub_tickets (jti, purge_at) VALUES (?, ?) ON CONFLICT(jti) DO NOTHING`,
		jti, purgeAt.Unix())
	if err != nil {
		return false, err
	}

	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	return affected == 1, nil
}

func (repo *repo) CreateSession(ctx context.Context, rec web.HubSessionRecord) error {
	_, err := repo.db.ExecContext(ctx, `
		INSERT INTO hub_sessions (id, secret_hash, username, name, email, created_at, last_seen, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.ID, rec.SecretHash, rec.Username, rec.Name, rec.Email,
		rec.CreatedAt.Unix(), rec.LastSeen.Unix(), rec.ExpiresAt.Unix())

	return err
}

func (repo *repo) GetSession(ctx context.Context, id string) (web.HubSessionRecord, error) {
	var (
		rec                          web.HubSessionRecord
		created, lastSeen, expiresAt int64
	)

	err := repo.db.QueryRowContext(ctx, `
		SELECT id, secret_hash, username, name, email, created_at, last_seen, expires_at
		FROM hub_sessions WHERE id = ? AND secret_hash IS NOT NULL`, id).
		Scan(&rec.ID, &rec.SecretHash, &rec.Username, &rec.Name, &rec.Email, &created, &lastSeen, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return web.HubSessionRecord{}, web.ErrHubSessionNotFound
	}

	if err != nil {
		return web.HubSessionRecord{}, err
	}

	rec.CreatedAt = time.Unix(created, 0)
	rec.LastSeen = time.Unix(lastSeen, 0)
	rec.ExpiresAt = time.Unix(expiresAt, 0)

	return rec, nil
}

func (repo *repo) TouchSession(ctx context.Context, id string, now time.Time) error {
	_, err := repo.db.ExecContext(ctx, `UPDATE hub_sessions SET last_seen = ? WHERE id = ?`, now.Unix(), id)

	return err
}

func (repo *repo) RevokeSession(ctx context.Context, id string, now time.Time) error {
	_, err := repo.db.ExecContext(ctx,
		`UPDATE hub_sessions SET secret_hash = NULL, revoked_at = ? WHERE id = ? AND secret_hash IS NOT NULL`,
		now.Unix(), id)

	return err
}

func (repo *repo) RevokeUser(ctx context.Context, username string, now time.Time) (int, error) {
	res, err := repo.db.ExecContext(ctx,
		`UPDATE hub_sessions SET secret_hash = NULL, revoked_at = ? WHERE username = ? AND secret_hash IS NOT NULL`,
		now.Unix(), username)
	if err != nil {
		return 0, err
	}

	affected, err := res.RowsAffected()

	return int(affected), err
}

func (repo *repo) PurgeExpired(ctx context.Context, now time.Time) error {
	_, err := repo.db.ExecContext(ctx, `DELETE FROM hub_sessions WHERE expires_at < ?`, now.Unix())

	return err
}
