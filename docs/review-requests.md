# Review requests (LAM-113)

A review request asks named people to look at a range of text in a document and say yes or
no. It is not a separate resource underneath: it is a **comment** on the document with
`review_status` set, its reviewers in `comment_reviewer` (migrations 0017, 0018), and
`internal/comment` is the only code that writes either. So a request also appears in
`POST /comments/list`, is searchable, is edited (its note) and deleted by its author through
`/comments/{id}`, and cannot be turned back into a plain comment (the database refuses it
while reviewers are attached; the API has no such transition).

| Operation | Permission | Notes |
|---|---|---|
| `POST /api/v1/review-requests` | `ticket:write` | Create. Token callers may (the AI-agent case). |
| `POST /api/v1/review-requests/list` | `ticket:read` | Filter vocabulary below. |
| `PUT /api/v1/review-requests/{id}` | `ticket:write` | Reviewers only: `review_status`. |

## What the anchor is

`field_id`, `region_start`, `region_end` say where in the document the request is.

* **Aspect document:** `field_id` is required and is a field of its aspect type
  (`aspect_type_field.id`); omitting it or naming another type's field is a 422 at
  `body.field_id`. If the field is later removed from the type, `field_id` becomes null and
  the request keeps its offsets and `anchor_text`.
* **Normal document:** send **no** `field_id` (naming one is a 422 at `body.field_id`). The
  range is in the document's one value, `document.body["content"]` (ADR 6), and the stored
  `field_id` is null. A null `field_id` therefore means `content` on a normal document; the
  document's `type` says which reading applies.
* The offsets index into **the stored value**: the Markdown string at
  `document.body[field_id]` (or `body["content"]`) (ADR 6), exactly as `GET /documents/{id}` returns it, **not** the
  rendered text, not the editor's document and not ProseMirror positions. Markup characters
  (`**`, `[label](lf://...)`) count.
* The unit is a **character: a Unicode code point**. Not a byte, and not a UTF-16 unit, so a
  character outside the Basic Multilingual Plane (most emoji) is **one** offset, where
  JavaScript's `string.length` and `slice` count two. A JS client converts with
  `Array.from(s)` (or by walking code points) before slicing. Newlines are one character
  each, as stored (`\n`).
* `region_start` is the offset of the first character in the range, counting from 0.
  `region_end` is the offset **after** the last, so the range is `value[region_start:region_end]`
  and its length is `region_end - region_start`. `0 <= region_start <= region_end <=` the
  field's length. Equal offsets are a caret, a zero-length point. An absent or empty field
  has length 0, so only `0, 0` is accepted. A range past the end is a 422 at `body.region_end`.
* The offsets are checked against the field's text **at the time of the request**. They are
  not updated when the document is edited: an edit before the range leaves them pointing at
  different words. The server does not re-anchor (ADR 6: "re-anchors or orphans it when the
  text changes" is a reader's job, and the UI's). Stale approvals (below) use the offsets as
  stored, so they inherit that drift.

### `anchor_text`

The text the range held when the request was made (`value[region_start:region_end]`, `""` for
a caret), stored in `comment.anchor_text` (migration 0041) and returned as `anchor_text`. It
is a snapshot and never changes. It exists so a later reader can tell whether the reviewed
text has changed (compare it with the text now at the range; if the offsets have drifted,
search the field for it) without keeping every document version. LAM-122 builds on it; this
API does not interpret it. Rows created before 0041 and plain comments have `anchor_text`
null.

## Idempotency-Key

`POST /review-requests` takes `Idempotency-Key` exactly as `POST /comments` does and uses the
same column and index (0040): the same key from the same account returns the request as it is
now, `Idempotent-Replayed: true`, and creates and publishes nothing; the body of a replay is
ignored. Keys are shared with plain comments, so a key already used by one of your plain
comments is a 422 at `header.Idempotency-Key`.

## Reviewers and status

* `reviewer_ids`: 1 to 50 accounts, each a member of the document's workspace, listed once. The
  author may be one. The first offender is named by position: `body.reviewer_ids[1]`.
* There is **one** `review_status` per request, not one per reviewer
  (`comment_reviewer` has no verdict column): `pending` on create; any listed reviewer sets it
  to `approved`, `changes_requested` or `pending` (reopen) with `PUT`. The last write wins.
  A reviewer cannot approve "for" the others, and one approval does not mean all reviewers
  approved. Per-reviewer verdicts need a new column and are not part of this API.
* Anyone else who can see the request gets `403 not_a_reviewer`; one who cannot see it, or an
  id that is a plain comment, gets `404 review_request_not_found`.
* `stale_reason` is null unless an approval was invalidated (below); a `PUT` that leaves `pending`
  (approves, or requests changes) clears it.
* `updated_at` is when the note or the status last changed (or it went stale). It is **not** an
  edit marker: the realtime event's `at` and the stale-approval write read it, so a status change
  moves it. `edited_at` (LAM-132) is when the note was last edited through `PUT /comments/{id}`,
  `null` if never; show "(edited)" from that. A `PUT` that sets the status it already has changes
  and announces nothing.
* A decision (LAM-132). `PUT` takes an optional `decision_note`, a short message for the requester
  sent with `approved` or `changes_requested` (blank is none; with `pending` it is a 422 at
  `body.decision_note`). The same write records `decided_by` (the caller) and `decided_at`, and
  they come back with the note on every read and event. They describe the **last** decision and
  are kept when the request returns to `pending`, by a reviewer or because it went stale, so a
  client can still say who approved. A request never decided has all three `null`; one decided
  before 0046 has `decided_at` but `decided_by` `null`. A retry that sets the status it already
  has is a no-op and its note is dropped, so retries are safe; to change the note, change the
  status. Plain comments never carry these fields.
* The reviewer list is fixed at creation; there is no endpoint to add or remove reviewers yet.

## Stale approvals (LAM-122)

An approval is about the text the reviewer saw. Every document write (`PUT /documents/{id}`,
through `document.Save`) checks the document's **approved** review requests, in the same
transaction as the write. Only `approved` ones: a `pending` or `changes_requested` request has
no approval to lose.

The rule, with nothing else in it:

1. The anchored text is the field's text (`search.FieldText` of `body[field_id]`, or of
   `body["content"]` when `field_id` is null) sliced by `region_start`..`region_end` in code
   points, taken from the body before the write and from the body after it. A field that has
   shrunk below the range yields what is left of it.
