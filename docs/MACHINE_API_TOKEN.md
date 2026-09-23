# Machine API Token Management

The persistent machine bearer (`config/api.token`) authenticates trusted
machine integrations — GPT Tunnel, automation, scripts — against every
`/v1` endpoint. This document defines its human management surface:
rotation through the Web Admin Settings page.

## Credential domain

- `api.token` = one 256-bit token, hex-encoded to 64 lowercase
  characters + newline, mode exactly `0600`, under `config/` (`0700`).
- Minted once under singleton ownership on first start; strictly loaded
  on every later start. Malformed, oversized, symlinked, or
  non-`0600` content fails startup closed — never repaired.
- Persistent across daemon restarts and generations — unlike the
  descriptor bearer, which rotates per generation.
- Exactly one machine token exists at any time: no previous-token
  grace period, no key lists, scopes, expiry, or revocation state.
  "Revoke old access" IS "rotate".

## Authorization

Token management is **admin-only**: only the authenticated `relay_admin`
browser session may call it.

| Credential presented | Result |
|----------------------|--------|
| none | `401 UNAUTHORIZED` |
| descriptor bearer | `403 ADMIN_REQUIRED` |
| machine bearer | `403 ADMIN_REQUIRED` — the credential cannot manage itself |
| admin cookie | allowed |

The unsafe POST additionally requires the established browser CSRF
contract: exact canonical `Origin` + `X-Relay-CSRF`.

## Routes (API v2, additive)

- `GET /v1/settings/machine-token` → `{daemon, configured}` — presence
  metadata only. The current token is NEVER readable: no endpoint
  returns it, no prefix, no path, no fingerprint.
- `POST /v1/settings/machine-token/rotate` →
  `{daemon, token, durabilityConfirmed}` — the ONLY channel that
  returns a credential, and only the newly generated one.

The rotate response carries `Cache-Control: no-store` (plus
`Pragma: no-cache`); the token exists in the response body only —
never in a URL, query, or cookie.

## Rotation commit model

`auth.Rotate` = generate → temp `0600` → write → fsync → close →
**rename** → dirsync. The rename is the commit point:

- **Pre-commit failure** (temp/write/fsync/rename): old `api.token`
  remains canonical on disk, the old token stays accepted, the error
  response carries NO token.
- **Post-commit dirsync failure**: the new token IS canonical — the
  daemon converges live authority to it, the response returns it with
  `durabilityConfirmed=false`. This is not a failure of the rotation;
  it means crash durability of the rename was not confirmed.

No error message ever contains token material.

## Live authority

`machineToken` is guarded by `machineMu` (RWMutex): readers hold the
read lock for the constant-time compare; rotation holds the write lock
across generate + commit + memory convergence, so requests observe
either the old or the committed credential — never a torn state.

- Requests already authenticated before the switch finish normally —
  rotation operates at request admission, not mid-request.
- Concurrent rotations serialize on the write lock; every generated
  token is valid and unique, and after all rotations complete exactly
  one credential — the last committed — authenticates.
- Descriptor bearer, instance ID, PID, listener, admin credentials, and
  browser sessions are untouched. Rotation is NOT a restart.

## Client obligations

- A machine integration using the old token gets `401` immediately after
  rotation — reconfigure it with the new token; there is no grace.
- **Never retry rotation automatically.** A transport failure after the
  POST was sent is ambiguous (the commit may have crossed already);
  rotate again only on explicit user action.
- A lost rotate response cannot be recovered — the only recovery is an
  explicit second rotation.

## Web Admin UX

Settings → "Machine API access": status (`Configured`), an AlertDialog
that warns machine clients will stop authenticating, and a one-time
result panel — masked by default, explicit Copy / Show / Dismiss. The
revealed token lives in component memory only: cleared on dismiss,
navigation, and logout; never in a store or browser persistence. When
`durabilityConfirmed=false` the panel shows a warning (icon + text,
not color alone) that the token is active but durability is
unconfirmed.
