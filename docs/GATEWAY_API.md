# Gateway API — RepoSuite Relay machine-client contract

This document is the contract the **GPT Tunnel** integration consumes.
Relay is the persistent local agent control plane; machine clients talk
to it over a stable, authenticated, loopback-only HTTP API. There is no
proxy layer and no shell-out — a client sends plain HTTP requests with a
bearer token.

## Stable origin

```
http://127.0.0.1:<stable-port>
```

- The port is selected **once**, at the daemon's first successful start,
  persisted to `${REPOSUITE_HOME}/relay/config/relay.json`
  (default `~/.reposuite/relay/config/relay.json`), and reused on every
  later start. It never changes automatically; a configured port that
  cannot be bound fails startup rather than re-selecting.
- Loopback only — `0.0.0.0`, LAN binding, hostnames, Unix sockets, and
  HTTPS are not supported.
- `GET /` is the reserved Web Admin entry point on the same origin
  (currently a fixed `RepoSuite Relay` placeholder). The API lives under
  `/v1` on the same origin.

## Authentication

Two credential domains exist. A request may present **either**:

| Token | File | Lifetime |
|-------|------|----------|
| Machine API bearer | `${REPOSUITE_HOME}/relay/config/api.token` | persistent (minted once, 0600) |
| Descriptor bearer | `${REPOSUITE_HOME}/relay/run/daemon.json` | per daemon generation (internal discovery) |

Machine clients (GPT Tunnel, automation) use the **machine API bearer**:

```
Authorization: Bearer <contents of api.token>
```

- The token is 256 bits, hex-encoded, owner-private (`0600`). It is never
  returned by any API, never in `daemon.json`, never printed by `status`.
- Comparison is constant-time. A missing or wrong bearer gets one uniform
  response: `401 {"error":{"code":"UNAUTHORIZED",...}}` — no
  secret-specific difference.

Gateway configuration is therefore just:

```
relay.url        = http://127.0.0.1:<stable-port>
relay.token_file = ~/.reposuite/relay/config/api.token
```

## API version

`apiVersion: 1`, reported by `GET /v1/daemon` as `daemon.apiVersion`.
Clients must check it — an incompatible peer must not be sent commands.

## Endpoints

All bodies are JSON. All errors are
`{"error":{"code":"<CODE>","message":"<human>"}}` with a stable `code`.
Request bodies are bounded at 64 KiB.

### `GET /v1/daemon`

Daemon identity and counters.

```json
{"daemon":{"instanceId":"<hex32>","pid":1234,"apiVersion":1,
           "uptimeSeconds":123.4,"sessionCount":18,
           "activeSessions":3,"coldSessions":15}}
```

`activeSessions` = sessions bound to a live runtime generation;
`coldSessions` = durable sessions with no runtime (they wake on next
use). The counts are a cheap projection — nothing is woken.

### `GET /v1/sessions`

```json
{"daemon":{...},"sessions":[<SessionInfo>,...]}
```

Includes COLD sessions. `SessionInfo` fields: `key`, `sessionId`,
`runtimeId` (ephemeral generation id, `""` when cold), `runtimeState`,
`nativeSessionId`, `harness` (`codex`|`devin`|`opencode`|`fixture`),
`cwd`, `state` (`idle`|`active`|`waiting_input`), `activity`,
`model`/`mode`, `generation` (successful native bindings), `pid`,
`createdAt`, `generationStartedAt`, `metrics` (last-known, never
persisted).

### `GET /v1/sessions/{key}`

```json
{"daemon":{...},"session":<SessionInfo>}
```

`404 SESSION_NOT_FOUND` for unknown keys.

### `POST /v1/sessions/codex` · `POST /v1/sessions/devin` · `POST /v1/sessions/opencode`

```json
{"key":"<key>","cwd":"<abs path>","model":"optional","mode":"optional"}
```

Creates a durable **COLD** session — no native process starts until the
first prompt. `409 SESSION_EXISTS` on a duplicate key;
`400 INVALID_SESSION_KEY` / `INVALID_REQUEST` on bad input. There are no
executable/args fields — Relay runs only its built-in adapters.

### `POST /v1/sessions/{key}/prompt`

