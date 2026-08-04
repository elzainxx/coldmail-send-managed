-- send_managed_0004_create_pools.up.sql — Phase 1: send_managed.pools (first-class, plural pools).
-- Many shared and many dedicated pools are permitted (no UNIQUE on owner_user_id — U2). Owned
-- by send_managed_owner so FORCE RLS (0012) enforces. FKs to public.users assume the platform table
-- exists (guaranteed by the pre-flight existence check that runs before the chain).

CREATE TABLE send_managed.pools (
    -- Identity
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,

    -- Pool type and ownership
    kind send_managed.pool_kind NOT NULL,
    owner_user_id UUID,

    -- Fallback chain (self-referential)
    fallback_pool_id UUID,

    -- Configuration
    name TEXT NOT NULL,
    routing_strategy send_managed.routing_strategy NOT NULL DEFAULT 'round_robin',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Constraints
    CONSTRAINT pools_user_fk
        FOREIGN KEY (user_id)
        REFERENCES public.users(id)
        ON DELETE RESTRICT,

    CONSTRAINT pools_owner_user_fk
        FOREIGN KEY (owner_user_id)
        REFERENCES public.users(id)
        ON DELETE CASCADE,

    CONSTRAINT pools_fallback_fk
        FOREIGN KEY (fallback_pool_id)
        REFERENCES send_managed.pools(id)
        ON DELETE SET NULL,

    CONSTRAINT pools_kind_ownership_check
        CHECK (
            (kind = 'shared' AND owner_user_id IS NULL)
            OR (kind = 'dedicated' AND owner_user_id IS NOT NULL)
        ),

    CONSTRAINT pools_fallback_not_self_check
        CHECK (fallback_pool_id IS DISTINCT FROM id)
);

ALTER TABLE send_managed.pools OWNER TO send_managed_owner;

INSERT INTO send_managed.schema_migrations (version) VALUES ('0004');
