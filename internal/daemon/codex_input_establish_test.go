package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/rceman/reposuite-relay/internal/api"
	"github.com/rceman/reposuite-relay/internal/harness/codex"
	"github.com/rceman/reposuite-relay/internal/paths"
	"github.com/rceman/reposuite-relay/internal/session"
)

// codexAdapter reaches the in-process Codex adapter (test-only view of
// retained input authority).
func codexAdapter(t *testing.T, d *Daemon) *codex.Adapter {
	t.Helper()
	e, ok := d.adapters[session.HarnessCodex]
	if !ok {
		t.Fatal("no codex adapter")
	}
	a, ok := e.adapter.(*codex.Adapter)
	if !ok {
		t.Fatal("adapter is not codex")
	}
	return a
}

// TestCodexInputAnnounceStateSaveFailure: the durable waiting_input
// transition is part of the announcement transaction — when it fails,
// no input.requested may be published and no pending entry may survive.
func TestCodexInputAnnounceStateSaveFailure(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	var fail atomic.Bool
	fail.Store(true)
	d, p, c, _ := startInProcess(t, func(o *Options) {
		codexOptions(o)
		o.StoreHooks = sessionMetaFault(&fail, `"state": "waiting_input"`)
	})
	sess := serveCodex(t, c, "statefail")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "statefail", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	// The announcement fails at the state commit: the native request gets
	// an RPC error, the fake treats it as an answer and completes.
	waitDurableTypes(t, c, "statefail", api.EventMessageAgentCompleted)
	waitSessionState(t, c, "statefail", session.StateIdle)

	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputRequested); n != 0 {
		t.Fatalf("input.requested count = %d, want 0 — state commit failed first", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 0 {
		t.Fatalf("input.aborted count = %d, want 0 — never announced", n)
	}
	if got := codexAdapter(t, d).PendingInputs(sess.SessionID); len(got) != 0 {
		t.Fatalf("pending inputs = %v, want none after failed announcement", got)
	}
}

// TestCodexInputAnnouncePublishFailure: waiting_input committed but the
// input.requested append fails — the announcing entry is removed (never
// answerable), no input.aborted is invented for a request that never
// committed, and a stuck durable waiting_input is normalized on restart.
func TestCodexInputAnnouncePublishFailure(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")
	home, root := codexRoot(t)
	p, err := paths.Resolve(home, root)
	if err != nil {
		t.Fatal(err)
	}

	var failRequested atomic.Bool
	var failActive atomic.Bool
	opts := Options{SelfExe: testBinary()}
	codexOptions(&opts)
	opts.StoreHooks = storeWriteFault(func(f *os.File, b []byte) error {
		if strings.HasSuffix(f.Name(), "transcript.jsonl") {
			if failRequested.Load() && bytes.Contains(b, []byte(api.EventInputRequested)) {
				// Arming inside the matched write is exact: only session
				// writes AFTER this failed append are faulted — the
				// announcement's waiting_input commit already landed,
				// and seq reservations earlier in the prompt are
				// untouched.
				failActive.Store(true)
				return errInjectedWrite
			}
			return nil
		}
		// The rollback write (settle → active/idle) fails too — a stale
		// durable waiting_input must survive to be normalized on restart.
		// waiting_input itself is never matched.
		if failActive.Load() &&
			(bytes.Contains(b, []byte(`"state": "active"`)) ||
				bytes.Contains(b, []byte(`"state": "idle"`))) {
			return errInjectedWrite
		}
		return nil
	})

	d, c, stop := startDaemonGeneration(t, p, opts)
	sess := serveCodex(t, c, "pubfail")
	// Arm the request fault only after the session exists; the rollback
	// fault arms itself inside the matched input.requested failure.
	failRequested.Store(true)
	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "pubfail", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	// The fake receives an RPC error for the failed announcement and
	// completes the turn.
	waitDurableTypes(t, c, "pubfail", api.EventMessageAgentCompleted)

	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputRequested); n != 0 {
		t.Fatalf("input.requested count = %d, want 0", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 0 {
		t.Fatalf("input.aborted count = %d, want 0 — no request was committed", n)
	}
	if got := codexAdapter(t, d).PendingInputs(sess.SessionID); len(got) != 0 {
		t.Fatalf("pending inputs = %v, want none after failed announcement", got)
	}
	stop()

	// The rollback never committed either: durable state is a stale
	// waiting_input with ZERO unresolved requests — restart normalizes.
	failRequested.Store(false)
	failActive.Store(false)
	_, c2, stop2 := startDaemonGeneration(t, p, opts)
	defer stop2()
	st, err := c2.Status(ctx, "pubfail")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.State != session.StateIdle {
		t.Fatalf("restored state = %s, want idle (stale waiting_input normalized)", st.Session.State)
	}
	raw, err = os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 0 {
		t.Fatalf("input.aborted count = %d, want 0 — nothing to abort", n)
	}
}

// TestCodexAnnounceRuntimeDeathRace: the runtime dies while the durable
// input.requested append is held open. The only legal histories are
// requested→aborted (announcement committed) or neither record (it
// failed) — never aborted-before-requested, never a ghost entry.
func TestCodexAnnounceRuntimeDeathRace(t *testing.T) {
	t.Setenv("FAKE_CODEX_MODE", "input")

	var once sync.Once
	var released atomic.Bool
	blocked := make(chan struct{})
	release := make(chan struct{})
	unblock := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	t.Cleanup(unblock)
	d, p, c, _ := startInProcess(t, func(o *Options) {
		codexOptions(o)
		o.StoreHooks = transcriptFault(func(b []byte) error {
			if bytes.Contains(b, []byte(api.EventInputRequested)) {
				once.Do(func() {
					close(blocked)
					<-release
				})
			}
			return nil
		})
	})
	sess := serveCodex(t, c, "dieannounce")

	ctx, cancel := tctx(t)
	defer cancel()
	if _, err := c.Prompt(ctx, "dieannounce", "ask me", "", ""); err != nil {
		t.Fatal(err)
	}
	// Wait until the announcement holds its durable append open.
	<-blocked
	// Kill the runtime mid-announcement: the death path must not publish
	// input.aborted before input.requested exists.
	st, err := c.Status(ctx, "dieannounce")
	if err != nil {
		t.Fatal(err)
	}
	if st.Session.PID == 0 {
		t.Fatal("no live runtime PID")
	}
	if err := syscall.Kill(st.Session.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	unblock()

	waitDurableTypes(t, c, "dieannounce", api.EventRuntimeExited)
	waitSessionState(t, c, "dieannounce", session.StateIdle)

	raw, err := os.ReadFile(transcriptPath(t, p, sess.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if got := codexAdapter(t, d).PendingInputs(sess.SessionID); len(got) != 0 {
		t.Fatalf("pending inputs = %v, want none", got)
	}
	// The append was blocked, not failed — the committed path must show
	// input.requested followed by input.aborted.
	if n := countType(t, raw, api.EventInputRequested); n != 1 {
		t.Fatalf("input.requested count = %d, want 1", n)
	}
	if n := countType(t, raw, api.EventInputAborted); n != 1 {
		t.Fatalf("input.aborted count = %d, want 1", n)
	}
	reqAt := bytes.Index(raw, []byte(`"type":"`+api.EventInputRequested+`"`))
	abAt := bytes.Index(raw, []byte(`"type":"`+api.EventInputAborted+`"`))
	if reqAt < 0 || abAt < reqAt {
		t.Fatal("input.aborted must never precede input.requested")
	}
	var ab struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(lastPayload(t, raw, api.EventInputAborted), &ab); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ab.Reason, "runtime exited") {
		t.Fatalf("abort reason = %q, want runtime exit", ab.Reason)
	}
}
