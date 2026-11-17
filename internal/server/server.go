// Package server exposes the WebSocket ingestion endpoint and the read API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"agent-security/internal/config"
	"agent-security/internal/engine"
	"agent-security/internal/store"

	"github.com/google/uuid"
)

type Server struct {
	cfg    config.Config
	engine *engine.Engine
	store  store.Store
	log    *slog.Logger

	// stop is cancelled at shutdown to end in-flight calls; calls tracks them.
	stop    context.Context
	stopNow context.CancelFunc
	calls   sync.WaitGroup
}

func New(cfg config.Config, e *engine.Engine, st store.Store, log *slog.Logger) *Server {
	stop, cancel := context.WithCancel(context.Background())
	return &Server{cfg: cfg, engine: e, store: st, log: log, stop: stop, stopNow: cancel}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws/call", s.handleCall)
	mux.HandleFunc("GET /calls/{id}", s.handleGetCall)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

// Shutdown ends all in-flight calls as if the callers hung up, then waits for
// their plugins to finish and their results to be saved, or for ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	s.stopNow()
	done := make(chan struct{})
	go func() { s.calls.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) handleGetCall(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid call id"})
		return
	}
	c, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		s.log.Error("get call", "call_id", id, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type pinger interface{ Ping(context.Context) error }

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.store.(pinger); ok {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := p.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
