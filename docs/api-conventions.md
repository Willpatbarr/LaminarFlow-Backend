# API conventions — the rules E-LAM-0004 left behind

Written at the close of the epic, against the code rather than against the tickets.
`docs/api-errors.md` covers the envelope and the status codes; this is everything else a
new resource has to get right. Each rule here is enforced by a test, named beside it —
a convention that only lives in a document is a convention until someone is in a hurry.

## 1. Mark every operation, or it is public

`Metadata: map[string]any{RequireAuth: true}` on every `huma.Register`. There is no
group wrapper: an operation is protected because someone wrote the line.

That makes omission the failure mode, so it is guarded globally rather than per
resource. `TestEveryOperationDeclaresItsAuth` walks the built OpenAPI document and fails
on any operation that is neither marked nor in its `public` allowlist. **Adding a public
endpoint means adding a line to that allowlist with a reason** — a deliberate act, not a
line someone forgot.

**Raw routes are invisible to that test.** A handler registered on the mux directly — today only
`GET /api/v1/ws`, the change feed (`docs/realtime.md`) — is not in the OpenAPI document, so
neither this guard nor `TestEveryRouteCarriesTheVersionPrefix` sees it. It goes in `rawRoutes`
(`internal/api/realtime.go`) instead, which `TestRawRoutesCarryTheVersionPrefix` and
`TestEveryRawRouteRefusesAnUnauthenticatedCaller` walk, and it is documented by hand.

## 2. A code is permanent, and has to be findable

One constant per code in `internal/api`, never a literal at a call site. Every constant
appears in `docs/api-errors.md`.

`TestEveryErrorCodeIsDocumented` parses this package's own source for `Code*` constants
and fails on one the reference does not mention. It found `invalid_login`, shipped
undocumented under LAM-56 — which is the whole argument for parsing rather than keeping
a second list.

Prefer the derived default. `defaultCode` turns a status into a code, so a failure that
is only "this status" needs no new name.

## 3. A filter vocabulary lives with its resource, never with the compiler

`internal/filter` holds the language — types, validation, compilation, cursors — and
knows no table names. Each resource declares a `filter.Schema` beside its own service
(`ticket.Fields`).

Two reasons, and they point the same way. A compiler that could emit table names would
trip the `sqlguard` rules that guard those tables. And the vocabulary is exactly the
part that differs per resource.

Every field name in a schema is a **persisted** public surface: it appears in request
bodies *and* in `saved_view.config`, so renaming one is a data migration. Adding one is
free.

## 4. A list is a registration, not a copy

`registerList` is the whole list path, once. A new resource is a `listSpec` literal plus
an adapter that maps its rows.

The tension between one generic handler and an honest OpenAPI document turned out not to
exist: nothing about a filter's *shape* differs per resource, so one schema is true, and
what differs is which field names are accepted. `listDescription` publishes those from
the same whitelist the server checks against, so the document cannot offer a field the
server would reject.

The service half is shared the same way. `filter.Schema.Plan` takes the request and returns
the WHERE, ORDER BY, cursor columns and arguments - page-size bound, empty-filter
normalisation, cursor check and fault locations included - so a resource's `List` is its
own scope predicate, its own column list and its own row scan. `ticket`, `team` and
`project` all run through it; a fourth should not need to retype any of it.

## 5. Scope and archive sit underneath the filter, never inside it

The caller's membership predicate and `archived_at IS NULL` are composed into every
statement by the service. They are not filter fields and must not be expressible as any,
so no request body can widen them.

Both live in one constant per service rather than being retyped per method — `liveOnly`
and `callerCanReach` in `internal/ticket`, `callerCanReach` in `internal/aspect`.

Teams and projects are scoped to *the caller's workspace*, singular: `account.WorkspaceOf`,
the oldest membership — the definition `GET /auth/me` reports (ADR 0004). A team or project
list is what a client hydrates its local store from, so it has to be the workspace `me`
names. Tickets still use the wider "any workspace the caller is a member of" predicate; the
two coincide in the single-workspace model and differ only for an account with several
memberships.

