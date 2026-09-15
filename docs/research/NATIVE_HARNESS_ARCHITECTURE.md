# Native Harness Architecture — Research Report

Status: research complete, awaiting Planner review (ADR-005 Proposed).
Date: 2026-09-15. Base: main `633fa3f`.

Product direction: **RepoSuite Relay is a persistent structured
agent-session daemon with native harness adapters** — not a terminal
relay. PTY/terminal emulation is not part of the canonical architecture.

All findings below were measured against the actually installed binaries,
not documentation: `codex 0.154.0`, `opencode 1.17.20`,
`devin 3000.10.21`. Protocol traces were captured live.

---

## 1. Codex app-server (verified)

**Runtime command:** `codex app-server --listen stdio://` (default).
Also `unix://PATH`, `ws://IP:PORT`, `off`. A managed shared daemon exists:
`codex app-server daemon start|stop|restart`; `codex agents` is a TUI
browsing sessions on that daemon — **multi-session shared runtime is the
native topology**, confirmed live.

**Protocol:** newline-delimited JSON-RPC 2.0. `generate-json-schema`
produced the authoritative v2 surface: 99 client methods, 81 server
notifications, 10 server→client requests. Live probe:

```
> {"jsonrpc":"2.0","id":1,"method":"initialize",...}
< {"id":1,"result":{"userAgent":"rsr-probe/0.154.0",...}}
> {"id":2,"method":"thread/list"}
< result.data[] = persisted threads, stable UUID ids
```

**Native concepts:** `thread` (session; `thread/start`, `thread/resume`,
`thread/read`, `thread/list`, `thread/fork`, `thread/archive`),
`turn` (`turn/start` input, `turn/interrupt`, `turn/steer`),
`item` (userMessage, agentMessage, reasoning, commandExecution…;
`item/started`, `item/completed`, `item/agentMessage/delta`).

- **Streaming:** `item/agentMessage/delta` + `turn/completed`.
- **Exact session ID:** UUID `threadId`; `thread/resume {threadId}` —
  verified exact-ID cold resume after full process restart (see §5).
- **Requested input:** `ToolRequestUserInput` server→client request —
  `questions[]` with `header`, `id`, `question`, `options[]`
  (A/B/C-style) **or** `options:null` + `isOther`/`isSecret` (free text).
  ⚠ Environment-gated: a live turn reported
  `request_user_input is unavailable in Default` — the mechanism exists
  in the protocol but is not enabled in the default environment.
- **Approvals:** `approvalPolicy` (untrusted/on-request/never/granular) +
  `approvalsReviewer` (user/auto_review/guardian_subagent) at both
  `thread/start` and `turn/start` scope. `never` = bypass. Approval
  requests are typed server→client requests (exec/file-change/
  permissions/apply-patch/mcp-elicitation).
- **Model/mode:** `model`, `serviceTier`, `personality`, `effort`
  per-thread AND per-turn (`turn/start.model`, `serviceTierForTurn`);
  `model/list`; `model/rerouted` notification.
- **Metrics:** `thread/tokenUsage/updated` notifications;
  `account/rateLimits/read`, `account/usage/read`,
  `account/rateLimits/updated`.

## 2. OpenCode server (`opencode serve`) (verified)

**Runtime command:** `opencode serve --port <p>` — headless HTTP server
on 127.0.0.1, no auth by default. Authoritative OpenAPI 3.1 spec at `/doc`.

**Native concepts:** `session` (`ses_*` id — durable server-side store;
100+ historical sessions listed), `message`, `part`, SSE event stream.

- `POST /session`, `GET /session/{id}` (exact-ID resume verified across
  full restart), `session/prompt`, `session/prompt_async`,
  `session/abort`, `v2.session.interrupt`, `session.fork`.
- **Requested input:** `question.asked` / `question.replied` /
  `question.rejected` SSE events + `POST /question/{requestID}/reply|reject`,
  `GET /question` — `questions[]` = `{header, question,
  options[{label,description}], multiple, custom}`. Verified live: a turn
  produced `que_…` with 3 options. ⚠ **Pending questions do NOT survive
  server restart** — after kill+restart `GET /question` returns empty and
  the tool part is permanently stranded `status:"running"` →
  `waiting_input` is a hard sleep blocker.
