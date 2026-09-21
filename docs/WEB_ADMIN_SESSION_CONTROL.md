# Web Admin Session Control

Canonical semantics for the embedded Web Admin's session control surface.
This document is current authority — `docs/WEB_ADMIN_SECURITY.md` covers
the security model; this file covers the session lifecycle UX contract.

## Routes

| Route | Purpose |
|---|---|
| `/sessions` | Registry snapshot — explicit Refresh only, no per-session streams |
| `/sessions/new` | Create session (provider, key, cwd, optional model/mode) |
| `/sessions/<key>` | Session detail: transcript + live events + controls |

All extensionless routes resolve through the canonical SPA fallback —
exactly one HTML document (`index.html`) is ever served, with the
document CSP. `SessionInfo.key` is the only route identity (never
`sessionId`, `nativeSessionId`, or `runtimeId`).

## Auth

Everything goes through the admin browser cookie domain: `GET` is
cookie-authenticated; unsafe methods additionally require the exact
`Origin` and `X-Relay-CSRF` from `/auth/session`. The SPA never touches
descriptor or machine bearer tokens. CSRF attaches centrally in
`lib/api/client.ts` — never per-component.

## History → live cutover

The session page bootstraps exactly once:

1. `GET /v1/sessions/<key>/transcript?limit=200` → durable tail +
   `throughSeq` (the canonical event cursor — **not** the last record
   seq; transient events and reserved gaps advance past it).
2. Hydrate the timeline from durable records.
3. `GET /v1/sessions/<key>/events?after=<throughSeq>` → NDJSON stream:
   atomic replay prefix, then live events.

No event published between the snapshot and the subscribe is missed —
the replay prefix covers it. Sequence values are uint64 on the wire; the
frontend requires `Number.isSafeInteger` and fails closed (protocol
error) rather than rounding.

## NDJSON stream

`lib/api/stream.ts` parses `Response.body` with `TextDecoder` in stream
mode — arbitrary chunk boundaries, multi-line chunks, and UTF-8 splits
are handled; a final unterminated line still parses. One frame is bounded
by the canonical `MaxEventFrameBytes` (4 MiB); an over-bound frame aborts
the stream as a protocol error. Terminal `{error:{code,message}}` frames
(`STREAM_CLOSED`, `SUBSCRIBER_EVICTED`) are recognized separately from
events and trigger reconnect, not decode failure.

## Reconnect and cursor recovery

- Stream end/error → reconnect `events?after=<lastAppliedSeq>` with
  bounded backoff (250 ms → 2 s cap). Replay duplicates are deduplicated
  by seq; events are never reordered.
- `409 CURSOR_TOO_OLD` — the replay window no longer proves the cursor:
  discard transient state, re-fetch the transcript, subscribe after the
  **new** `throughSeq`.
- `409 CURSOR_AHEAD` — same recovery; after three consecutive failures
  the stream is declared offline with a protocol error.
- `401` — the browser session expired (daemon restart): refresh
  `/auth/session`, go offline; the auth gate returns to login. No
  protected retry loop.
- Route change / key change / unmount / logout → `AbortController`
  aborts the stream and any pending reconnect timer.

## Timeline

`lib/session/timeline.ts` is a pure reducer (no DOM/Svelte):

- `message.user` → user row; `message.agent.completed` → agent row.
- `message.agent.delta` (transient) accumulates into a live draft keyed
  by `turnId`+`itemId`; the durable completion replaces the draft in
  place — durable truth is always authoritative.
- `turn.failed`, `turn.interrupted`, `harness.started`, `runtime.exited`,
  `session.native`, `session.config`, `harness.error`, `input.*` render
  as quiet system rows; `metrics.updated` updates the metrics panel.
- Unknown event types render a diagnostic row (`Event: <type>`) — never
  a crash.
- All text renders as plain text nodes (`whitespace-pre-wrap`) — no
  `{@html}`, no Markdown renderer yet.

Pending requested input is reconstructed from **durable** records
(`input.requested` adds; `input.resolved`/`input.aborted` removes by
exact `inputId`), so a browser reload restores the pending state.

## History window

The transcript endpoint returns a bounded tail (≤ `TranscriptMaxLimit`
= 1000). "Load more history" grows the requested limit 200 → 400 → 800 →
1000; each growth rehydrates and re-subscribes from the new `throughSeq`.
At the maximum with `hasMoreBefore`, the UI says older history exists
beyond the API window — it never claims completeness.

## Controls

| Control | Route | Notes |
|---|---|---|
| Prompt | `POST /v1/sessions/<key>/prompt` | `{text}` only — harness-neutral (ACP rejects per-turn overrides). Accepted prompt on a COLD session wakes it intentionally. Empty text rejected client-side. |
| Cancel | `POST /v1/sessions/<key>/cancel` | Enabled on `active`/`waiting_input`. A raced `NO_ACTIVE_TURN` is a quiet reconcile, not an error. |
| Input | `POST /v1/sessions/<key>/input` | `{inputId, answers:[{questionId, answers:[…]}]}`; options are multi-select-safe (`[]string`); `isOther` offers free text; `isSecret` uses a password field. The pending card reconciles only through `input.resolved`/`input.aborted`. |
| Config | `PATCH /v1/sessions/<key>/config` | Sends only changed fields; empty patch never sent. `SessionInfo.model/mode` = **desired**; `SessionMetrics.model/mode` = **effective** — labeled separately, never conflated. |
| Delete | `DELETE /v1/sessions/<key>` | Durable deletion, AlertDialog-confirmed with the key named; success aborts the stream and returns to `/sessions`. |
| Create | `POST /v1/sessions/<provider>` | `codex`/`devin`/`opencode`; durable COLD (generation 0, pid 0) — no harness process until first prompt. |

## Secret requested input

A native `isSecret` question is preserved in the durable
`input.requested` payload (`"isSecret": true`). Its answer is forwarded
to the harness but **never persisted**: the durable `input.resolved`
record withholds secret answers and lists the question IDs under
`"redacted"`. The browser clears the field on submit and stores nothing.
Non-secret answers record normally — `answers[questionId] = []string`.

## Resource guarantees

- Detail bootstrap (`GET session`, `GET transcript`, `GET events`) is a
  pure observer — a COLD session stays `runtimeState=cold, pid=0,
  generation=0`.
- Only `/sessions/<key>` holds an event stream; the list page is a cheap
  snapshot with explicit Refresh.
- Session-projection refresh after state-affecting events is coalesced
  (250 ms) — never one fetch per token delta.
