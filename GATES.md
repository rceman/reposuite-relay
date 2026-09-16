# GATES.md — canonical production gates

Run every gate before finalizing a task. All commands use the canonical
toolchain: exactly **go1.27.1**.

## Gates

| # | Gate | Command / check |
|---|------|-----------------|
| 1 | Toolchain | `go env GOVERSION` prints `go1.27.1` |
| 2 | Format | `gofmt -l .` prints nothing |
| 3 | Tidy | `go mod tidy` produces no diff (`git diff --exit-code -- go.mod go.sum`) |
| 4 | Vet | `go vet ./...` clean |
| 5 | Test | `go test ./...` all pass |
| 6 | Race | `go test -race -count=1 ./...` all pass |
| 7 | Build | `go build ./...` ok |
| 8 | Dependency shape | go.mod has **no third-party `require`**; no `replace`; no `github.com/creack/pty`, `github.com/rceman/xterm-go`, or `github.com/gitpod-io/xterm-go` anywhere in Go code or the module; no `AIRELAY_`/`~/.airelay` usage |
| 9 | CLI | `reposuite-relay version` and `reposuite-relay help` succeed; unknown command fails non-zero |
| 10 | Daemon regression | covered by gate 5 — singleton race, two-session isolation, duplicate key, stale socket, malformed client, shutdown quiescence, create/stop-vs-shutdown, forced-kill stop, orphan cleanup, permissions, churn |

## Notes

- Terminal/PTY regression gates were removed in Task A1 — the terminal
  engine and PTY layer are superseded (ADR-005) and no longer part of
  production. Historical evidence: `docs/research/GO_TERMINAL_SPIKE.md`
  and `testdata/codex-startup.raw` (static captured research data; the
  capture machinery was removed with the terminal layer).
- Live harness invocations and model-quota operations are **never**
  part of canonical gates.