- **Free model:** `opencode/muse-spark-1.3-contributor-free` runs turns
  without a paid key (used for the measurements below).
- **Permissions:** `permission.asked/replied` events +
  `permission/{requestID}/reply` (`once|always|reject`).
- **Model/mode:** `v2.session.switchModel`, `v2.session.switchAgent`
  (mode = agent), `v2.model.list`, `provider.list`.
- **Metrics:** `session.tokens {input,output,reasoning,cache}`,
  `session.cost`, `v2.session.context`.
- **History:** `session.history`, `session.messages` — transcript is
  server-side and durable.
- **Events:** `/event` + `/global/event` SSE; 94 event types incl.
  `session.idle`, `session.status`, `message.part.delta`.

## 3. OpenCode ACP (`opencode acp`) (verified)

JSON-RPC 2.0 over stdio per the Agent Client Protocol.

```
< initialize.result.agentCapabilities:
  loadSession:true, mcpCapabilities:{http,sse},
  promptCapabilities:{embeddedContext,image},
  sessionCapabilities:{close,fork,list,resume}
< session/new → sessionId "ses_..." (SAME durable ses_ store as serve)
```

- `session/list` works; **`session/load` with exact `ses_` id succeeds in
  ~0.55s after full process restart** — cold resume confirmed even for a
  zero-turn session.
- `configOptions` in `session/new` result expose `model` select with the
  full provider/model list; **`session/set_config_option` switches the
  model mid-session** (verified: `currentValue` updated to
  `opencode/muse-spark-1.3-contributor-free`).
- One process hosted 5 concurrent sessions (multi-session confirmed).
- Live turn on the free `muse-spark-1.3-contributor-free` model:
  prompt→first `agent_message_chunk` 2.07s, `stopReason:end_turn`,
  `usage{input 7476, output 12, thought 5}`.

## 4. Devin ACP (`devin acp`) (verified)

JSON-RPC 2.0 over stdio, ACP v1 + Cognition extensions.

```
< agentCapabilities: loadSession:true,
  sessionCapabilities:{list,delete,additionalDirectories},
  promptCapabilities:{image,embeddedContext},
  _meta.cognition.ai/{multiRootWorkspace,sessionRename,sessionShare,
    documentLifecycle,userEdits,terminalLifecycle,userConfig,
    userShellCommand,editableCommands,commandRevision,chains,megaplan,
    ruleMentions}
```

