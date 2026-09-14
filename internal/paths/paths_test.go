package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultRoot(t *testing.T) {
	p := Resolve("/home/dev", "")
	if got, want := p.RepoSuiteRoot(), "/home/dev/.reposuite"; got != want {
		t.Fatalf("root=%q want %q", got, want)
	}
}

func TestCustomRepoSuiteHome(t *testing.T) {
	p := Resolve("/home/dev", "/srv/reposuite/x/../state/")
	if got, want := p.RepoSuiteRoot(), "/srv/reposuite/state"; got != want {
		t.Fatalf("root=%q want %q", got, want)
	}
}

func TestRelayPaths(t *testing.T) {
	p := Resolve("/h", "")
	cases := map[string]string{
		p.RelayRoot():    "/h/.reposuite/relay",
		p.RunDir():       "/h/.reposuite/relay/run",
		p.DaemonSocket(): "/h/.reposuite/relay/run/relayd.sock",
		p.LogDir():       "/h/.reposuite/relay/logs",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestNoAirelayPaths(t *testing.T) {
	for _, s := range []string{"/dev/null", "/home/x"} {
		p := Resolve(s, "")
		for _, path := range []string{
			p.RepoSuiteRoot(), p.RelayRoot(), p.RunDir(), p.DaemonSocket(), p.LogDir(),
		} {
			if strings.Contains(path, "airelay") {
				t.Fatalf("airelay leaked into path %q", path)
			}
		}
	}
}

func TestEnsureCreatesPrivateDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rs")
	p := Resolve("/unused", root)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{p.RelayRoot(), p.RunDir(), p.LogDir()} {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("missing %s: %v", dir, err)
		}
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode %o want 0700", dir, perm)
		}
	}
}
