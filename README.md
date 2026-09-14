# RepoSuite Relay

A managed agent runtime/session relay, implemented in Go.

**Status: early development.** Linux-only.

RepoSuite Relay keeps agent (harness) terminal sessions alive independently
of any viewer: a persistent per-user daemon owns the PTY, the harness
process, and an authoritative headless terminal; viewers attach and detach
without owning the session's lifetime.

- Standalone CLI: `reposuite-relay`
- Future RepoSuite umbrella invocation: `reposuite relay ...`
- Terminal engine: [`rceman/xterm-go`](https://github.com/rceman/xterm-go)
  (maintained downstream fork of `gitpod-io/xterm-go`)
- Canonical toolchain: exactly `go1.27.1`

## Currently implemented

```console
$ reposuite-relay version
reposuite-relay 0.1.0-dev
$ reposuite-relay help
$ reposuite-relay paths    # resolved RepoSuite/Relay state paths
```

Daemon, session management, hibernation, and attach are designed
(`docs/adr/ADR-002`) but **not yet implemented** — no such commands exist
yet.

## Layout

- `cmd/reposuite-relay` — CLI
- `internal/terminal` — headless terminal model (xterm-go + host services)
- `internal/pty` — Linux PTY runner
- `internal/paths` — `${REPOSUITE_HOME}/relay` path contract
- `docs/` — domain vocabulary, ADRs, feasibility research, dependency policy

## Development

See `AGENTS.md` and `GATES.md`. Canonical gates:

```bash
go vet ./... && go test ./... && go test -race -count=1 ./... && go build ./...
```

Relay state defaults to `~/.reposuite/relay`
(`$REPOSUITE_HOME` overrides the RepoSuite root). Nothing is read or written
under `~/.airelay` — Airelay is a separate legacy product.
