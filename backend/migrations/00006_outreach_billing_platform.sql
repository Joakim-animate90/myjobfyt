-- +goose Up
-- +goose StatementBegin

CREATE TYPE contact_provenance AS ENUM (
    'job_posting', 'company_careers_page', 'licensed_provider', 'candidate_supplied'
);
CREATE TYPE contact_verification AS ENUM ('unverified', 'verified', 'invalid');
CREATE TYPE message_direction AS ENUM ('outbound', 'inbound');
CREATE TYPE message_status AS ENUM (
    'queued', 'sent', 'delivered', 'bounced', 'complained', 'failed', 'suppressed'
);
CREATE TYPE send_channel AS ENUM ('platform_smtp', 'user_smtp');
CREATE TYPE suppression_reason AS ENUM ('bounce', 'complaint', 'unsubscribe', 'manual');
CREATE TYPE subscription_status AS ENUM (
    'trialing', 'active', 'past_due', 'canceled', 'incomplete'
);
CREATE TYPE background_job_status AS ENUM (
    'queued', 'running', 'succeeded', 'failed', 'dead'
);

-- Every contact records its provenance and lawful basis. Pattern-guessed addresses are
-- structurally impossible: 'guessed' is not a value of contact_provenance.
CREATE TABLE recruiter_contacts (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id     uuid NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    job_posting_id uuid REFERENCES job_postings (id) ON DELETE SET NULL,
    email          citext NOT NULL,
    name           text,
    title          text,
    provenance     contact_provenance NOT NULL,
    legal_basis    text NOT NULL,
    verification_status contact_verification NOT NULL DEFAULT 'unverified',
    verified_at    timestamptz,
    source_url     text,
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT recruiter_contacts_email_company_uniq UNIQUE (company_id, email)
);

CREATE INDEX recruiter_contacts_email_idx   ON recruiter_contacts (email);
CREATE INDEX recruiter_contacts_posting_idx ON recruiter_contacts (job_posting_id)
    WHERE job_posting_id IS NOT NULL;

-- Checked before EVERY send, without exception.
CREATE TABLE suppression_entries (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Either a full address or a bare domain.
    email_or_domain citext NOT NULL,
    scope       text NOT NULL DEFAULT 'global',
    user_id     uuid REFERENCES users (id) ON DELETE CASCADE,
    reason      suppression_reason NOT NULL,
    detail      text,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT suppression_scope_valid CHECK (scope IN ('global', 'user')),
    CONSTRAINT suppression_user_scope_has_user CHECK (
        (scope = 'global' AND user_id IS NULL) OR (scope = 'user' AND user_id IS NOT NULL)
    )
);

CREATE UNIQUE INDEX suppression_global_uniq ON suppression_entries (email_or_domain)
    WHERE scope = 'global';
CREATE UNIQUE INDEX suppression_user_uniq   ON suppression_entries (user_id, email_or_domain)
    WHERE scope = 'user';

CREATE TABLE user_smtp_configs (
    user_id     uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    host        text NOT NULL,
    port        int  NOT NULL,
    username    text NOT NULL,
    -- Envelope-encrypted. Write-only through the API: never returned to a client,
    -- never logged. A plaintext column here would be a credential breach waiting for
    -- a database disclosure.
    password_ciphertext bytea NOT NULL,
    key_id      text NOT NULL,
    from_name   text,
    use_tls     boolean NOT NULL DEFAULT true,
    verified_at timestamptz,
    last_error  text,
    disabled_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT smtp_port_range CHECK (port BETWEEN 1 AND 65535)
);

CREATE TABLE outreach_messages (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    application_id uuid REFERENCES applications (id) ON DELETE SET NULL,
    contact_id     uuid REFERENCES recruiter_contacts (id) ON DELETE SET NULL,
    direction      message_direction NOT NULL,
    subject        text,
    body           text,
    provider_message_id text,
    reply_to_address citext,
    tracking_token text,
    sent_via       send_channel,
    status         message_status NOT NULL DEFAULT 'queued',
    in_reply_to    uuid REFERENCES outreach_messages (id) ON DELETE SET NULL,
    sent_at        timestamptz,
    opened_at      timestamptz,
    replied_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT outreach_outbound_has_channel CHECK (
        direction <> 'outbound' OR sent_via IS NOT NULL
    )
);

