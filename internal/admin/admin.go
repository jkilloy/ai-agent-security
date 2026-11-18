// Package admin serves the call review UI and the read-only JSON API it uses.
//
//	GET /admin/                  the UI (static, embedded)
//	GET /api/stats               counts of stored calls
//	GET /api/calls               newest first; ?limit, ?before=<cursor>, ?q=<search>, ?verdict=
//	GET /api/calls/{id}          one call with all plugin results
//	GET /api/calls/{id}/audio    the call audio as 16-bit PCM WAV (supports Range)
package admin

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"agent-security/internal/call"
	"agent-security/internal/store"

	"github.com/google/uuid"
)

//go:embed static
var staticFS embed.FS

// Store is what the admin UI reads.
type Store interface {
	Get(ctx context.Context, id string) (*call.Call, error)
	List(ctx context.Context, q store.ListQuery) ([]call.Call, error)
	Stats(ctx context.Context) (store.Stats, error)
}

type Options struct {
	// If Password is set, every admin route requires HTTP basic auth.
	User     string
	Password string
}

type Handler struct {
	store Store
	opts  Options
	log   *slog.Logger
}

func New(st Store, opts Options, log *slog.Logger) *Handler {
	if opts.User == "" {
		opts.User = "admin"
	}
	return &Handler{store: st, opts: opts, log: log}
}

// Register adds the admin routes to mux.
func (h *Handler) Register(mux *http.ServeMux) {
	static, _ := fs.Sub(staticFS, "static")
	files := http.StripPrefix("/admin/", http.FileServerFS(static))
	mux.Handle("GET /admin/", h.wrap(files))
	mux.Handle("GET /admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	mux.Handle("GET /api/stats", h.wrap(http.HandlerFunc(h.stats)))
	mux.Handle("GET /api/calls", h.wrap(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/calls/{id}", h.wrap(http.HandlerFunc(h.get)))
	mux.Handle("GET /api/calls/{id}/audio", h.wrap(http.HandlerFunc(h.audio)))
}

// wrap adds security headers and, when configured, basic auth.
func (h *Handler) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cache-Control", "no-store")
		if h.opts.Password != "" {
			u, p, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(h.opts.User)) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(h.opts.Password)) != 1 {
				hd.Set("WWW-Authenticate", `Basic realm="call review", charset="UTF-8"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	s, err := h.store.Stats(r.Context())
	if err != nil {
		h.fail(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

type cursor struct {
	T  time.Time `json:"t"`
	ID string    `json:"id"`
}

func encodeCursor(c call.Call) string {
	b, _ := json.Marshal(cursor{c.StartedAt, c.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, error) {
	var c cursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}

var verdicts = map[string]bool{
	"": true, store.FilterFlagged: true, store.FilterErrors: true,
	"watermark_detected": true, "likely_synthetic": true, "no_evidence": true, "insufficient_audio": true,
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := store.ListQuery{Search: q.Get("q"), Verdict: q.Get("verdict")}
	if len(lq.Search) > 200 {
		lq.Search = lq.Search[:200]
	}
	if !verdicts[lq.Verdict] {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unknown verdict filter"})
		return
	}
	lq.Limit, _ = strconv.Atoi(q.Get("limit"))
	if lq.Limit <= 0 || lq.Limit > 200 {
		lq.Limit = 50
	}
	if b := q.Get("before"); b != "" {
		c, err := decodeCursor(b)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		lq.BeforeTime, lq.BeforeID = c.T, c.ID
	}
	calls, err := h.store.List(r.Context(), lq)
	if err != nil {
		h.fail(w, "list calls", err)
		return
	}
	resp := map[string]any{"calls": calls, "next": nil}
	if len(calls) == lq.Limit {
		resp["next"] = encodeCursor(calls[len(calls)-1])
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handler) load(w http.ResponseWriter, r *http.Request) (*call.Call, bool) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid call id"})
		return nil, false
	}
	c, err := h.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "call not found or expired"})
		return nil, false
	}
	if err != nil {
		h.fail(w, "get call", err)
		return nil, false
	}
	return c, true
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.load(w, r); ok {
		writeJSON(w, http.StatusOK, c)
	}
}

func (h *Handler) audio(w http.ResponseWriter, r *http.Request) {
	c, ok := h.load(w, r)
	if !ok {
		return
	}
	f, err := FormatFromVariables(c.Variables)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "cannot play this call: " + err.Error()})
		return
	}
	file, err := os.Open(c.AudioPath)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "audio file not available"})
		return
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		h.fail(w, "stat audio", err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	http.ServeContent(w, r, "", st.ModTime(), newWAVReader(file, st.Size(), f))
}

func (h *Handler) fail(w http.ResponseWriter, what string, err error) {
	h.log.Error("admin: "+what, "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