## 6. A closed set lives where its consumers are

`setting.key` is the worked example. What settings exist is a fact about the product
known at compile time, and every consumer is Go, so the registry is Go — a `setting_key`
table would be a second copy a deploy could disagree with, and a `CHECK` would be a
migration per setting.

Making a typo a *compile* error needs the key to be a **struct with an unexported
field**, not a named string type: Go converts an untyped string constant to a named
string type implicitly, so `type Key string` looks like it does the job and does not.
One function turns a string into a key, and it consults the registry.

`auth.Permission` (LAM-61) is the second. What the API can do is known at compile time and
reported by `GET /auth/me` as a list of `resource:action` strings; the type is the same
struct-with-an-unexported-field shape, `auth.ParsePermission` is its one door, and
`auth.Permissions` is the one function that says who holds what. Roles will narrow that
function's answer without touching its shape.

A token holds exactly the permissions it was issued with (LAM-91). Each `RequireAuth`
operation declares the one it needs in its metadata, through `needs(auth.TicketWrite)`,
and the middleware answers `403 insufficient_scope` when a token's scopes lack it; a
session holds every permission. Scopes are permission names, validated with
`auth.ParsePermission` when a token is created. A route that is not given a permission
is open to every credential, so a new operation takes `needs(...)` rather than a bare
`RequireAuth`. Token management (`/auth/tokens`) is session-only, via `RequireSession`.

The opposite call is also on the record: `board.group_by` and `status.category` are
`CHECK`s, because they are facts about a *row* and every renderer branches on them.
`internal/setting`'s tests assert the registry and `board.group_by`'s CHECK agree, in
both directions.

A fourth, deliberately the other way: `status.color` and `label.color` (LAM-172) are a closed
set of eight hue names with no `CHECK`. `internal/hue` is the one door (`hue.Normalise` turns a
name or a hex into a stored name, or refuses), and the services call it before any SQL. A `CHECK`
was declined so that rolling back to code that still writes hexes does not fail every status and
label write. Migration `0051` repeats the hex-to-hue mapping in SQL; `internal/migrate`'s test runs
it against `hue.Of` so the two cannot drift.

## 7. Seeding runs inside the creation transaction

`internal/team` and `internal/project` seed what a new team and a new project cannot be
useful without. Never a migration — a migration runs once per database, not once per
team — and never after the commit, or the entity is briefly observable
half-configured.

Everything seeded is an ordinary row. There is no `is_system` flag on any of these
tables, deliberately.

## 8. Verify a new guard fails

ADR 0001's standard, and it held for every guard this epic added. Write a throwaway file
with the offending SQL, confirm the named failure, remove it. A guard that has only ever
passed is a guard nobody has tested.

`sqlguard` has three concepts — `Owner`, `Readers`, `Fixtures` — and **should not gain a
fourth**. If one is needed, the guard is the wrong shape and should become something
else.

A table gets a rule when it has one writer whose invariants matter, and the rule is
worth having even when almost everything reads the table.

`team`, `project`, `account` and `workspace_member` are all joined by nearly every service's scoping
predicate. Writing each of those readers out would produce a list that permits
everything and asserts nothing, which is why `team` and `project` originally shipped
with no rule at all. `sqlguard.AnyReader` says *reads unguarded, writes are not* using
the `Readers` field that already exists — so the guard still has three concepts, and
the rule still asserts the thing that matters:

| Table | What a second writer would break |
| --- | --- |
| `team` | Seeding. A team with no statuses, no aspect types, no saved view — and no error |
| `project` | The board a project renders with |
| `account` | LAM-51's repair. An orphaned private view, owned by and visible to nobody |
| `workspace`, `workspace_member` | LAM-61's registration transaction. An account with no membership — a login that reaches an app with nowhere to go |

