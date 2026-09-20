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

## 5. Scope and archive sit underneath the filter, never inside it

The caller's membership predicate and `archived_at IS NULL` are composed into every
statement by the service. They are not filter fields and must not be expressible as any,
so no request body can widen them.

Both live in one constant per service rather than being retyped per method — `liveOnly`
and `callerCanReach` in `internal/ticket`, `callerCanReach` in `internal/aspect`.

## 6. A closed set lives where its consumers are

`setting.key` is the worked example. What settings exist is a fact about the product
known at compile time, and every consumer is Go, so the registry is Go — a `setting_key`
table would be a second copy a deploy could disagree with, and a `CHECK` would be a
migration per setting.

Making a typo a *compile* error needs the key to be a **struct with an unexported
field**, not a named string type: Go converts an untyped string constant to a named
string type implicitly, so `type Key string` looks like it does the job and does not.
One function turns a string into a key, and it consults the registry.

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

`team`, `project` and `account` are all joined by nearly every service's scoping
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

`AnyReader` beside a named reader is refused: the named entry would be decoration, and
the next person adding a reader to that list would change nothing.

Use it only where a full list would name most of the module. Where the reader set is
small, write it out — "internal/search may read `ticket`" carries a reason, and a
wildcard does not.
