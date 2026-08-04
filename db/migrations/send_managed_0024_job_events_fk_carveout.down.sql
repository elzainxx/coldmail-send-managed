-- send_managed_0024_job_events_fk_carveout.down.sql — restore send_managed_0008's UNCONDITIONAL reject-trigger
-- body verbatim.
--
-- Reversing this migration deliberately REINSTATES the wedge it fixed (ADR-0012): after this runs,
-- `DELETE /v1/admin/accounts/{id}` returns 500 again for any account that has ever been assigned a
-- job, and deleting a worker with job history fails identically — because the FK's own
-- `UPDATE ONLY … SET account_id = NULL` maintenance statement is rejected once more. That is what
-- "reversible" means here: the down path returns the schema EXACTLY to its pre-0024 shape rather than
-- to a half-corrected one, since a down migration that keeps part of an up migration's behavior is not
-- a reverse and cannot be reasoned about.
--
-- No data migration is needed in either direction. The carve-out only ever changes which UPDATEs are
-- PERMITTED; it rewrites nothing. Rows whose account_id/box_id were nulled by a real parent delete
-- while 0024 was applied stay nulled after this down — correctly, because the accounts and workers they
-- pointed at are gone and re-pointing them is precisely the history rewrite the trigger forbids.
--
-- The function body is restored with CREATE OR REPLACE (same signature), so the trigger created by
-- send_managed_0008 keeps pointing at it and is not dropped/recreated.

CREATE OR REPLACE FUNCTION send_managed.fn_job_events_reject_update()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'send_managed.job_events is append-only: UPDATE forbidden'
        USING HINT = 'Events are immutable audit records',
              ERRCODE = 'integrity_constraint_violation';
END;
$$ LANGUAGE plpgsql;
ALTER FUNCTION send_managed.fn_job_events_reject_update() OWNER TO send_managed_owner;

DELETE FROM send_managed.schema_migrations WHERE version = '0024';
