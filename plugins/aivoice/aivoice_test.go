package aivoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// The detector service (uvicorn) pings every 20 s and closes the connection if
// no pong arrives. The client only answers pings while it is reading, so it
// must read for the whole call, not just after sending "end".
func TestAnswersPingsDuringLongCalls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ready","release":"base"}`))
		go func() { // like uvicorn: ping, and drop the connection if there's no pong
			pctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			defer cancel()
			if err := c.Ping(pctx); err != nil {
				c.Close(websocket.StatusPolicyViolation, "keepalive ping timeout")
			}
		}()
		n := 0
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageBinary {
				break
			}
			n += len(data)
		}
		rep, _ := json.Marshal(map[string]any{"type": "report", "report": map[string]any{"bytes": n}})
		_ = c.Write(ctx, websocket.MessageText, rep)
	}))
	t.Cleanup(srv.Close)

	b := audio.NewBroadcaster(nil)
	in := &plugin.Input{CallID: "c", Variables: map[string]any{"encoding": "mulaw", "sample_rate": float64(8000)}, Audio: b.Subscribe()}
	go func() { // a call that outlasts the ping timeout
		_ = b.Publish([]byte("ab"))
		time.Sleep(800 * time.Millisecond)
		_ = b.Publish([]byte("cd"))
		b.Close()
	}()
	out, err := (&Plugin{URL: wsURL(srv)}).Analyze(context.Background(), in)
	if err != nil || string(out) != `{"bytes":4}` {
		t.Fatalf("out = %s, err = %v", out, err)
	}
}

// If the service ends the session mid-call, its reason is returned, not a
// write error such as "broken pipe".
func TestServiceErrorMidCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		_, _, _ = c.Read(ctx)
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ready","release":"base"}`))
		_, _, _ = c.Read(ctx) // first audio frame
		_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"error","error":"RuntimeError: worker died"}`))
		c.Close(websocket.StatusInternalError, "")
	}))
	t.Cleanup(srv.Close)

	b := audio.NewBroadcaster(nil)
	in := &plugin.Input{CallID: "c", Variables: map[string]any{"encoding": "mulaw", "sample_rate": float64(8000)}, Audio: b.Subscribe()}
	go func() {
		for range 50 {
			_ = b.Publish([]byte("abcd"))
			time.Sleep(10 * time.Millisecond)
		}
		b.Close()
	}()
	_, err := (&Plugin{URL: wsURL(srv)}).Analyze(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "worker died") {
		t.Fatalf("err = %v", err)
	}
}
