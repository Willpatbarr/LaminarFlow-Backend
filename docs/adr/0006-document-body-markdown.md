# 6. Documents are edited in TipTap and stored as Markdown strings — links are `lf://` URLs

Status: Accepted — 2026-10-28 (LAM-88, with the document API of LAM-87)

## Context

A document's content is a JSON object, `document.body`, keyed by `aspect_type_field.id` (ADR 1,
LAM-22). Until now nothing decided what a *value* in it is. The frontend has no editor
dependency, so the choice of editor and the shape a value is stored in are the same decision:
the editor produces something, and the backend stores and indexes it.

Three facts constrain it.

* **`internal/search/text.go` indexes whatever is stored.** `fieldText` reduces a decoded JSON
  value to plain text for `search_index`. A string is indexed as it is; a map or array is walked
  and every string inside it is indexed. The stored form therefore has to be something that
  walk turns into text worth searching.
* **API clients read and write bodies directly.** Master Spec §4.2 requires the stored form to
  stay "clean and machine-readable". Scripts and API tokens are half of this API's callers
  (API Design notes §5); they do not run an editor.
* **The editor has to do several things:** Markdown typed and pasted natively; internal links
  to a ticket, document or project (LAM-108); text-range anchors for Review Requests (LAM-113);
  images; and several instances on one page, one per aspect field (LAM-109).

## Decision

### 1. The editor is TipTap

TipTap (a framework over ProseMirror). The product owner chose it; this record does not
re-open the choice. TipTap's editor instances are independent objects, so one per aspect field
needs nothing special; Markdown shortcuts are input rules and pasted Markdown a paste rule;
a link to a ticket is an extension on the link mark (LAM-108); and ProseMirror positions are
what a text-range anchor is computed from. LAM-96 builds the component and is where each of
those is shown to work.

### 2. One field value is one Markdown string

`body[field_id]` is a JSON **string** holding Markdown. Nothing wraps it: no `{"format":
"markdown", "value": …}`, no version tag, no editor JSON beside it. An empty field is an
absent key (a field added later is simply a key absent from every body, LAM-44) or `""`;
both mean empty and a client treats them alike.

The dialect is CommonMark plus the GitHub extensions that have an editor node (tables, task
lists, strikethrough). The editor's schema is limited to what that dialect can round-trip. A
feature Markdown cannot express — text colour, alignment, a custom block — is not offered,
because it would be silently lost the first time the value is saved. Choosing the editor's
Markdown serializer is LAM-96's.

A **normal** document has no aspect type and so no field ids. Its one value is stored under
the key `content`: `{"content": "# Meeting notes\n…"}`. `document.ContentKey` is the constant.
When a normal document becomes an aspect one (`PUT /documents/{id}` with `type: "aspect"`), that
string moves into the type's first field — by `(position, label, id)` — and nothing else in the
old body is carried over (LAM-87). A body key on an aspect document that is not a field id of
its type is a 422.

### 3. How an API client reads and writes it

A client reads `GET /api/v1/documents/{id}` and gets `body` as an object of strings, and writes
`POST /documents` or `PUT /documents/{id}` with the same shape. `PUT` replaces the whole body, so
a client sends every field it wants kept, and sends `If-Match` with the version it read.

```json
{
  "type": "aspect",
  "aspect_type_id": "88888888-8888-4888-8888-888888888888",
  "body": {
    "99999999-9999-4999-8999-999999999999": "Columns assume **one status each**, see [LAM-85](lf://ticket/3f2c…)."
  }
}
```

A client needs no editor library and no knowledge of ProseMirror: it reads and writes text. The
request schema is `map<string, string>`, so a number, object or `null` as a value is refused by
the schema (a 422 at the key) rather than stored. The service underneath is not so strict —
`Save` stores any JSON, which is why `fieldText` still handles every type — but the API is the
contract and its contract is strings.

### 4. Internal links are Markdown links with an `lf://` URL

