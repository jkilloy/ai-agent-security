// Package plugin defines the analysis plugin contract and a process-wide
// registry. Plugins register themselves from an init function.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"agent-security/internal/audio"
)

// Input is what each plugin receives for a call.
type Input struct {
	CallID string
	// Variables are the key/value pairs from the call's first message. Each
	// plugin gets its own deep copy, so changing it has no effect elsewhere.
	Variables map[string]any
	// Audio is this plugin's own live subscription to the call audio. It
	// yields frames as they arrive and io.EOF when the call ends.
	Audio *audio.Stream
}

// Plugin analyzes a call and returns free-form JSON. Analyze is called once per
// call, when the call starts, and should read Audio until io.EOF (or until it
// has seen enough). The ctx is cancelled if the plugin runs longer than the
// configured timeout after the call ends.
type Plugin interface {
	Name() string
	Analyze(ctx context.Context, in *Input) (json.RawMessage, error)
}

var (
	mu       sync.RWMutex
	registry = map[string]Plugin{}
)

// Register adds a plugin to the registry. It panics on an empty or duplicate
// name, since that is a programming error found at startup.
func Register(p Plugin) {
	mu.Lock()
	defer mu.Unlock()
	name := p.Name()
	if name == "" {
		panic("plugin: empty name")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("plugin: %q registered twice", name))
	}
	registry[name] = p
}

// All returns the registered plugins sorted by name.
func All() []Plugin {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Plugin, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
