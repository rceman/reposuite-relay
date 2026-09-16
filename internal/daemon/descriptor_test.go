package daemon

// Runtime descriptor lifecycle: publication is readiness, identity/token
// rotate per daemon generation, and cleanup only ever removes the owning
// instance's descriptor.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/client"
	"github.com/rceman/reposuite-relay/internal/paths"
)

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// TestDescriptorLifecycle: 0600 mode, valid schema, endpoint matches the
// bound listener, no temp residue.
func TestDescriptorLifecycle(t *testing.T) {
	d, p, _, _ := startInProcess(t, nil)
	raw, err := os.ReadFile(p.DaemonDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	var desc api.Descriptor
	if err := json.Unmarshal(raw, &desc); err != nil {
		t.Fatal(err)
	}
	if desc.Version != api.DescriptorVersion {
		t.Fatalf("version = %d", desc.Version)
	}
	if desc.InstanceID != d.InstanceID() || len(desc.InstanceID) != 32 {
		t.Fatalf("instanceId mismatch: %q", desc.InstanceID)
	}
	if desc.PID != os.Getpid() {
		t.Fatalf("pid = %d, want %d", desc.PID, os.Getpid())
	}
	if desc.Endpoint != d.endpoint() {
		t.Fatalf("endpoint %q != listener %q", desc.Endpoint, d.endpoint())
	}
	if !isHex64(desc.BearerToken) {
		t.Fatalf("malformed bearer token")
	}
	fi, err := os.Stat(p.DaemonDescriptor())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("descriptor mode %o", fi.Mode().Perm())
	}
	// No temp residue.
	entries, _ := os.ReadDir(p.RunDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".daemon.json.tmp-") {
			t.Fatalf("descriptor temp residue: %s", e.Name())
		}
	}
	// The token is never leaked into status output.
	ctx, cancel := tctx(t)
	defer cancel()
	resp, err := client.Dial(p)
	if err != nil {
		t.Fatal(err)
	}
	info, err := resp.DaemonInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	infoRaw, _ := json.Marshal(info)
	if strings.Contains(string(infoRaw), desc.BearerToken) {
		t.Fatal("bearer token leaked into daemon info")
	}
}

// TestDescriptorRotatesPerGeneration: a fresh daemon generation has a new
// instance ID, a new token, and (possibly) a new ephemeral endpoint.
func TestDescriptorRotatesPerGeneration(t *testing.T) {
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
	run := func() (*Daemon, api.Descriptor) {
		t.Helper()
		d, err := Start(p, Options{SelfExe: "/bin/true"})
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() { served <- d.Serve() }()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := client.Dial(p); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		raw, err := os.ReadFile(p.DaemonDescriptor())
		if err != nil {
			t.Fatal(err)
		}
		var desc api.Descriptor
		if err := json.Unmarshal(raw, &desc); err != nil {
			t.Fatal(err)
		}
		d.Shutdown()
		select {
		case <-served:
		case <-time.After(4 * time.Second):
			t.Fatal("daemon did not stop")
		}
		// Graceful shutdown removes the owning descriptor.
		if _, err := os.Stat(p.DaemonDescriptor()); !os.IsNotExist(err) {
			t.Fatal("own descriptor not removed at shutdown")
		}
		return d, desc
	}
	_, first := run()
	_, second := run()
	if first.InstanceID == second.InstanceID {
		t.Fatal("instance ID reused across daemon generations")
	}
	if first.BearerToken == second.BearerToken {
		t.Fatal("bearer token reused across daemon generations")
	}
}

// TestDescriptorRemovalOnlyForOwner: a descriptor replaced by another
// generation must never be deleted by the old daemon's cleanup.
func TestDescriptorRemovalOnlyForOwner(t *testing.T) {
	d, p, _, served := startInProcess(t, nil)
	// Simulate a successor generation owning the descriptor.
	foreign := api.Descriptor{
		Version:     api.DescriptorVersion,
		InstanceID:  strings.Repeat("f", 32),
		PID:         os.Getpid(),
		Endpoint:    "http://127.0.0.1:9",
		BearerToken: strings.Repeat("e", 64),
	}
	raw, _ := json.Marshal(foreign)
	if err := os.WriteFile(p.DaemonDescriptor(), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	d.Shutdown()
	select {
	case <-served:
	case <-time.After(4 * time.Second):
		t.Fatal("daemon did not stop")
	}
	got, err := os.ReadFile(p.DaemonDescriptor())
	if err != nil {
		t.Fatal("foreign descriptor was removed by stale cleanup")
	}
	var after api.Descriptor
	if err := json.Unmarshal(got, &after); err != nil || after.InstanceID != foreign.InstanceID {
		t.Fatalf("foreign descriptor modified: %s", got)
	}
}

// TestDescriptorAbsentOnFailedStart: a startup failure (corrupt store)
// leaves no descriptor behind and releases the lock.
func TestDescriptorAbsentOnFailedStart(t *testing.T) {
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
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Corrupt canonical store entry.
	dir := filepath.Join(p.SessionsDir(), strings.Repeat("a", 32))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Start(p, Options{SelfExe: "/bin/true"}); err == nil {
		t.Fatal("corrupt store must fail startup")
	}
	if _, err := os.Stat(p.DaemonDescriptor()); !os.IsNotExist(err) {
		t.Fatal("descriptor published despite failed startup")
	}
	// The lock was released: a second Start also fails on the store, not
	// on ErrAlreadyRunning.
	if _, err := Start(p, Options{SelfExe: "/bin/true"}); err == ErrAlreadyRunning {
		t.Fatal("lock not released after failed startup")
	}
}
