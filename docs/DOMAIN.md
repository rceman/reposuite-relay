# RepoSuite Relay — Domain vocabulary

This document defines the shared vocabulary for RepoSuite Relay. Terms
marked *target* are the accepted architecture (ADR-005/ADR-006); terms
marked *current* describe what the code does today.

## Product

**RepoSuite Relay** — a persistent structured agent-session daemon with
native harness adapters. It owns harness sessions on behalf of viewers
and keeps them alive independently of any single attachment. It is not a
terminal relay: no PTY, no terminal emulator, no xterm dependency in the
core.

- Standalone CLI: `reposuite-relay`
- Future RepoSuite umbrella invocation: `reposuite relay ...` — dispatches
  to the same Relay implementation.
- Daemon: one `relayd` per RepoSuite state root — currently realized as
  the hidden `reposuite-relay __daemon` mode of the same binary (the
  `reposuite-relayd` name remains the future packaged form).
  - *Current:* loopback TCP `127.0.0.1:0` with HTTP/JSON commands,
    streaming NDJSON events, and a `0600`
    `${REPOSUITE_HOME}/relay/run/daemon.json` descriptor carrying
    endpoint + instance ID + per-generation bearer token. Singleton via
    `flock` on `run/relayd.lock` (Linux; platform-appropriate locking on
    macOS/Windows is *target*).

## Sessions

**RelaySession** *(current)* — the durable logical Relay session
identity: ID, key, harness, `NativeSessionID`, state, model/mode,
generation, timestamps. It survives TUI/web disconnect, harness runtime
sleep/death, and relayd restart — persisted under
`sessions/<id>/session.json` by `internal/store`.

**HarnessRuntime** *(current)* — the ephemeral owned runtime for one
harness generation: ID, generation, state
(`starting|warm|active|stopping`), PID, start time, bound sessions,
per-session activity. Daemon-memory only — never persisted; its absence
IS `COLD`. A runtime may host multiple sessions (a shared Codex
app-server); sleep authority is per-runtime.