CREATE INDEX outreach_user_idx        ON outreach_messages (user_id, created_at DESC);
CREATE INDEX outreach_application_idx ON outreach_messages (application_id)
    WHERE application_id IS NOT NULL;
CREATE UNIQUE INDEX outreach_tracking_token_uniq ON outreach_messages (tracking_token)
    WHERE tracking_token IS NOT NULL;
CREATE UNIQUE INDEX outreach_reply_address_uniq  ON outreach_messages (reply_to_address)
    WHERE reply_to_address IS NOT NULL;

CREATE TABLE outreach_events (
    id          bigserial PRIMARY KEY,
    message_id  uuid NOT NULL REFERENCES outreach_messages (id) ON DELETE CASCADE,
    type        text NOT NULL,
    payload     jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT outreach_event_type_valid CHECK (
        type IN ('sent', 'delivered', 'opened', 'replied', 'bounced', 'complained', 'failed')
    )
);

CREATE INDEX outreach_events_message_idx ON outreach_events (message_id, occurred_at);

-- Plans hold quotas and feature flags as data, so a tier change is a config change
-- rather than a code change scattered through the domain packages.
CREATE TABLE plans (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        text NOT NULL,
    name        text NOT NULL,
    price_cents int  NOT NULL DEFAULT 0,
    currency    char(3) NOT NULL DEFAULT 'EUR',
    interval    text NOT NULL DEFAULT 'month',
    quotas      jsonb NOT NULL DEFAULT '{}'::jsonb,
    features    jsonb NOT NULL DEFAULT '{}'::jsonb,
    is_public   boolean NOT NULL DEFAULT true,
    sort_order  int NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT plans_slug_uniq UNIQUE (slug),
    CONSTRAINT plans_price_non_negative CHECK (price_cents >= 0),
    CONSTRAINT plans_interval_valid CHECK (interval IN ('month', 'year', 'none'))
);

CREATE TABLE subscriptions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    plan_id     uuid NOT NULL REFERENCES plans (id) ON DELETE RESTRICT,
    provider    text NOT NULL DEFAULT 'stripe',
    provider_subscription_id text,
    provider_customer_id     text,
    status      subscription_status NOT NULL DEFAULT 'active',
    current_period_start timestamptz,
    current_period_end   timestamptz,
    cancel_at_period_end boolean NOT NULL DEFAULT false,
    canceled_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    -- One live subscription per user; a second would make entitlement resolution
    -- ambiguous at exactly the moment money is involved.
    CONSTRAINT subscriptions_user_uniq UNIQUE (user_id),
    CONSTRAINT subscriptions_period_order CHECK (
        current_period_start IS NULL OR current_period_end IS NULL
        OR current_period_start < current_period_end
    )
);

CREATE UNIQUE INDEX subscriptions_provider_id_uniq
    ON subscriptions (provider, provider_subscription_id)
    WHERE provider_subscription_id IS NOT NULL;

CREATE TABLE quota_usage (
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    quota_key    text NOT NULL,
    period_start date NOT NULL,
    used         bigint NOT NULL DEFAULT 0,
    -- The limit in force when the period began, so a mid-period plan change does not
    -- retroactively rewrite what the candidate was told.
    limit_snapshot bigint NOT NULL,
    updated_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (user_id, quota_key, period_start),
    CONSTRAINT quota_usage_non_negative CHECK (used >= 0)
);

CREATE TABLE invoices (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    provider_invoice_id text NOT NULL,
    amount_cents int NOT NULL,
    currency    char(3) NOT NULL,
    status      text NOT NULL,
    issued_at   timestamptz,
    paid_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT invoices_provider_uniq UNIQUE (provider_invoice_id)
);

-- Webhook idempotency keyed on the PROVIDER's event id, because payment providers
-- deliver duplicates and out-of-order events routinely and their id is the only stable
-- identity available.
CREATE TABLE provider_webhook_events (
    provider          text NOT NULL,
    provider_event_id text NOT NULL,
    event_type        text NOT NULL,
    payload           jsonb NOT NULL,
    processed_at      timestamptz,
    received_at       timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (provider, provider_event_id)
);

