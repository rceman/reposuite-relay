package codex

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeBin is the production binary built once per test run: the adapter
// tests spawn it in the hidden `__fake-codex` mode so the deterministic
// fake app-server speaks the protocol on a clean stdout. (A re-exec'd Go
// test binary cannot: the test framework writes to stdout too.)
var fakeBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "relay-codex-test-bin")
	if err != nil {
		panic(err)
	}
	fakeBin = filepath.Join(dir, "reposuite-relay")
	out, err := exec.Command("go", "build", "-o", fakeBin,
		"github.com/rceman/reposuite-relay/cmd/reposuite-relay").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build test binary: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeCommand returns the fake app-server command.
func fakeCommand() (Command, error) {
	return Command{Path: fakeBin, Args: []string{"__fake-codex"}}, nil
}
