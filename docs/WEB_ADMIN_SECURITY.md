# Web Admin Authentication & Security

This document defines the browser-facing security model of RepoSuite
Relay. The Web Admin is an **embedded SvelteKit single-page
application** on the same loopback origin as the machine API: the
production frontend is built once with Node/Vite, committed under
`web/build/`, embedded into the binary with `go:embed`, and served by
relayd itself — production requires no Node, no Vite, and no second
HTTP server. The UI rendered in the browser is client-side only; Go
still owns every `/auth/*` and `/v1/*` endpoint.

## Three credential domains

Relay intentionally has three independent credential domains:

| Domain | Storage | Lifetime | Clients |
|--------|---------|----------|---------|
| Descriptor bearer | `run/daemon.json` | per daemon generation | internal/local discovery |
| Machine API bearer | `config/api.token` | persistent | automation (GPT Tunnel, scripts) |
| Admin password + browser session | `config/admin.json` (hash only); session memory-only | password persistent; session per daemon generation | the human Web Admin |

The domains never share values: a browser session cookie is not a bearer
token, bearer tokens carry no CSRF obligations, and neither bearer is ever
exposed to the browser surface.

## Admin credential (`config/admin.json`)

```json
{
  "schemaVersion": 1,
  "username": "admin",
  "passwordHash": "$argon2id$v=19$m=65536,t=3,p=2$<b64 salt>$<b64 key>"
}
```

- **Argon2id**, PHC-encoded. Production parameters: memory 65536 KiB
  (64 MiB), iterations 3, parallelism 2, 128-bit `crypto/rand` salt,
  256-bit derived key. `golang.org/x/crypto/argon2` is the single
  approved cryptographic dependency.
- The PHC parser is strict: exact field set in canonical `m,t,p` order,
  `v=19` only, bounded parameters (memory 8 MiB–1 GiB, iterations 1–8,
  parallelism 1–8, salt 16–64 B, key 32–64 B). An out-of-bounds record
  is rejected before hashing so verification cannot be turned into a
  denial of service.
- File contract: regular file, mode exactly `0600`, ≤8 KiB, exactly one
  JSON object, unknown fields rejected. A symlink, special file, broader
  mode, malformed JSON, or unsupported schema **fails daemon startup
  closed** — malformed credentials are never repaired or rewritten.
- Commit is create-once: same-directory temp → fsync → atomic hard-link
  create (`os.Link` fails if the target exists) → directory fsync. A
  concurrent setup race produces exactly one winner; the loser receives
  `409 Conflict` and never clobbers the credential.
- Password policy: 12–256 bytes, valid UTF-8, no NUL, never silently
  trimmed. Username: `[A-Za-z0-9._-]{1,64}`.

## First-run setup and login

`GET /` always serves the same anonymous SPA document — auth state is
never rendered into HTML. The app bootstraps from
`GET /auth/session`, which returns:

```
unconfigured:   {configured:false, authenticated:false, formToken:<setup>}
logged out:     {configured:true,  authenticated:false, formToken:<login>}
authenticated:  {configured:true,  authenticated:true, username, csrfToken}
```

When `admin.json` is absent the SPA renders the one-time setup form
(username, password, confirmation). `POST /auth/setup` requires the exact
canonical Origin and the per-daemon HMAC form token obtained from
`/auth/session`; on success it commits `admin.json` atomically, creates
an authenticated browser session, and redirects to `/`.

When configured, the SPA renders the login form. `POST /auth/login`
runs the same Origin + form-token checks and issues a session cookie on
success. Verification is uniform: the submitted username is compared
against the stored username in constant time, and every admitted
configured attempt performs exactly one Argon2id verification against
the same stored credential hash — username correctness never
short-circuits password verification. Wrong username, wrong password,
and both wrong share one public `401` response shape.

Login throttling is in-memory and clock-seamed: 5 failures within 5
minutes returns `429` with a bounded `Retry-After` (~30 s), which the
login view surfaces. A successful login clears failure state.

`POST /auth/logout` requires the live session plus its CSRF token and
revokes the session server-side. The CSRF token is the only token ever
returned by `/auth/session`; never the session token, password, hash,
or either bearer.

## Browser memory policy

The SPA holds `username`, `csrfToken`, and `formToken` in application
memory only — page lifetime. Nothing auth-related is written to
`localStorage`, `sessionStorage`, or IndexedDB, and the `HttpOnly`
session cookie is unreachable from JavaScript. A reload re-bootstraps
from `/auth/session`; an expired session yields `authenticated:false`
and the login view.

## Browser sessions

- 256-bit random token, `crypto/rand`; the store indexes sessions by
  `SHA-256(token)` — the raw token exists only in the cookie.
- Cookie: `HttpOnly`, `SameSite=Strict`, `Path=/`. `Secure` is
  deliberately **not** set: the canonical transport is
  `http://127.0.0.1:<port>` and Secure would make the cookie unusable
  over the accepted loopback contract.
