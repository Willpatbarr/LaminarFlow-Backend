-- LAM-204: joining and leaving a sprint is recorded in the activity log.
--
-- Sprint membership is written through ticket_sprint (POST /sprints/{id}/tickets, DELETE
-- /sprints/{id}/tickets/{ticket_id}), not the ticket's PUT, so the log never saw it. Two new
-- kinds, shaped like label_added / label_removed because a ticket can be in several sprints:
--
--   sprint_added    summary_after  = the sprint's id, name_after  = its name
--   sprint_removed  summary_before = the sprint's id, name_before = its name
--
-- The names are a snapshot taken when the change happened (as 0048 does for statuses), so a
-- later rename does not rewrite the entry and deleting the sprint does not erase its name.
-- name_before / name_after already exist (0048) and have no CHECK, so only the kind list
-- changes. A document has no sprints: activity_document_kind is left as it is.

-- +migrate Up

ALTER TABLE activity DROP CONSTRAINT activity_kind_is_known;

ALTER TABLE activity
    ADD CONSTRAINT activity_kind_is_known
        CHECK (kind IN ('created', 'status_changed', 'assignee_changed', 'parent_changed',
                        'label_added', 'label_removed', 'archived', 'unarchived',
                        'aspect_type_changed', 'field_changed',
                        'sprint_added', 'sprint_removed'));

-- +migrate Down

DELETE FROM activity WHERE kind IN ('sprint_added', 'sprint_removed');

ALTER TABLE activity DROP CONSTRAINT activity_kind_is_known;

ALTER TABLE activity
    ADD CONSTRAINT activity_kind_is_known
        CHECK (kind IN ('created', 'status_changed', 'assignee_changed', 'parent_changed',
                        'label_added', 'label_removed', 'archived', 'unarchived',
                        'aspect_type_changed', 'field_changed'));
