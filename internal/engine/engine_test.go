package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"agent-security/internal/audio"
	"agent-security/internal/plugin"
)

type fn struct {
	name string
	f    func(ctx context.Context, in *plugin.Input) (json.RawMessage, error)
}

func (p fn) Name() string { return p.name }
func (p fn) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	return p.f(ctx, in)
}

func drain(ctx context.Context, s *audio.Stream) (int, error) {
	n := 0
	for {
		f, err := s.Next(ctx)
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n += len(f)
	}
}

func TestEngineCollectsResultsAndIsolatesFailures(t *testing.T) {
	plugins := []plugin.Plugin{
		fn{"counter", func(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
			n, err := drain(ctx, in.Audio)
			if err != nil {
				return nil, err
			}
			return json.Marshal(map[string]any{"bytes": n, "caller": in.Variables["caller"]})
		}},
		fn{"mutator", func(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
			in.Variables["caller"] = "tampered"
			return json.RawMessage(`{"ok":true}`), nil
		}},
		fn{"failer", func(context.Context, *plugin.Input) (json.RawMessage, error) {
			return nil, errors.New("boom")
		}},
		fn{"panicker", func(context.Context, *plugin.Input) (json.RawMessage, error) {
			panic("kaboom")
		}},
		fn{"badjson", func(context.Context, *plugin.Input) (json.RawMessage, error) {
			return json.RawMessage(`{not json`), nil
		}},
		fn{"slow", func(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
			<-ctx.Done() // respects cancellation, but never finishes on its own
			return nil, ctx.Err()
		}},
		fn{"stuck", func(context.Context, *plugin.Input) (json.RawMessage, error) {
			select {} // ignores cancellation entirely
		}},
	}
	e := New(plugins, 100*time.Millisecond, slog.New(slog.DiscardHandler))
	b := audio.NewBroadcaster(nil)
	run := e.Start(context.Background(), "call-1", map[string]any{"caller": "+1555"}, b)
	_ = b.Publish(make([]byte, 100))
	_ = b.Publish(make([]byte, 60))
	b.Close()

	start := time.Now()
	res := run.Wait()
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("Wait took %v", el)
	}

	want := map[string]string{
		"counter":  `{"bytes":160,"caller":"+1555"}`,
		"mutator":  `{"ok":true}`,
		"failer":   `{"error":"boom"}`,
		"panicker": `{"error":"panic: kaboom"}`,
		"badjson":  `{"error":"plugin returned invalid JSON"}`,
		"stuck":    `{"error":"timed out"}`,
	}
	for name, w := range want {
		if got := string(res[name]); got != w {
			t.Errorf("%s = %s, want %s", name, got, w)
		}
	}
	if got := string(res["slow"]); !strings.Contains(got, "timed out") {
		t.Errorf("slow = %s", got)
	}
	if len(res) != len(plugins) {
		t.Errorf("got %d results", len(res))
	}
}
