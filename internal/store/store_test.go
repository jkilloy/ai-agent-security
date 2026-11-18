package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"agent-security/internal/call"

	"github.com/google/uuid"
)

// Runs against a real Postgres when TEST_DATABASE_URL is set, e.g.
// TEST_DATABASE_URL=postgres://callsec:callsec@localhost:5432/callsec_test?sslmode=disable
func TestPostgresListAndStats(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.pool.Exec(ctx, "TRUNCATE calls"); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	mk := func(age time.Duration, vars map[string]any, results map[string]string, expired bool) string {
		c := &call.Call{ID: uuid.NewString(), Variables: vars, Results: map[string]json.RawMessage{}, StartedAt: now.Add(-age),
			EndedAt: now.Add(-age + time.Second), ExpiresAt: now.Add(time.Hour)}
		if expired {
			c.ExpiresAt = now.Add(-time.Second)
		}
		for k, v := range results {
			c.Results[k] = json.RawMessage(v)
		}
		if err := db.Save(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	wm := mk(1*time.Minute, map[string]any{"caller": "+15550001"}, map[string]string{"aivoice": `{"verdict":"watermark_detected"}`}, false)
	syn := mk(2*time.Minute, map[string]any{"caller": "+15550002"}, map[string]string{"aivoice": `{"verdict":"likely_synthetic"}`}, false)
	clean := mk(3*time.Minute, map[string]any{"caller": "100%_real"}, map[string]string{"aivoice": `{"verdict":"no_evidence"}`, "x": `{"error":"boom"}`}, false)
	old := mk(48*time.Hour, map[string]any{}, map[string]string{}, false)
	mk(time.Minute, map[string]any{"caller": "+15550001"}, nil, true) // expired: never listed

	list := func(q ListQuery) []string {
		cs, err := db.List(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, c := range cs {
			ids = append(ids, c.ID)
		}
		return ids
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v want %v", name, got, want)
			}
		}
	}
	eq("all newest first", list(ListQuery{}), []string{wm, syn, clean, old})
	eq("flagged", list(ListQuery{Verdict: FilterFlagged}), []string{wm, syn})
	eq("errors", list(ListQuery{Verdict: FilterErrors}), []string{clean})
	eq("verdict", list(ListQuery{Verdict: "no_evidence"}), []string{clean})
	eq("search var", list(ListQuery{Search: "550002"}), []string{syn})
	eq("search escapes wildcards", list(ListQuery{Search: "100%_"}), []string{clean})
	eq("search id prefix", list(ListQuery{Search: wm[:8]}), []string{wm})
	first := list(ListQuery{Limit: 2})
	eq("page 1", first, []string{wm, syn})
	c2, _ := db.Get(ctx, syn)
	eq("page 2", list(ListQuery{Limit: 2, BeforeTime: c2.StartedAt, BeforeID: c2.ID}), []string{clean, old})

	s, err := db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 4 || s.Last24h != 3 || s.Flagged != 2 || s.Errors != 1 || s.Verdicts["watermark_detected"] != 1 {
		t.Fatalf("stats = %+v", s)
	}
}