```
[label](lf://ticket/<id>)
[label](lf://document/<id>)
[label](lf://project/<id>)
```

`lf` is the scheme, the host is the kind of thing linked, the path is its id: a lower-case,
hyphenated UUID, as the API returns it. The three kinds are the whole set today; a new kind is
additive, and a reader that meets one it does not know treats the link as it would any other it
cannot resolve.

* **The id is the link; the label is a snapshot.** The editor renders a chip from the current
  title of the target (LAM-108), not from the stored label, so a rename does not leave stale
  text on screen. The stored label exists so the Markdown reads sensibly where nothing resolves
  it.
* **A link that does not resolve** (deleted, archived, or in a workspace the viewer cannot
  reach) renders as its label, inert. Resolution is the client's job and a missing target is
  not an error.
* **The server stores the string and does not parse it.** It does not check that a linked id
  exists and keeps no table of links. Ticket–document links as a relation are LAM-111's, a
  separate API; the Markdown links are for reading.

Rejected: *relative app URLs* (`/tickets/<id>`) — they bake the frontend's routes into stored
data, so a route change is a data migration, and they are indistinguishable from a link to
something else on the same host. *Wiki links* (`[[LAM-85]]`) — not Markdown links, and
ambiguous when a title changes. *HTML* — no.

### 5. What search indexes

`fieldText` indexes a string **unchanged**, so `search_index.content` is the Markdown source of
each field. `text.go` is not touched by this decision. Markup characters and link targets are in
the indexed text; PostgreSQL's `english` text-search configuration (0019) treats punctuation as
separators, so `**one status**` indexes the words *one* and *status*, and a link's label words are
indexed like any others.

Stripping Markdown to plain text before indexing is a refinement, not a requirement, and it is
cheap to add later: `fieldText` is the single extraction function, and `Rebuild` regenerates
the whole index from the stored bodies, so changing what is extracted and rebuilding is safe
(ADR 1). It would become worth doing when search shows a highlighted excerpt, because an excerpt
cut from the raw source shows the markup.

### Why not editor JSON

Storing TipTap's own document (a ProseMirror JSON tree) was the alternative, and the only
serious one.

* It round-trips everything the editor can do, which Markdown does not.
* But it ties stored data to an editor library: replacing TipTap, or changing its schema, is a
  migration of every body. A Markdown string outlives the editor.
* An API client has to understand the node schema to read or write a paragraph. That is not
  "clean and machine-readable"; it is machine-readable only to code that ships ProseMirror.
* `fieldText` would index it by walking the tree, so node type names (`"paragraph"`, `"text"`)
  would be indexed alongside the words, and the extractor would need a special case for the
  shape to stop it. Markdown needs none.
* Keeping both a Markdown string and the JSON is two sources of truth that can disagree — the
  failure ADR 1 exists to prevent for blob and index.

## Consequences

* The editor can only offer what Markdown can express. That is deliberate, and it is the cost
  of the choice; if a feature needs more, it needs a new record, not a quiet extra key.
* **Text-range anchors cannot live in the string.** A Review Request (LAM-113) records its anchor
  — the quoted text and enough context to find it again — in its own row, and re-anchors or
  orphans it when the text changes. The stored Markdown carries no marker for it.
* **Concurrent edits are last-write-wins per document**, softened by `If-Match` and `version`
  (LAM-87): a stale writer gets a 409 carrying the current copy, and a person chooses. There is
  no merge of two edits to the same string and no collaborative cursor. Nothing here prevents
  adding one later; nothing here provides it.
* **Images are Markdown image syntax** (`![alt](url)`). Where the file lives, and the endpoint
  that accepts it, are not decided here; no upload API exists yet.
* A document's stored size is bounded only by the request limit. Title has a limit (200
  characters); body values do not, yet.
* `PUT` replacing the whole body means two people editing two *different* fields of one aspect
  document still conflict, by version. The field-per-row design of `aspect_type_field` protects
  the *type* from that (LAM-44); the document body does not get it.