- Expiry: 30-minute idle timeout and 12-hour absolute timeout, measured
  on an injectable clock. Expired sessions are rejected and removed.
- The store is bounded at 16 sessions; at capacity the oldest session is
  revoked after expired ones are pruned.
- Sessions are **memory-only**: a daemon restart invalidates every
  browser session. That is intentional — the durable credential is the
  password hash, and the login cost is trivial.

## CSRF, Origin, and Host

- **Setup/login** (pre-auth): exact `Origin: http://127.0.0.1:<port>` plus
  an HMAC form token keyed by a per-daemon random secret; the token
  rotates on restart. SameSite alone is never the defense — there is no
  authenticated cookie yet.
- **Authenticated mutations**: every unsafe method (`POST`, `PUT`,
  `PATCH`, `DELETE`) authenticated by the admin cookie requires the exact
  Origin plus `X-Relay-CSRF` matching the session's token. `GET`/`HEAD`
  are exempt. Bearer-authenticated `/v1` requests carry no CSRF
  obligation at all — machine clients are unaffected.
- **Host**: only `127.0.0.1:<configured-port>` is accepted, on every
  route, before dispatch. `localhost`, alternate names, foreign domains,
  other ports, and `X-Forwarded-Host` tricks all fail closed — this is
  the DNS-rebinding defense.
- **No CORS**: no `Access-Control-*` headers are ever emitted.

## Security headers

Headers depend on the response class:

**SPA document** — `/`, `/sessions`, and every extensionless SPA route
are served through `serveIndex()` and carry the document policy:

```
Content-Security-Policy: default-src 'none';
                         script-src 'self' 'sha256-…' [per inline block];
                         style-src 'self' ['sha256-…' per inline block];
                         connect-src 'self'; img-src 'self';
                         font-src 'self'; form-action 'self';
                         base-uri 'none'; frame-ancestors 'none';
                         object-src 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
Cache-Control: no-store
```

**Auth endpoints** — `/auth/*` responses carry the auth security
policy (`webAuth.securityHeaders`): `Content-Security-Policy:
default-src 'none'; form-action 'self'; base-uri 'none';
frame-ancestors 'none'`, `X-Content-Type-Options: nosniff`,
`Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, and
`Cache-Control: no-store`.

**Generic static assets** — `_app/immutable/*.js`, `_app/immutable/*.css`,
`version.json`, favicon, etc. are not HTML documents and intentionally
carry no CSP. They get the baseline headers `X-Content-Type-Options:
nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, plus
their cache policy: `public, max-age=31536000, immutable` for hashed
`_app/immutable` files and `no-store` for everything else. A missing
CSP on a JS/CSS asset is not a defect — nosniff plus the correct
content type keeps them non-executable as documents.

The SvelteKit entry document contains one inline bootstrap `<script>`
(module loader). Instead of `'unsafe-inline'`, relayd computes the
SHA-256 of every inline script/style block in the served document at
startup and emits exactly those hashes — `'unsafe-inline'` and
`'unsafe-eval'` never appear. HTML responses are `Cache-Control:
no-store`; hashed `_app/immutable` assets get long-lived immutable
caching. Missing assets return a real 404 (never the SPA fallback), and
paths under `/_app/` or with `..`/`//`/`\`/NUL fail closed. All
application assets are embedded — no CDN, webfont, analytics, or remote
resource is loaded.

**One canonical document.** The build emits exactly `index.html`
(routes are client-side only — no route HTML is generated), and it is
reachable only at `/` and extensionless SPA routes. `GET /index.html`
redirects to `/`; any other `*.html` is a 404 — a build artifact can
never be served through the generic (document-CSP-less) asset path. A
regression test fails if the embedded build ever contains an HTML file
other than `index.html`.

No HSTS: the transport is plain loopback HTTP.

## Development boundary

`npm run dev` in `web/` serves the SPA through Vite with a proxy for
`/auth/*` and `/v1/*` to `RELAY_DEV_TARGET`. Because relayd enforces the
exact canonical Host/Origin, the proxy deliberately rewrites both to
the configured target — that relaxation exists only inside the local
Vite proxy; relayd itself has no dev-origin bypass and its enforcement
is unchanged.

## Secret non-disclosure

- **Plaintext password**: never persisted, never returned, never logged.
- **Password hash**: exists only in `config/admin.json` — never returned
  through the Web Admin HTML, `/auth/*`, `/v1/*`, `status`, or logs.
- **Browser session token**: memory/cookie only — never returned in a
  body, log, status, or durable file (the store indexes `SHA-256`).
- **Machine/descriptor bearers**: retain their accepted domains and are
  never exposed through the Web Admin surface.

`status`/`status --json` report only `adminAuthStatus` ∈
`missing|configured|invalid` and never mutate the credential file.
