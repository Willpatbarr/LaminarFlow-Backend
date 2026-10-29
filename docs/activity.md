# Activity log (LAM-89)

A history of meaningful changes to tickets and documents: who did what, and when. Written by the change
itself, inside its transaction, and read with one list endpoint. Table `activity` (migration `0043`), owned by
`internal/activity`; `TestNoActivitySQLOutsideThisPackage` keeps it that way.

## Reading it

`POST /api/v1/activity/list` - the usual list conventions (`docs/api-conventions.md`), with one rule of its own:
the filter must hold **top-level `eq` conditions on both `entity_type` (`ticket` or `document`) and `entity_id` (a
uuid), in an `and` group**. A history is always one entity's. `kind`, `actor_id` and `created_at` may narrow it
further; sort and cursor work as everywhere, and the default and only sensible order is newest first.

| Answer | When |
| --- | --- |
| `200` | `items[]` of `ActivityBody`, `next_cursor`, `has_more` |
| `422 filter_invalid` | `entity_type` or `entity_id` missing, not top-level, not `eq`, or malformed. `errors[].location` is `body.filter.entity_type` / `body.filter.entity_id`, one entry per problem |
| `404 not_found` | The entity is absent, archived, or in a workspace you are not a member of: one answer for all three. So an archived ticket's history is hidden until it is restored |

**Token scope: `ticket:read`.** No new scope. Documents already sit under the ticket scopes (`get-document`,
`create-comment`), and a history is a read of the thing it belongs to: a token that may read a ticket or a document
may read what happened to it. There is no write operation; entries exist only as a side effect of the changes below.

## Entry

`id`, `entity_type`, `entity_id`, `actor_id`, `kind`, `field`, `summary_before`, `summary_after`, `created_at`.

* `actor_id` is the account that made the change. A token acts as its owner, so a change made with a token is
  recorded as that account; the token itself is not recorded. Null when the account has since been deleted
  (the history stays) or the write had no account (a document written for a workspace by code, not by a caller).
* `summary_before` / `summary_after` are not diffs. For `status_changed`, `assignee_changed`, `parent_changed`
  (the epic), `label_added` (id in `summary_after`), `label_removed` (id in `summary_before`),
  `aspect_type_changed` and the `milestone` field they are the **ids** before and after (null for none), so an entry
  stays true after a rename; resolve names from what you already hold. For `title` and `description` they are the text,
  cut to 200 characters with an ellipsis.
* `created_at` is the transaction's time, so the entries of one write share it; among those the order is arbitrary
  but stable (by `id`).

## What is recorded

| Entity | Kind | When |
| --- | --- | --- |
| ticket | `created` | `POST /tickets` (not a replay of an `Idempotency-Key`) |
| ticket | `status_changed` | `PUT /tickets/{id}`, `POST /tickets/{id}/move` into another status, deleting a status the ticket was moved off |
| ticket | `assignee_changed`, `parent_changed` | `PUT /tickets/{id}` changed `assignee_id` / `epic_id` |
| ticket | `label_added`, `label_removed` | `PUT /tickets/{id}` changed `label_ids`: one entry per label |
| ticket | `field_changed` | `field` is `title`, `description` or `milestone` |
| ticket | `archived`, `unarchived` | `DELETE /tickets/{id}`, `POST /tickets/{id}/unarchive` |
| document | `created` | `POST /documents` (not a replay) |
| document | `field_changed` | `field` is `title`, or `fields`: the set of aspect fields that have a value changed (before/after are the sorted field ids, comma-joined) |
| document | `aspect_type_changed` | `PUT /documents/{id}` changed the type: normal to aspect, back, or to another aspect type |
| document | `archived`, `unarchived` | `DELETE /documents/{id}`, `POST /documents/{id}/unarchive` |

Only a change records: a `PUT` that resends the state it replaces writes nothing. **Not recorded:** a document body
save (the text of the body or of a field), a ticket's position within a column, sprint membership, document links,
comments and review requests (they have their own lists and events).

## Live updates

Each entry is announced as `activity.created` on the change feed, after the entity's own event (`docs/realtime.md`).
