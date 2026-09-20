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
     "location": "body.filter.groups[0].conditions[2].field",
     "value": "assignee_name"}
  ]
}
```

| Field | For | Stability |
| --- | --- | --- |
| `title` | a person | Tracks the status text |
| `detail` | a person | **Free to reword at any time** |
| `code` | code | **Fixed forever once shipped** |
| `status` | both | The HTTP status, repeated for convenience |
| `errors[]` | code | Present on validation failures; `location` is the path |
| `type`, `instance` | tooling | RFC 9457 fields, unused so far |

**Never branch on `detail`.** It is prose, it will be reworded, and nothing will fail when
it is. `code` exists so that branching has a target — that is the whole reason the field
was added on top of RFC 9457, which has no equivalent.

`code` is always present. A field that is sometimes absent sends a caller back to reading
prose, so it has no `omitempty` and the OpenAPI document marks it required.

## Codes shipped so far

| Code | Status | Raised by |
| --- | --- | --- |
| `not_found` | 404 | An unmatched path under `/api/` |
| `ticket_not_found` | 404 | Any ticket operation: absent, archived, or outside your scope |
| `filter_invalid` | 422 | A list body whose filter, sort or page size was rejected. `errors[]` names each problem |
| `cursor_mismatch` | 400 | A list cursor replayed under a different filter or sort. Start again from the first page |
| `bad_cursor` | 400 | A list cursor that does not decode. Pass back a `next_cursor` unchanged |
| `aspect_type_not_found` | 404 | An aspect type or one of its fields: absent, or outside your scope |
| `field_set_mismatch` | 409 | A field reorder that did not name every field exactly once. Refetch and retry |
| `setting_target_not_found` | 404 | The workspace or team a setting was addressed to |
| `setting_not_set` | 404 | A real key with no value stored. Use the default |
| `setting_unknown_key` | 422 | A key that is not in the registry. Previously this stored a row nobody read |
| `setting_wrong_scope` | 422 | A real key written at the wrong level — team key at workspace scope, or the reverse |
| `setting_invalid_value` | 422 | A value whose shape is not what the key holds |
| *derived* | any | Everything huma raises on its own behalf — `defaultCode` turns the status text into a code |

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
