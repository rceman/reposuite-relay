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
| 8 | Dependency shape | no `replace` in go.mod; no `github.com/gitpod-io/xterm-go` in code; no `AIRELAY_`/`~/.airelay` usage |
| 9 | CLI | `reposuite-relay version` and `reposuite-relay help` succeed; unknown command fails non-zero |
| 10 | Terminal regression | covered by gate 5 — query/reply, snapshot roundtrip, SGR continuation, alt buffer, BCE/bg, Unicode, chunking, concurrency, Codex fixture replay |
| 11 | Daemon regression | covered by gate 5 — singleton race, two-session isolation, duplicate key, stale socket, malformed client, shutdown, permissions, churn |

## Notes

- Node is **not** a production gate. The xterm.js oracle comparison lives on
  the spike branches as research/release-validation evidence.
- `CAPTURE_CODEX=1` regenerates `testdata/codex-startup.raw` in an isolated
  HOME; it is opt-in, never part of normal gates, and never touches real
  user state.
