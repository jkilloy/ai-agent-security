// Package audiostats is an example plugin: it counts frames and bytes and, if
// the call declared its audio format, estimates the audio duration.
package audiostats

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"agent-security/internal/plugin"
)

func init() { plugin.Register(Plugin{}) }

type Plugin struct{}

func (Plugin) Name() string { return "audiostats" }

type result struct {
	Frames          int64    `json:"frames"`
	Bytes           int64    `json:"bytes"`
	LargestFrame    int      `json:"largest_frame"`
	DurationSeconds *float64 `json:"duration_seconds,omitempty"`
}

func (Plugin) Analyze(ctx context.Context, in *plugin.Input) (json.RawMessage, error) {
	var r result
	for {
		f, err := in.Audio.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		r.Frames++
		r.Bytes += int64(len(f))
		r.LargestFrame = max(r.LargestFrame, len(f))
	}
	if bps := bytesPerSecond(in.Variables); bps > 0 {
		d := float64(r.Bytes) / bps
		r.DurationSeconds = &d
	}
	return json.Marshal(r)
}

// bytesPerSecond derives the byte rate from the optional "encoding",
// "sample_rate" and "channels" variables. It returns 0 if unknown.
func bytesPerSecond(v map[string]any) float64 {
	rate, _ := v["sample_rate"].(float64)
	if rate <= 0 {
		return 0
	}
	channels := 1.0
	if c, ok := v["channels"].(float64); ok && c > 0 {
		channels = c
	}
	enc, _ := v["encoding"].(string)
	var width float64
	switch enc {
	case "mulaw", "ulaw", "alaw", "pcm8":
		width = 1
	case "pcm16", "linear16", "s16le":
		width = 2
	case "pcm32", "f32le":
		width = 4
	default:
		return 0
	}
	return rate * channels * width
}