- `session/new` → `sessionId` = human slug (`petal-warlock`); **`bypass`
  appears as a `configOptions` mode** ("Bypass Permissions — auto-approve
  all tool calls") alongside `accept-edits`/`smart`/`ask`/`plan`.
- `session/prompt` → streamed `session/update` chunks
  (`agent_thought_chunk`, `agent_message_chunk`), result
  `stopReason:"end_turn"` + `usage{inputTokens,outputTokens,totalTokens}`.
- **`session/load` with exact slug id succeeded in ~0.13s after full
  restart** (zero-turn sessions return `session_not_found` — the session
  materializes on first turn); `session/list` enumerates durable sessions.
- Requested input: no generic elicitation capability advertised; ACP
  `session/request_permission` (options[]) is the in-band ask mechanism;
  Devin's `_meta` surface is rich but a free-form A/B/C question was not
  observed → PARTIAL/UNKNOWN.

## 4b. Capability matrix

| capability | Codex app-server | OpenCode serve | OpenCode ACP | Devin ACP |
|---|---|---|---|---|
| native structured protocol | YES (JSON-RPC 2.0) | YES (HTTP+SSE, OpenAPI) | YES (ACP JSON-RPC) | YES (ACP JSON-RPC) |
| transport | stdio/unix/ws | HTTP+SSE | stdio | stdio |
| multi-session runtime | YES (daemon + 5 threads live) | YES (server hosts all) | YES (5 verified) | YES (5 verified) |
| exact session ID | YES (UUID threadId) | YES (ses_*) | YES (ses_*) | YES (slug) |
| session create | YES `thread/start` | YES `POST /session` | YES `session/new` | YES `session/new` |
| session resume | YES `thread/resume` (≥1 turn) | YES `GET /session/{id}` | YES `session/load` | YES `session/load` |
| streaming output | YES (`item/agentMessage/delta`) | YES (SSE part.delta) | YES (`session/update`) | YES (`session/update` chunks) |
| cancel | YES `turn/interrupt` | YES `abort`/`interrupt` | YES `session/cancel` | YES `session/cancel` |
| requested user input | PARTIAL (`ToolRequestUserInput`, env-gated) | YES (`question.asked`+reply) | PARTIAL (ACP request_permission) | PARTIAL (request_permission; generic ask unobserved) |
| model selection | YES (thread+turn `model`) | YES (`switchModel`) | YES (configOptions) | YES (`--model`, configOptions) |
| mode/fast selection | YES (`effort`, `serviceTier`) | YES (`switchAgent`, `variant`) | YES (`session/set_config_option`, verified) | YES (mode: code/smart/ask/plan/bypass) |
| context metrics | YES (`tokenUsage/updated`) | YES (`session.context`) | PARTIAL | YES (usage in prompt result) |
| quota metrics | YES (`account/rateLimits/*`) | PARTIAL (`session.cost`) | PARTIAL | PARTIAL |
| bypass permissions | YES (`approvalPolicy:never`, reviewer) | YES (permission config/agent mode) | PARTIAL | YES (`bypass` mode) |
| client reconnect | YES (reconnect socket, thread/list) | YES (stateless HTTP) | YES (re-spawn + load) | YES (re-spawn + load) |
| runtime restart resume | YES (proven) | YES (proven) | YES (proven) | YES (proven) |
| schema discoverability | YES (`generate-json-schema`) | YES (`/doc` OpenAPI) | YES (ACP spec + initialize caps) | YES (ACP + `_meta` caps) |

## 5. Cold-resume / resource measurements

Measured via `/proc/<pid>/smaps_rollup` (PSS preferred) + process-tree
walk + fd count. Cold = runtime absent = 0 procs / 0 RAM (trivially true
for all — every runtime is just a child process we spawn).

| state | Codex app-server | OpenCode serve | OpenCode acp | Devin acp |
|---|---|---|---|---|
| model used | `gpt-5.6-luna` @ effort `medium` (sub) | `muse-spark-1.3-contributor-free` (free) | `muse-spark-1.3-contributor-free` (free) | `swe-1-7-medium` (sub; session default) |
| tree pids | 2 (codex+node) | 1 | 1 | 1 |
| STARTED PSS | 94.6 MB | 300.6 MB | ~356 MB (1 sess) | ~27 MB (init) |
| 1 idle sess PSS | 124.6 | 327.6 | — | ~27–90 MB |
| 5 idle sess PSS | 170.1 (+~15/thread) | 330.2 (+0.7/sess) | 485.8 | 89.8 |
| post-turn PSS | 225.8 | 650.8 | 572.5 | 51.2 |
| fds | 56–73 | 24–34 | 36–41 | 28–29 |
| spawn→ready | 0.25s | 2.06s | 1.73s | 0.19s |
| exact cold resume | YES (`thread/resume`, ~0.3s; needs ≥1 turn materialized; ephemeral=opt-out) | YES (`GET /session/{ses_id}`, immediate) | YES (`session/load`, 0.55s) | YES (`session/load`, 0.13s) |
| prompt→first event | 5.98s @ medium effort (2.59s/5.39s were xhigh) | ~3.7s (free model) | 2.07s (free model) | 2.88s fresh / 35.6s on resumed session (one sample, high variance) |
| transcript survives | YES (items/turns on resumed thread) | YES (`/session/{id}/message`) | YES (same ses store) | YES (load replays) |
| model cfg survives | YES (`gpt-5.6-luna`, effort incl. per-turn override — verified persists) | YES (session record) | YES | YES |
| waiting_input survives restart | NO (server→client req bound to live transport; tool env-gated) | **NO — proven** (pending `que_` lost across restart; tool part stranded `running` forever) | NO (in-flight req dies with process) | NO (same) |
| verdict | **COLD_RESUME_SUPPORTED** (idle sessions) | **COLD_RESUME_SUPPORTED** | **COLD_RESUME_SUPPORTED** | **COLD_RESUME_SUPPORTED** |

Real-quota turns consumed: Codex ×3 (xhigh PONG, post-resume xhigh, medium-effort PONG)
+ one failed request_user_input probe; OpenCode serve ×2 (PONG +
question-triggering turn, free `muse-spark-1.3-contributor-free`);
OpenCode ACP ×1 (free model); Devin ACP ×2. All minimal single-turn
prompts, no coding work.

**Interpretation:** expensive server / cheap sessions — OpenCode ~300 MB
PSS base, ~0.7 MB per extra idle session; Codex ~95 MB base, ~15 MB per
thread; Devin lightest (~27–90 MB). All four runtimes sleep to **0
processes / 0 RAM** trivially — the question is resume fidelity, which
all four pass for *idle* sessions. `waiting_input` is a sleep blocker
everywhere (pending ask requests are transport-bound, not durable).

## 5b. Runtime transport security (verified)

Harness runtimes are **private implementation details of relayd** —
external clients (TUI/Web/Gateway/automation) never connect to them
directly. Verified transport/auth facts:

| fact | Codex app-server | OpenCode serve | OpenCode acp | Devin acp |
|---|---|---|---|---|
| preferred Relay transport | **stdio** (default) or `unix://` | HTTP loopback | **stdio only** | **stdio only** |
| network listener required | NO | YES (HTTP) | NO | NO |
| default bind | none (stdio) | 127.0.0.1 | n/a | n/a |
| loopback restriction | n/a | YES (`--hostname`, default is loopback; `--mdns` switches to 0.0.0.0 — never enable) | n/a | n/a |
| unix socket | YES — `unix://PATH` created **`srw-------` (0600)** verified | NO | n/a | n/a |
| authentication | stdio: n/a; ws: `--ws-auth capability-token\|signed-bearer-token` (for non-loopback); token via `--ws-token-file`/`--ws-shared-secret-file` (file, not argv) | YES — verified: `OPENCODE_SERVER_PASSWORD` env → HTTP Basic `opencode:<pw>`; all endpoints incl. `/event` SSE and `/session` return 401 without it | n/a (pipe fd) | n/a (pipe fd) |
| credential supply | env/`--ws-token-file` path | env `OPENCODE_SERVER_PASSWORD` (not argv) | — | — |
| argv/log exposure | token via file/sha — safe | env — safe | — | — |
| ephemeral credential per runtime | YES (random token file) | YES (random `OPENCODE_SERVER_PASSWORD` per spawn) | n/a | n/a |
| misconfiguration risk | ws on non-loopback w/o auth | `--mdns`/non-loopback bind or missing password = open server | none | none |

**Policy:** prefer `stdio` (Codex/ACP) → `unix://` socket 0600 (Codex
where a socket is needed) → loopback HTTP **with** generated Basic
password (OpenCode serve — Relay should *always* set a fresh random
`OPENCODE_SERVER_PASSWORD` per runtime spawn; never rely on the
unauthenticated default). Runtime credentials are **ephemeral per
generation** — generated with `crypto/rand` at wake, held only in the
runtime record, discarded at sleep; never part of `nativeSessionId`,
event history, logs, or status output.

**Fail-closed startup:** after spawning, the adapter must verify the
expected posture (e.g. `ss`/connect-check: listener is loopback-only;
a no-auth request returns 401). If the runtime comes up `0.0.0.0` or
unauthenticated, stop it and fail startup rather than accept the weaker
configuration.

## 6. Proposed canonical model (minimal)

- **RelayDaemon** — existing singleton control plane (keep).
- **HarnessRuntime** — replaces ActiveGeneration:
  `{runtimeID, harness, transport(stdio|unix|http+sse), processTree,
  state: COLD|STARTING|WARM|ACTIVE|STOPPING, capabilities}`. One runtime
  hosts N sessions where the protocol allows (all four do).
- **HarnessAdapter** — per-harness protocol codec (codex JSON-RPC,
  ACP for devin+opencode-acp, opencode HTTP+SSE).
- **RelaySession** — `{key, sessionId, nativeSessionId, runtimeID|-,
  state, config{model,mode}, transcriptCursor, createdAt}` — persistent
  logical identity; does NOT require a live runtime.
- **CanonicalEvent** (monotonic `seq` per session, for replay):
  `session.state`, `message.user`, `message.agent.delta`,
  `message.agent.completed`, `input.requested`, `input.resolved`,
  `session.metrics`, `session.error`. Tool events kept native-internal.
- **RequestedInput** — `{requestId, questions[{id,header,question,
  options[]|null(free-text),multiple,custom,secret}], blocking}` —
  responder-agnostic (human/planner/agent).
- **SessionMetrics** — all optional: `{model, mode, contextUsed,
  contextLimit, tokens{in,out,reasoning,cache}, quota{used,limit,
  resetAt}}`; footer degrades to whatever exists.

## 7. RuntimeSupervisor (design only)

`internal/runtime` later: `EnsureAwake(runtimeID)` → spawn + ready-poll;
`ResumeExact(nativeSessionId)`; `Dispatch(prompt)`; tracks
`sessionsPerRuntime`; `SafeToSleep` = all bound sessions idle AND no
pending input request AND no in-flight turn; `Stop` graceful; tree
measurement via `/proc`. Sleep policy = per-runtime (never per-session);
two-tier WARM-grace→COLD to be tuned from measured latency — no values
hard-coded yet. UI attach must not wake; only native ops (prompt,
model change, cancel) wake.

## 8. xterm-go / creack/pty disposition

- **`internal/terminal` + `github.com/rceman/xterm-go` → REMOVE FROM
  RELAY CORE.** No harness needs terminal emulation; the canonical
  transport is structured protocol. (The fork stays published and
  history remains for reference.)
- **`internal/pty` + `github.com/creack/pty` → REMOVE FROM RELAY CORE.**
  No managed child needs a PTY — all three harnesses have structured
  transports (stdio JSON-RPC or HTTP).
- Removal is a follow-up task (do not strip in this research commit);
  docs label both "legacy spike, superseded by ADR-005".

## 9. Dependency policy

stdlib-first: NDJSON/JSON-RPC over stdio+unix, HTTP+SSE via `net/http` —
all hand-rollable small. **No new third-party deps are justified** for
the adapter core. `golang.org/x/term` is the only plausible add — and
only for the TUI task (raw mode), deferred until then. No Bubble Tea /
Cobra / gRPC / vendor SDKs.

## 10. Human UI MVP (proposal)

Transcript viewport (user + agent text), prompt line, requested-input
selector (choice list or free text), footer `{model | mode | ctx
used/limit | quota%}` with graceful field omission, states
starting/running/idle/waiting_input/error/stopped. Keys: Enter send,
Ctrl+C cancel turn, Ctrl+M model, Ctrl+F mode, Ctrl+Q detach,
PgUp/PgDn scroll. stdlib + `x/term` only.

## 11. Proposed next train

A. **Core-model correction** — evolve daemon/session foundation:
   `HarnessRuntime` + `RelaySession{nativeSessionId}` replace
   ActiveGeneration; protocol keeps v1 shape, adds runtime ops.
B. **Codex app-server adapter** — first vertical slice:
   serve/status/stream/interrupt/resume via stdio JSON-RPC.
C. **Canonical event stream** — seq-numbered events + transcript
   cursor on the existing socket protocol.
D. **Minimal TUI.**
E. **Devin ACP adapter** (shares the ACP client), then OpenCode ACP.
F. **OpenCode-serve adapter** + WSS/Gateway boundary.

One vertical slice (Codex) before generalizing adapters.

## 12. Open questions / risks

- Codex `request_user_input` is environment-gated — which config enables
  it? (Blocking for the A/B/C UI on Codex.)
- ~~OpenCode question durability~~ — **answered: NO** — pending `que_`
  requests are lost across restart; the tool part stays `running`
  forever. `waiting_input` is a proven sleep blocker on all four paths.
- Devin generic (non-permission) agent questions — not observed;
  `_meta` surface suggests richer channels exist.
- Codex `thread/resume` needs ≥1 materialized turn — zero-turn sessions
  are not resumable (acceptable: an unmaterialized session is empty).
- ~~ACP model setter~~ — **answered:** `session/set_config_option`
  (configId=`model`) switches mid-session on OpenCode ACP; Devin exposes
  mode via configOptions (`bypass` etc.) — exact Devin model setter
  verify at adapter build (`--model` covers creation-time).