2. Changed characters = `max(len(before), len(after))` less the common prefix less the common
   suffix, in code points (the suffix is measured after the prefix, so they never overlap).
   One typo is 1; rewriting a sentence is about its length. It is a size, not an edit distance.
3. If changed characters are **more than** `review.stale_threshold_percent` percent of
   `len(before)` (default 10; integer arithmetic, `changed * 100 > percent * len(before)`),
   the request is set to `pending` with `stale_reason = "anchored text changed"`. Exactly on the
   threshold is not stale. There is no small-text exception: in five characters, one is 20%.

### Turning a document into an Aspect (LAM-136)

`PUT /documents/{id}` that turns a normal document into an aspect document moves its `content`
text into the type's first field. In the same transaction, every review request on the document
whose `field_id` is null and which has a region (open or decided, any status) is pointed at that
field: `field_id` becomes the first field, `region_start`/`region_end` stay (the text moved
unchanged), and `review_status` is untouched. The text is not "changed", so an approval is not made
stale by the conversion itself. Each re-anchored request is announced as `review_request.updated`. Plain comments with a region and a
null `field_id` (an API client can create them) are re-pointed by the same rule and announced as
`comment.updated`; having no approval, one whose anchor cannot be found is simply left as it is.

If the text did not move as is (the same request also filled the first field, so the old `content`
was dropped), each request is re-anchored by searching its stored `anchor_text` in the new fields,
in the type's order, and the first match gives the new field and offsets (in code points); an
approval so re-anchored stays approved, as it is the same text. A request whose `anchor_text` is
empty (a caret) or cannot be found is left exactly as it was: `field_id` null, offsets unchanged.
It then matches no field, and an approved one goes back to pending as "anchored text changed".

Approving again (or requesting changes) clears `stale_reason`. The change is announced as
`review_request.updated` ([realtime.md](realtime.md#review-request-events-lam-113)).

`review.stale_threshold_percent` is a workspace setting (`PUT /settings/{key}?scope=workspace&target_id=...`),
an integer 0 to 100; unset means 10. A stored value that no longer validates is read as 10 rather
than blocking document writes.

Known limit, not fixed here: offsets are not shifted by edits before the region, so a request's
anchor can drift and the rule then compares whatever sits at the old offsets. Fixing it needs an
anchor-tracking design. Also not here: a create, archive or an aspect-field edit made without a
document write (none exists today) does not run the rule.

## Listing

`POST /review-requests/list` takes the shared `ListRequest` (filter, sort, `limit`, cursor).
Filter fields: `document_id`, `field_id`, `author_id`, `reviewer_id`, `review_status`,
`created_at`. Default order is oldest first.

* `reviewer_id` is a set: `{"field":"reviewer_id","op":"eq","value":"<my account id>"}` is
  "assigned to me".
* **Open means pending.** If no condition anywhere in the filter names `review_status`, only
  `pending` requests are returned. Naming it (e.g. `approved`, or `in` of all three) turns
  that default off.
* Only review requests are returned, never plain comments, and only those on documents the
  caller can reach.

## Errors and events

* New codes `review_request_not_found` (404) and `not_a_reviewer` (403), and the 422 locations
  for this API: [api-errors.md](api-errors.md).
* Events `review_request.created` and `review_request.updated`:
  [realtime.md](realtime.md#review-request-events-lam-113). A note edit or a delete is the
  comment's `comment.updated` / `comment.deleted`.

## Not here

editing the
reviewer list, per-reviewer verdicts, review requests on tickets, and
re-anchoring when the document changes.

## Choosing reviewers: `POST /api/v1/members/list`

`reviewer_ids` must be members of the document's workspace, so a client needs to list them.
`POST /api/v1/members/list` (permission `team:read`) returns the people in the **caller's
workspace** (the one `GET /auth/me` reports, ADR 0004), as a standard list (`filter`, `sort`,
`limit`, `cursor`):

| Item field | Notes |
|---|---|
| `account_id` | The id `author_id` and `reviewers[].account_id` carry. |
| `display_name`, `email` | Filterable and sortable. |
| `role` | `owner`, `admin` or `member` (workspace role). |
| `created_at` | When the person joined the workspace. |

The default sort is display name. `password_hash` is not returned and not in the filter
vocabulary. It is the workspace and not a team on purpose: reviewers are validated against
`workspace_member`, and nothing writes `team_member` yet, so a team roster would be empty and
could offer people the reviewer check then refuses. A client also uses it to show author
names on comments (`comment.author_id` is `null` only after the author's account was deleted).
