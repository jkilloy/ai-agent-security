package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"agent-security/internal/audio"
	"agent-security/internal/call"

	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// Messages sent to the client.
type serverMsg struct {
	Type   string `json:"type"`
	CallID string `json:"call_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

// handleCall implements the call protocol:
//
//  1. client → text: JSON object of variables
//  2. server → text: {"type":"accepted","call_id":...}
//  3. client → binary frames: audio
//  4. client → text {"type":"end"}, or the client disconnects
//  5. server → text: {"type":"complete","call_id":...} (if still connected)
func (s *Server) handleCall(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.log.Warn("websocket accept failed", "err", err)
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(s.cfg.MaxFrameBytes)

	// The call's lifetime: ends on hangup, max duration, or server shutdown.
	callCtx, cancelCall := context.WithTimeout(r.Context(), s.cfg.MaxCallDuration)
	defer cancelCall()
	stopAfter := context.AfterFunc(s.stop, cancelCall)
	defer stopAfter()

	vars, err := s.readVariables(callCtx, conn)
	if err != nil {
		s.log.Info("rejected call", "remote", r.RemoteAddr, "err", err)
		conn.Close(websocket.StatusPolicyViolation, truncate(err.Error(), 120))
		return
	}

	s.calls.Add(1)
	defer s.calls.Done()

	c := &call.Call{
		ID:        uuid.NewString(),
		Variables: vars,
		StartedAt: time.Now().UTC(),
	}
	log := s.log.With("call_id", c.ID)

	c.AudioPath = filepath.Join(s.cfg.AudioDir, c.ID+".raw")
	f, err := os.OpenFile(c.AudioPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		log.Error("create audio file", "err", err)
		conn.Close(websocket.StatusInternalError, "storage unavailable")
		return
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	b := audio.NewBroadcaster(bw)

	// Subscribe plugins before acknowledging, so they see every frame.
	// Plugins run on a context detached from the request so a hangup doesn't
	// cancel their analysis; the engine enforces its own timeout.
	run := s.engine.Start(context.WithoutCancel(callCtx), c.ID, vars, b)

	accepted := true
	if err := writeMsg(callCtx, conn, serverMsg{Type: "accepted", CallID: c.ID}); err != nil {
		log.Info("client gone before accept", "err", err)
		accepted = false
	}
	log.Info("call started", "variables", len(vars))

	reason := "ended"
	if accepted {
		reason = s.readAudio(callCtx, conn, b)
	}

	// Call over: close the stream, finish the audio file, collect results.
	b.Close()
	if err := bw.Flush(); err != nil {
		log.Error("flush audio file", "err", err)
	}
	if err := f.Close(); err != nil {
		log.Error("close audio file", "err", err)
	}
	_, c.AudioBytes = b.Stats()
	c.EndedAt = time.Now().UTC()
	c.ExpiresAt = c.EndedAt.Add(s.cfg.Retention)
	log.Info("call ended", "reason", reason, "audio_bytes", c.AudioBytes, "duration", c.EndedAt.Sub(c.StartedAt).String())

	c.Results = run.Wait()

	saveCtx, cancelSave := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelSave()
	if err := s.store.Save(saveCtx, c); err != nil {
		log.Error("save call", "err", err)
		conn.Close(websocket.StatusInternalError, "failed to save results")
		return
	}
	log.Info("call saved", "plugins", len(c.Results))

	// Best effort: the client may already have hung up.
	wctx, cancelW := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelW()
	if err := writeMsg(wctx, conn, serverMsg{Type: "complete", CallID: c.ID}); err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
	}
}

func (s *Server) readVariables(ctx context.Context, conn *websocket.Conn) (map[string]any, error) {
	// Close with a proper policy-violation frame on timeout. (Cancelling the
	// Read's context instead would drop the connection without a close code.)
	timer := time.AfterFunc(s.cfg.FirstMessageTimeout, func() {
		conn.Close(websocket.StatusPolicyViolation, "no variables message within timeout")
	})
	defer timer.Stop()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading first message: %w", err)
	}
	if typ != websocket.MessageText {
		return nil, errors.New("first message must be a JSON object text frame")
	}
	var vars map[string]any
	if err := json.Unmarshal(data, &vars); err != nil || vars == nil {
		return nil, errors.New("first message must be a JSON object")
	}
	return vars, nil
}

// readAudio pumps frames into b until the call ends and returns why it ended.
func (s *Server) readAudio(ctx context.Context, conn *websocket.Conn, b *audio.Broadcaster) string {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			switch {
			case s.stop.Err() != nil:
				return "server shutdown"
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				return "max call duration"
			case websocket.CloseStatus(err) != -1:
				return "client closed"
			default:
				return "hangup"
			}
		}
		switch typ {
		case websocket.MessageBinary:
			if err := b.Publish(data); err != nil {
				s.log.Error("publish audio", "err", err)
				return "storage error"
			}
		case websocket.MessageText:
			var m struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(data, &m) == nil && m.Type == "end" {
				return "end message"
			}
			s.log.Warn("ignoring unexpected text message", "len", len(data))
		}
	}
}

func writeMsg(ctx context.Context, conn *websocket.Conn, m serverMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
