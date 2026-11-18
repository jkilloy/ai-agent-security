package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-security/internal/call"

	"github.com/jackc/pgx/v5"
)

// Verdict filters for List. "flagged" means any AI-voice evidence; "errors"
// means at least one plugin returned {"error": ...}.
const (
	FilterFlagged = "flagged"
	FilterErrors  = "errors"
)

// ListQuery selects a page of calls, newest first.
type ListQuery struct {
	Limit int
	// Cursor: return calls that started before (BeforeTime, BeforeID).
	BeforeTime time.Time
	BeforeID   string
	// Search matches a call ID prefix or any text in the variables.
	Search string
	// Verdict is an aivoice verdict, FilterFlagged or FilterErrors.
	Verdict string
}

// Stats summarizes stored (unexpired) calls.
type Stats struct {
	Total    int64            `json:"total"`
	Last24h  int64            `json:"last_24h"`
	Flagged  int64            `json:"flagged"`
	Errors   int64            `json:"errors"`
	Verdicts map[string]int64 `json:"verdicts"`
}

const flaggedSQL = `results->'aivoice'->>'verdict' IN ('watermark_detected','likely_synthetic')`
const errorsSQL = `EXISTS (SELECT 1 FROM jsonb_each(results) e WHERE jsonb_typeof(e.value) = 'object' AND e.value ? 'error')`

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func (p *Postgres) List(ctx context.Context, q ListQuery) ([]call.Call, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	where := []string{"expires_at > now()"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if !q.BeforeTime.IsZero() {
		where = append(where, fmt.Sprintf("(started_at, id::text) < (%s, %s)", arg(q.BeforeTime), arg(q.BeforeID)))
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		e := escapeLike(s)
		where = append(where, fmt.Sprintf("(id::text LIKE %s OR variables::text ILIKE %s)", arg(e+"%"), arg("%"+e+"%")))
	}
	switch q.Verdict {
	case "":
	case FilterFlagged:
		where = append(where, flaggedSQL)
	case FilterErrors:
		where = append(where, errorsSQL)
	default:
		where = append(where, "results->'aivoice'->>'verdict' = "+arg(q.Verdict))
	}
	sql := `SELECT id::text, variables, results, audio_path, audio_bytes, started_at, ended_at, expires_at
		FROM calls WHERE ` + strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY started_at DESC, id::text DESC LIMIT %s", arg(q.Limit))
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (call.Call, error) {
		var c call.Call
		err := r.Scan(&c.ID, &c.Variables, &c.Results, &c.AudioPath, &c.AudioBytes, &c.StartedAt, &c.EndedAt, &c.ExpiresAt)
		return c, err
	})
}

func (p *Postgres) Stats(ctx context.Context) (Stats, error) {
	s := Stats{Verdicts: map[string]int64{}}
	err := p.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE started_at > now() - interval '24 hours'),
		       count(*) FILTER (WHERE `+flaggedSQL+`),
		       count(*) FILTER (WHERE `+errorsSQL+`)
		FROM calls WHERE expires_at > now()`).Scan(&s.Total, &s.Last24h, &s.Flagged, &s.Errors)
	if err != nil {
		return s, err
	}
	rows, err := p.pool.Query(ctx, `
		SELECT results->'aivoice'->>'verdict', count(*) FROM calls
		WHERE expires_at > now() AND results->'aivoice'->>'verdict' IS NOT NULL GROUP BY 1`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		var n int64
		if err := rows.Scan(&v, &n); err != nil {
			return s, err
		}
		s.Verdicts[v] = n
	}
	return s, rows.Err()
}
