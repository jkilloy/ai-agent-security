package admin

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-security/internal/call"
	"agent-security/internal/store"
)

func TestG711Tables(t *testing.T) {
	cases := []struct {
		got, want int16
	}{
		{mulawTable[0xFF], 0}, {mulawTable[0x00], -32124}, {mulawTable[0x80], 32124},
		{alawTable[0xD5], 8}, {alawTable[0x55], -8}, {alawTable[0xAA], 32256},
	}
	for i, c := range cases {
		if c.got != c.want {
			t.Errorf("case %d: got %d want %d", i, c.got, c.want)
		}
	}
}

func TestWAVReaderConvertsAndSeeks(t *testing.T) {
	// 5 stereo mu-law frames + 1 stray byte (partial frame, must be ignored).
	src := []byte{0x80, 0x00, 0xFF, 0x7F, 0x80, 0x00, 0xFF, 0x7F, 0x80, 0x00, 0x42}
	f := AudioFormat{Encoding: "mulaw", SampleRate: 8000, Channels: 2}
	r := newWAVReader(bytes.NewReader(src), int64(len(src)), f)
	all, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 44+10*2 {
		t.Fatalf("len = %d", len(all))
	}
	if string(all[0:4]) != "RIFF" || string(all[8:16]) != "WAVEfmt " || binary.LittleEndian.Uint16(all[22:]) != 2 ||
		binary.LittleEndian.Uint32(all[24:]) != 8000 || binary.LittleEndian.Uint32(all[40:]) != 20 {
		t.Fatalf("bad header % x", all[:44])
	}
	if s := int16(binary.LittleEndian.Uint16(all[44:])); s != 32124 {
		t.Fatalf("first sample = %d", s)
	}
	// Seek to an odd byte inside the data and read the rest.
	if _, err := r.Seek(47, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	if !bytes.Equal(rest, all[47:]) {
		t.Fatalf("seek read mismatch")
	}
}

func TestFormatFromVariables(t *testing.T) {
	if _, err := FormatFromVariables(map[string]any{"encoding": "opus", "sample_rate": 8000.0}); err == nil {
		t.Fatal("expected error for opus")
	}
	if _, err := FormatFromVariables(map[string]any{"encoding": "pcm16"}); err == nil {
		t.Fatal("expected error for missing rate")
	}
	f, err := FormatFromVariables(map[string]any{"encoding": "PCMU", "sample_rate": 8000.0, "channels": 2.0})
	if err != nil || f.Encoding != "mulaw" || f.Channels != 2 {
		t.Fatalf("%+v %v", f, err)
	}
}

type fakeStore struct {
	calls []call.Call
	last  store.ListQuery
}

func (f *fakeStore) Get(_ context.Context, id string) (*call.Call, error) {
	for i := range f.calls {
		if f.calls[i].ID == id {
			return &f.calls[i], nil
		}
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) List(_ context.Context, q store.ListQuery) ([]call.Call, error) {
	f.last = q
	return f.calls[:min(len(f.calls), q.Limit)], nil
}

func (f *fakeStore) Stats(context.Context) (store.Stats, error) {
	return store.Stats{Total: int64(len(f.calls))}, nil
}

const id1 = "11111111-1111-1111-1111-111111111111"
const id2 = "22222222-2222-2222-2222-222222222222"

func setup(t *testing.T, opts Options) (*httptest.Server, *fakeStore) {
	dir := t.TempDir()
	audio := filepath.Join(dir, id1+".raw")
	pcm := make([]byte, 16000) // 1 s of 8 kHz pcm16 silence
	if err := os.WriteFile(audio, pcm, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	fs := &fakeStore{calls: []call.Call{
		{ID: id1, Variables: map[string]any{"encoding": "pcm16", "sample_rate": 8000.0}, AudioPath: audio, StartedAt: now},
		{ID: id2, Variables: map[string]any{}, AudioPath: filepath.Join(dir, "missing.raw"), StartedAt: now.Add(-time.Minute)},
	}}
	mux := http.NewServeMux()
	New(fs, opts, slog.New(slog.DiscardHandler)).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, fs
}

func get(t *testing.T, url string, hdr map[string]string) *http.Response {
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestUIAndAPI(t *testing.T) {
	srv, fs := setup(t, Options{})

	r := get(t, srv.URL+"/admin/", nil)
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || !strings.Contains(string(body), "Call review") {
		t.Fatalf("ui: %d", r.StatusCode)
	}
	if !strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") {
		t.Fatal("missing CSP")
	}
	if r := get(t, srv.URL+"/admin/app.js", nil); r.StatusCode != 200 {
		t.Fatalf("app.js: %d", r.StatusCode)
	}

	// Paging: limit 1 returns a cursor that decodes back to the last call.
	r = get(t, srv.URL+"/api/calls?limit=1&q=caller&verdict=flagged", nil)
	var page struct {
		Calls []call.Call `json:"calls"`
		Next  string      `json:"next"`
	}
	_ = json.NewDecoder(r.Body).Decode(&page)
	if len(page.Calls) != 1 || page.Next == "" || fs.last.Search != "caller" || fs.last.Verdict != "flagged" {
		t.Fatalf("page = %+v last = %+v", page, fs.last)
	}
	get(t, srv.URL+"/api/calls?before="+page.Next, nil)
	if fs.last.BeforeID != id1 {
		t.Fatalf("cursor not applied: %+v", fs.last)
	}

	for path, want := range map[string]int{
		"/api/calls?verdict=bogus":     400,
		"/api/calls?before=zz!!":       400,
		"/api/calls/not-a-uuid":        400,
		"/api/calls/" + id1:            200,
		"/api/calls/" + id2 + "/audio": 422, // no encoding in variables
		"/api/stats":                   200,
	} {
		if r := get(t, srv.URL+path, nil); r.StatusCode != want {
			t.Errorf("%s = %d, want %d", path, r.StatusCode, want)
		}
	}

	// Audio as WAV, with byte ranges.
	r = get(t, srv.URL+"/api/calls/"+id1+"/audio", map[string]string{"Range": "bytes=0-3"})
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 206 || string(b) != "RIFF" || r.Header.Get("Content-Type") != "audio/wav" {
		t.Fatalf("audio range: %d %q %s", r.StatusCode, b, r.Header.Get("Content-Type"))
	}
	r = get(t, srv.URL+"/api/calls/"+id1+"/audio", nil)
	if r.ContentLength != 44+16000 {
		t.Fatalf("audio length %d", r.ContentLength)
	}
}

func TestBasicAuth(t *testing.T) {
	srv, _ := setup(t, Options{User: "admin", Password: "s3cret"})
	for _, p := range []string{"/admin/", "/api/stats", "/api/calls", "/api/calls/" + id1 + "/audio"} {
		if r := get(t, srv.URL+p, nil); r.StatusCode != 401 || r.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("%s without auth = %d", p, r.StatusCode)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/stats", nil)
	req.SetBasicAuth("admin", "wrong")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Errorf("wrong password = %d", r.StatusCode)
	}
	req.SetBasicAuth("admin", "s3cret")
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 200 {
		t.Errorf("right password = %d", r.StatusCode)
	}
}