`AnyReader` beside a named reader is refused: the named entry would be decoration, and
the next person adding a reader to that list would change nothing.

Use it only where a full list would name most of the module. Where the reader set is
small, write it out — "internal/search may read `ticket`" carries a reason, and a
wildcard does not.

## 9. A write may carry the version it last saw: `If-Match`

Enforced by `internal/ticket`'s `conflict_test.go` (the service) and `internal/api`'s
`ticket_conflict_test.go` (the wire). A local-first client edits a ticket, then syncs later;
if someone changed or archived it meanwhile, overwriting silently loses their work and the
client cannot say why. LAM-65.

**The format.** `PUT /tickets/{id}`, `DELETE /tickets/{id}` (archive) and
`POST /tickets/{id}/unarchive` take an optional request header:

    If-Match: "4"

The version in double quotes — exactly what the `ETag` response header carries. One value
only: a list of tags, a weak tag (`W/"4"`), `*`, an unquoted number and `"0"` are all a `422`
on `header.If-Match`. A malformed precondition is refused, never guessed at.

**A header, not a body field.** Archive and unarchive have no body, and a precondition that
`PUT` takes in one place and `DELETE` in another is two rules. `If-Match` is also what HTTP
already calls this, so a client library or proxy that knows it behaves. (RFC 9110 would make
a failed `If-Match` a `412`; the API-wide rule is that a stale write is `409` with a body
the client can act on — `docs/api-errors.md` — so it is.)

**`version`.** On `TicketBody` (every ticket response, the list included). Starts at 1 on
create; rises by exactly 1 on every successful update, archive and unarchive; nothing else
moves it. Each of those responses also carries it as `ETag: "<version>"` — the only way an
archive or unarchive, which return `204`, can tell a client its ticket is now at 5. Do not
use `updated_at` as a precondition: it is a transaction timestamp and two writes can share
one.

**No header, no precondition.** A request without `If-Match` behaves exactly as it did
before: last write wins, and a write to an archived ticket is `ticket_not_found`. API-token
scripts that have never heard of versions are unaffected, which is why this is not
mandatory and will not become so inside v1 (ADR 0003).

**With it:**

| Situation | Answer |
| --- | --- |
| Ticket at that version, live | The write happens. Version + 1 |
| Ticket live, at another version | `409 ticket_conflict`, nothing written |
| Ticket archived (update or archive) | `409 ticket_archived`, whatever version was sent |
| Unarchive, ticket archived at that version | Restored. Version + 1 |
| Unarchive, ticket archived at another version, or live at another version | `409 ticket_conflict` |
| Unarchive, ticket live *at that version* | `404 ticket_not_found`, as without the header |
| Ticket absent, or not yours | `404 ticket_not_found`. The 409 never reveals a ticket the caller cannot reach |

**Forcing.** A client that has seen the 409 and decided its edit should win resends with
the `current.version` from the 409 — still conditional, so a *third* writer in the gap
is caught too. Omitting the header is a blind overwrite and is for scripts.

**Atomic.** The check is the last predicate of the `UPDATE` that writes
(`… WHERE version = $n RETURNING`); there is no read before it. Only when it matches no row
does a second statement run — in the same transaction — to say *why*. Two writers with the
same version cannot both pass; the second blocks on the first's row lock and re-evaluates
against what it committed.

**Reading an archived ticket stays `404`.** `current` in a 409 is the single exception, and
`internal/ticket/conflict.go` and the idempotency lookup in `internal/ticket/refusal.go`
(§10) are the only places that read `ticket` without `liveOnly`.

## 10. A create may carry a key so a retry is not a duplicate: `Idempotency-Key`

Enforced by `internal/ticket`'s `idempotency_test.go` (the service, including 16 concurrent
creates) and `internal/api`'s `idempotency_test.go` (the wire). A client creates a ticket, the
connection drops, and it cannot tell whether the ticket exists. Retrying blindly makes a
duplicate; not retrying loses the work. LAM-75.

