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
- Native harness adapters under `internal/harness/<name>`: the Codex
  app-server adapter (`internal/harness/codex`) with stdio JSON-RPC,
  thread/turn handling, requested-input mapping, and the deterministic
  fake app-server test seam; the shared ACP protocol layer
  (`internal/harness/acp`) with the Devin (`internal/harness/devin`) and
  OpenCode (`internal/harness/opencode`) adapters on top of it.
- Canonical event payloads are shared by every adapter
  (`internal/api/events.go`): one wire shape per event regardless of the
  harness that produced it.
- `internal/runtime` supervisor: one shared or dedicated harness process
  per runtime key, claim-first creation, activity/sleep-blocker policy,
  bounded process-group teardown, runtime-gone notification.
- Runtime wake from COLD and the native session control surface
  (`serve codex`, `prompt`, `status`, `config`, `input`, `cancel`,
  `stop`) over the loopback HTTP API and CLI.

NOT YET IMPLEMENTED:

- Codex approval flows and `turn/steer`; rate-limit surfaces beyond the
  canonical metrics projection.
- ACP requested-input: neither ACP harness exposes Relay's input surface,
  so `input` is `UNSUPPORTED_OPERATION` there (approvals are handled by
  policy, never converted into input).
- ACP session listing/deletion beyond Relay's own registry, ACP
  `authenticate` beyond the initialize handshake, and native ACP tool-call
  or plan surfacing (ignored deliberately, not guessed at).
- TUI, Gateway/WSS bridge.
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
  supply executables or arguments; `internal/harness/codex` resolves the
  installed `codex` CLI itself and runs `codex app-server`.
- Native harness servers are private to relayd: never exposed on a
  socket, descriptor, or API surface. This holds for ACP too: the ACP
  agent is a stdio child of relayd, and no API route proxies it.
- ACP streaming notifications carry the **native** session identity, so
  every ACP adapter resolves native → Relay session before publishing; an
  unknown native session is ignored, never guessed at.
- Materialization is per-vendor and must be honored exactly: OpenCode's
  `ses_*` is durable from `session/new` (so a zero-turn cold resume is
  exact), Devin's slug is durable only after a completed turn (an unproven
  slug is dropped with its generation and never resumed). A non-empty
  durable identity that cannot be parsed is `NATIVE_SESSION_LOST` before
  any process is spawned.
- Devin's model is a PROCESS-level choice (`devin acp --model`), so Devin
  runtime keys are partitioned by model. A WARM idle model change detaches
  the session from the incompatible generation (the shared old runtime
  stays alive) and the next prompt enters the desired-model generation
  with the exact proven slug via `session/load`; a model change during a
  turn or unresolved input is `SESSION_BUSY`. Relay selects the
  advertised `bypass` mode explicitly and records no mode it did not set.
- Approvals are answered by the shared permission policy: select by
  advertised kind, never by position, and fail closed (outcome
  `cancelled`) when unclassifiable. An approval is never converted into
  Relay requested input.
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
  leader, descendants, and the app-server's own children. Process reap
  alone is not quiescence: adapter-owned goroutines (transport readers,
  turn-completion workers) may still be finishing durable writes, and
  crash-path `OnRuntimeGone` runs on a supervisor watcher goroutine.
  `Supervisor.WaitWatchers()` and each adapter's `WaitQuiescent()` are
  the drain seams teardown (daemon shutdown, tests) must call after
  `StopAll` before reclaiming state those goroutines could still touch.

### Cross-harness semantics (hardened)

- **Desired vs effective config:** `RelaySession.Model`/`Mode` are the
  durable DESIRED values; `SessionMetrics.Model`/`Mode` are last-known
  EFFECTIVE values, populated only from runtime-observed state. A COLD
  config change writes desired state and wakes nothing; the deferred
  values are applied through the advertised native config surface at the
  next materialization, BEFORE the first prompt (a rejected deferred
  value fails before `session/prompt` and never becomes "effective").
  A live native mutation is a supervisor sleep blocker
  (`BeginMutation`), and any config change during a turn or unresolved
  input is `SESSION_BUSY` on all native harnesses.
- **Per-turn overrides:** `prompt --model`/`--effort` are Codex-only;
  OpenCode and Devin reject non-empty overrides with
  `UNSUPPORTED_OPERATION` before `message.user` is persisted.
- **Runtime.Key vs Runtime.ID:** the supervisor key (`codex/default`,
  `opencode/acp`, `devin/acp/<model>`) is a stable internal grouping —
  never an API value. `Runtime.ID` is one process generation's
  ephemeral id; `runtimeId` in prompt responses, `harness.started`,
  `runtime.exited`, and `SessionInfo` all name the same generation.
- **Generation = successful native runtime bindings:** create 0, first
  bind 1, same-runtime prompts unchanged, cold exact resume +1.
  Materialization is orthogonal: a failed first turn may leave
  `Generation=1` with `nativeSessionId=""` (a binding existed, no
  resumable identity was proven).
- **Bind is a transaction:** `Supervisor.Bind` runs before the durable
  commit that carries the generation bump; a `Bind` failure leaves
  Generation untouched, and a failed durable commit rolls the
  supervisor binding back synchronously (`Unbind` + detach the native
  handle + remove routing) — no phantom binding, no phantom
  generation. `session.native` and `harness.started` are post-commit
  events only.
- **Terminal exactly-once:** `acp.Turn` carries an atomic terminal
  claim — the prompt-response path or the runtime-death `Fail` wins,
  the loser is discarded. `completeTurn` is the sole publisher of
  `turn.*` terminal records; `OnRuntimeGone` snapshots the in-flight
  turn under the adapter lock and only `Fail`s it, then publishes
  `runtime.exited` per affected session.
