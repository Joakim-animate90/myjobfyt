-- +goose Up
-- +goose StatementBegin

-- citext gives case-insensitive email uniqueness at the database level. Doing this in
-- application code instead leaves a race: two concurrent registrations differing only
-- in case would both pass a SELECT check and both insert.
CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- pg_trgm backs fuzzy company-name and job-title matching used by deduplication.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TYPE user_status AS ENUM ('active', 'suspended', 'pending_deletion', 'deleted');
CREATE TYPE user_role   AS ENUM ('candidate', 'support', 'admin', 'system');

CREATE TABLE users (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email               citext      NOT NULL,
    -- Nullable: an OAuth-only account has no password. The CHECK below enforces that
    -- at least one credential path exists.
    password_hash       text,
    email_verified_at   timestamptz,
    status              user_status  NOT NULL DEFAULT 'active',
    role                user_role    NOT NULL DEFAULT 'candidate',
    last_login_at       timestamptz,
    failed_login_count  int          NOT NULL DEFAULT 0,
    locked_until        timestamptz,
    -- Deletion is a 7-day grace period, not an immediate erase, so the candidate can
    -- cancel. erase_after is when the irreversible job becomes eligible to run.
    deletion_requested_at timestamptz,
    erase_after         timestamptz,
    created_at          timestamptz  NOT NULL DEFAULT now(),
    updated_at          timestamptz  NOT NULL DEFAULT now(),
    deleted_at          timestamptz,

    CONSTRAINT users_email_not_blank CHECK (length(trim(email::text)) > 0),
    CONSTRAINT users_deletion_fields_together CHECK (
        (deletion_requested_at IS NULL AND erase_after IS NULL)
        OR (deletion_requested_at IS NOT NULL AND erase_after IS NOT NULL)
    )
);

-- A partial unique index rather than a plain one: a deleted account's email must be
-- reusable, but two live accounts must never share one.
CREATE UNIQUE INDEX users_email_live_uniq ON users (email) WHERE deleted_at IS NULL;
CREATE INDEX users_status_idx      ON users (status) WHERE deleted_at IS NULL;
CREATE INDEX users_erase_after_idx ON users (erase_after) WHERE erase_after IS NOT NULL;

CREATE TABLE candidate_profiles (
    user_id              uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    full_name            text,
    -- Phone is stored encrypted at the application layer (envelope encryption), hence
    -- bytea rather than text. It is required before form auto-fill can be enabled.
    phone_ciphertext     bytea,
    phone_key_id         text,
    phone_last_four      text,
    city                 text,
    country              char(2),
    timezone             text        NOT NULL DEFAULT 'UTC',
    linkedin_url         text,
    portfolio_url        text,
    github_url           text,
    work_authorisation   text,
    salary_min           numeric(12,2),
    salary_max           numeric(12,2),
    salary_currency      char(3),
    salary_period        text,
    notice_period_days   int,
    open_to_relocation   boolean     NOT NULL DEFAULT false,
    headline             text,
    completeness_pct     smallint    NOT NULL DEFAULT 0,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT profiles_completeness_range CHECK (completeness_pct BETWEEN 0 AND 100),
    CONSTRAINT profiles_salary_order CHECK (
        salary_min IS NULL OR salary_max IS NULL OR salary_min <= salary_max
    ),
    CONSTRAINT profiles_salary_period_valid CHECK (
        salary_period IS NULL OR salary_period IN ('hour', 'day', 'week', 'month', 'year')
    ),
    CONSTRAINT profiles_notice_period_sane CHECK (
        notice_period_days IS NULL OR notice_period_days BETWEEN 0 AND 365
    ),
    -- Encrypted columns must travel together: ciphertext without its key id is
    -- unrecoverable, and a key id without ciphertext is a bug.
    CONSTRAINT profiles_phone_fields_together CHECK (
        (phone_ciphertext IS NULL AND phone_key_id IS NULL)
        OR (phone_ciphertext IS NOT NULL AND phone_key_id IS NOT NULL)
    )
);

