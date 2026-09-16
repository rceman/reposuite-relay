# RepoSuite Relay

A persistent structured agent-session daemon with native harness
adapters, implemented in Go.

**Status: early development** — the ADR-006 local control plane and the
canonical event-stream foundation are implemented; native harness
adapters are next.

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
$ reposuite-relay serve fixture --key demo   # start a fixture session (dev/test harness)
$ reposuite-relay list                       # managed sessions
$ reposuite-relay status demo                # one session
$ reposuite-relay stop demo                  # stop it
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
1000 durable sessions costs bookkeeping only. No harness adapter
publishes real events yet — the broker is exercised synthetically in
tests.

**Not yet implemented:** native harness adapters (Codex/OpenCode/Devin)
with exact native resume, real agent-message publication, runtime wake
from COLD, TUI, Gateway/WSS, cross-platform singleton locking.

## Layout

- `cmd/reposuite-relay` — CLI (a local HTTP client of relayd)
- `internal/daemon` — daemon lifecycle, HTTP control plane, descriptor
- `internal/client` — the single local client (descriptor + auth + API)
- `internal/api` — local API v1 wire contract (DTOs, error codes)
- `internal/events` — canonical event broker (seq reservation, ring, subs)
- `internal/session` — `RelaySession`/`HarnessRuntime` domain + registry
- `internal/store` — durable session/transcript filesystem store
- `internal/fixture` — deterministic fixture harness child
- `internal/paths` — `${REPOSUITE_HOME}/relay` path contract
- `docs/` — domain vocabulary, ADRs, feasibility research
- `testdata/` — historical captured research data (terminal spike)

## Development

See `AGENTS.md` and `GATES.md`. Canonical gates:

```bash
go vet ./... && go test ./... && go test -race -count=1 ./... && go build ./...
```

Relay state defaults to `~/.reposuite/relay`
(`$REPOSUITE_HOME` overrides the RepoSuite root). Nothing is read or written
under `~/.airelay` — Airelay is a separate legacy product.