CREATE TABLE notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    category   text NOT NULL,
    title      text NOT NULL,
    body       text,
    link       text,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX notifications_unread_idx ON notifications (user_id, created_at DESC)
    WHERE read_at IS NULL;

CREATE TABLE notification_preferences (
    user_id  uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    category text NOT NULL,
    in_app   boolean NOT NULL DEFAULT true,
    email    boolean NOT NULL DEFAULT true,
    digest   boolean NOT NULL DEFAULT false,

    PRIMARY KEY (user_id, category)
);

-- Scheduler-owned recurring and one-off work that is not event-driven.
CREATE TABLE background_jobs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind            text NOT NULL,
    status          background_job_status NOT NULL DEFAULT 'queued',
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    attempts        int NOT NULL DEFAULT 0,
    max_attempts    int NOT NULL DEFAULT 5,
    last_error      text,
    scheduled_for   timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    finished_at     timestamptz,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT background_jobs_attempts_sane CHECK (attempts >= 0 AND max_attempts >= 1)
);

CREATE UNIQUE INDEX background_jobs_idempotency_uniq ON background_jobs (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE INDEX background_jobs_due_idx ON background_jobs (scheduled_for)
    WHERE status = 'queued';

-- The backbone of at-least-once safety for event consumers. Postgres rather than
-- Redis: an evicted key would silently re-enable a duplicate side effect.
CREATE TABLE processed_messages (
    idempotency_key  text PRIMARY KEY,
    consumer         text NOT NULL,
    state            text NOT NULL,
    attempts         int NOT NULL DEFAULT 1,
    -- A lease, not a lock. An in_flight row whose lease has lapsed means the worker
    -- holding it is gone and the claim may be taken over, which is what stops one
    -- crashed worker from stranding a message forever.
    lease_expires_at timestamptz,
    processed_at     timestamptz,
    expires_at       timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT processed_messages_state_valid CHECK (state IN ('in_flight', 'done')),
    CONSTRAINT processed_messages_done_has_timestamp CHECK (
        state <> 'done' OR processed_at IS NOT NULL
    )
);

CREATE INDEX processed_messages_expiry_idx ON processed_messages (expires_at)
    WHERE state = 'done';
CREATE INDEX processed_messages_lease_idx  ON processed_messages (lease_expires_at)
    WHERE state = 'in_flight';

-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER user_smtp_configs_set_updated_at
    BEFORE UPDATE ON user_smtp_configs
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER plans_set_updated_at
    BEFORE UPDATE ON plans
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER subscriptions_set_updated_at
    BEFORE UPDATE ON subscriptions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER quota_usage_set_updated_at
    BEFORE UPDATE ON quota_usage
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS quota_usage_set_updated_at ON quota_usage;
DROP TRIGGER IF EXISTS subscriptions_set_updated_at ON subscriptions;
DROP TRIGGER IF EXISTS plans_set_updated_at ON plans;
DROP TRIGGER IF EXISTS user_smtp_configs_set_updated_at ON user_smtp_configs;
DROP TABLE IF EXISTS processed_messages;
DROP TABLE IF EXISTS background_jobs;
DROP TABLE IF EXISTS notification_preferences;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS provider_webhook_events;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS quota_usage;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS plans;
DROP TABLE IF EXISTS outreach_events;
DROP TABLE IF EXISTS outreach_messages;
DROP TABLE IF EXISTS user_smtp_configs;
DROP TABLE IF EXISTS suppression_entries;
DROP TABLE IF EXISTS recruiter_contacts;
DROP TYPE IF EXISTS background_job_status;
DROP TYPE IF EXISTS subscription_status;
DROP TYPE IF EXISTS suppression_reason;
DROP TYPE IF EXISTS send_channel;
DROP TYPE IF EXISTS message_status;
DROP TYPE IF EXISTS message_direction;
DROP TYPE IF EXISTS contact_verification;
DROP TYPE IF EXISTS contact_provenance;
-- +goose StatementEnd
