-- LAM-3 step 2: the search index.
--
-- Every row here is DERIVED from a document's body blob and can be thrown
-- away and regenerated (step 4). Nothing may be stored here that does not
-- exist in a blob somewhere - the moment that stops being true, the rebuild
-- becomes lossy and the table stops being disposable.

CREATE TABLE search_index (
    document_id   uuid  NOT NULL  REFERENCES document(id) ON DELETE CASCADE,
    field_id      text  NOT NULL,
    content       text  NOT NULL,

    PRIMARY KEY (document_id, field_id)
);