package telemetry

import (
	"os"
	"path/filepath"
	"time"
)

// Config is the daemon-level RepoDex telemetry configuration — a nested
// optional section of config/relay.json using existing Relay naming style.
type Config struct {
	Enabled  bool   `json:"enabled"`
	StateDir string `json:"stateDir,omitempty"`
	// BatchMaxEvents / BatchMaxBytes / BatchMaxDelayMs bound one ingest
	// POST — Relay-imposed bounds since RepoDex defines none.
	BatchMaxEvents  int `json:"batchMaxEvents,omitempty"`
	BatchMaxBytes   int `json:"batchMaxBytes,omitempty"`
	BatchMaxDelayMs int `json:"batchMaxDelayMs,omitempty"`
	// QueueLimit bounds the healthy-path in-memory queue per overflow side.
	QueueLimit int `json:"memoryQueueLimit,omitempty"`
	// SpoolLimitBytes bounds the crash-safe outage fallback directory.
	SpoolLimitBytes int64 `json:"spoolLimitBytes,omitempty"`
}

func (c Config) withDefaults() Config {
	if c.BatchMaxEvents <= 0 {
		c.BatchMaxEvents = 64
	}
	if c.BatchMaxBytes <= 0 {
		c.BatchMaxBytes = 256 << 10
	}
	if c.BatchMaxDelayMs <= 0 {
		c.BatchMaxDelayMs = 2000
	}
	if c.QueueLimit <= 0 {
		c.QueueLimit = 4096
	}
	if c.SpoolLimitBytes <= 0 {
		c.SpoolLimitBytes = 32 << 20
	}
	return c
}

func (c Config) batchDelay() time.Duration {
	return time.Duration(c.BatchMaxDelayMs) * time.Millisecond
}

// RepoDexStateDir resolves the RepoDex state directory. Precedence:
//
//	configured repodex.stateDir > REPODEX_STATE_DIR env > ~/reposuite/repodex
//
// The explicit durable config wins over the environment: an operator who
// pinned a state dir in relay.json must not have it silently overridden
// by a stray env var; the env var is the override for the DEFAULT only.
func (c Config) RepoDexStateDir() string {
	if c.StateDir != "" {
		return c.StateDir
	}
	if d := os.Getenv("REPODEX_STATE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", "reposuite", "repodex")
	}
	return filepath.Join(home, "reposuite", "repodex")
}
