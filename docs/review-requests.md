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
  text changes" is a reader's job, LAM-122 and the UI).

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
* `updated_at` is when the note or the status last changed. A `PUT` that sets the status it
  already has changes and announces nothing.
* The reviewer list is fixed at creation; there is no endpoint to add or remove reviewers yet.

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
anything that reacts to the document changing (LAM-122).
