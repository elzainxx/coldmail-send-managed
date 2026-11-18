-- Pre-flight external-table existence check (OD-8 / ADR-0007).
-- Runs BEFORE the migration chain, in CI and again at deploy time. Checks EXISTENCE only
-- (not column shape — a shape check would break whenever the platform legitimately adds a
-- column). Fails fast and loud so an FK-bearing migration never dies with an opaque
-- "relation public.job_items does not exist".
--
-- ONE COLUMN IS REQUIRED BY NAME, and that is not a shape check: public.job_items.rendered_text
-- (the optional plain-text alternative of a message, Phase 3 close-out B3). POST /v1/run copies it, in the
-- same statement that copies rendered_body, so a platform schema that does not have it yet would make every
-- run answer 500. The platform adds the column (nullable, no default) in its own migration BEFORE this
-- module is deployed against it; this check turns "deployed in the wrong order" into a refused migration
-- instead of a failing run. Any further columns the platform adds are still none of this module's business.
--
-- Assumed shapes (platform-owned, confirm before Phase 1 live — see ADR-0007):
--   public.users(id uuid pk, email, is_active, created_at)
--   public.job_items(id uuid pk, campaign_id, user_id, recipient_email,
--                    rendered_subject, rendered_body, rendered_text (NULL), reply_to, status, created_at)
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_tables WHERE schemaname='public' AND tablename='users') THEN
        RAISE EXCEPTION 'Preflight failed: public.users does not exist. Deploy the platform schema first.';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_tables WHERE schemaname='public' AND tablename='job_items') THEN
        RAISE EXCEPTION 'Preflight failed: public.job_items does not exist. Deploy the platform schema first.';
    END IF;
    IF NOT EXISTS (SELECT FROM information_schema.columns
                    WHERE table_schema='public' AND table_name='job_items' AND column_name='rendered_text') THEN
        RAISE EXCEPTION 'Preflight failed: public.job_items has no rendered_text column. Deploy the platform migration that adds it (nullable text) first.';
    END IF;
END $$;
