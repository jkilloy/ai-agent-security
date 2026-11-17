// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	ListenAddr          string
	DatabaseURL         string
	AudioDir            string
	Retention           time.Duration
	CleanupInterval     time.Duration
	PluginTimeout       time.Duration
	FirstMessageTimeout time.Duration
	MaxCallDuration     time.Duration
	MaxFrameBytes       int64
	ShutdownTimeout     time.Duration
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:  getStr("LISTEN_ADDR", ":8080"),
		DatabaseURL: os.Getenv("DATABASE_URL"),
		AudioDir:    getStr("AUDIO_DIR", "/data/audio"),
	}
	var err error
	durations := []struct {
		dst *time.Duration
		key string
		def time.Duration
	}{
		{&c.Retention, "RETENTION", 720 * time.Hour},
		{&c.CleanupInterval, "CLEANUP_INTERVAL", time.Hour},
		{&c.PluginTimeout, "PLUGIN_TIMEOUT", 30 * time.Second},
		{&c.FirstMessageTimeout, "FIRST_MESSAGE_TIMEOUT", 10 * time.Second},
		{&c.MaxCallDuration, "MAX_CALL_DURATION", 4 * time.Hour},
		{&c.ShutdownTimeout, "SHUTDOWN_TIMEOUT", 60 * time.Second},
	}
	for _, d := range durations {
		if *d.dst, err = getDuration(d.key, d.def); err != nil {
			return c, err
		}
	}
	if c.MaxFrameBytes, err = getInt("MAX_FRAME_BYTES", 1<<20); err != nil {
		return c, err
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("DATABASE_URL is required")
	}
	if c.CleanupInterval <= 0 {
		return c, fmt.Errorf("CLEANUP_INTERVAL must be positive")
	}
	return c, nil
}

func getStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return d, nil
}

func getInt(key string, def int64) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return n, nil
}
