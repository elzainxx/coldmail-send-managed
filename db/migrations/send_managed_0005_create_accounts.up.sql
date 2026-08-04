-- send_managed_0005_create_accounts.up.sql — Phase 1: send_managed.accounts (crown-jewel credential table).
-- Only the SMTP secret is encrypted (AES-256-GCM, key OUTSIDE the DB — §9); host/port/username
-- stay plaintext. Three credential columns exactly (no credential_alg / credential_encrypted_at
-- — DECIDED not adopted). Owned by send_managed_owner so FORCE RLS (0012) genuinely enforces the RLS
-- policy against send_managed_app (ADR-0004). No credential-integrity CHECKs are added in Phase 1
-- (the 03-encryption draft marks them recommended/optional, not locked — the encode contract is
-- Phase 2); the three columns are NOT NULL as locked in 01-backbone.

CREATE TABLE send_managed.accounts (
    -- Identity
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL,

    -- Pool membership (exactly one pool per account)
    pool_id UUID NOT NULL,

    -- SMTP connection details (plaintext; not secret)
    smtp_host TEXT NOT NULL,
    smtp_port INTEGER NOT NULL DEFAULT 587,
    smtp_username TEXT NOT NULL,

    -- Encrypted credential (only the SMTP secret; encrypt/decrypt is Phase 2)
    credential_ciphertext BYTEA NOT NULL,   -- AEAD ciphertext WITH 16-byte GCM tag appended
    credential_key_id TEXT NOT NULL,        -- opaque key-version label; drives rotation
    credential_nonce BYTEA NOT NULL,        -- 12-byte GCM nonce, unique per encryption

    -- SendManaged identity
    from_address TEXT NOT NULL,
    from_name TEXT,

    -- Status and health (independent flags; score keeps running underneath)
    account_status_score SMALLINT NOT NULL DEFAULT 100,
    paused BOOLEAN NOT NULL DEFAULT FALSE,
    pause_reason send_managed.pause_reason,
    quarantined BOOLEAN NOT NULL DEFAULT FALSE,
    disabled BOOLEAN NOT NULL DEFAULT FALSE,

    -- Daily limit tracking (lazy reset is source of truth — §11)
    daily_limit INTEGER NOT NULL DEFAULT 500,
    daily_sent_count INTEGER NOT NULL DEFAULT 0,
    daily_reset_day DATE NOT NULL DEFAULT CURRENT_DATE,

    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_success_at TIMESTAMPTZ,
    last_failure_at TIMESTAMPTZ,

    -- Constraints
    CONSTRAINT accounts_user_fk
        FOREIGN KEY (user_id)
        REFERENCES public.users(id)
        ON DELETE CASCADE,

    CONSTRAINT accounts_pool_fk
        FOREIGN KEY (pool_id)
        REFERENCES send_managed.pools(id)
        ON DELETE RESTRICT,

    CONSTRAINT accounts_smtp_port_check
        CHECK (smtp_port > 0 AND smtp_port <= 65535),

    CONSTRAINT accounts_status_score_check
        CHECK (account_status_score >= 0 AND account_status_score <= 100),

    CONSTRAINT accounts_daily_limit_check
        CHECK (daily_limit > 0),

    CONSTRAINT accounts_daily_sent_check
        CHECK (daily_sent_count >= 0 AND daily_sent_count <= daily_limit),

    CONSTRAINT accounts_from_address_valid_check
        CHECK (from_address ~* '^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$'),

    -- Every pause names its reason; the daily reset clears ONLY daily_limit pauses.
    CONSTRAINT accounts_pause_reason_check
        CHECK (
            (paused = FALSE AND pause_reason IS NULL)
            OR (paused = TRUE AND pause_reason IS NOT NULL)
        )
);

ALTER TABLE send_managed.accounts OWNER TO send_managed_owner;

INSERT INTO send_managed.schema_migrations (version) VALUES ('0005');
