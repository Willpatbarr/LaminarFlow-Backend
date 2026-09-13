# Schema examples

One JSON file per table, each holding a single fully-populated example row.
Reference material only — nothing reads these files, no test asserts on them.

The IDs are shared across files, so the foreign keys line up: every file points
at the same workspace, team, project, account and ticket. Read them as one
coherent snapshot rather than 26 unrelated samples.

## Where a row cannot be "fully filled"

Five tables carry constraints that make some column combinations illegal, so
filling every column at once is not possible. The branch each example takes:

| Table | Constraint | Branch shown |
| --- | --- | --- |
| `document` | `document_at_most_one_scope` | `project_id` set, `team_id` null |
| `comment` | `comment_exactly_one_target` | `document_id` + `field_id`, `ticket_id` null |
| `search_index` | `search_index_exactly_one_source` | `document_id` + `field_id`, no ticket or comment |
| `setting` | `setting_exactly_one_scope` | `workspace_id` set, `team_id` null |
| `saved_view` | `saved_view_board_matches_layout` | `layout: "board"`, so `board_id` is set |

Generated columns are included as they read back, not as they are written:
`comment.is_review_request`, `search_index.search`.

`schema_migrations.json` is the migration runner's own bookkeeping table
(`internal/migrate/runner.go`), not part of the domain schema.
