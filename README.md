# RepoSuite Relay

A persistent structured agent-session daemon with native harness
adapters, implemented in Go.

**Status: early development** — architecture transition to ADR-005/006
(native harness-first) is in progress.

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

A single persistent daemon (Linux: Unix socket
`~/.reposuite/relay/run/relayd.sock`, singleton-locked) owns durable
`RelaySession`s under `~/.reposuite/relay/sessions/<id>/` — sessions
survive daemon restarts and reload COLD (no process, no runtime). The
`fixture` harness is a deterministic development/test child — **not** a
real agent harness.

**Not yet implemented:** native harness adapters (Codex/OpenCode/Devin)
with exact native resume, runtime wake from COLD, the ADR-006 loopback
HTTP/JSON + NDJSON control plane (the current Linux Unix socket remains
until that migration), attach, TUI.

## Layout

- `cmd/reposuite-relay` — CLI
- `internal/daemon` — daemon lifecycle, socket server, control dispatch
- `internal/session` — `RelaySession`/`HarnessRuntime` domain + registry
- `internal/store` — durable session/transcript filesystem store
- `internal/fixture` — deterministic fixture harness child
- `internal/protocol` — bounded JSON control protocol
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
