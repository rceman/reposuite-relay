package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/config"
	"github.com/rceman/reposuite-relay/internal/paths"
)

// testPaths resolves an isolated Relay root for daemon tests.
func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, dir := range []string{home, root} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// serveDaemon starts the daemon and shuts it down at test end, waiting
// for the complete quiescence sequence before returning.
func serveDaemon(t *testing.T, d *Daemon) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { _ = d.Serve(); close(done) }()
	t.Cleanup(func() {
		d.Shutdown()
		select {
		case <-done:
		case <-time.After(60 * time.Second):
			t.Error("daemon did not finish shutdown within 60s")
		}
	})
	return done
}

// stopDaemon shuts d down fully and waits for quiescence.
func stopDaemonNow(t *testing.T, d *Daemon, done chan struct{}) {
	t.Helper()
	d.Shutdown()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("daemon did not finish shutdown")
	}
}

// rawGet performs one unauthenticated-or-bearer GET against a live daemon.
func rawGet(t *testing.T, endpoint, path, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestFirstStartSelectsAndPersistsPort: no config → the OS-selected port
// becomes the durable endpoint and the descriptor advertises it.
func TestFirstStartSelectsAndPersistsPort(t *testing.T) {
	p := testPaths(t)
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d)

	cfg, err := config.Load(p.RelayConfig())
	if err != nil {
		t.Fatalf("first start must persist relay.json: %v", err)
	}
	if cfg.ListenPort <= 0 {
		t.Fatalf("persisted port %d invalid", cfg.ListenPort)
	}
	bound := d.ln.Addr().(*net.TCPAddr).Port
	if cfg.ListenPort != bound {
		t.Fatalf("persisted port %d != bound port %d", cfg.ListenPort, bound)
	}
	desc, err := client.ReadDescriptor(p)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Endpoint != fmt.Sprintf("http://127.0.0.1:%d", cfg.ListenPort) {
		t.Fatalf("descriptor endpoint %q, want configured port %d", desc.Endpoint, cfg.ListenPort)
	}
}

// TestRestartReusesExactPort: every later generation binds the configured
// port — never a re-selection.
func TestRestartReusesExactPort(t *testing.T) {
	p := testPaths(t)
	d1, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	done1 := serveDaemon(t, d1)
	first := d1.ln.Addr().(*net.TCPAddr).Port
	stopDaemonNow(t, d1, done1)

	d2, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatal(err)
	}
	serveDaemon(t, d2)
	second := d2.ln.Addr().(*net.TCPAddr).Port
	if second != first {
		t.Fatalf("restart bound %d, want the configured %d", second, first)
	}
	cfg, err := config.Load(p.RelayConfig())
	if err != nil || cfg.ListenPort != first {
		t.Fatalf("config drifted: %+v, %v", cfg, err)
	}
}

// TestOccupiedConfiguredPortFailsStartup: a configured port that cannot
// be bound fails startup — config untouched, no fallback endpoint.
func TestOccupiedConfiguredPortFailsStartup(t *testing.T) {
	p := testPaths(t)
	// Commit a config, then occupy its port.
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	cfg := config.Config{SchemaVersion: config.SchemaVersion, ListenHost: config.ListenHost, ListenPort: port}
	if err := config.Commit(p.RelayConfig(), cfg); err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	if _, err := Start(p, Options{SelfExe: testBinary()}); err == nil {
		t.Fatal("startup on an occupied configured port must fail")
	} else {
		if !strings.Contains(err.Error(), cfg.Address()) {
			t.Fatalf("error %q must name the configured address %s", err, cfg.Address())
		}
	}
	got, err := config.Load(p.RelayConfig())
	if err != nil || got != cfg {
		t.Fatalf("config mutated on failed bind: %+v, %v", got, err)
	}
	if _, err := os.Stat(p.DaemonDescriptor()); !os.IsNotExist(err) {
		t.Fatal("descriptor published for a failed bind")
	}
	// Freeing the port lets the exact configured endpoint come up.
	probe.Close()
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatalf("bind after free: %v", err)
	}
	serveDaemon(t, d)
	if d.ln.Addr().(*net.TCPAddr).Port != port {
		t.Fatal("daemon did not bind the configured port")
	}
}

// TestMalformedAndUnsupportedConfigFailClosed: corrupt or unsupported
// relay.json fails startup — never silently rewritten.
func TestMalformedAndUnsupportedConfigFailClosed(t *testing.T) {
	for name, raw := range map[string]string{
		"garbage":     "not json",
		"bad-schema":  `{"schemaVersion":2,"listenHost":"127.0.0.1","listenPort":1}`,
		"lan-host":    `{"schemaVersion":1,"listenHost":"0.0.0.0","listenPort":1}`,
		"port-zero":   `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":0}`,
		"extra-field": `{"schemaVersion":1,"listenHost":"127.0.0.1","listenPort":1,"x":1}`,
	} {
		p := testPaths(t)
		if err := p.Ensure(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p.RelayConfig(), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Start(p, Options{SelfExe: testBinary()}); err == nil {
			t.Fatalf("%s: malformed/unsupported config must fail startup", name)
		}
		after, _ := os.ReadFile(p.RelayConfig())
		if string(after) != raw {
			t.Fatalf("%s: invalid config was rewritten", name)
		}
	}
}

// TestConcurrentFirstStartOneAuthority: concurrent first-start
// contenders converge on exactly one daemon, one canonical config, one
// port.
func TestConcurrentFirstStartOneAuthority(t *testing.T) {
	p := testPaths(t)
	const n = 8
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Start(p, Options{SelfExe: testBinary()})
			if err != nil {
				results <- err
				return
			}
			done := make(chan struct{})
			go func() { _ = d.Serve(); close(done) }()
			d.Shutdown()
			<-done
			results <- nil
		}()
	}
	wg.Wait()
	close(results)
	wins, loses := 0, 0
	for err := range results {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrAlreadyRunning) {
			loses++
		} else {
			t.Fatalf("unexpected start error: %v", err)
		}
	}
	if wins != 1 || loses != n-1 {
		t.Fatalf("wins=%d loses=%d, want exactly 1 winner", wins, loses)
	}
	cfg, err := config.Load(p.RelayConfig())
	if err != nil || cfg.ListenPort <= 0 {
		t.Fatalf("no canonical config persisted: %+v, %v", cfg, err)
	}
}

// TestConfigCommitFailureClosesListener: a failed first-start config
// commit leaves no listener, no descriptor, no readiness.
func TestConfigCommitFailureClosesListener(t *testing.T) {
	p := testPaths(t)
	// Pre-create the relay root writable and the config dir read-only:
	// Ensure succeeds but Commit cannot write there.
	if err := os.MkdirAll(p.RelayRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.ConfigDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p.ConfigDir(), 0o700)
	if _, err := Start(p, Options{SelfExe: testBinary()}); err == nil {
		t.Skip("environment permits writes to 0500 config dir")
	}
	if _, err := os.Stat(p.DaemonDescriptor()); !os.IsNotExist(err) {
		t.Fatal("readiness descriptor published for an uncommitted endpoint")
	}
	if _, err := os.Stat(p.RelayConfig()); !os.IsNotExist(err) {
		t.Fatal("config committed despite write failure")
	}
	// The failed attempt's bound port is released: a retry under a
	// writable dir must succeed cleanly.
	if err := os.Chmod(p.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := Start(p, Options{SelfExe: testBinary()})
	if err != nil {
		t.Fatalf("retry after writable config dir: %v", err)
	}
	serveDaemon(t, d)
}
