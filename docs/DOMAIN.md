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
(`STARTING|WARM|ACTIVE|STOPPING`), PID, start time. Daemon-memory only —
never persisted; its absence IS `COLD`. One runtime may host multiple
sessions (target); sleep authority is per-runtime (target).

**HarnessAdapter** *(target)* — the protocol-specific bridge between a
native harness protocol (Codex app-server, OpenCode ACP/serve, Devin
ACP) and Relay's canonical session/event model. No adapter is
implemented yet.

**RuntimeSupervisor** *(target)* — owns runtime wake/sleep/lifecycle:
ensure-awake, protocol init, exact native resume, prompt dispatch,
per-runtime `SafeToSleep`, stop, and process-tree accounting.

**NativeSessionID** *(current)* — the exact harness-native resume
identity stored on `RelaySession` (empty for fixture, which has no
native durable identity). Resume always uses the exact identity — never
"newest", `--last`, timestamps, or implicit current session.

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

**RequestedInput** *(target)* — a blocking question/input request a
harness surfaces through the adapter (permission prompt, choice,
free-text). It is responder-agnostic: any attached client may answer.
An unresolved `RequestedInput` is a **sleep blocker** — a runtime must
not sleep while one is pending (native pending-request state is not
resumable).

## Events

**Canonical events** *(current foundation)* — sequence-numbered per
session: `session.state`, `message.user`, `message.agent.delta`,
`message.agent.completed`, `input.requested`, `input.resolved`,
`session.metrics`, `session.error`. Durable records persist to the
session transcript; transient deltas (streaming, fast-changing metrics)
are live-only. The sequence allocator, replay ring, subscriptions, and
NDJSON streaming are implemented by `internal/events`; no harness
adapter publishes real events yet.

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
