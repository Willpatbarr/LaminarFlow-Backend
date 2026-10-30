# API errors — the envelope and the status codes

Settled by LAM-53. The envelope itself is `internal/api/errors.go`, and the reasoning for
its shape lives in the doc comment on `Error` rather than here — this file is the part a
caller needs, not the part an implementer does.

## The envelope

Every failure, from every endpoint, in `application/problem+json`:

```json
{
  "title": "Unprocessable Entity",
  "status": 422,
  "detail": "the filter, sort or page size was not accepted",
  "code": "filter_invalid",
  "errors": [
    {"message": "unknown field",
     "location": "body.filter.groups[0].conditions[2].field"}
  ]
}
```

| Field | For | Stability |
| --- | --- | --- |
| `title` | a person | Tracks the status text |
| `detail` | a person | **Free to reword at any time** |
| `code` | code | **Fixed forever once shipped** |
| `status` | both | The HTTP status, repeated for convenience |
| `errors[]` | code | Present on validation failures; `location` is the path. `value` is present only for `query` and `path` locations — see [What `errors[].value` holds](#what-errorsvalue-holds) |
| `type`, `instance` | tooling | RFC 9457 fields, unused so far |

**Never branch on `detail`.** It is prose, it will be reworded, and nothing will fail when
it is. `code` exists so that branching has a target — that is the whole reason the field
was added on top of RFC 9457, which has no equivalent.

`code` is always present. A field that is sometimes absent sends a caller back to reading
prose, so it has no `omitempty` and the OpenAPI document marks it required.

## What `errors[].value` holds

LAM-135. huma echoes the offending input into `errors[].value`, and for a body that is the
whole of the problem: `POST /auth/login` with one stray field answered 422 with the entire
request body in `value`, plaintext password included. Anything that records response bodies
— a proxy, a browser tool, an error reporter — kept it. The rules now apply to every error
the API writes, from one place (`scrubErrorValues` in `internal/api/errors.go`), so no
route can forget them:

| Location starts with | `value` |
| --- | --- |
| `body` | **Never sent.** Whatever its name or type |
| `header`, `cookie` | **Never sent** |
| `query`, `path` | Sent, unless a segment of the location is credential-like, and with credential-like keys inside it replaced by `"[redacted]"` at any depth, in objects and arrays |

Credential-like means the key, lower-cased with `_`, `-` and spaces removed, contains
`password`, `passwd`, `passphrase`, `secret`, `token`, `apikey`, `accesskey`, `privatekey`,
`credential`, `authorization`, `cookie`, `bearer` or `session` — so `password`,
`new_password`, `Current-Password` and `apiKey` all match. `key` alone does not: a setting's
key is a name.

**Why drop every body value, not just the credential-named ones.** The caller just sent that
value and holds a copy, so echoing it adds nothing `location` and `message` do not already
say — `body.email` + `expected string` is a full diagnosis. And a key list cannot find a
secret that is in the wrong place: a password typed into the email box, a token pasted as a
label, a password sent as an array. Dropping is the only rule that cannot be out-guessed.
The shipped frontend never read `value`.

**Never read `value` to branch.** It is a debugging aid for URL parameters and may be absent;
`code` and `location` are the contract. The OpenAPI document still lists `value` on
`ErrorDetail` because the schema is huma's, and the field is optional (`omitempty`), so
the shape did not change — a client that handled its absence already works.

Logs: nothing logs request bodies or error values. The server has no access-log or
recovery middleware (`main.go` serves the bare mux), `net/http` logs a handler panic with
its stack and never the request, and huma's `could not write error` line names the
transformer failure and not the body. A middleware that logs bodies would break this rule,
so add none without scrubbing first.

## Codes shipped so far

| Code | Status | Raised by |
| --- | --- | --- |
| `not_found` | 404 | An unmatched path under `/api/`. Also `POST /tickets/{id}/documents` and `DELETE /tickets/{id}/documents/{document_id}` (LAM-111): the ticket, or for the POST the document, is absent, outside your workspaces, or the two are in different workspaces. One code for all of them, so it does not say which |
| `invalid_login` | 401 | Wrong email or wrong password — one answer for both. A *missing* credential is the derived `unauthorized` instead, and the pair is what lets a caller tell "log in" from "you got that wrong" |
| `email_taken` | 409 | Registration with an address that already has an account, compared case-insensitively. Log in instead, or use another address. This reveals the address is registered — see ADR 0004 for why that is accepted |
| `password_too_short` | 422 | Registration with a password under 10 characters (characters, not bytes). Nothing was created |
| `password_too_long` | 422 | Registration with a password over 72 bytes, bcrypt's limit. Rejected rather than silently truncated |
| `ticket_not_found` | 404 / 422 | Any ticket operation: absent, archived, or outside your scope. Also a 422 at `body.ticket_id` on add-ticket-to-sprint (LAM-93) when the ticket is not a live ticket of the sprint's project |
| `ticket_conflict` | 409 | A ticket write that sent `If-Match` and lost: the ticket was edited since that version. Nothing was written. The body carries the current ticket and who changed it and when — see [Conflicts carry the other side](#conflicts-carry-the-other-side) |
| `ticket_archived` | 409 | The same, when the ticket was *archived* since that version. Same extra members, describing the archive. Only with `If-Match`: without it, a write to an archived ticket is still `ticket_not_found`. Also raised by a create replaying an `Idempotency-Key` whose ticket has since been archived — see [Idempotent create](api-conventions.md#10-a-create-may-carry-a-key-so-a-retry-is-not-a-duplicate-idempotency-key) |
| `status_not_found` | 404 / 422 | On a status path (LAM-83): 404 for an unknown or foreign `{status_id}`; 422 at `query.move_to` for a `move_to` that is not another status of the team. On a ticket (422): `status_id` on a ticket create or update names no status the ticket's project can use: no such status, or a status of a different team. One code for both, deliberately. `errors[0].location` is `body.status_id`; on a board column (LAM-85) a status outside the board's team, at `body.status_ids[i]` or `body.status_ids`; on a ticket move, `body.status_id`. Nothing was written |
| `assignee_not_found` | 422 | `assignee_id` names no account that is a member of the ticket's workspace: no such account, or an account from another workspace. One code for both, so the error cannot be used to probe for account ids. `errors[0].location` is `body.assignee_id`. Nothing was written |
| `filter_invalid` | 422 | A list body whose filter, sort or page size was rejected. `errors[]` names each problem. Also a saved view (LAM-86) whose `config` does not compile against the ticket vocabulary - the filter, `sort`, `group` or `columns`; the same one entry per fault, located at `body.config...` (for example `body.config.filter.conditions[0].field`) |
| `cursor_mismatch` | 400 | A list cursor replayed under a different filter or sort. Start again from the first page |
| `bad_cursor` | 400 | A list cursor that does not decode. Pass back a `next_cursor` unchanged |
| `aspect_type_not_found` | 404 | An aspect type or one of its fields: absent, or outside your scope. Also a document create or update (LAM-87) whose `aspect_type_id` names no type in your workspace, and the field usage count for a field the type does not have |
| `field_set_mismatch` | 409 | A field reorder that did not name every field exactly once. Refetch and retry |
| `status_in_use` | 422 | Deleting a status would strand tickets: tickets use it and `move_to` is absent, or it is the team's last status and tickets exist. `detail` says how many. `errors[0].location` is `query.move_to`. Nothing was deleted |
| `status_set_mismatch` | 422 | A status reorder that did not name every status of the team exactly once. `errors[0].location` is `body.status_ids`. Refetch and retry |
| `board_not_found` | 404 / 422 | Any board operation (LAM-85): an unknown board, or one outside your scope. 422 at `body.board_id` on a saved view create (LAM-86) naming a board outside the view's team, or its project when it has one |
| `column_not_found` | 404 | A column operation: the `{column_id}` is not a column of the `{id}` board |
| `status_already_mapped` | 422 | Mapping a status into a column when another column of the same board already holds it (a status sits in one column of a board). `errors[0].location` is `body.status_ids[i]` on add-column and `body.status_ids` on set-column-statuses. `status_not_found` is the sibling for a status outside the board's team. Nothing was written |
| `column_set_mismatch` | 422 | A column reorder that did not name every column of the board exactly once. `errors[0].location` is `body.column_ids`. Refetch and retry |
| `neighbour_not_found` | 422 | A ticket move whose `before_id` or `after_id` is not a live ticket in the target column (absent, archived, another project or status, or the ticket itself). `errors[0].location` is that member. Nothing was written |
| `epic_not_found` | 404 / 422 | LAM-93. 404 on an epic operation: an unknown `{id}`, or one outside your scope. 422 on a ticket create or update whose `epic_id` is no epic of the ticket's project (absent, or another project's - one code for both). `errors[0].location` is `body.epic_id`. Nothing was written |
| `milestone_not_found` | 404 / 422 | The same for milestones: 404 on a milestone operation, 422 at `body.milestone_id` on a ticket write |
| `sprint_not_found` | 404 | A sprint operation, including add-ticket-to-sprint and remove-ticket-from-sprint: an unknown `{id}`, or one outside your scope. A ticket that cannot join the sprint is `ticket_not_found` (422) at `body.ticket_id` instead: absent, archived, out of scope, or in another project |
| `label_not_found` | 404 / 422 | 404 on a label operation. 422 on a ticket create or update whose `label_ids` names a label that is no label of the ticket's project's team (absent, or another team's - one code for both). `errors[0].location` is `body.label_ids[i]`, the first bad entry. Nothing was written |
| `label_name_taken` | 409 | Creating or renaming a label to a name the team already has (compared exactly, as 0023's key does). Choose another name |
| `setting_target_not_found` | 404 | The workspace or team a setting was addressed to |
| `setting_not_set` | 404 | A real key with no value stored. Use the default |
| `setting_unknown_key` | 422 | A key that is not in the registry. Previously this stored a row nobody read |
| `setting_wrong_scope` | 422 | A real key written at the wrong level — team key at workspace scope, or the reverse |
| `setting_invalid_value` | 422 | A value whose shape is not what the key holds |
| `team_not_found` | 404 / 422 | The workspace a team was created in, or a team outside your scope. 422 at `body.team_id` on a document create (LAM-87) or a saved view create or list (LAM-86) whose `team_id` is not a team of your workspace |
| `team_name_taken` | 409 | Team names are unique per workspace. Send it again under another name |
| `project_not_found` | 404 / 422 | The team a project was created in, or a project outside your scope. 422 at `body.project_id` on a document create (LAM-87) whose `project_id` is not a project of your workspace; the same at `body.project_id` on a saved view create (LAM-86) naming a project outside the view's team |
| `document_not_found` | 404 | Any document operation (LAM-87): absent, archived, or outside your scope. Also the answer to a write that sent no `If-Match` to an archived document, and to unarchiving a document that is not archived |
| `document_conflict` | 409 | A document write that sent `If-Match` and lost: the document was edited, or archived, since that version. Also a `POST /documents` replaying an `Idempotency-Key` whose document has since been archived (LAM-126; `current.archived_at` is set). Nothing was written. The body carries `current` and `changed_at`; `current.archived_at` is set when it was archived — see [Conflicts carry the other side](#conflicts-carry-the-other-side) |
| `attachment_too_large` | 413 | `POST /attachments` (LAM-90) with a file over the limit (`MAX_ATTACHMENT_BYTES`, 25 MiB by default). Refused while the file streams, or from `Content-Length` before any of it is read; nothing is stored. The body adds `limit_bytes`, the limit in force |
| `attachment_type_not_allowed` | 415 | `POST /attachments` (LAM-90) with a file whose contents are not an accepted type — HTML, SVG, XML, script, audio and video among them — or whose declared `Content-Type` names one. The type is decided from the file's first bytes; the list of accepted types is on the operation. A request that is not `multipart/form-data` is a 415 with the derived `unsupported_media_type` |
| `document_field_unknown` | 422 | An aspect document's `body` has a key that is not a field id of its aspect type. `errors[0].location` is `body.body.<that key>`. Nothing was written. Fields are added with `POST /aspect-types/{id}/fields` first |
| `aspect_format_unsupported` | 422 | `POST /aspect-types/import` (LAM-110) with a `format_version` this server does not read. `errors[0].location` is `body.format_version`. Nothing was created. Every other refusal of an import file is the derived `unprocessable_entity` with `errors[0].location` naming the field (`body.name`, `body.fields[2].label`, an unknown key). See [aspect-type-format.md](aspect-type-format.md) |
| `aspect_type_has_no_fields` | 422 | A normal document turned into an aspect one whose type has no fields, so its text has nowhere to go. `errors[0].location` is `body.aspect_type_id`. Add a field to the type, or choose another |
| `comment_not_found` | 404 | `PUT` or `DELETE /comments/{id}` (LAM-99): absent, or on a ticket or document the caller cannot see (archived, another workspace) - indistinguishable on purpose. Creating a comment on a target the caller cannot see is the derived `not_found`, not this code |
| `comment_not_author` | 403 | Editing or deleting a comment you can see and did not write. A comment whose author's account was deleted (`author_id` null) is nobody's to change. Nothing was changed |
| `review_request_not_found` | 404 | `PUT /review-requests/{id}` (LAM-113): absent, not a review request (a plain comment's id), or on a document the caller cannot see (archived, another workspace) - indistinguishable on purpose. Creating one on a document the caller cannot see is the derived `not_found`, not this code |
| `not_a_reviewer` | 403 | `PUT /review-requests/{id}` by someone who can see the request and is not in its `reviewers` - its author included, unless also listed. Nothing was changed |
| `saved_view_not_found` | 404 | Any saved view operation (LAM-86): absent, outside your workspace, or another person's private view - indistinguishable on purpose |
| `saved_view_not_owner` | 403 | Updating or deleting a shared view you can see and do not own. A view with no owner (`owner_account_id` null: the team's seeded default, or a shared view whose owner's account was deleted) may be changed by any member who can reach its team |
| `origin_not_allowed` | 403 | `GET /api/v1/ws` from a page whose `Origin` is not this host. Raised before the credential is looked at. See [docs/realtime.md](realtime.md) |
| `no_workspace` | 403 | `GET /api/v1/ws` by a valid credential whose account belongs to no workspace, so there is nothing to watch |
| `insufficient_scope` | 403 | A valid API token on an operation it may not call (LAM-91): the token lacks the permission the operation declares (`detail` names it, e.g. `ticket:write`), or `GET /api/v1/ws` was opened by a token holding neither `ticket:read` nor `team:read` (LAM-129), or the operation is session-only - `POST /auth/tokens`, `POST /auth/tokens/list`, `DELETE /auth/tokens/{id}`. A session never gets it |
| `unknown_scope` | 422 | `POST /auth/tokens` with a scope that is not a permission name. `errors[0].location` is `body.scopes[<index>]`. Nothing was created |
| `token_not_found` | 404 | `DELETE /auth/tokens/{id}` for a token that is absent, another account's, or already revoked - indistinguishable on purpose |
| `account_not_found` | 404 | An account deletion that found nothing. A retried delete is the ordinary way to see it |
| `unprocessable_entity` (derived) | 422 | Includes the length and blank rules on `title`, `description` and team and project `name` — see [Length limits](#length-limits) — a malformed `Idempotency-Key` (`header.Idempotency-Key`), a comment (LAM-99) that names no target or two (`body`), anchors `field_id` to a ticket or to a field outside the document's aspect type (`body.field_id`), sends half a region or one that is negative or backwards (`body.region_start`, `body.region_end`), or has a blank or over-long `body` (`body.body`) - a comment create also refuses `review_status`, which belongs to review requests (LAM-113), a review request create (LAM-113) that omits `field_id` on an aspect document, or sends one that is not a field of the document's aspect type or on a normal document (`body.field_id`), or reuses an `Idempotency-Key` of one of the caller's plain comments (`header.Idempotency-Key`), a range past the end of the field's text (`body.region_end`), no reviewers or more than 50 (`body.reviewer_ids`), a reviewer listed twice or who is not a member of the document's workspace (`body.reviewer_ids[<index>]`), or a `PUT` with a `review_status` that is null or not one of `pending`, `approved`, `changes_requested` (`body.review_status`), an attachment upload (LAM-90) that names no owner or two (`body.ticket_id`, `body.document_id`), an owner field that is not a uuid or arrives after the file (`body.ticket_id`), or an empty file (`body.file`), and a search (LAM-100) whose `q` is empty, only whitespace or over 200 characters, or whose `limit` is over 50 (`body.q`, `body.limit`). Search adds no code of its own |
| *derived* | any | Everything huma raises on its own behalf — `defaultCode` turns the status text into a code |

## Conflicts carry the other side

`ticket_conflict`, `ticket_archived` and `document_conflict` are the only failures with members beyond the table above. They are RFC 9457 *extension members*, so they sit at the top level beside `code`:

```json
{
  "title": "Conflict",
  "status": 409,
  "detail": "the ticket has changed since the version you sent",
  "code": "ticket_conflict",
  "current": {
    "id": "8b1c…", "project_id": "…", "title": "Fix login properly", "description": "",
    "status_id": null, "assignee_id": null,
    "updated_at": "2026-10-13T17:02:11Z", "updated_by": "4f0e…", "version": 6
  },
  "changed_by": {"account_id": "4f0e…", "display_name": "Sarah Okafor"},
  "changed_at": "2026-10-13T17:02:11Z"
}
```

| Member | Meaning |
| --- | --- |
| `current` | The server's copy right now, in the shape of `GET /tickets/{id}` (`TicketBody`). Present for `ticket_archived` too, although the ticket reads as not found everywhere else — this is the one place an archived ticket is shown, and only to a caller who can reach it |
| `changed_by` | `{account_id, display_name}` of whoever made the change that caused the refusal. For `ticket_archived`, who archived it. **Null** when not recorded (a ticket written before `updated_by` existed) or when that account has since been deleted — show it as "someone" |
| `changed_at` | When. For `ticket_archived`, when it was archived |

`changed_by.account_id` equal to your own account means the change was yours: a retry of a write whose first attempt landed but whose response was lost. Compare `current` with what you meant to write before treating it as a conflict.

`detail` is prose and may be reworded; compose any user-facing sentence ("edited by Sarah 2 minutes ago") from `changed_by` and `changed_at`.

The members are declared in the OpenAPI document as the `TicketConflict` schema, on the 409 of `update-ticket`, `archive-ticket` and `unarchive-ticket`, so generated client types include them. See `docs/api-conventions.md` §9 for the precondition itself.

`document_conflict` (LAM-87) is the same idea with one code: `current` is the `DocumentBody`, `changed_at` is `current.updated_at`, and there is no `changed_by` (documents do not record their writer yet). It does not split edited from archived — read `current.archived_at`. Its schema is `DocumentConflict`, on the 409 of `create-document` (a replay of an archived original), `update-document`, `archive-document` and `unarchive-document`.

## Length limits

A string a person writes has a ceiling, counted in **characters, not bytes** (Unicode code
points — the same rule as the password floor, and what the OpenAPI `maxLength` counts). A
200-character title in any alphabet is accepted; a flag emoji is two code points and counts
as two.

| Field | Rule |
| --- | --- |
| `ticket.title` (create and update) | 1 to 200, and not only whitespace |
| `ticket.description` (create and update) | up to 50,000; empty is fine |
| `team.name`, `project.name` (create) | 1 to 100, and not only whitespace |
| `document.title` (create and update) | up to 200; empty is fine (an untitled document) |
| `search.q` (`POST /search/list`) | 1 to 200, and not only whitespace |

A value outside the rule is a `422` with the derived code `unprocessable_entity` and one
`errors[]` entry whose `location` is `body.title`, `body.description` or `body.name`, whether
it was too long (the schema's `maxLength`), empty (`minLength`) or whitespace only (the
service). The numbers are `maxLength` in the OpenAPI document, which is where a client
should read them from; they are `ticket.MaxTitleLength`, `ticket.MaxDescriptionLength` and
`team.MaxNameLength` / `project.MaxNameLength` in the Go code.

They are enforced by the API and the service, not by a database constraint, so a row stored
before the limit existed stays readable and the limit meets it on its next write: a ticket
that already has a 5,000-character title cannot be updated until the title is shortened.

### Colours (LAM-172)

`status.color` and `label.color` (create and update) are one of eight hue names: `slate`, `blue`,
`teal`, `green`, `amber`, `clay`, `rust`, `violet`. A name is accepted in any case. A `#rgb` or
`#rrggbb` hex is accepted too and stored as the nearest hue name (the response shows the name, not
what was sent), so an old client that still sends `#3b82f6` gets `blue`. Anything else, such as
`red` or `#12345`, is a `422` with the derived code `unprocessable_entity` at `body.color`, and
nothing is written. The shade a hue is drawn in belongs to the client, per theme. Migration `0051`
converted stored hexes; its mapping and fallback are in the file's header.

## Status codes

The two boundaries that drift are 400/422 and 409/422. Both have a one-line rule.

| Code | Means | The line that separates it |
| --- | --- | --- |
| `400` | The request could not be read | Malformed JSON, wrong content type. Nothing was understood |
| `401` | No credential, or an invalid one | Says nothing about whether the thing exists |
| `403` | Valid credential, insufficient scope | **Not** for a resource you cannot see — that is `404` |
| `404` | No such path, or no such resource | Also a resource outside your scope. See below |
| `409` | The request is fine; the world disagrees | A unique violation, an edit against a stale version |
| `422` | Read and understood, and wrong | Schema validation, and business rules like an unknown filter field |
| `500` | A fault on this side | Never carries detail about the fault |

- **400 versus 422.** `400` means the body could not be parsed. `422` means it parsed, was
  understood, and is not acceptable. If the server can name the offending field, it is `422`.
- **409 versus 422.** `422` means the request is wrong on its own terms, and would be wrong
  whenever it were sent. `409` means the request is fine and the current state refuses it —
  send it again later and it may succeed.
- **403 versus 404.** A resource in another workspace answers `404`, not `403`. That collapse
  is deliberate: `403` confirms the resource exists, which is a fact the caller has not
  earned. `internal/document.Save` already does this, and every resource follows it.

`503` is used only by `/healthz/db`, which is a supervisor probe rather than an API endpoint.
Probes live outside `/api/` and answer `{"status":"ok"}` in their own shape.

## Known limitation — `$schema`

Responses written by huma carry an extra `$schema` field; the handler that answers unmatched
`/api/` paths does not, because it never passes through huma's schema-link transformer.

It is metadata for tooling, not contract, and no caller should read it. Closing the gap would
mean either duplicating huma's URL derivation, which drifts on upgrade, or dropping the
transformer for successful responses too — a larger change than LAM-53, belonging to whoever
owns the OpenAPI document. `TestEveryFailureSharesOneEnvelope` excludes `$schema` and says so.

## Adding a code

1. Add a constant to the block in `internal/api/errors.go`. Never a literal at a call site —
   a typo'd literal stores a value no caller will ever match, and raises nothing.
2. Prefer the derived default. `defaultCode` turns a status into a code (`404` → `not_found`),
   so a failure that is only "this status" needs no new name at all.
3. Treat the name as permanent. Renaming a shipped code breaks every script that checks it,
   and scripts are half the callers this API is built for (API Design notes §5).
