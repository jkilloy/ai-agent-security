// Package aivoice detects AI-generated voices in a call by streaming its audio
// to the detector service (detectors/ in this repo), which runs AI-speech
// watermark detectors and synthetic-speech classifiers, and keeps them up to
// date. The service's per-call report is returned as this plugin's result.
//
// Configuration (environment):
//
//	AIVOICE_URL               detector service stream URL, e.g. ws://detectors:8000/v1/stream.
//	                          The plugin is not registered when unset.
//	AIVOICE_DEFAULT_ENCODING  encoding to assume when the call variables have no "encoding"
//	AIVOICE_DEFAULT_SAMPLE_RATE  sample rate to assume when the variables have no "sample_rate"
//
// Call variables used: encoding, sample_rate, channels, channel_labels.
package aivoice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"agent-security/internal/plugin"

	"github.com/coder/websocket"
)

func init() {
	url := os.Getenv("AIVOICE_URL")
	if url == "" {
		return
	}
	rate, _ := strconv.Atoi(os.Getenv("AIVOICE_DEFAULT_SAMPLE_RATE"))
	plugin.Register(&Plugin{
		URL:               url,
		DefaultEncoding:   os.Getenv("AIVOICE_DEFAULT_ENCODING"),
		DefaultSampleRate: rate,
	})
}

type Plugin struct {
	URL               string
	DefaultEncoding   string
	DefaultSampleRate int
	DialTimeout       time.Duration
}

func (*Plugin) Name() string { return "aivoice" }

type startMsg struct {
	CallID        string `json:"call_id"`
	Encoding      string `json:"encoding"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	ChannelLabels []any  `json:"channel_labels,omitempty"`
}

type serviceMsg struct {
	Type    string          `json:"type"`
	Error   string          `json:"error"`
	Release string          `json:"release"`
	Report  json.RawMessage `json:"report"`
}

func (p *Plugin) start(in *plugin.Input) (startMsg, error) {
	m := startMsg{CallID: in.CallID, Encoding: p.DefaultEncoding, SampleRate: p.DefaultSampleRate, Channels: 1}
	if v, ok := in.Variables["encoding"].(string); ok && v != "" {
		m.Encoding = v
	}
	if v, ok := in.Variables["sample_rate"].(float64); ok {
		m.SampleRate = int(v)
	}
	if v, ok := in.Variables["channels"].(float64); ok {
		m.Channels = int(v)
	}
	if v, ok := in.Variables["channel_labels"].([]any); ok {
		m.ChannelLabels = v
	}
	if m.Encoding == "" || m.SampleRate == 0 {
		return m, errors.New(`call variables must include "encoding" and "sample_rate" (or set AIVOICE_DEFAULT_ENCODING / AIVOICE_DEFAULT_SAMPLE_RATE)`)
	}
	return m, nil
}

func (p *Plugin) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	start, err := p.start(in)
	if err != nil {
		return nil, err
	}

	dialTimeout := p.DialTimeout
	if dialTimeout == 0 {
		dialTimeout = 10 * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, _, err := websocket.Dial(dctx, p.URL, nil)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("connect to detector service: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	b, _ := json.Marshal(start)
	if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, fmt.Errorf("send start: %w", err)
	}
	ready, err := readMsg(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("detector service: %w", err)
	}
	if ready.Type != "ready" {
		return nil, fmt.Errorf("detector service rejected call: %s", ready.Error)
	}

	for {
		frame, err := in.Audio.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
			return nil, fmt.Errorf("stream audio: %w", err)
		}
	}

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`)); err != nil {
		return nil, fmt.Errorf("send end: %w", err)
	}
	res, err := readMsg(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("waiting for report: %w", err)
	}
	if res.Type != "report" || len(res.Report) == 0 {
		return nil, fmt.Errorf("detector service error: %s", res.Error)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	return res.Report, nil
}

func readMsg(ctx context.Context, conn *websocket.Conn) (serviceMsg, error) {
	var m serviceMsg
	_, data, err := conn.Read(ctx)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(data, &m)
}
