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
- Relay owns durable metadata + compact transcript (next core step, A2);
  native stores remain authoritative for model context.
- Native harness servers are **private to relayd** — never exposed to
  clients.
- Canonical local protocol (ADR-006): loopback TCP `127.0.0.1:0`,
  HTTP/JSON commands, streaming NDJSON events, `run/daemon.json`
  descriptor.
- Product target: **Linux, macOS, Windows**.

## Current implementation status (transition)

- The daemon/session lifecycle foundation (ADR-003) and the fixture
  harness exist and pass gates.
- The current control plane is still the **existing Linux Unix-socket**
  implementation pending the ADR-006 migration; platform-specific
  locking (`flock`) is an implementation detail, not an invariant —
  the invariant is exactly one relayd per state root, fail-closed.
- Native harness adapters, durable persistence, TUI: **not implemented**.

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

## Architecture invariants

- One daemon per RepoSuite state root; exclusive singleton ownership with
  fail-closed competing startup (flock on Linux; platform-equivalent
  elsewhere per ADR-006).
- The control protocol is versioned bounded JSON; it never carries
  client-supplied commands. The daemon only runs the built-in `fixture`
  harness today.
- Relay-owned durable session metadata is the canonical direction (A2);
  the in-memory registry is current implementation, not the target.
- "Cold" session means **zero** harness runtime resources.
- Do not casually modify immutable spike branches (`spike/*` are
  immutable research references).

## Style

Small, explicit packages. No speculative interfaces, service containers,
event buses, or persistence abstractions. Optimize for correctness,
readability, low overhead, and auditability.
