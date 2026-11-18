-- DEV FIXTURE ONLY — NOT the real platform schema (ADR-0007/OD-8).
-- The send_managed module reads two platform-owned tables read-only (public.users,
-- public.job_items). In prod these are owned and created by the PLATFORM. Here we seed
-- minimal fixtures of the ASSUMED shapes so the migration pre-flight (existence check)
-- passes locally and the worker/jobs FKs in Phase 1 have real targets.
--
-- Confirm the exact columns against the real platform schema before Phase 1 runs live
-- (standing action item — the pre-flight checks EXISTENCE only, not column shape).

CREATE TABLE IF NOT EXISTS public.users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      text,
    is_active  boolean DEFAULT true,
    created_at timestamptz DEFAULT now()
);

CREATE TABLE IF NOT EXISTS public.job_items (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    campaign_id      uuid,
    user_id          uuid,
    recipient_email  text,
    rendered_subject text,
    rendered_body    text,
    rendered_text    text,
    reply_to         text,
    status           text,
    created_at       timestamptz DEFAULT now()
);