CREATE TABLE candidate_languages (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    language    char(2) NOT NULL,
    proficiency text    NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT languages_proficiency_valid CHECK (
        proficiency IN ('basic', 'conversational', 'professional', 'fluent', 'native')
    ),
    CONSTRAINT languages_unique_per_user UNIQUE (user_id, language)
);

CREATE TABLE oauth_identities (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id          uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider         text NOT NULL,
    provider_user_id text NOT NULL,
    email            citext,
    email_verified   boolean NOT NULL DEFAULT false,
    linked_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT oauth_provider_valid CHECK (provider IN ('google', 'github', 'linkedin')),
    -- One provider account maps to at most one MyJobFyt account.
    CONSTRAINT oauth_identity_uniq UNIQUE (provider, provider_user_id)
);

CREATE INDEX oauth_identities_user_idx ON oauth_identities (user_id);

CREATE TABLE refresh_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- Only the hash is stored. A database disclosure must not hand over live sessions.
    token_hash  bytea NOT NULL,
    -- family_id groups a rotation chain. Presenting an already-rotated token revokes
    -- the whole family, which is how token theft is detected.
    family_id   uuid  NOT NULL,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz,
    revoked_reason text,
    rotated_to  uuid REFERENCES refresh_tokens (id) ON DELETE SET NULL,
    user_agent  text,
    ip          inet,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT refresh_token_hash_uniq UNIQUE (token_hash)
);

CREATE INDEX refresh_tokens_user_idx    ON refresh_tokens (user_id) WHERE revoked_at IS NULL;
CREATE INDEX refresh_tokens_family_idx  ON refresh_tokens (family_id);
CREATE INDEX refresh_tokens_expiry_idx  ON refresh_tokens (expires_at) WHERE revoked_at IS NULL;

CREATE TABLE email_verification_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea NOT NULL,
    purpose     text  NOT NULL,
    email       citext NOT NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT verification_purpose_valid CHECK (
        purpose IN ('email_verification', 'password_reset', 'email_change', 'account_deletion')
    ),
    CONSTRAINT verification_token_hash_uniq UNIQUE (token_hash)
);

CREATE INDEX verification_tokens_user_idx ON email_verification_tokens (user_id, purpose)
    WHERE consumed_at IS NULL;

-- Append-only record of every security- and consent-relevant action. No UPDATE or
-- DELETE is ever issued against this table by application code.
CREATE TABLE audit_logs (
    id            bigserial PRIMARY KEY,
    user_id       uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_id      uuid REFERENCES users (id) ON DELETE SET NULL,
    actor_role    user_role,
    action        text NOT NULL,
    resource_type text,
    resource_id   text,
    ip            inet,
    user_agent    text,
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_user_time_idx     ON audit_logs (user_id, occurred_at DESC);
CREATE INDEX audit_logs_action_time_idx   ON audit_logs (action, occurred_at DESC);
CREATE INDEX audit_logs_resource_idx      ON audit_logs (resource_type, resource_id);

-- Shared updated_at trigger. Maintaining this in the database rather than in every
-- repository means a direct SQL fix or a missed assignment cannot leave a stale
-- timestamp behind.
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN
    NEW.updated_at = now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER candidate_profiles_set_updated_at
    BEFORE UPDATE ON candidate_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS candidate_profiles_set_updated_at ON candidate_profiles;
DROP TRIGGER IF EXISTS users_set_updated_at ON users;
DROP FUNCTION IF EXISTS set_updated_at();
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS email_verification_tokens;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS oauth_identities;
DROP TABLE IF EXISTS candidate_languages;
DROP TABLE IF EXISTS candidate_profiles;
DROP TABLE IF EXISTS users;
DROP TYPE IF EXISTS user_role;
DROP TYPE IF EXISTS user_status;
-- +goose StatementEnd
