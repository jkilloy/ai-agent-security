// Package engine runs every registered plugin against a call's live audio and
// collects their results.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"agent-security/internal/audio"
	"agent-security/internal/plugin"
)

type Engine struct {
	plugins []plugin.Plugin
	timeout time.Duration
	log     *slog.Logger
}

// New creates an engine. timeout is how long plugins may keep running after
// the call's audio ends.
func New(plugins []plugin.Plugin, timeout time.Duration, log *slog.Logger) *Engine {
	return &Engine{plugins: plugins, timeout: timeout, log: log}
}

type result struct {
	name string
	data json.RawMessage
}

// Run is the set of plugins working on one call.
type Run struct {
	e       *Engine
	cancel  context.CancelFunc
	results chan result
	pending map[string]bool
}

// Start subscribes every plugin to b and starts them. It must be called before
// any audio is published so plugins see the whole call.
func (e *Engine) Start(ctx context.Context, callID string, vars map[string]any, b *audio.Broadcaster) *Run {
	ctx, cancel := context.WithCancel(ctx)
	r := &Run{
		e:       e,
		cancel:  cancel,
		results: make(chan result, len(e.plugins)),
		pending: make(map[string]bool, len(e.plugins)),
	}
	for _, p := range e.plugins {
		in := &plugin.Input{CallID: callID, Variables: deepCopy(vars), Audio: b.Subscribe()}
		r.pending[p.Name()] = true
		go r.runOne(ctx, p, in)
	}
	return r
}

func (r *Run) runOne(ctx context.Context, p plugin.Plugin, in *plugin.Input) {
	name := p.Name()
	var out json.RawMessage
	start := time.Now()
	defer func() {
		in.Audio.Release()
		if rec := recover(); rec != nil {
			r.e.log.Error("plugin panicked", "plugin", name, "call_id", in.CallID, "panic", rec, "stack", string(debug.Stack()))
			out = errorJSON(fmt.Sprintf("panic: %v", rec))
		}
		r.e.log.Debug("plugin finished", "plugin", name, "call_id", in.CallID, "elapsed", time.Since(start))
		r.results <- result{name, out}
	}()

	data, err := p.Analyze(ctx, in)
	switch {
	case err != nil:
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("timed out: %w", err)
		}
		r.e.log.Warn("plugin failed", "plugin", name, "call_id", in.CallID, "err", err)
		out = errorJSON(err.Error())
	case data == nil:
		out = json.RawMessage("null")
	case !json.Valid(data):
		out = errorJSON("plugin returned invalid JSON")
	default:
		out = data
	}
}

// Wait is called once the call's audio has ended. It gives plugins up to the
// engine timeout to finish, then cancels them. Plugins that still haven't
// returned shortly after cancellation are recorded as timed out and abandoned.
func (r *Run) Wait() map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(r.pending))
	deadline := time.NewTimer(r.e.timeout)
	defer deadline.Stop()
	var grace <-chan time.Time

	for len(r.pending) > 0 {
		select {
		case res := <-r.results:
			out[res.name] = res.data
			delete(r.pending, res.name)
		case <-deadline.C:
			r.cancel()
			grace = time.After(2 * time.Second)
		case <-grace:
			for name := range r.pending {
				r.e.log.Error("plugin ignored cancellation; abandoning", "plugin", name)
				out[name] = errorJSON("timed out")
			}
			r.pending = nil
		}
	}
	r.cancel()
	return out
}

func errorJSON(msg string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return b
}

// deepCopy copies a decoded-JSON value tree.
func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return deepCopy(t)
	case []any:
		s := make([]any, len(t))
		for i, e := range t {
			s[i] = copyValue(e)
		}
		return s
	default:
		return v
	}
}
