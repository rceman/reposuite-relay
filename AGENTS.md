# AGENTS.md — reposuite-relay

## Product

RepoSuite Relay is a **persistent structured agent-session daemon with
native harness adapters** (ADR-005). It is not a terminal relay: no PTY,
no terminal emulator, no xterm dependency in the production core.

## Accepted target architecture

- `RelaySession` — durable logical session identity; `HarnessRuntime` —
  the ephemeral native harness process/server. They are distinct: a
  session may exist with **zero** harness runtime resources.
- Resume always uses the **exact native session identity** — never
  "newest"/`--last`/implicit.
- An idle `HarnessRuntime` may be fully stopped; `waiting_input`
  (unresolved native input request) blocks runtime sleep.
- Relay owns durable metadata + compact transcript; native stores remain
  authoritative for model context.
- Native harness servers are **private to relayd** — never exposed to
  clients.
- Canonical local protocol (ADR-006): loopback TCP `127.0.0.1:0`,
  HTTP/JSON commands, streaming NDJSON events, `run/daemon.json`
  descriptor, per-daemon bearer token.
- Canonical session events carry a per-session monotonic uint64 seq;
  transient events consume seq without being persisted, so sequence
  space is durably reserved in blocks (`SeqHighWatermark`).
- The canonical event cursor is what history snapshots hand to clients:
  after a restart it is the persisted watermark, NOT the last durable
  record. Exact replay is gated by an explicit replay floor
  (`replayFloor <= after <= cursor`); below it is `CURSOR_TOO_OLD`, above
  it is `CURSOR_AHEAD`.
- COLD sessions are cheap: event state holds no transcript file
  descriptors (open → work → close) and the replay ring is allocated
  lazily on first publish. One canonical NDJSON frame bound (4 MiB) is
  shared by publication, the event server, and the client scanner.
- Product target: **Linux, macOS, Windows**.

## Current implementation status

IMPLEMENTED:

- Durable `RelaySession` persistence: `internal/store` —
  `sessions/<id>/{session.json,transcript.jsonl,transcript.idx}`, atomic
  create/save/delete with explicit commit points, indexed bounded tail
  reads, daemon-restart COLD recovery.
- `RelaySession` ≠ `HarnessRuntime`: runtime is daemon-memory only and
  never persisted; restart reloads sessions COLD.
- Loopback HTTP/JSON control plane (ADR-006): `internal/client` +
  `internal/daemon` — `/v1/daemon`, `/v1/sessions[...]`, transcript, and
  NDJSON event endpoints on `127.0.0.1:0`.
- `run/daemon.json` descriptor discovery: atomic 0600 publication,
  instance ID + 256-bit bearer token rotated per daemon generation,
  fail-closed client validation, owner-only removal.
- Per-daemon Bearer auth on every endpoint (constant-time compare).
- NDJSON canonical event-stream foundation (`internal/events`):
  per-session seq allocator with durable block reservation, exact
  history-cursor snapshots, explicit replay floor, lazily allocated
  bounded replay ring, bounded subscribers with deterministic eviction,
  `after=N` replay, `CURSOR_TOO_OLD` / `CURSOR_AHEAD`.
- Transcript history endpoint with server-side record-count AND byte
  bounds (`hasMoreBefore`), never an unbounded response.

NOT YET IMPLEMENTED:

- Codex adapter (app-server), Devin/OpenCode adapters.
- Real agent-message publication (no harness produces canonical events
  yet; the broker is exercised synthetically in tests).
- TUI, Gateway/WSS bridge.
- Runtime wake from COLD, RuntimeSupervisor policy.
- Cross-platform singleton locking (currently Linux `flock`; the
  invariant is one relayd per state root, fail-closed).

## Scope

- Go implementation, stdlib-first. No CLI/DI/logging/config frameworks.
- Production toolchain: **exactly go1.27.1** (`go 1.27.1`, no floating
  toolchain). CI fails unless `go env GOVERSION == go1.27.1`.
- Linux-only code while the current implementation is Linux-specific;
  do not add speculative Windows/macOS abstractions before the
  control-plane task that actually ports them.

## Gates

Before finalizing any task, run the canonical gates in `GATES.md` and
report each as PASS/FAIL/N/A with evidence.

