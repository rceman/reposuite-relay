# Dependency: rceman/xterm-go

RepoSuite Relay's terminal engine is our maintained downstream fork of
`gitpod-io/xterm-go` (a pure-Go port of headless xterm.js, MIT license).

## Pins

- **Canonical module:** `github.com/rceman/xterm-go`
- **Canonical commit:** `f690bbc9735dc7a3a34fd1025da37b6d72289588`
  (module version `v0.0.0-20260914111126-f690bbc9735d`)
- **Upstream:** `github.com/gitpod-io/xterm-go`
- **Upstream baseline:** `dae5128cb6b377a559b07d4a2d9eb1321f05e390`

No `replace` directive is used; the canonical module path is imported
directly.

## Maintenance model

See `DOWNSTREAM.md` in the xterm-go repository. In short:

- upstream changes are adopted **selectively** (inspect, review, port,
  test, adopt);
- local fixes may land in the fork immediately when Relay needs them,
  gated by tests;
- no automatic upstream synchronization;
- the fork stays a generic terminal library — no RepoSuite logic.

## Update policy (bumping the Relay pin)

1. Inspect the upstream/downstream diff for the candidate commit.
2. Run xterm-go's own `go test` and `go test -race`.
3. Run Relay's terminal regression suite (`go test ./...` here).
4. Run the real Codex fixture replay (`internal/terminal` codex tests).
5. Verify snapshot + live-continuation tests specifically.
6. Then update the Relay `go.mod` pin to the new exact commit.

## Known limitation

DECSTBM-in-snapshot homes the restored cursor (upstream-inherited;
documented in `docs/research/GO_TERMINAL_SPIKE.md`). Non-blocking;
targeted for a future focused fix in `rceman/xterm-go`.

## Pre-stable-release audit

**Required** before RepoSuite Relay stable/public release: a focused
code/security audit of `rceman/xterm-go` covering parser attack surface
(CSI/OSC/DCS/APC), malformed/unbounded sequences, panic paths,
memory/allocation abuse, serializer correctness, concurrency/races,
unsafe/syscall usage, fuzzing posture, and supply-chain review. Ordinary
unit tests do not constitute that audit.
