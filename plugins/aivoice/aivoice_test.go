package aivoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-security/internal/audio"
	"agent-security/internal/plugin"

	"github.com/coder/websocket"
)

// fakeService implements the detector service protocol and reports how many
// audio bytes it received.
func fakeService(t *testing.T, reject string) (*httptest.Server, chan startMsg) {
	starts := make(chan startMsg, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var s startMsg
		_ = json.Unmarshal(data, &s)
		starts <- s
		if reject != "" {
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","error":"`+reject+`"}`))
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ready","release":"base"}`))
		n := 0
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				n += len(data)
				continue
			}
			break // end message
		}
		rep, _ := json.Marshal(map[string]any{"type": "report", "report": map[string]any{"verdict": "no_evidence", "bytes": n}})
		_ = c.Write(ctx, websocket.MessageText, rep)
	}))
	t.Cleanup(srv.Close)
	return srv, starts
}

func wsURL(s *httptest.Server) string { return "ws" + strings.TrimPrefix(s.URL, "http") }

func run(_ *testing.T, p *Plugin, vars map[string]any, frames ...string) (json.RawMessage, error) {
	b := audio.NewBroadcaster(nil)
	in := &plugin.Input{CallID: "call-1", Variables: vars, Audio: b.Subscribe()}
	for _, f := range frames {
		_ = b.Publish([]byte(f))
	}
	b.Close()
	return p.Analyze(context.Background(), in)
}

func TestStreamsAudioAndReturnsReport(t *testing.T) {
	srv, starts := fakeService(t, "")
	p := &Plugin{URL: wsURL(srv)}
	out, err := run(t, p, map[string]any{"encoding": "mulaw", "sample_rate": float64(8000), "channels": float64(2),
		"channel_labels": []any{"caller", "agent"}, "secret": "not forwarded"}, "abcd", "efgh")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"bytes":8,"verdict":"no_evidence"}` {
		t.Fatalf("report = %s", out)
	}
	s := <-starts
	if s.CallID != "call-1" || s.Encoding != "mulaw" || s.SampleRate != 8000 || s.Channels != 2 || len(s.ChannelLabels) != 2 {
		t.Fatalf("start = %+v", s)
	}
}

func TestDefaultsAndMissingFormat(t *testing.T) {
	srv, starts := fakeService(t, "")
	if _, err := run(t, &Plugin{URL: wsURL(srv)}, map[string]any{}); err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Fatalf("expected missing-format error, got %v", err)
	}
	p := &Plugin{URL: wsURL(srv), DefaultEncoding: "pcm16", DefaultSampleRate: 16000}
	if _, err := run(t, p, map[string]any{}, "xx"); err != nil {
		t.Fatal(err)
	}
	if s := <-starts; s.Encoding != "pcm16" || s.SampleRate != 16000 || s.Channels != 1 {
		t.Fatalf("start = %+v", s)
	}
}

func TestServiceRejectsOrIsDown(t *testing.T) {
	srv, _ := fakeService(t, "unsupported encoding")
	_, err := run(t, &Plugin{URL: wsURL(srv)}, map[string]any{"encoding": "opus", "sample_rate": float64(8000)})
	if err == nil || !strings.Contains(err.Error(), "unsupported encoding") {
		t.Fatalf("got %v", err)
	}
	_, err = run(t, &Plugin{URL: "ws://127.0.0.1:1/v1/stream"}, map[string]any{"encoding": "pcm16", "sample_rate": float64(8000)})
	if err == nil || !strings.Contains(err.Error(), "connect") {
		t.Fatalf("got %v", err)
	}
}