**The format.** `POST /tickets` takes an optional request header:

    Idempotency-Key: 2b0a7c9e-5d1f-4c8e-9a53-0f2f8d6b1e47

1 to 128 printable ASCII characters (space to `~`); a UUID is the obvious choice. Anything
else — empty, longer than 128, non-ASCII or a control character, or the header sent twice —
is a `422` on `header.Idempotency-Key`. `POST /tickets`, `POST /documents` (LAM-126), `POST /comments` (LAM-99) and `POST /attachments` (LAM-90) take it,
and so does every other create (LAM-127, below);
the other writes are
already safe to repeat (`PUT` replaces, archive and unarchive are refused when repeated, and
all three can carry `If-Match`).

**No header, no change.** A create without it behaves exactly as before.

**The same key from the same account** returns the ticket the first call created instead of
creating another.

| Situation | Answer |
| --- | --- |
| First use of the key | Creates. `200`, `Idempotent-Replayed: false` |
| Key already used by this account, ticket live | `200`, `Idempotent-Replayed: true`, **the original ticket as it is now** (edits made since are in it; so is `version`) |
| Key already used, ticket since archived | `409 ticket_archived`, `current` = the ticket, `changed_by` / `changed_at` = who archived it and when. Nothing is created |
| Same key, different account | Independent: that account's own ticket. Keys are scoped to the creator, so one account's key can never return, or reveal, another's ticket |
| Key used, but the account can no longer reach the ticket's project | `404 ticket_not_found`, as for any ticket it cannot reach |

**200 for both, and a header to tell them apart.** A fresh create has always answered `200`,
and v1 is additive-only (ADR 0003), so it cannot become `201`. Making only the replay differ
(`200` vs `201`) would put the distinction in the one place a client that never heard of
the key would misread it. `Idempotent-Replayed` is `true` or `false` whenever a key was sent,
and absent otherwise.

**A replay writes nothing.** No second row, no version bump, no search reindex, and no
`ticket.created` on the change feed ([realtime.md](realtime.md)) — the first create already
told everyone, and a second event would make every watcher insert the same ticket twice.

**A replay ignores its body.** The key identifies the *request*, and the answer is whatever
that request created. A replay with a different title, project, status or anything else gets
the original back, unchanged; to create a different ticket, send a different key. There is
no body fingerprint and no "key reused with different parameters" error: a retry after an
edit to a form is far more common than a client that reuses a key by mistake, and a hard
refusal would turn a retry into a failure. (The body still has to be valid: a replay with an
over-long title is a `422` before the key is looked at.)

**Why an archived original is a 409, not the archived body or a 404.** A ticket body does not
say it is archived, so answering `200` with it would tell a sync client its create succeeded
and the ticket is live. A `404` would tell it the create never happened, and the natural next
step — create again, under a new key — makes the duplicate this exists to prevent. `409
ticket_archived` is the answer §9 already gives for "this ticket was archived and here is
the whole of it": the client learns that the ticket exists, its id and contents, who archived
it and when, and can restore it (`POST /tickets/{id}/unarchive`, sending `current.version` as
`If-Match`) or discard its local copy. Either way, no second ticket.

**Race-safe.** The key is stored on the ticket row with its creator (`created_by`), under a
partial unique index, and the insert is `INSERT … ON CONFLICT (created_by, idempotency_key)
DO NOTHING`. Concurrent requests with one key all reach the same insert; exactly one writes,
the others wait for it to commit and read its row. There is no check-then-insert. The
concurrent test fires 16 at once.

**No expiry.** A key lives as long as its ticket; nothing deletes or recycles it. A client
must not reuse a key for an unrelated create, ever. A retention window is a possible later
change to the stored column, not to this contract.

