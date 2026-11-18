-- send_managed_0030_jobs_rendered_text.up.sql -- Phase 3 close-out B3: THE OPTIONAL PLAIN-TEXT PART.
--
-- send_managed.jobs freezes a denormalized copy of the platform's rendered message at run time
-- (rendered_subject, rendered_body, reply_to ...). This adds the optional plain-text alternative beside
-- rendered_body: a NULLABLE text column that `POST /v1/run` copies from the platform-owned
-- public.job_items.rendered_text (the platform adds that column in its own migration and writes it; the
-- module only ever reads it) in the same statement that copies rendered_body.
--
--   rendered_text IS NULL, '' or whitespace only  =>  no plain-text part: the worker renders the single
--       text/html message it has always rendered, byte for byte.
--   rendered_text has a non-whitespace character  =>  the worker renders multipart/alternative: the
--       text/plain part first, the text/html part last (RFC 2046 section 5.1.4).
--
-- ADDITIVE: one nullable column with no default, so every existing row reads as ABSENT, and an older Control
-- binary running against this schema never names the column. The CHECK bounds the size of the part (1 MiB in
-- bytes, the same number as the admin API's body cap) so one oversized text cannot bloat a row or a push_job
-- frame: an INSERT that exceeds it fails 23514, which POST /v1/run answers as 400 validation_failed (the
-- existing job_item_invalid path) and writes nothing. Rule 13: the predicate says IS NULL explicitly.

ALTER TABLE send_managed.jobs
    ADD COLUMN rendered_text TEXT,
    ADD CONSTRAINT jobs_rendered_text_size_check
        CHECK (rendered_text IS NULL OR octet_length(rendered_text) <= 1048576);

COMMENT ON COLUMN send_managed.jobs.rendered_text IS
    'Optional plain-text alternative of the message, frozen at run from public.job_items.rendered_text (NULL when the platform wrote none). NULL, empty and whitespace-only all mean "no text part": the worker then sends the single text/html message exactly as before. Otherwise the worker builds multipart/alternative, text/plain first, text/html last. At most 1 MiB (jobs_rendered_text_size_check).';

INSERT INTO send_managed.schema_migrations (version) VALUES ('0030');
