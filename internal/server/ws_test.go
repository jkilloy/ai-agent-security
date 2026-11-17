package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-security/internal/call"
	"agent-security/internal/config"
	"agent-security/internal/engine"
	"agent-security/internal/plugin"
	"agent-security/internal/store"

	"github.com/coder/websocket"
)

type memStore struct {
	mu    sync.Mutex
	calls map[string]*call.Call
	saved chan string
}

func newMemStore() *memStore {
	return &memStore{calls: map[string]*call.Call{}, saved: make(chan string, 10)}
}

func (m *memStore) Save(_ context.Context, c *call.Call) error {
	m.mu.Lock()
	m.calls[c.ID] = c
	m.mu.Unlock()
	m.saved <- c.ID
	return nil
}

func (m *memStore) Get(_ context.Context, id string) (*call.Call, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.calls[id]; ok {
		return c, nil
	}
	return nil, store.ErrNotFound
}

func (m *memStore) Expired(context.Context, time.Time, int) ([]store.Expired, error) { return nil, nil }
func (m *memStore) Delete(context.Context, string) error                             { return nil }

type byteCounter struct{}

func (byteCounter) Name() string { return "bytes" }
func (byteCounter) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	b, err := io.ReadAll(in.Audio)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"n": len(b), "vars": in.Variables})
}

func setup(t *testing.T) (*httptest.Server, *memStore, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{
		AudioDir:            dir,
		Retention:           time.Hour,
		PluginTimeout:       time.Second,
		FirstMessageTimeout: 200 * time.Millisecond,
		MaxCallDuration:     time.Minute,
		MaxFrameBytes:       1 << 20,
	}
	log := slog.New(slog.DiscardHandler)
	st := newMemStore()
	srv := New(cfg, engine.New([]plugin.Plugin{byteCounter{}}, cfg.PluginTimeout, log), st, log)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, st, dir
}

func dial(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws/call"
	c, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func readMsg(t *testing.T, c *websocket.Conn) serverMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m serverMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFullCall(t *testing.T) {
	ts, st, dir := setup(t)
	ctx := context.Background()
	c := dial(t, ts)

	_ = c.Write(ctx, websocket.MessageText, []byte(`{"caller":"+15551234567","sample_rate":8000}`))
	acc := readMsg(t, c)
	if acc.Type != "accepted" || acc.CallID == "" {
		t.Fatalf("accept = %+v", acc)
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("hello "))
	_ = c.Write(ctx, websocket.MessageBinary, []byte("world"))
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`))

	done := readMsg(t, c)
	if done.Type != "complete" || done.CallID != acc.CallID {
		t.Fatalf("complete = %+v", done)
	}

	resp, err := http.Get(ts.URL + "/calls/" + acc.CallID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got call.Call
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Variables["caller"] != "+15551234567" || got.AudioBytes != 11 {
		t.Fatalf("call = %+v", got)
	}
	if r := string(got.Results["bytes"]); r != `{"n":11,"vars":{"caller":"+15551234567","sample_rate":8000}}` {
		t.Fatalf("results = %s", r)
	}
	if !got.ExpiresAt.After(got.EndedAt) {
		t.Fatalf("expires %v ended %v", got.ExpiresAt, got.EndedAt)
	}
	audio, err := os.ReadFile(filepath.Join(dir, acc.CallID+".raw"))
	if err != nil || string(audio) != "hello world" {
		t.Fatalf("audio file = %q, %v", audio, err)
	}
	_ = st
}

func TestHangupStillSaves(t *testing.T) {
	ts, st, _ := setup(t)
	ctx := context.Background()
	c := dial(t, ts)
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"caller":"x"}`))
	acc := readMsg(t, c)
	_ = c.Write(ctx, websocket.MessageBinary, []byte("abc"))
	c.CloseNow() // abrupt hangup, no end message

	select {
	case id := <-st.saved:
		if id != acc.CallID {
			t.Fatalf("saved %s, want %s", id, acc.CallID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call not saved after hangup")
	}
	got, _ := st.Get(ctx, acc.CallID)
	if got.AudioBytes != 3 || string(got.Results["bytes"]) != `{"n":3,"vars":{"caller":"x"}}` {
		t.Fatalf("call = %+v results=%s", got, got.Results["bytes"])
	}
}

func TestRejectsBadFirstMessage(t *testing.T) {
	ts, st, _ := setup(t)
	cases := map[string]func(*websocket.Conn){
		"array":  func(c *websocket.Conn) { _ = c.Write(context.Background(), websocket.MessageText, []byte(`[1,2]`)) },
		"null":   func(c *websocket.Conn) { _ = c.Write(context.Background(), websocket.MessageText, []byte(`null`)) },
		"binary": func(c *websocket.Conn) { _ = c.Write(context.Background(), websocket.MessageBinary, []byte(`{}`)) },
		"silent": func(*websocket.Conn) {}, // first-message timeout
	}
	for name, send := range cases {
		t.Run(name, func(t *testing.T) {
			c := dial(t, ts)
			send(c)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := c.Read(ctx)
			if s := websocket.CloseStatus(err); s != websocket.StatusPolicyViolation {
				t.Fatalf("close status = %v (err %v)", s, err)
			}
		})
	}
	select {
	case id := <-st.saved:
		t.Fatalf("rejected call was saved: %s", id)
	default:
	}
}

func TestGetCallValidation(t *testing.T) {
	ts, _, _ := setup(t)
	for path, want := range map[string]int{
		"/calls/not-a-uuid":                           http.StatusBadRequest,
		"/calls/00000000-0000-0000-0000-000000000000": http.StatusNotFound,
		"/healthz": http.StatusOK,
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, resp.StatusCode, want)
		}
	}
}