- **ACP RPC correlation:** responses are delivered to pending calls;
  late responses to context-abandoned calls are consumed once from a
  bounded set (64); any other response id is protocol corruption and
  closes the connection (`ErrProtocolCorruption`).

## Test seams

- `__fixture` — deterministic same-binary child (fixture harness).
- `__fake-codex` — deterministic fake Codex app-server: scripted
  JSON-RPC, no model call, no network, no quota. Selected with
  `FAKE_CODEX_MODE` (`happy`, `fail-turn`, `input`, `die-on-turn`,
  `resume-error`, `stubborn`, `child`). Hidden modes are never listed in
  help and never part of the public CLI contract.
- `__fake-acp` — deterministic fake ACP agent (both vendors) behind the
  same rule: scripted ACP over stdio, no model call, no network, no quota.
  Selected with `FAKE_ACP_VENDOR` (`opencode`, `devin`), `FAKE_ACP_MODE`
  (`happy`, `thought`, `cancel`, `permission`, `permission-unknown`,
  `config-reject`, `config-slow`, `fail-turn`, `no-load`, `load-error`,
  `die-on-prompt`, `die-on-permission`, `die-idle`, `unknown-request`,
  `malformed`, `oversized`, `stubborn`, `child`) and `FAKE_ACP_STATE`
  (the fake's own on-disk native session store, so exact resume and
  zero-turn behavior are observable from outside).
- `internal/harness/harnessenv` is **test-only** scaffolding (real store +
  broker + supervisor + fake-agent command) shared by the adapter suites.
  It is not imported by production code.

## Task reports

- For prompt-driven tasks (a message with `PROMPT_ID:`), always end the
  final report with the `PROMPT_ID:` line — and `PROMPT_STATUS:
  COMPLETE` / `BLOCKED` when the prompt defines it.

## Git remotes

- `origin` is HTTPS (`https://github.com/rceman/reposuite-relay`) and this
  environment has no HTTPS credential helper or `gh` — `git push` to it
  fails with "could not read Username". Push over SSH instead, with the
  explicit URL (no config change needed):

  `git push git@github.com:rceman/reposuite-relay.git <branch>`

  SSH auth as `rceman` works via `~/.ssh/config` (`Host github.com` →
  `id_rsa_github_rceman`). Verify with `git ls-remote
  git@github.com:rceman/reposuite-relay.git refs/heads/<branch>` — the
  local `origin/*` tracking refs do not refresh from URL pushes, so
  `ls-remote` is the authority on remote state.

## Repository hygiene

- **Harness adapters live under `internal/harness/<name>`.** Everything
  that speaks a vendor's native protocol (DTOs, JSON-RPC, thread/turn
  handling, process adapter, fake protocol, adapter tests) belongs to
  that package. `internal/harness` is a namespace: no shared abstraction
  until two adapters need the same concrete contract. Generic
  responsibility stays generic — `internal/runtime` (process
  supervision), `internal/events`, `internal/session`, `internal/store`,
  `internal/daemon` (orchestration + HTTP).
- **`internal/harness/acp` owns only ACP protocol-family concerns** —
  framing, handshake, session/turn mechanics, streaming update routing,
  approval policy, the fake ACP agent. It must never contain
  vendor-specific behavior or Relay session policy. Vendor differences
  (identity shape, materialization rule, process-level model, mode
  policy) stay in `internal/harness/devin` and
  `internal/harness/opencode`.
- **Hand-written Go file hard limit: <=3000 o200k_base tokens**, counting
  the complete file (code, comments, strings, tests, tooling). Files with
  the standard `// Code generated ... DO NOT EDIT.` header are excluded;
  there is no hand-written allowlist. Split by responsibility when a file
  grows past the limit — never delete useful comments, minify code, or add
  exemption comments to fit. Substantial files should land around
  1000-2500 tokens to leave headroom.
- **Normal formatting: `gofmt`. Structural formatting: `gofmt-struct`** —
  multi-field keyed struct literals are written one field per line; maps,
  arrays, slices, unkeyed, and single-field literals are untouched.
- **Fast changed-file gate:** `scripts/check-go-files.sh [BASE]` (default
  `HEAD`) — gofmt + gofmt-struct + token budget over changed and untracked
  Go files.
- **Full gate (authoritative):** `scripts/check-go-files.sh --all`
  (`scripts/check-go-format.sh --all` for structure only). CI never
  rewrites; run `scripts/check-go-format.sh --write --all` locally.
- Exact counting lives in the nested developer module `tools/`
  (tiktoken-go): production stays stdlib-only and never imports `tools/`.
  The encoding payload is cached once at `$REPOSUITE_TOKEN_CACHE_DIR`
  (default `$XDG_CACHE_HOME/reposuite-relay/tiktoken`), so repeated gates
  do no network work.

Why: bounded agent context, cohesive source ownership, easier review, and
no giant accumulation files.

## Architecture invariants

- One daemon per RepoSuite state root; exclusive singleton ownership with
  fail-closed competing startup (flock on Linux; platform-equivalent
  elsewhere per ADR-006).
- The control plane is the versioned local HTTP API (`/v1`, API version
  in the descriptor) with bounded JSON bodies; it never carries
  client-supplied commands. Clients cannot supply executables or
  arguments: relayd executes only its own built-in harness
  implementations — the `fixture` test harness and the Codex native
  adapter (`internal/harness/codex`).
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
