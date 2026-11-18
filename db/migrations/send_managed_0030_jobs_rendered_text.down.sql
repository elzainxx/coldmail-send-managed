-- send_managed_0030_jobs_rendered_text.down.sql -- reverse send_managed_0030 (dev/CI only; prod is
-- forward-only): drop the size CHECK, then the column.
--
-- WHAT THIS DOWN DESTROYS. Every stored plain-text part: the column is dropped with its data. That is the
-- point of a down, and it loses nothing the platform needs: public.job_items.rendered_text (the platform's
-- own column) still holds the text, and the older Control that a revert goes back to never names the column,
-- so a job that was not sent yet is sent as a single text/html message. This down belongs to a source
-- revert: run alone against the new source, the claim query (which names the column) would fail.

ALTER TABLE send_managed.jobs DROP CONSTRAINT IF EXISTS jobs_rendered_text_size_check;
ALTER TABLE send_managed.jobs DROP COLUMN IF EXISTS rendered_text;

DELETE FROM send_managed.schema_migrations WHERE version = '0030';
