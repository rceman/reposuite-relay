# Web Admin Authentication & Security Foundation

This document defines the browser-facing security model of RepoSuite
Relay. The Web Admin is a server-rendered shell on the same loopback
origin as the machine API; this milestone provides the authentication
and security foundation only — the dashboard itself is a later milestone.

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
- The PHC parser is strict: exact field set and order, no duplicates,
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

When `admin.json` is absent, `GET /` renders the one-time setup form
(username, password, confirmation). `POST /auth/setup` requires the exact
canonical Origin and a per-daemon HMAC form token embedded in the
rendered page; on success it commits `admin.json` atomically, creates an
authenticated browser session, and redirects to `/`.

When configured, `GET /` renders the login form. `POST /auth/login`
runs the same Origin + form-token checks, verifies the password with a
constant-time Argon2id comparison, and issues a session cookie. A wrong
username still verifies against a fixed dummy hash so gross timing does
not reveal whether the username matched; failures are one uniform `401`.

Login throttling is in-memory and clock-seamed: 5 failures within 5
minutes returns `429` with a bounded `Retry-After` (~30 s). A successful
login clears failure state.

`POST /auth/logout` requires the live session plus its CSRF token and
revokes the session server-side. `GET /auth/session` reports
`{configured, authenticated, username?, csrfToken?}` — the CSRF token is
the only token ever returned; never the session token, password, hash,
or either bearer.

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

Web Admin responses (`/` and `/auth/*`) carry:

```
Content-Security-Policy: default-src 'none'; form-action 'self';
                         base-uri 'none'; frame-ancestors 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: no-referrer
X-Frame-Options: DENY
Cache-Control: no-store
```

No HSTS: the transport is plain loopback HTTP.

## Secret non-disclosure

Nowhere in responses, status output, logs, or files may appear: the
plaintext password, the password hash, a browser session token, the
machine bearer, or the descriptor bearer. `status`/`status --json`
report only `adminAuthStatus` ∈ `missing|configured|invalid` and never
mutate the credential file.
