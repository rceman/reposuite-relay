# RepoSuite Relay

A persistent structured agent-session daemon with native harness
adapters, implemented in Go.

**Status: early development** — the ADR-006 local control plane, the
canonical event-stream foundation, and three native harness adapters
(Codex app-server, Devin ACP, OpenCode ACP) are implemented.

RepoSuite Relay keeps agent sessions alive independently of any viewer:
a persistent per-user daemon owns durable session identities
(`RelaySession`), wakes ephemeral native harness runtimes
(`HarnessRuntime`) on demand, and bridges clients to harness-native
structured protocols — viewers attach and detach without owning the
session's lifetime.

- Standalone CLI: `reposuite-relay`
- Future RepoSuite umbrella invocation: `reposuite relay ...`
- Canonical toolchain: exactly `go1.27.1`
- Production core: **stdlib only** (no third-party dependencies)
- Product target: Linux, macOS, Windows (ADR-006)

## Currently implemented

```console
$ reposuite-relay version
reposuite-relay 0.1.0-dev
$ reposuite-relay help
$ reposuite-relay paths                      # resolved RepoSuite/Relay state paths
$ reposuite-relay serve codex --key work     # create a Codex session (COLD: no process yet)
$ reposuite-relay serve opencode --key oc    # create an OpenCode ACP session (COLD)
$ reposuite-relay serve devin --key dv       # create a Devin ACP session (COLD)
$ reposuite-relay prompt work --text "..."   # submit a prompt as a native turn
$ reposuite-relay status work                # state, runtime, activity, generation
$ reposuite-relay input work --input in_... --answer q1=alpha   # answer requested input
$ reposuite-relay cancel work                # interrupt the in-flight turn
$ reposuite-relay config work --model M      # change the accepted model/mode
$ reposuite-relay stop work                  # delete the session
$ reposuite-relay serve fixture --key demo   # fixture session (dev/test harness)
$ reposuite-relay list                       # managed sessions
$ reposuite-relay daemon status              # daemon liveness (no autostart)
$ reposuite-relay daemon stop                # graceful shutdown
```

A single persistent daemon owns durable `RelaySession`s under
`~/.reposuite/relay/sessions/<id>/` — sessions survive daemon restarts
and reload COLD (no process, no runtime). Clients talk to it over the
ADR-006 **local HTTP control plane**: the daemon binds an ephemeral
loopback port (`127.0.0.1:0`), publishes a user-private `0600`
`~/.reposuite/relay/run/daemon.json` descriptor (endpoint, instance ID,
rotating 256-bit bearer token), and authenticates every endpoint. The
CLI auto-starts the daemon when genuinely absent; `daemon status` and
`daemon stop` never do. The `fixture` harness is a deterministic
development/test child — **not** a real agent harness.

**Native Codex sessions.** `serve codex` creates a durable session with
**zero** harness resources; the first prompt wakes one shared
`codex app-server` process (spawned by relayd, never exposed to clients)
and starts a native `thread`. Each Relay session keeps its own exact
native thread id, persisted only after a turn completes — so resume
always reattaches the exact thread (`thread/resume`) and never
"newest"/`--last`. A prompt publishes `message.user` durably before the
turn is submitted; streaming deltas are live-only while the completed
agent message, interruptions, failures, requested input, runtime exits,
and native materialization are durable transcript records. Unresolved
requested input is a hard runtime sleep blocker. A runtime that dies
fails its in-flight turn durably and drops every bound session COLD;
deleting one session detaches only its thread and leaves the shared
app-server serving the others.

The canonical event-stream foundation is in place: per-session monotonic
sequence numbers with durable block reservation (`SeqHighWatermark`),
an exact history cursor (`throughSeq` is the canonical event cursor, so
the transcript → `events?after=throughSeq` cutover is gap-free even
across restarts), an explicit replay floor (`CURSOR_TOO_OLD` below it,
`CURSOR_AHEAD` above the cursor), a lazily allocated bounded replay ring,
bounded subscribers with deterministic eviction, and one shared 4 MiB
NDJSON frame bound across publication, server, and client.
`GET /v1/sessions/{key}/transcript?limit=N` returns byte- and
count-bounded durable history (`hasMoreBefore`). COLD sessions hold no
transcript file descriptors and no replay rings — a daemon restoring
1000 durable sessions costs bookkeeping only.

**Native ACP sessions.** `serve opencode` and `serve devin` create
durable sessions over the Agent Client Protocol. OpenCode shares one
`opencode acp` runtime; Devin's runtime is partitioned per process model
(`devin acp --model`). Resume is always the exact native session
(`session/load`) — OpenCode's identity is durable from `session/new` (so
a zero-turn cold resume is exact), while Devin's is persisted only after
a completed turn and an unproven one is never resumed. Model/mode changes
go through the runtime's own advertised config options; approvals are
answered by policy (never converted into requested input, which is
`UNSUPPORTED_OPERATION` for ACP harnesses).

**Not yet implemented:** Codex approval requests and `turn/steer`,
rate-limit surfaces, ACP requested-input, TUI, Gateway/WSS,
cross-platform singleton locking.

## Layout

- `cmd/reposuite-relay` — CLI (a local HTTP client of relayd)
- `internal/daemon` — daemon lifecycle, HTTP control plane, descriptor
- `internal/client` — the single local client (descriptor + auth + API)
- `internal/api` — local API v1 wire contract (DTOs, error codes)
- `internal/events` — canonical event broker (seq reservation, ring, subs)
- `internal/session` — `RelaySession`/`HarnessRuntime` domain + registry
- `internal/store` — durable session/transcript filesystem store
- `internal/runtime` — `RuntimeSupervisor`: runtime ownership, bindings,
  activity, sleep blockers, bounded process-tree teardown
- `internal/harness/codex` — Codex app-server adapter (JSON-RPC, protocol types,
  event mapping, deterministic fake app-server for tests)
- `internal/harness/acp` — shared ACP protocol layer (framing, session/turn
  mechanics, update routing, approval policy, deterministic fake ACP agent)
- `internal/harness/devin`, `internal/harness/opencode` — ACP adapters
- `internal/fixture` — deterministic fixture harness child
- `internal/paths` — `${REPOSUITE_HOME}/relay` path contract
- `tools/` — nested developer-tool module: exact `o200k_base` token
  counter and `gofmt-struct` (never imported by production)
- `scripts/` — Go hygiene gates (`check-go-files.sh`, `check-go-format.sh`)
- `docs/` — domain vocabulary, ADRs, feasibility research
- `testdata/` — historical captured research data (terminal spike)

## Development

See `AGENTS.md` and `GATES.md`. Canonical gates:

```bash
scripts/check-go-files.sh HEAD   # fast gate: changed + untracked Go files
scripts/check-go-files.sh --all  # full gate: gofmt + gofmt-struct + <=3000 tokens
go vet ./... && go test ./... && go test -race -count=1 ./... && go build ./...
(cd tools && go test ./... && go test -race ./... && go build ./...)
```

Relay state defaults to `~/.reposuite/relay`
(`$REPOSUITE_HOME` overrides the RepoSuite root). Nothing is read or written
under `~/.airelay` — Airelay is a separate legacy product.

Each adapter resolves the installed CLI itself (`codex` →
`codex app-server`, `opencode` → `opencode acp`, `devin` → `devin acp
[--model M]`) — clients can never supply a command or executable. Tests
never call a model: they spawn a deterministic fake app-server
(`__fake-codex`) or fake ACP agent (`__fake-acp`) in the same binary.
