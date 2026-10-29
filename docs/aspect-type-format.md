# Aspect type JSON format (LAM-110)

An aspect type can leave one team as a JSON file and arrive in another (or the same) team
as a **new** type. This is the file.

```json
{
  "format_version": 1,
  "name": "Class",
  "fields": [
    {"label": "Purpose", "position": 1},
    {"label": "Methods", "position": 2}
  ]
}
```

| Key | Rule |
| --- | --- |
| `format_version` | Required integer. `1` is the only version. Any other number is `422 aspect_format_unsupported` at `body.format_version`. A change an old reader would misread bumps it. |
| `name` | Required. 1 to 100 characters, not only whitespace. |
| `fields` | Required array, at most 100 entries, may be empty. |
| `fields[n].label` | Required. 1 to 100 characters, not only whitespace. |
| `fields[n].position` | Required integer, 0 to 1,000,000. Ordering only. |

**Strict.** An unknown key anywhere (top level or inside a field) is a `422` naming it
(`body.color`, `body.fields[0].id`), never silently dropped. Characters are Unicode code
points, as in [Length limits](api-errors.md#length-limits). Limits apply to import; an
existing type with a longer name still exports and its import is refused naming the field.

**No ids.** Neither the type's, a field's nor the team's. A field id is the key of
`document.body`, so a copy in another team would key into nothing. Import mints fresh ones.
Consequence: a document's values are not part of this format, only the type's shape.

## Endpoints

`GET /api/v1/aspect-types/{id}/export` returns the file above, fields in position order,
with the stored positions. `404 aspect_type_not_found` for a type that is absent or outside
your workspace. Writes nothing and publishes no event.

`POST /api/v1/aspect-types/import` takes the file plus `team_id` (uuid) beside
`format_version`, and answers like `POST /aspect-types` (200, `AspectTypeBody` with the new
type and its fields). `404 aspect_type_not_found` for a team you cannot reach.

- **Always a new type.** Import only inserts; it never edits or replaces an existing type,
  even one of the same name.
- **Name clash.** `aspect_type.name` is not unique, so a clash is not an error. If the team
  already has a type with exactly that name the new one is `"<name> (2)"`, then `"(3)"`,
  and so on (the base is shortened if the suffix would pass 100 characters). **Read the name
  in the response**, which is the name that was kept. Matching is exact (case and spacing
  count). Concurrent imports into one team are serialised so they cannot both take `(2)`.
- **Order.** Fields are inserted sorted by `position`, ties keeping file order, and numbered
  1..n, so sparse or repeated positions in a file come out tidy.
- **All or nothing.** A refused file creates nothing; one transaction.
- Aspect types publish no change-feed events today, and import adds none.

## Token scope

No new scope. Export needs **`aspect:read`**; import needs **`aspect:write`**, the same as
creating a type. A token holding only `aspect:read` can take a type out but not put one in;
a session holds both. Wrong scope is `403 insufficient_scope`.
