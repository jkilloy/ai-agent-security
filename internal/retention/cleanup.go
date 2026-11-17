// Package retention periodically deletes calls (and their audio files) whose
// retention period has passed.
package retention

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"agent-security/internal/store"
)

const batchSize = 500

type Cleaner struct {
	Store    store.Store
	Interval time.Duration
	Log      *slog.Logger
	Now      func() time.Time // for tests; defaults to time.Now
}

// Run sweeps once immediately and then every Interval until ctx is done.
func (c *Cleaner) Run(ctx context.Context) {
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		if n, err := c.Sweep(ctx); err != nil && ctx.Err() == nil {
			c.Log.Error("retention sweep failed", "err", err, "deleted", n)
		} else if n > 0 {
			c.Log.Info("retention sweep", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep deletes all currently expired calls and returns how many it removed.
// The audio file is removed before the row, so a failure never leaves an
// orphaned file without a record pointing at it.
func (c *Cleaner) Sweep(ctx context.Context) (int, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	deleted := 0
	for {
		batch, err := c.Store.Expired(ctx, now(), batchSize)
		if err != nil {
			return deleted, err
		}
		if len(batch) == 0 {
			return deleted, nil
		}
		for _, e := range batch {
			if e.AudioPath != "" {
				if err := os.Remove(e.AudioPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return deleted, err
				}
			}
			if err := c.Store.Delete(ctx, e.ID); err != nil {
				return deleted, err
			}
			deleted++
		}
		if len(batch) < batchSize {
			return deleted, nil
		}
	}
}
