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

The opposite call is also on the record: `board.group_by` and `status.category` are
`CHECK`s, because they are facts about a *row* and every renderer branches on them.
`internal/setting`'s tests assert the registry and `board.group_by`'s CHECK agree, in
both directions.

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
`internal/ticket/conflict.go` is the only place that reads `ticket` without `liveOnly`.