**Documents (LAM-126).** `POST /documents` follows every rule above with "document" for
"ticket": the same key from the same account returns the original document as it is now with
`Idempotent-Replayed: true` and publishes no second `document.created`; a key used by another
account is independent; the body of a replay is ignored; the key is looked up before the
team, project and aspect type are checked, so a replay still answers after they are gone. The
one difference is the archived original: it answers `409 document_conflict` (the only code a
document conflict has) with `current` the archived document (`archived_at` set) and
`changed_at` its `updated_at`; unarchiving it makes the key live again. The key is stored on
`document.created_by` / `document.idempotency_key` (migration 0039) under the same partial
unique index and `ON CONFLICT … DO NOTHING` insert. Enforced by `internal/api`'s
`document_idempotency_test.go`.

**Every other create (LAM-127).** `POST /teams`, `/projects`, `/teams/{id}/statuses`, `/epics`,
`/sprints`, `/milestones`, `/labels`, `/projects/{id}/boards`, `/boards/{id}/columns`,
`/saved-views`, `/aspect-types`, `/aspect-types/import` and `/aspect-types/{id}/fields` take the
same header with the same rules: the same key returns the original resource as it is now with
`Idempotent-Replayed: true` (`false` on the first use, absent with no key), `200` both times,
and a replay publishes no second event. Their bodies are ignored on a replay, and a key is
checked for shape (422 on `header.Idempotency-Key`) before anything else.