```json
{"text":"...","model":"optional-codex-only","effort":"optional-codex-only"}
```

Wakes a native runtime on demand and submits a turn. `message.user` is
persisted before submission. Response:

```json
{"daemon":{...},"key":"<key>","turnId":"<id>",
 "nativeSessionId":"<native id>","runtimeId":"<generation id>"}
```

Notable errors: `404 SESSION_NOT_FOUND`, `409 SESSION_BUSY` (turn or
unresolved input in flight), `501 UNSUPPORTED_OPERATION` (per-turn
overrides on ACP harnesses), `410/500 NATIVE_SESSION_LOST` (exact native
identity could not be resumed — Relay never substitutes another),
`502 RUNTIME_UNAVAILABLE`.

### `POST /v1/sessions/{key}/cancel`

Interrupts the in-flight turn. `404 NO_ACTIVE_TURN` when idle.

### `PATCH /v1/sessions/{key}/config`

```json
{"model":"optional","mode":"optional"}
```

Writes **desired** config; effective values land at the next native
materialization. `409 SESSION_BUSY` during a turn or unresolved input;
`400 INVALID_CONFIG` when the harness rejects the values.

### `POST /v1/sessions/{key}/input`

Answers a native requested-input request (`inputId` + per-question
answers). `501 UNSUPPORTED_OPERATION` on ACP harnesses (approvals are
policy-answered, never surfaced as input).

### `GET /v1/sessions/{key}/transcript?limit=N`

Durable history page — count- and byte-bounded
(`hasMoreBefore: true` when truncated). Records are the canonical durable
events: `message.user`, `message.agent.completed`, `turn.interrupted`,
`turn.failed`, `runtime.exited`, `harness.started`,
`input.requested`/`input.resolved`/`input.aborted`, `session.native`,
`session.config`. The response includes `throughSeq` — the canonical
event cursor for exact stream cutover.

### `GET /v1/sessions/{key}/events?after=N`

Live NDJSON canonical event stream (long-lived). `after=N` replays from
the durable cursor: `400 CURSOR_TOO_OLD` below the replay floor,
`400 CURSOR_AHEAD` above the cursor. Frames are bounded at 4 MiB;
durable events are the transcript set plus transient
`message.agent.delta`, `metrics.updated`, `harness.error`.

### `DELETE /v1/sessions/{key}`

Stops the session's runtime resources and deletes the durable
RelaySession. Deleting one session never kills a shared runtime used by
others.

### `POST /v1/daemon/shutdown`

Graceful daemon shutdown. Durable sessions survive; they reload COLD on
the next start.

## Common errors

| Code | Meaning |
|------|---------|
| `401 UNAUTHORIZED` | missing/invalid bearer (uniform) |
| `400 INVALID_REQUEST` / `INVALID_SESSION_KEY` | malformed input |
| `404 SESSION_NOT_FOUND` | unknown key |
| `409 SESSION_EXISTS` / `SESSION_BUSY` | duplicate key / in-flight turn |
| `410/500 NATIVE_SESSION_LOST` | exact resume impossible |
| `501 UNSUPPORTED_OPERATION` | capability mismatch (ACP input, overrides) |
| `502 RUNTIME_UNAVAILABLE` | native runtime failed to start |
| `503 DAEMON_SHUTTING_DOWN` | mutating call during shutdown |

## Invariants a client can rely on

- **Exact resume:** `nativeSessionId` is durable once reported; Relay
  resumes exactly it — never "newest" or implicit.
- **Generation:** counts successful native runtime bindings; a failed
  bind leaves it untouched.
- **COLD is real:** a cold session owns no process; a client count of
  cold sessions is a faithful signal that nothing is running.
- **Loopback only, no CORS:** the API never serves off-loopback; the
  future Web Admin is same-origin on `/`.
- **No auto-start via status:** `reposuite-relay status` reads config +
  descriptor only; managed commands may spawn the daemon lazily.

## What this API is NOT (yet)

- No project/worker entities — Gateway maps `project + worker → session
  key` and stays the authority for project identity.
- No token management, scopes, rotation, OAuth/JWT.
- No Web Admin, no admin login.
