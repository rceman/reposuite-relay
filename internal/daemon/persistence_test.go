package daemon_test

// Persistence regression tests: durable RelaySession survives daemon
// restart while the HarnessRuntime never does. Everything runs against
// the real production binary in isolated roots — no model quota, no live
// harness.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRestartRestoresSessionCold is the core A2 proof: a fixture session
// created under one daemon reloads — same RelaySession ID, same key,
// harness, cwd, createdAt, generation — after daemon restart, with no
// runtime (COLD, no PID, no runtimeId, no invented native identity).
func TestRestartRestoresSessionCold(t *testing.T) {
	home, root := testRoot(t)
	_, f := serveKey(t, home, root, "alpha")
	fixturePID, _ := strconv.Atoi(f["pid"])
	if f["runtimeState"] != "warm" || f["sessionId"] == "" || f["runtimeId"] == "" {
		t.Fatalf("fresh session fields: %v", f)
	}
	created := field(t, mustCLI(t, home, root, "session", "status", "alpha"), "createdAt")
	cwd := field(t, mustCLI(t, home, root, "session", "status", "alpha"), "cwd")

	// Daemon shutdown stops the runtime but retains the durable session.
	stopDaemon(t, home, root)
	waitDead(t, fixturePID, 3*time.Second)

	// `list` autostarts a fresh daemon; alpha must be there, COLD.
	out := mustCLI(t, home, root, "list")
	if !strings.Contains(out, "key=alpha ") {
		t.Fatalf("alpha not restored: %s", out)
	}
	s := mustCLI(t, home, root, "session", "status", "alpha")
	if field(t, s, "sessionId") != f["sessionId"] {
		t.Fatalf("sessionId changed across restart: %s", s)
	}
	if field(t, s, "runtimeState") != "cold" || field(t, s, "pid") != "0" {
		t.Fatalf("restored session must be COLD: %s", s)
	}
	if field(t, s, "generation") != "1" || field(t, s, "createdAt") != created ||
		field(t, s, "cwd") != cwd || field(t, s, "harness") != "fixture" ||
		field(t, s, "state") != "idle" {
		t.Fatalf("durable fields mutated: %s", s)
	}
	if strings.Contains(s, "nativeSessionId") {
		t.Fatalf("no native identity may be invented: %s", s)
	}
	// runtimeId is empty for COLD: prints as `runtimeId= ` before the next field.
	if !strings.Contains(s, "runtimeId= runtimeState=") {
		t.Fatalf("unexpected runtimeId on COLD session: %s", s)
	}
	// No fixture process was spawned by the restart.
	_, count := daemonStatus(t, home, root)
	if count != 1 {
		t.Fatalf("sessionCount=%d want 1", count)
	}

	// Durable logical session blocks duplicate create — without a child.
	out, err := cli(t, home, root, "serve", "fixture", "--key", "alpha")
	if err == nil || !strings.Contains(out, "SESSION_EXISTS") {
		t.Fatalf("duplicate create after restart: %v %s", err, out)
	}
	if _, count = daemonStatus(t, home, root); count != 1 {
		t.Fatalf("duplicate create spawned a session: count=%d", count)
	}

	// Explicit stop deletes the durable session.
	mustCLI(t, home, root, "stop", "alpha")
	stopDaemon(t, home, root)
	out = mustCLI(t, home, root, "list")
	if strings.Contains(out, "key=alpha ") {
		t.Fatalf("stopped session must be gone after restart: %s", out)
	}
	_, count = daemonStatus(t, home, root)
	if count != 0 {
		t.Fatalf("sessionCount=%d want 0", count)
	}
	stopDaemon(t, home, root)
}

// TestSessionsStoreLayout: canonical dirs land under <root>/relay/sessions
// keyed by session ID — never by client key — with private permissions.
func TestSessionsStoreLayout(t *testing.T) {
	home, root := testRoot(t)
	_, f := serveKey(t, home, root, "layout")
	defer stopDaemon(t, home, root)

	sessionsDir := filepath.Join(root, "relay", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != f["sessionId"] {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("sessions dir must contain exactly the session id: %v (id %s)", names, f["sessionId"])
	}
	dir := filepath.Join(sessionsDir, f["sessionId"])
	for _, file := range []string{"session.json", "transcript.jsonl", "transcript.idx"} {
		fi, err := os.Lstat(filepath.Join(dir, file))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v", file, err)
		}
	}
	if fi, _ := os.Lstat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("session dir mode %o", fi.Mode().Perm())
	}
	// session.json must not contain runtime internals.
	raw, _ := os.ReadFile(filepath.Join(dir, "session.json"))
	for _, forbidden := range []string{`"pid"`, `"runtimeId"`, "runtimeStartedAt", "token", "password"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("session.json leaks runtime field %s: %s", forbidden, raw)
		}
	}
}

// TestCorruptStoreBlocksStartup: malformed canonical session metadata
// must fail daemon startup — and release singleton ownership so the error
// is visible, not hidden behind a zombie lock.
func TestCorruptStoreBlocksStartup(t *testing.T) {
	home, root := testRoot(t)
	_, f := serveKey(t, home, root, "victim")
	stopDaemon(t, home, root)

	// Corrupt the canonical session metadata.
	dir := filepath.Join(root, "relay", "sessions", f["sessionId"])
	if err := os.WriteFile(filepath.Join(dir, "session.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := cli(t, home, root, "list")
	if err == nil {
		t.Fatalf("daemon must not start on corrupt store: %s", out)
	}
	// Daemon must not be left running / holding the lock.
	daemonStopped(t, home, root)
}

// TestStopColdSessionDeletes: a restored COLD session can be stopped —
// no process stop is needed, durable record is deleted.
func TestStopColdSessionDeletes(t *testing.T) {
	home, root := testRoot(t)
	_, f := serveKey(t, home, root, "cold")
	stopDaemon(t, home, root)

	mustCLI(t, home, root, "list") // autostart fresh daemon
	if _, err := os.Lstat(filepath.Join(root, "relay", "sessions", f["sessionId"])); err != nil {
		t.Fatalf("durable session missing before stop: %v", err)
	}
	mustCLI(t, home, root, "stop", "cold")
	if _, err := os.Lstat(filepath.Join(root, "relay", "sessions", f["sessionId"])); !os.IsNotExist(err) {
		t.Fatal("durable session dir must be deleted")
	}
	out, err := cli(t, home, root, "session", "status", "cold")
	if err == nil || !strings.Contains(out, "SESSION_NOT_FOUND") {
		t.Fatalf("stopped cold session must be gone: %v %s", err, out)
	}
	stopDaemon(t, home, root)
}

// TestPersistedSessionCount: daemon status counts durable sessions, not
// only live runtimes — a restart with 5 sessions and 0 processes reports 5.
func TestPersistedSessionCount(t *testing.T) {
	home, root := testRoot(t)
	for _, k := range []string{"c1", "c2", "c3", "c4", "c5"} {
		serveKey(t, home, root, k)
	}
	stopDaemon(t, home, root)
	mustCLI(t, home, root, "list")
	_, count := daemonStatus(t, home, root)
	if count != 5 {
		t.Fatalf("sessionCount=%d want 5 (cold sessions count)", count)
	}
	stopDaemon(t, home, root)
}