*One mechanism, two storage shapes.* The five creates above store the key on their own row
(`created_by` / `idempotency_key` and a partial unique index; migrations 0032, 0039, 0040, 0044),
because each has semantics of its own (an archived original is a 409; a review request refuses a
plain comment's key). They are unchanged. Everything else shares **one table**,
`idempotency_key (account_id, key, operation, resource_id, claimed_at)` with the primary key
`(account_id, key, operation)` (migration 0045), run by `internal/idempotency` and wired in
`internal/api/idempotent.go`. A new create gets the header by embedding `KeyedInput` in its input
struct and wrapping the service call in `keyed(...)`; there is no schema change per endpoint.
Why a table rather than thirteen more columns: the creates span thirteen tables, a status, column
or field is a child row of a parent, and an operation id in the key keeps `create-epic` and
`create-milestone` from ever answering each other.

- **Scope is account + operation.** The operation is the OpenAPI operation id. The same key on
  `create-epic` and on `create-milestone` is two independent creates; so is the same key from
  two accounts. For the child creates (status, column, field) the replay reads the
  resource under the parent in the path; a key reused for a different parent finds no such
  child there, is treated as a deleted resource, and creates anew, replacing the old record.
  Send a fresh key per create, as the outbox does.
- **Race-safe by claim, not by check.** The first request `INSERT`s the row with no
  `resource_id` (`ON CONFLICT DO NOTHING`), creates the resource, then records its id.
  A concurrent request that loses the insert polls (20 ms) until the id appears and replays it;
  16 concurrent creates make one resource. A create that fails deletes its claim, so the retry
  works. A claim with no resource for more than 30 seconds (the process died between claim and
  record) is taken over; the cost is one orphaned resource.
- **Deleted resources free the key.** These resources are hard-deleted, like comments (0040): a
  replay whose recorded resource is gone drops the stale row and creates anew. There is no
  archived-original 409 here because none of these resources has an archive state.
- **Not covered, on purpose.** `POST /auth/tokens` (a replay would have to return the one-time
  secret again, or return a token nobody can read), `/auth/register`, `/auth/login`, and the
  non-create `POST`s (`/list`, `reorder`, `move`, link, search). Those are reads or
  already safe to repeat.
- **No expiry** (as above): a key lives until its resource is deleted or its account is.

Enforced by `internal/api`'s `idempotency_shared_test.go`, which runs every create above through
the same assertions (replay header and id, no second event, per-account and per-operation scope,
deleted-resource reuse, failure leaves the key free, 16 concurrent, OpenAPI documents the header).

## 11. A named id must exist, and belong to the right place

Enforced by `internal/ticket`'s `ticket_test.go` and `internal/api`'s `ticket_refs_test.go`.
`status_id` and `assignee_id` on a ticket create or update are checked as part of the write.
Each failure is a `422` at its own field with its own code: `status_not_found` at
`body.status_id`, `assignee_not_found` at `body.assignee_id` (`docs/api-errors.md`).

- A **status** must belong to the team of the ticket's project. A status of another team is
  the same error as one that does not exist.
- An **assignee** must be a member of the ticket's workspace. An account from elsewhere is
  the same error as one that does not exist, so the error is no oracle for account ids.
- Both are predicates of the `INSERT` / `UPDATE` itself, not a read before it, so there is no
  window between check and write; only when the statement writes nothing does a second
  statement (same transaction) work out which rule failed. A foreign-key violation that
  still gets through is mapped by constraint name (`ticket_status_id_fkey`,
  `ticket_assignee_account_id_fkey`), never by its message.
- The schema does not enforce the team rule (`ticket.status_id` is a plain foreign key and
  `ticket` carries no `team_id`), so this is the only thing that does; a migration adding a
  composite key is the stronger fix and is not made here.
- Order, when more than one thing is wrong on an update: ticket absent or unreachable
  (`404`), then stale `If-Match` (`409`), then `status_not_found`, then `assignee_not_found`.
  One error at a time.

`status_id` and `assignee_id` are **optional keys on create** — absent means null — and stay
required keys (null allowed) on `PUT`, which is a full replace. `description` is optional on
create the same way. `bare` is optional on team and project creation, default false.

## 12. Files: `/attachments`

LAM-90. A file belongs to exactly one ticket or document, and its metadata is
`id`, `ticket_id` / `document_id`, `filename`, `content_type`, `size_bytes`, `created_by`,
`created_at` and `url`.

**Upload** is `POST /attachments`, `multipart/form-data`: the owner (`ticket_id` or
`document_id`) and then `file`. The owner field must come first — it is checked before a byte is
stored, and a stream cannot be rewound — which `FormData` does if you append it first. The
response is the metadata. It takes `Idempotency-Key` (§10): a replay returns the first
attachment and does not read the body.

**Limits and types.** The size limit is per file and is enforced while the file is read, so an
oversize upload is refused part-way: `413 attachment_too_large` with `limit_bytes`. The type is
decided server-side from the file's first bytes; what the client declared only breaks ties
between kinds of text and Office files. Only a closed list is stored (raster images, pdf,
text, csv, markdown, json, zip, gzip, Office files, generic binary): HTML, SVG and XML are
`415 attachment_type_not_allowed`. The filename is sanitised and is only ever used to name a
download; the file is stored under a generated uuid.

**The stable URL.** `url` is `/api/v1/attachments/{id}`, relative to the origin, and does not
change. A document that embeds an image stores that URL in its Markdown: `![alt](/api/v1/attachments/<id>)`.
It needs the same credential as any other call; in a browser the session cookie goes with an
`<img>` request, so it renders. A token must hold `ticket:read`.

**Download** returns the bytes with the stored `Content-Type`, `X-Content-Type-Options: nosniff`
and a restrictive `Content-Security-Policy`. `Content-Disposition` is `inline` for png, jpeg,
gif and webp, so they embed, and `attachment` (with `filename*`, RFC 6266) for everything else.

**Auth and scope.** A session, or a token with `ticket:write` to upload and delete and
`ticket:read` to download and list — no new scope, as for comments. Visibility is the owner's:
a caller who cannot see the ticket or document gets `404 not_found` for its files too, and any
member who can see it may delete them.

**Where the bytes live.** `ATTACHMENT_DIR` (a volume in the deployed stack — see
[deploy.md](deploy.md)), behind the `Storage` interface in `internal/storage`.