## Runtime boundaries

- Relay state lives only under `${REPOSUITE_HOME}/relay`
  (default `~/.reposuite/relay`). See `internal/paths`.
- **Never** touch `~/.airelay`, `AIRELAY_*`, Airelay sockets/PIDs/state, or
  live Airelay/Codex sessions. Airelay is reference material, not a runtime
  dependency.
- Tests must use isolated temporary roots — never the developer's real
  `~/.reposuite` or `~/.airelay`.
- Private state is user-private (mode `0700`/`0600` on Unix).

## Dependencies

- **stdlib only** in the production core today — `go.mod` must have no
  third-party `require` entries. Additions need a concrete requirement
  and Planner approval.
- Never import `github.com/gitpod-io/xterm-go`, `github.com/rceman/xterm-go`,
  or `github.com/creack/pty` — all superseded (ADR-005). The standalone
  `rceman/xterm-go` library is maintained independently; do not touch it
  here.

## Harness adapter invariants (ADR-005, implemented)

- The daemon runs only its own built-in harness commands. Clients never
  supply executables or arguments; `internal/codex` resolves the
  installed `codex` CLI itself and runs `codex app-server`.
- Native harness servers are private to relayd: never exposed on a
  socket, descriptor, or API surface.
- Exact resume only: `thread/resume` uses the persisted
  `nativeSessionId`; a mismatch or failure is `NATIVE_SESSION_LOST` —
  never "newest"/`--last`/implicit.
- Native identity is materialized (persisted) only when proven — for
  Codex, after the first completed turn.
- Durable session metadata is mutated only through the daemon's
  `codex.SessionUpdate` → `Daemon.materialize` path, under
  `Managed.MetaMu`. Readers use `Managed.Snapshot()`; never read mutable
  durable fields directly from another goroutine.
- A turn is in flight until its terminal durable record is published;
  unresolved requested input is a hard runtime sleep blocker.
- Runtime death (unexpected) fails in-flight work durably; a deliberate
  stop has no in-flight work by construction. The supervisor reports
  each runtime generation gone exactly once (`OnGone`).
- Runtime creation is claim-first: the first caller publishes a
  `starting` claim and spawns outside the lock; concurrent wake-ups WAIT
  for that generation instead of spawning a second process. The spawn
  closure returns only when the runtime is usable (process started AND
  protocol initialized) and owns reaping any process it started.
- Every process stop is bounded and reaps the whole process group —
  leader, descendants, and the app-server's own children.

## Test seams

- `__fixture` — deterministic same-binary child (fixture harness).
- `__fake-codex` — deterministic fake Codex app-server: scripted
  JSON-RPC, no model call, no network, no quota. Selected with
  `FAKE_CODEX_MODE` (`happy`, `fail-turn`, `input`, `die-on-turn`,
  `resume-error`, `stubborn`, `child`). Hidden modes are never listed in
  help and never part of the public CLI contract.

## Architecture invariants

- One daemon per RepoSuite state root; exclusive singleton ownership with
  fail-closed competing startup (flock on Linux; platform-equivalent
  elsewhere per ADR-006).
- The control plane is the versioned local HTTP API (`/v1`, API version
  in the descriptor) with bounded JSON bodies; it never carries
  client-supplied commands. The daemon only runs the built-in `fixture`
  harness today.
- The descriptor is local authority: clients validate it strictly
  (loopback 127.0.0.1, http, supported version, well-formed token) and
  fail closed on any incompatible peer — they never auto-start against
  one and never delete stale descriptors (the lock owner replaces them).
- Store operations have explicit commit points; a post-commit failure is
  never reported as a clean rollback (`UncertainError` fails the daemon
  closed, `CleanupError` applies the committed outcome).
- Relay-owned durable session metadata is canonical: `internal/store`
  persists `RelaySession`; `HarnessRuntime` is daemon-memory only and
  must never be persisted (no PID/RuntimeID/credentials on disk).
- "Cold" session means **zero** harness runtime resources.
- Do not casually modify immutable spike branches (`spike/*` are
  immutable research references).

## Style

Small, explicit packages. No speculative interfaces, service containers,
event buses, or persistence abstractions. Optimize for correctness,
readability, low overhead, and auditability.
