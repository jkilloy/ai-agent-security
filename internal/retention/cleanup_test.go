package retention

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-security/internal/call"
	"agent-security/internal/store"
)

type fakeStore struct {
	rows map[string]store.Expired
	exp  map[string]time.Time
}

func (f *fakeStore) Save(context.Context, *call.Call) error          { return nil }
func (f *fakeStore) Get(context.Context, string) (*call.Call, error) { return nil, store.ErrNotFound }
func (f *fakeStore) Delete(_ context.Context, id string) error       { delete(f.rows, id); return nil }
func (f *fakeStore) Expired(_ context.Context, now time.Time, limit int) ([]store.Expired, error) {
	var out []store.Expired
	for id, e := range f.rows {
		if !f.exp[id].After(now) && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestSweepDeletesExpiredRowsAndFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	fs := &fakeStore{rows: map[string]store.Expired{}, exp: map[string]time.Time{}}
	mk := func(id string, exp time.Time, withFile bool) {
		p := filepath.Join(dir, id+".raw")
		if withFile {
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		fs.rows[id] = store.Expired{ID: id, AudioPath: p}
		fs.exp[id] = exp
	}
	mk("old", now.Add(-time.Hour), true)
	mk("old-nofile", now.Add(-time.Minute), false)
	mk("fresh", now.Add(time.Hour), true)

	c := &Cleaner{Store: fs, Interval: time.Hour, Log: slog.New(slog.DiscardHandler), Now: func() time.Time { return now }}
	n, err := c.Sweep(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	if _, ok := fs.rows["fresh"]; !ok || len(fs.rows) != 1 {
		t.Fatalf("remaining rows = %v", fs.rows)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.raw")); !os.IsNotExist(err) {
		t.Fatalf("old audio still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.raw")); err != nil {
		t.Fatalf("fresh audio removed: %v", err)
	}
}
