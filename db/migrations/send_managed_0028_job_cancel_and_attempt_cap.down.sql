-- send_managed_0028_job_cancel_and_attempt_cap.down.sql -- reverse send_managed_0028 (dev/CI only; prod is
-- forward-only). Removes, in dependency order: the two dial rows, the cancel guard (trigger, then its
-- function), the four columns (which takes the three CHECKs that reference them with it), and restores
-- the ORIGINAL jobs_completed_at_valid_check from send_managed_0007, verbatim.
--
-- WHAT THIS DOWN REFUSES TO DO, ON PURPOSE. Restoring the original completed_at CHECK re-validates every
-- row against a constraint that has no idea `cancelled` exists, so it FAILS (23514, and the whole down
-- rolls back) if any job is in state `cancelled`: such a row is `state NOT IN (sent, failed, bounced)
-- AND completed_at IS NOT NULL`. That is the right outcome. The alternative -- deleting or rewriting the
-- cancelled rows to make the constraint pass -- would destroy the record that the platform asked for the
-- stop, which a schema revert has no business doing. A database that has cancelled rows is a database
-- that has used the feature, and reverting means deciding what to do with those rows first.
--
-- 0027 (the enum value) is NOT undone by this file or by its own down: Postgres cannot drop an enum
-- value. After this down the `cancelled` label still exists in the type but nothing can legally hold it.
--
-- WHAT THIS DOWN CANNOT DO. The Go side READS the two dials (dispatch/config.go LoadConfig, which errors
-- when a row is absent) and writes the new columns, so running this WITHOUT reverting the Go source
-- leaves a Control that does not start -- the same deliberate, loud coupling as send_managed_0025/0026's
-- downs. This down belongs to a source revert, not to a standalone `psql -f`.

DELETE FROM send_managed.config WHERE key IN ('job_max_failed_attempts', 'job_failed_max_age_sec');

DROP TRIGGER IF EXISTS trg_jobs_cancel_guard ON send_managed.jobs;
DROP FUNCTION IF EXISTS send_managed.fn_jobs_cancel_guard();

-- The three 0028 CHECKs reference these columns and are dropped with them; named explicitly anyway so the
-- intent survives a reader who does not know that rule.
ALTER TABLE send_managed.jobs
    DROP CONSTRAINT IF EXISTS jobs_terminal_reason_state_check,
    DROP CONSTRAINT IF EXISTS jobs_first_failed_at_check,
    DROP CONSTRAINT IF EXISTS jobs_failed_attempt_count_check;

ALTER TABLE send_managed.jobs DROP CONSTRAINT jobs_completed_at_valid_check;
ALTER TABLE send_managed.jobs
    DROP COLUMN IF EXISTS terminal_reason,
    DROP COLUMN IF EXISTS cancel_requested_at,
    DROP COLUMN IF EXISTS first_failed_at,
    DROP COLUMN IF EXISTS failed_attempt_count;
ALTER TABLE send_managed.jobs ADD CONSTRAINT jobs_completed_at_valid_check
    CHECK (
        (state IN ('sent', 'failed', 'bounced') AND completed_at IS NOT NULL)
        OR (state NOT IN ('sent', 'failed', 'bounced') AND completed_at IS NULL)
    );

DELETE FROM send_managed.schema_migrations WHERE version = '0028';
