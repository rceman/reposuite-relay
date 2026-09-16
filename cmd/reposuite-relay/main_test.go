package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCommands(t *testing.T) {
	if got := run([]string{"version"}, "reposuite-relay"); got != 0 {
		t.Fatalf("version exit=%d", got)
	}
	if got := run([]string{"help"}, "reposuite-relay"); got != 0 {
		t.Fatalf("help exit=%d", got)
	}
	if got := run([]string{"bogus"}, "reposuite-relay"); got == 0 {
		t.Fatal("unknown command should fail non-zero")
	}
	if got := run(nil, "reposuite-relay"); got == 0 {
		t.Fatal("no args should fail non-zero")
	}
	// version/help must not depend on valid filesystem configuration.
	t.Setenv("REPOSUITE_HOME", "relative/path")
	if got := run([]string{"version"}, "reposuite-relay"); got != 0 {
		t.Fatalf("version must not depend on REPOSUITE_HOME, exit=%d", got)
	}
	if got := run([]string{"help"}, "reposuite-relay"); got != 0 {
		t.Fatalf("help must not depend on REPOSUITE_HOME, exit=%d", got)
	}
}

// TestPathsRejectsLegacyAirelayRoot: REPOSUITE_HOME under ~/.airelay must
// fail non-zero before any filesystem write. Uses a synthetic temp HOME.
func TestPathsRejectsLegacyAirelayRoot(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "reposuite-relay")
	if out, err := exec.Command("go", "build", "-o", bin,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	home := t.TempDir()
	legacy := filepath.Join(home, ".airelay")
	for _, env := range []string{legacy, legacy + "/relay", legacy + "/x/y"} {
		cmd := exec.Command(bin, "paths")
		cmd.Env = []string{"HOME=" + home, "REPOSUITE_HOME=" + env, "PATH=" + os.Getenv("PATH")}
		o, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("REPOSUITE_HOME=%q must fail, got %q", env, o)
		}
		if !strings.Contains(string(o), "legacy Airelay") {
			t.Fatalf("REPOSUITE_HOME=%q error should explain the rejection, got %q", env, o)
		}
	}
	// Safe sibling remains allowed.
	cmd := exec.Command(bin, "paths")
	cmd.Env = []string{"HOME=" + home, "REPOSUITE_HOME=" + filepath.Join(home, ".reposuite"),
		"PATH=" + os.Getenv("PATH")}
	o, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("safe root must succeed: %v: %s", err, o)
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
		{[]string{"paths"}, "daemon_descriptor:", false},
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
	if !strings.Contains(string(o), root+"/relay/run/daemon.json") {
		t.Fatalf("descriptor path wrong:\n%s", o)
	}
	if !strings.Contains(string(o), root+"/relay/sessions") {
		t.Fatalf("sessions dir missing:\n%s", o)
	}
	if strings.Contains(string(o), "relayd.sock") {
		t.Fatalf("obsolete socket path advertised:\n%s", o)
	}
	if strings.Contains(string(o), "airelay") {
		t.Fatalf("airelay leaked:\n%s", o)
	}
}
