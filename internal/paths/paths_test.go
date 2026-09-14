package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustResolve(t *testing.T, home, env string) Paths {
	t.Helper()
	p, err := Resolve(home, env)
	if err != nil {
		t.Fatalf("Resolve(%q, %q): %v", home, env, err)
	}
	return p
}

func TestDefaultRoot(t *testing.T) {
	p := mustResolve(t, "/home/dev", "")
	if got, want := p.RepoSuiteRoot(), "/home/dev/.reposuite"; got != want {
		t.Fatalf("root=%q want %q", got, want)
	}
}

func TestCustomRepoSuiteHome(t *testing.T) {
	p := mustResolve(t, "/home/dev", "/srv/reposuite/x/../state/")
	if got, want := p.RepoSuiteRoot(), "/srv/reposuite/state"; got != want {
		t.Fatalf("root=%q want %q", got, want)
	}
}

func TestRelayPaths(t *testing.T) {
	p := mustResolve(t, "/h", "")
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

// TestLegacyAirelayOverlap: Relay state must never live inside ~/.airelay.
// All paths are synthetic; no real filesystem entries are touched.
func TestLegacyAirelayOverlap(t *testing.T) {
	home := "/home/user"
	reject := []string{
		"/home/user/.airelay",
		"/home/user/.airelay/relay",
		"/home/user/.airelay/x/y",
		"/home/user/.airelay/./deep/../still-inside",
	}
	for _, env := range reject {
		if _, err := Resolve(home, env); err == nil {
			t.Errorf("REPOSUITE_HOME=%q must be rejected", env)
		}
	}
	allow := []struct {
		env  string
		want string
	}{
		{"/home/user/.reposuite", "/home/user/.reposuite"},
		{"/home/user/.airelay2", "/home/user/.airelay2"},
		{"/tmp/airelay", "/tmp/airelay"},
		{"/srv/reposuite", "/srv/reposuite"},
		{"/home/user/.airelay/../.reposuite", "/home/user/.reposuite"},
	}
	for _, tc := range allow {
		p, err := Resolve(home, tc.env)
		if err != nil {
			t.Errorf("REPOSUITE_HOME=%q must be allowed: %v", tc.env, err)
			continue
		}
		if p.RepoSuiteRoot() != tc.want {
			t.Errorf("REPOSUITE_HOME=%q resolved to %q want %q", tc.env, p.RepoSuiteRoot(), tc.want)
		}
	}
}

func TestNoAirelayPaths(t *testing.T) {
	for _, s := range []string{"/dev/null", "/home/x"} {
		p := mustResolve(t, s, "")
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
	p := mustResolve(t, "/unused", root)
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
