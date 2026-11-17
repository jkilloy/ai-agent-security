// Package store persists finished calls in Postgres.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"agent-security/internal/call"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned by Get when no call has the given ID.
var ErrNotFound = errors.New("store: call not found")

// Store is the persistence interface the server and cleanup task depend on.
type Store interface {
	Save(ctx context.Context, c *call.Call) error
	Get(ctx context.Context, id string) (*call.Call, error)
	// Expired returns up to limit calls whose expires_at is before now.
	Expired(ctx context.Context, now time.Time, limit int) ([]Expired, error)
	Delete(ctx context.Context, id string) error
}

// Expired identifies a call due for deletion.
type Expired struct {
	ID        string
	AudioPath string
}

//go:embed migrations/*.sql
var migrations embed.FS

type Postgres struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and applies migrations.
func Open(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	// Wait briefly for the database to come up (e.g. under docker compose).
	var pingErr error
	for range 30 {
		if pingErr = pool.Ping(ctx); pingErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if pingErr != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", pingErr)
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, n := range names {
		sql, err := migrations.ReadFile(n)
		if err != nil {
			return err
		}
		if _, err := p.pool.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", n, err)
		}
	}
	return nil
}

func (p *Postgres) Save(ctx context.Context, c *call.Call) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO calls (id, variables, results, audio_path, audio_bytes, started_at, ended_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		c.ID, c.Variables, c.Results, c.AudioPath, c.AudioBytes, c.StartedAt, c.EndedAt, c.ExpiresAt)
	return err
}

func (p *Postgres) Get(ctx context.Context, id string) (*call.Call, error) {
	c := &call.Call{}
	err := p.pool.QueryRow(ctx, `
		SELECT id::text, variables, results, audio_path, audio_bytes, started_at, ended_at, expires_at
		FROM calls WHERE id = $1 AND expires_at > now()`, id).
		Scan(&c.ID, &c.Variables, &c.Results, &c.AudioPath, &c.AudioBytes, &c.StartedAt, &c.EndedAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (p *Postgres) Expired(ctx context.Context, now time.Time, limit int) ([]Expired, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT id::text, audio_path FROM calls
		WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Expired, error) {
		var e Expired
		err := r.Scan(&e.ID, &e.AudioPath)
		return e, err
	})
}

func (p *Postgres) Delete(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM calls WHERE id = $1`, id)
	return err
}
