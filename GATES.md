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
| 9 | CLI | `reposuite-relay version` and `reposuite-relay help` succeed; unknown command fails non-zero; the native session surface (`serve codex`, `prompt`, `status`, `config`, `input`, `cancel`, `stop`) works end to end against the fake app-server |
| 10 | Daemon regression | covered by gate 5 — singleton race (one daemon, one descriptor generation, all clients converge), two-session isolation, duplicate key, malformed/bounded HTTP requests, shutdown quiescence, create/stop-vs-shutdown, forced-kill stop, orphan cleanup, permissions, churn |
| 11 | Persistence regression | covered by gate 5 — durable session reload across daemon restart (COLD, same RelaySession ID), atomic store create/delete + temp/tombstone recovery, store commit-point semantics under injected failures, transcript index/rebuild/tail, explicit stop deletion, corrupt-store startup refusal, persisted-session count |
| 12 | Descriptor lifecycle | covered by gate 5 — atomic 0600 publication, endpoint matches the bound listener, fresh instance ID + token per generation, owner-only removal, absent on failed startup, malformed/non-loopback descriptors rejected client-side |
| 13 | HTTP auth | covered by gate 5 — every endpoint (daemon, sessions, transcript, NDJSON stream, shutdown) requires the bearer token; wrong/missing token is 401; token never appears in status output, session metadata, or transcript |
| 14 | Event sequencing | covered by gate 5 — durable block reservation before exposure, restart resumes strictly after the persisted watermark, transient-only history still never reuses seq, transcript-beyond-watermark fails startup |
| 15 | Event stream + bounds | covered by gate 5 — NDJSON replay-then-live with strictly increasing seq, `CURSOR_TOO_OLD`/`CURSOR_AHEAD` as JSON errors before streaming, disconnect releases the subscriber, bounded replay ring/subscriber queues with deterministic eviction, transcript limit bounds |
| 16 | Exact history cursor | covered by gate 5 — `throughSeq` is the canonical event cursor (post-restart: the persisted watermark, not the last durable record); snapshot is atomic with publishes; cutover at that cursor is gap-free for durable and transient intervening events |
| 17 | Replay floor | covered by gate 5 — floor starts at the persisted watermark and advances on ring overwrite; exact subscription only when `floor <= after <= cursor`; MaxUint64 cursor rejects deterministically |
| 18 | COLD resource bound | covered by gate 5 — 1000 restored sessions allocate no replay rings, retain no transcript file descriptors (Linux `/proc/self/fd` delta bound), and still serve exact-cursor history |
| 19 | Frame/page byte bounds | covered by gate 5 — publication refuses frames above the shared 4 MiB bound (nothing stored or exposed), a just-below-bound frame publishes, history pages obey count+byte budgets with `hasMoreBefore`, and an oversized canonical record fails explicitly |
| 20 | Harness protocol | covered by gate 5 — JSON-RPC request/response correlation, out-of-order responses, notification dispatch, inbound server requests with deferred responses, unknown requests answered, malformed/oversized frames fail closed, pending calls failed on process death, bounded stderr tail |
| 21 | Native session lifecycle | covered by gate 5 — COLD create spawns nothing, materialization only after a completed turn, exact native resume after runtime death and after daemon restart, `NATIVE_SESSION_LOST` on resume failure (including a malformed durable native id, which never spawns a runtime), one in-flight turn per session (`SESSION_BUSY`), cancel (`NO_ACTIVE_TURN` when idle), durable event order (`message.user` → `harness.started` → `session.native` → `message.agent.completed`), history→live cutover at `throughSeq` |
| 22 | Requested input | covered by gate 5 — durable `input.requested` with the exact input ID and questions, durable `waiting_input` before the event, runtime unsleepable while pending (`CanSleep` false, `StopIfIdle` → `ErrRuntimeBusy`), answer by exact ID, `UNKNOWN_INPUT` otherwise, `input.aborted` on turn end/cancel/death |
| 23 | Shared runtime + isolation | covered by gate 5 — concurrent cold wake-ups spawn exactly one app-server process (claim-first creation; 16-way supervisor race + racing HTTP prompts), several sessions share one app-server process with distinct native threads, deleting one detaches only its thread (runtime survives, siblings keep their exact native identity), runtime death projects every bound session COLD and a fresh generation resumes each exact thread |
| 24 | Process tree + COLD projection | covered by gate 5 — stopping a Codex runtime reaps the app-server and its own child process, the session survives COLD with its native identity, and a live event subscriber on a COLD session never wakes or retains a runtime |

## Notes

- Terminal/PTY regression gates were removed in Task A1 — the terminal
  engine and PTY layer are superseded (ADR-005) and no longer part of
  production. Historical evidence: `docs/research/GO_TERMINAL_SPIKE.md`
  and `testdata/codex-startup.raw` (static captured research data; the
  capture machinery was removed with the terminal layer).
- Live harness invocations and model-quota operations are **never**
  part of canonical gates. Harness gates use the deterministic fake
  app-server (hidden `__fake-codex` mode): scripted JSON-RPC only, no
  model call, no network, no quota.
- Gate 9's native surface check needs a `codex` on `PATH` that is the
  fake app-server (a one-line shell shim exec'ing
  `reposuite-relay __fake-codex`); production resolves the real CLI.
