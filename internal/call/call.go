// Package call defines the Call object that ties together a call's variables,
// its audio, and the analysis results produced by plugins.
package call

import (
	"encoding/json"
	"time"
)

// Call is the central object for one phone call. Variables come from the first
// WebSocket message; Results are filled in by plugins, keyed by plugin name.
type Call struct {
	ID         string                     `json:"id"`
	Variables  map[string]any             `json:"variables"`
	Results    map[string]json.RawMessage `json:"results"`
	AudioPath  string                     `json:"-"`
	AudioBytes int64                      `json:"audio_bytes"`
	StartedAt  time.Time                  `json:"started_at"`
	EndedAt    time.Time                  `json:"ended_at"`
	ExpiresAt  time.Time                  `json:"expires_at"`
}
