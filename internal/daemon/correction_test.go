package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/paths"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testBinary returns the production binary built by TestMain (published
// through REPOSUITE_TEST_BIN), which implements the `__fixture` child mode.
func testBinary() string {
	p := os.Getenv("REPOSUITE_TEST_BIN")
	if p == "" {
		panic("REPOSUITE_TEST_BIN not set — TestMain must build the production binary first")
	}
	return p
}

// startInProcess brings up a daemon on an isolated root with the given
// option overrides. It is Serve'd in a goroutine (result on `served`) and
// Shutdown on cleanup.
func startInProcess(t *testing.T, mutate func(*Options)) (*Daemon, paths.Paths, *client.Client, chan error) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	root := filepath.Join(base, "rs")
	for _, d := range []string{home, root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}
	// The production binary implements `__fixture`, so the default spawn
	// path is the real one; tests that need controlled/stubborn children
	// override SpawnFixture. The binary is built once by TestMain in the
	// external test package.
	opts := Options{SelfExe: testBinary()}
	if mutate != nil {
		mutate(&opts)
	}
	d, err := Start(p, opts)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	serveDone := make(chan struct{})
	go func() { served <- d.Serve(); close(serveDone) }()
	// Shutdown only INITIATES teardown — Serve then runs the quiescence
	// sequence (stop+reap every runtime, watchers, adapter readers) on its
	// own goroutine. Cleanup must wait for it: durable writes still in
	// flight would race this test's TempDir removal otherwise.
	t.Cleanup(func() {
		d.Shutdown()
		select {
		case <-serveDone:
		case <-time.After(60 * time.Second):
			t.Error("daemon did not finish shutdown within 60s")
		}
	})

	// Wait until the daemon answers an authenticated ping.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := client.Dial(p); err == nil {
			return d, p, c, served
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("in-process daemon did not become ready")
	return nil, paths.Paths{}, nil, nil
}

// tctx returns a bounded context for one client call.
func tctx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// serve creates a fixture session; the error is returned as-is so tests
// can assert stable API codes.
func serve(t *testing.T, c *client.Client, key string) (api.SessionInfo, error) {
	t.Helper()
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := c.ServeFixture(ctx, key, "/")
	return resp.Session, err
}

// wantAPIErr asserts err is an APIError with the given code.
func wantAPIErr(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want API error %s, got nil", code)
	}
	var ae *client.APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want API error %s, got %v", code, err)
	}
	if ae.Code != code {
		t.Fatalf("want API error %s, got %s (%v)", code, ae.Code, err)
	}
}

// fakeHarness is a controlled in-memory runtime process for supervisor
// tests: no OS process, deterministic stop accounting.
type fakeHarness struct {
	pid     int
	stopped int32
	stopErr error
	done    chan struct{}
}

func newFakeHarness(pid int) *fakeHarness {
	return &fakeHarness{
		pid:  pid,
		done: make(chan struct{}),
	}
}

func (f *fakeHarness) PID() int { return f.pid }

func (f *fakeHarness) Wait() <-chan struct{} { return f.done }

func (f *fakeHarness) Stop() error {
	atomic.AddInt32(&f.stopped, 1)
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return f.stopErr
}

// procAlive reports whether pid exists and is not a zombie (same-package
// copy; the _test package helper is not visible here).
func procAlive(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(data)
	i := strings.LastIndex(s, ")")
	return i >= 0 && i+2 < len(s) && s[i+2] != 'Z'
}

func waitProcDead(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !procAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %s", pid, d)
}
