package telemetry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirPrecedence(t *testing.T) {
	t.Setenv("REPODEX_STATE_DIR", "/env/dir")
	// Explicit config wins over env.
	c := Config{StateDir: "/cfg/dir"}
	if got := c.RepoDexStateDir(); got != "/cfg/dir" {
		t.Fatalf("config precedence: %s", got)
	}
	// Env overrides the default only.
	c = Config{}
	if got := c.RepoDexStateDir(); got != "/env/dir" {
		t.Fatalf("env fallback: %s", got)
	}
	// Neither set → ~/reposuite/repodex.
	t.Setenv("REPODEX_STATE_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home")
	}
	want := filepath.Join(home, "reposuite", "repodex")
	if got := (Config{}).RepoDexStateDir(); got != want {
		t.Fatalf("default: %s want %s", got, want)
	}
}