**HarnessAdapter** *(current)* — the protocol-specific bridge between a
native harness protocol and Relay's canonical session/event model.
Implemented: `internal/harness/codex` (Codex app-server over stdio),
`internal/harness/opencode` and `internal/harness/devin` (both over the
shared ACP layer `internal/harness/acp`). An adapter owns only
daemon-memory state; it never writes durable session metadata itself (the
daemon's `Materialize` hook does that under `MetaMu`).

**ACP** *(current)* — the Agent Client Protocol, spoken over stdio by
both the OpenCode and the Devin runtime. `internal/harness/acp` owns the
protocol family only: JSON-RPC framing, the `initialize` handshake and
capabilities, session open/load, prompt turns, streaming `session/update`
routing, `session/cancel`, the advertised config surface, approval
requests, and the deterministic fake agent. Vendor differences live in
the vendor adapter, never here.

**Runtime key** *(current)* — the supervisor key that partitions runtime
generations. Codex uses one shared app-server per generation; OpenCode
uses one shared ACP runtime (`opencode/acp`); Devin uses one runtime PER
PROCESS MODEL (`devin/acp/<model>`), because `devin acp --model` chooses
the model for every new session in that process.

**Native materialization** *(current)* — the moment a native identity is
proven durable and therefore persisted. It is per-vendor: Codex and Devin
persist only after the first COMPLETED turn (an interrupted or failed
first turn persists nothing), while OpenCode's `ses_*` identity is
durable from `session/new`, so a zero-turn OpenCode session resumes
exactly after a cold sleep. An unproven Devin slug is dropped with its
generation — it is never resumed.

**Permission policy** *(current)* — how Relay answers a native approval
request. Relay runs harnesses with full permissions, so the strongest
advertised allow option is selected BY KIND (`allow_always` before
`allow_once`), never by position; a request with no recognizable allow
option is answered `cancelled` (fail closed). An approval is never
converted into Relay `RequestedInput`.

**RuntimeSupervisor** *(current)* — owns runtime wake/sleep/lifecycle:
ensure-awake, protocol init, exact native resume, prompt dispatch,
per-runtime `SafeToSleep`, stop, and process-tree accounting. It is the
only owner of process state; it reports each runtime generation gone
exactly once (`OnGone`) whether the process died or was stopped
deliberately.

**Session activity** *(current)* — per-session runtime activity
(`idle`, `active`, `waiting_input`), distinct from both the logical
session state and the runtime process state. Activity is what decides
whether a runtime may sleep.

**NativeSessionID** *(current)* — the exact harness-native resume
identity stored on `RelaySession` (empty for fixture, which has no
native durable identity, and empty for an ACP session whose identity is
not yet proven). Resume always uses the exact identity — never "newest",
`--last`, timestamps, or implicit current session. A malformed durable
identity is `NATIVE_SESSION_LOST` and never spawns a process.

**RuntimePolicy** *(target)* — per-adapter/runtime sleep policy:
conceptually `warmGrace`, `coldResumeSupported`, `sleepBlockers`. No
universal idle timeout exists; defaults derive from measured PSS and
spawn/resume/cold-prompt latency.

**COLD resource model** *(current)* — a COLD session costs bookkeeping
only: event state retains no transcript file descriptors (operations
open → work → close) and the bounded replay ring is allocated lazily on
first publish. A daemon restoring 1000 durable sessions holds no
per-session descriptors and no replay rings.

**COLD** *(target)* — a runtime (or bound session state) with **zero**
harness resources: no process, no sockets, ~0 RAM. Cold sleep is not
suspension; the runtime is gone and is re-created on wake.

## Requests

**RequestedInput** *(current)* — a blocking question/input request a
harness surfaces through the adapter (choice, free-text). Implemented for
Codex only: neither ACP harness exposes Relay's input surface, so
`input` on an ACP session is `UNSUPPORTED_OPERATION` and ACP approvals
are handled entirely by the permission policy. It is responder-agnostic: any attached client may answer by
its Relay input ID. An unresolved `RequestedInput` is a **sleep
blocker** — a runtime must not sleep while one is pending (native
pending-request state is not resumable), and the logical session state
is `waiting_input`. Aborting (turn end, cancel, runtime death, session
delete) publishes `input.aborted`.

## Events

**Canonical events** *(current)* — sequence-numbered per session.
Durable: `message.user`, `message.agent.completed`, `turn.interrupted`,
`turn.failed`, `runtime.exited`, `harness.started`, `input.requested`,
`input.resolved`, `input.aborted`, `session.native`, `session.config`.
Transient (live-only): `message.agent.delta`, `metrics.updated`,
`harness.error`. Durable records persist to the session transcript;
transient deltas and fast-changing metrics are live-only. The sequence
allocator, replay ring, subscriptions, and NDJSON streaming are
implemented by `internal/events`, and the Codex adapter publishes real
events through them.

**Native session materialization** *(current)* — `session.native`
records the exact native identity the first time it is proven (for Codex:
after the first completed turn). A failed first turn persists nothing,
and `harness.started` reports `resumed: true|false` so clients can see
whether a runtime generation started or reattached a thread.

**Event sequence (`seq`)** *(current)* — a per-session strictly
monotonic uint64 assigned under the session lock. Durable events are
appended to the transcript **before** the seq is observable to any
subscriber. Transient events consume seq without being persisted, so a
restarted daemon cannot recompute the next seq from the transcript.

**Event cursor** *(current)* — the highest seq consumed in the current
daemon generation; after a restart it IS the persisted
`SeqHighWatermark`. History snapshots return it as `throughSeq`: the
cursor a client subscribes after (`events?after=throughSeq`) for a
gap-free cutover. It is deliberately not the last durable record seq —
transient events and reserved blocks push it beyond durable records, and
those seq values can never be replayed.

**Replay floor** *(current)* — the explicit boundary below which exact
replay cannot be guaranteed. It starts at the persisted watermark
(everything at or below belongs to previous generations) and advances
when the bounded replay ring overwrites an event. Exact subscription
requires `replayFloor <= after <= cursor`; below the floor the request is
refused with `CURSOR_TOO_OLD`, above the cursor with `CURSOR_AHEAD` —
never with silent gaps or `+1` arithmetic.

**Frame/page bounds** *(current)* — one canonical NDJSON frame bound
(4 MiB) is enforced identically at publication, by the event server, and
by the client scanner. Transcript pages are bounded by record count AND
source bytes with `hasMoreBefore` reporting truncation; a single record
larger than the page budget fails explicitly rather than allocating an
unbounded response.

**SeqHighWatermark** *(current)* — durable reservation of sequence
space on `RelaySession` (persisted in `session.json`). A daemon reserves
a block of sequence numbers (watermark update is committed **before**
any seq in the block is exposed); every seq ≤ the watermark is
permanently consumed — allocated, transient, or never used. Restart
resumes strictly above the watermark. A transcript record above the
watermark is canonical corruption and fails session startup.

**Transcript** *(current)* — Relay-owned compact durable event history
per session, filesystem-first
(`sessions/<id>/{session.json,transcript.jsonl,transcript.idx}`) with
indexed bounded-tail reads — implemented by `internal/store`. Relay
never duplicates model context; the native store stays authoritative
for that.

**Reconnect cutover** *(current)* — hydrate from the transcript
(`throughSeq`), then subscribe with `after=<throughSeq>`: the stream
replays everything after that seq from the bounded ring, then continues
live, so no event is lost or duplicated across the cutover. A cursor
older than the ring is refused with `CURSOR_TOO_OLD` rather than
silently returning an incomplete stream.

## Attachments

**Attachment** *(target)* — a client consuming a session's state and
events through the local protocol. Attach/read is a pure Relay-store
operation: it must **not** wake a `COLD` runtime. A prompt/input action
is what wakes the runtime.

## Superseded terms

`ActiveGeneration`, `VirtualTerminal`, controlling-attachment PTY
ownership, and the headless-terminal model are superseded by ADR-005.
Historical ADRs (ADR-001/002) and `docs/research/GO_TERMINAL_SPIKE.md`
preserve that evidence unchanged.
