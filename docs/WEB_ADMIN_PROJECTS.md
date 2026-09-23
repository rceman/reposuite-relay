# Web Admin — Relay-local Projects & Settings

The Settings surface (`/settings`) manages the Relay-local **Project**
catalog: human-facing grouping metadata for organizing many sessions.
Projects are presentation authority only — they carry no session
lifecycle, execution, or workflow semantics and are unrelated to any
GTW/Gateway project model.

## Durable authority

`internal/presentation` owns the catalog, persisted to
`config/presentation.json` (0600) beside `relay.json`/`admin.json` —
durable human configuration, never process state (`run/` is wrong).

Schema v1:

```json
{
  "schemaVersion": 1,
  "projects": [
    { "id": "prj_<32 lowercase hex>", "name": "RepoSuite Relay", "root": "/abs/path" }
  ]
}
```

- **Identity** — `prj_` + 128 bits of `crypto/rand`, server-generated.
  Never derived from name, path, or foreign IDs; renames and root edits
  preserve it.
- **Name** — 1..128 UTF-8 bytes after whitespace normalization; no NUL
  or control characters.
- **Root** — a required absolute path in `filepath.Clean` canonical
  form. The path is never stat'ed, created, or inspected — an offline
  or unmounted project directory is legitimate.
- **Bounds** — at most 256 projects; the file is bounded at 256 KiB.

## Load and commit semantics

- A missing `presentation.json` is an empty catalog — reads and startup
  never create it; the first mutation does.
- An existing file parses strictly: regular file, exactly 0600, size
  bound, single JSON value, no unknown fields, schema v1, valid IDs /
  names / canonical roots, no duplicate IDs or roots. Malformed
  authority fails daemon startup closed — before descriptor
  publication — and is never silently rewritten or repaired.
- Mutations are serialized under one manager mutex: snapshot →
  copy-on-write → validate → atomic commit → swap.
- **Commit point:** the atomic same-directory rename of a fully synced
  0600 temp file. Pre-commit failure leaves the old catalog and old
  memory; a post-commit failure (directory fsync) surfaces as
  `*PostCommitError` and memory converges to the committed catalog —
  never disk-new/memory-old.
- Canonical output order is `name`, `root`, `id` — deterministic.

## Matching

`Match` is pure lexical path semantics (component-aware `filepath.Rel`):

- `cwd == root` or `cwd` is a path-component descendant → match.
- `/work/foobar` does **not** match root `/work/foo`.
- Several matching roots → the longest/most-specific wins; duplicate
  roots are forbidden, so the winner is deterministic.
- No filesystem access, no harness wake, no transcript or session-store
  touch — it is a pure metadata projection.

## Session projection

`SessionInfo` gains derived `projectId`/`projectName` — computed at read
time from the catalog plus the session's own `cwd`, never persisted on
`session.json` (no schema change, no `MetaVersion` bump). Either both
fields are present or neither. A rename shows the same `projectId` with
the new name; a root edit re-derives membership on the next read; a
delete leaves sessions Ungrouped or matching a less-specific project —
nothing about the session, runtime, or transcript changes, and a COLD
session stays `runtimeState=cold`, `pid=0`, same `generation`.

## API

Same loopback `/v1` surface — same Host, credential domains, and
cookie-auth CSRF obligations (safe GETs need none; unsafe cookie calls
need exact Origin + `X-Relay-CSRF`).

| Route | Effect |
|---|---|
| `GET /v1/projects` | catalog in canonical order |
| `POST /v1/projects` | `{name, root}` → the created project |
| `PATCH /v1/projects/{id}` | changed fields only (`name?`, `root?`); ID immutable |
| `DELETE /v1/projects/{id}` | removes grouping metadata only |

Errors: `INVALID_REQUEST` (400) for validation; `PROJECT_NOT_FOUND`
(404) for an unknown ID; `PROJECT_ROOT_CONFLICT` (409) for a duplicate
canonical root; `INTERNAL` (500) for a post-commit durability
uncertainty — the mutation is still committed. This is additive v2
surface — no API version bump.

## Web Admin

- **Settings → Projects** (`/settings`, `IconSettings` in AppShell):
  list with name / root / matching-session count, inline create-edit
  form (PATCH sends changed fields only), deliberate AlertDialog delete
  explaining sessions are untouched.
- **Sessions** (`/sessions`) groups rows by the projected `projectId`:
  project sections in canonical catalog order (empty sections omitted
  here), sessions stable-sorted by key, Ungrouped last.
- **Overview** gains a Projects count, a compact per-project
  session/active/cold summary, and the project name on recent sessions.
- **Session detail** shows the derived `Project` (or `Ungrouped`) —
  observational only; project editing lives under Settings.
- **New session** offers an optional Project select that presets `cwd`
  to the project root. The POST body is unchanged (`key`, `cwd`,
  `model?`, `mode?`) — no `projectId` is ever sent; a hand-edited cwd
  is never forced back.
- Grouping uses `SessionInfo.projectId` — the frontend never
  reimplements path matching (`web/src/lib/projects/group.ts` is a pure
  helper). No browser storage holds project authority; the daemon file
  is canonical.

## Boundaries

- No `projectId` on `RelaySession` or `session.json`; no store
  migration.
- No filesystem picker, Git discovery, or auto-creation from session
  cwd — projects are explicitly configured.
- No GTW/Gateway semantics: no task, Lead, Worker, Planner, run, or
  workflow authority; no foreign identifiers imported.
