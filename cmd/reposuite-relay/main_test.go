package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommands(t *testing.T) {
	if got := run([]string{"version"}, ""); got != 0 {
		t.Fatalf("version exit=%d", got)
	}
	if got := run([]string{"help"}, ""); got != 0 {
		t.Fatalf("help exit=%d", got)
	}
	if got := run([]string{"bogus"}, ""); got == 0 {
		t.Fatal("unknown command should fail non-zero")
	}
	if got := run(nil, ""); got == 0 {
		t.Fatal("no args should fail non-zero")
	}
}

// TestCLI executes the built binary for real end-to-end coverage.
func TestCLI(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "reposuite-relay")
	out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		args    []string
		want    string
		wantErr bool
	}{
		{[]string{"version"}, "reposuite-relay 0.1.0-dev", false},
		{[]string{"help"}, "Usage:", false},
		{[]string{"paths"}, "daemon_socket:", false},
		{[]string{"nope"}, "unknown command", true},
	} {
		cmd := exec.Command(bin, tc.args...)
		cmd.Env = append(os.Environ(), "REPOSUITE_HOME="+t.TempDir())
		o, err := cmd.CombinedOutput()
		if tc.wantErr && err == nil {
			t.Fatalf("%v: expected failure, got %q", tc.args, o)
		}
		if !tc.wantErr && err != nil {
			t.Fatalf("%v: %v: %s", tc.args, err, o)
		}
		if !strings.Contains(string(o), tc.want) {
			t.Fatalf("%v: output %q missing %q", tc.args, o, tc.want)
		}
	}
}

func TestPathsCommandRespectsEnv(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "reposuite-relay")
	if out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	root := t.TempDir()
	cmd := exec.Command(bin, "paths")
	cmd.Env = append(os.Environ(), "REPOSUITE_HOME="+root)
	o, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, o)
	}
	if !strings.Contains(string(o), root+"/relay/run/relayd.sock") {
		t.Fatalf("socket path wrong:\n%s", o)
	}
	if strings.Contains(string(o), "airelay") {
		t.Fatalf("airelay leaked:\n%s", o)
	}
}
