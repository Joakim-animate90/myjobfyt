-- +goose Up
-- +goose StatementBegin

CREATE TYPE application_method AS ENUM (
    'ats_api', 'public_form', 'authorised_email', 'extension', 'manual'
);

CREATE TYPE application_status AS ENUM (
    'prepared',
    'blocked',
    'questions_pending',
    'awaiting_authorisation',
    'ready',
    'submitting',
    'submitted',
    'email_sent',
    'applied_externally',
    'extension_pending',
    'suppressed_cooldown',
    'external_required',
    'failed',
    -- A dispatched request whose response was lost. First-class, because recording
    -- uncertainty is correct and guessing is not.
    'outcome_unknown',
    'expired',
    'interview',
    'offer',
    'rejected'
);

CREATE TYPE authorisation_kind AS ENUM ('per_application', 'standing');
CREATE TYPE answer_source      AS ENUM ('candidate', 'saved', 'ai');

-- A bounded standing authorisation: one loop, a daily cap, a minimum score, an expiry,
-- and a snapshot of exactly what the candidate agreed to at the time they agreed to it.
CREATE TABLE standing_authorisations (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    loop_id     uuid NOT NULL REFERENCES loops (id) ON DELETE CASCADE,
    daily_cap   int  NOT NULL,
    min_score   smallint NOT NULL,
    expires_at  timestamptz NOT NULL,
    granted_at  timestamptz NOT NULL DEFAULT now(),
    granted_ip  inet,
    granted_user_agent text,
    revoked_at  timestamptz,
    snapshot    jsonb NOT NULL,

    CONSTRAINT standing_auth_cap_positive CHECK (daily_cap > 0),
    CONSTRAINT standing_auth_min_score_range CHECK (min_score BETWEEN 0 AND 100),
    -- An authorisation that never expires is not an informed consent.
    CONSTRAINT standing_auth_expiry_future CHECK (expires_at > granted_at)
);

-- At most one live standing authorisation per loop, so revocation is unambiguous.
CREATE UNIQUE INDEX standing_auth_one_live_per_loop
    ON standing_authorisations (loop_id) WHERE revoked_at IS NULL;
CREATE INDEX standing_auth_user_idx ON standing_authorisations (user_id);

CREATE TABLE applications (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    job_match_id    uuid NOT NULL REFERENCES job_matches (id) ON DELETE CASCADE,
    job_posting_id  uuid NOT NULL REFERENCES job_postings (id) ON DELETE RESTRICT,
    loop_id         uuid NOT NULL REFERENCES loops (id) ON DELETE CASCADE,
    -- RESTRICT, not CASCADE: a CV version used in a submitted application must never
    -- disappear, or the audit trail stops being a record of what was actually sent.
    cv_version_id   uuid NOT NULL REFERENCES cv_versions (id) ON DELETE RESTRICT,

    method          application_method NOT NULL,
    method_reason   text NOT NULL,
    status          application_status NOT NULL DEFAULT 'prepared',
    blocked_reason  text,
    failure_reason  text,
    retry_count     int NOT NULL DEFAULT 0,

    -- Immutable prepared artefacts: byte-for-byte what was or will be sent. Without
    -- these the audit trail is decorative.
    prepared_cover_letter text,
    prepared_email_subject text,
    prepared_email_body   text,
    prepared_form_fields  jsonb NOT NULL DEFAULT '{}'::jsonb,
    prepared_attachments  jsonb NOT NULL DEFAULT '[]'::jsonb,

    authorisation_id uuid,
    external_ref     text,

    prepared_at    timestamptz NOT NULL DEFAULT now(),
    authorised_at  timestamptz,
    submitted_at   timestamptz,
    confirmed_at   timestamptz,
    updated_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT applications_match_uniq UNIQUE (job_match_id),
    CONSTRAINT applications_retry_non_negative CHECK (retry_count >= 0),
    CONSTRAINT applications_blocked_has_reason CHECK (
        status <> 'blocked' OR blocked_reason IS NOT NULL
    ),
    CONSTRAINT applications_failed_has_reason CHECK (
        status <> 'failed' OR failure_reason IS NOT NULL
    ),

    -- THE PRODUCT'S INTEGRITY BOUNDARY.
    -- An application can never reach a sent state without a recorded authorisation.
    -- This lives in the database, not only in the service layer, because "we applied
    -- on a candidate's behalf without their consent" is the one failure this platform
    -- must be structurally incapable of.
    CONSTRAINT applications_sent_requires_authorisation CHECK (
        status NOT IN ('submitting', 'submitted', 'email_sent', 'outcome_unknown')
        OR authorisation_id IS NOT NULL
    ),
    CONSTRAINT applications_authorisation_fields_together CHECK (
        (authorisation_id IS NULL AND authorised_at IS NULL)
        OR (authorisation_id IS NOT NULL AND authorised_at IS NOT NULL)
    ),
    CONSTRAINT applications_submitted_has_timestamp CHECK (
        status NOT IN ('submitted', 'email_sent') OR submitted_at IS NOT NULL
    )
);

CREATE INDEX applications_user_idx        ON applications (user_id, prepared_at DESC);
CREATE INDEX applications_loop_idx        ON applications (loop_id, prepared_at DESC);
CREATE INDEX applications_status_idx      ON applications (status, prepared_at)
    WHERE status IN ('prepared', 'ready', 'awaiting_authorisation', 'questions_pending', 'submitting');
CREATE INDEX applications_posting_idx     ON applications (job_posting_id);
-- Supports the per-candidate, per-company cooldown lookup.
CREATE INDEX applications_submitted_idx   ON applications (user_id, submitted_at DESC)
    WHERE submitted_at IS NOT NULL;

CREATE TABLE application_authorisations (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id uuid NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind           authorisation_kind NOT NULL,
    standing_id    uuid REFERENCES standing_authorisations (id) ON DELETE RESTRICT,
    granted_at     timestamptz NOT NULL DEFAULT now(),
    granted_by     uuid REFERENCES users (id) ON DELETE SET NULL,
    ip             inet,
    user_agent     text,
    -- What the candidate was shown and agreed to. Immutable.
    snapshot       jsonb NOT NULL,

    CONSTRAINT app_auth_one_per_application UNIQUE (application_id),
    CONSTRAINT app_auth_standing_requires_parent CHECK (
        kind <> 'standing' OR standing_id IS NOT NULL
    ),
    CONSTRAINT app_auth_per_application_has_no_parent CHECK (
        kind <> 'per_application' OR standing_id IS NULL
    )
);

CREATE INDEX app_auth_user_idx     ON application_authorisations (user_id, granted_at DESC);
CREATE INDEX app_auth_standing_idx ON application_authorisations (standing_id)
    WHERE standing_id IS NOT NULL;

-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE applications
    ADD CONSTRAINT applications_authorisation_fk
    FOREIGN KEY (authorisation_id) REFERENCES application_authorisations (id) ON DELETE RESTRICT;
-- +goose StatementEnd

-- +goose StatementBegin
-- Append-only. Status is DERIVED from this log, never mutated in place, so the full
-- history of an application can always be reconstructed.
CREATE TABLE application_events (
    id             bigserial PRIMARY KEY,
    application_id uuid NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    type           text NOT NULL,
    from_status    application_status,
    to_status      application_status,
    actor          text NOT NULL DEFAULT 'system',
    payload        jsonb NOT NULL DEFAULT '{}'::jsonb,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT app_events_actor_valid CHECK (actor IN ('system', 'candidate', 'admin', 'provider'))
);

CREATE INDEX app_events_application_idx ON application_events (application_id, occurred_at);
CREATE INDEX app_events_user_time_idx   ON application_events (user_id, occurred_at DESC);
CREATE INDEX app_events_correlation_idx ON application_events (correlation_id)
    WHERE correlation_id IS NOT NULL;

CREATE TABLE application_questions (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id  uuid NOT NULL REFERENCES applications (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    question_text   text NOT NULL,
    normalised_text text NOT NULL,
    kind            text NOT NULL DEFAULT 'text',
    options         text[] NOT NULL DEFAULT '{}',
    required        boolean NOT NULL DEFAULT true,
    answer          text,
    answer_source   answer_source,
    -- Sensitive categories (disability, ethnicity, gender, visa, salary history,
    -- criminal record, age) are never auto-answered, on any tier.
    is_sensitive    boolean NOT NULL DEFAULT false,
    sensitive_category text,
    answered_at     timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT app_questions_kind_valid CHECK (
        kind IN ('text', 'textarea', 'select', 'multiselect', 'boolean', 'number', 'file', 'date')
    ),
    CONSTRAINT app_questions_answer_has_source CHECK (
        answer IS NULL OR answer_source IS NOT NULL
    ),
    -- Enforced here as well as in the service layer: an AI-answered sensitive question
    -- is a discrimination-exposure problem, not merely a policy violation.
    CONSTRAINT app_questions_sensitive_never_ai CHECK (
        NOT is_sensitive OR answer_source IS DISTINCT FROM 'ai'
    )
);

CREATE INDEX app_questions_application_idx ON application_questions (application_id);
CREATE INDEX app_questions_pending_idx     ON application_questions (user_id, created_at)
    WHERE answered_at IS NULL;

-- Answers are saved and reused across applications, matched by normalised question
-- text so the candidate answers "why do you want to work here?" once.
CREATE TABLE saved_answers (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    normalised_question text NOT NULL,
    answer              text NOT NULL,
    kind                text NOT NULL DEFAULT 'text',
    is_sensitive        boolean NOT NULL DEFAULT false,
    use_count           int NOT NULL DEFAULT 0,
    last_used_at        timestamptz,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT saved_answers_uniq UNIQUE (user_id, normalised_question)
);

CREATE INDEX saved_answers_trgm_idx
    ON saved_answers USING gin (normalised_question gin_trgm_ops);

-- Per-candidate, per-company suppression window, reproducing the observed six-month
-- rule as configurable policy. Surfaced to the candidate with the prior contact date,
-- so "Already Applied" is an explanation rather than a mystery.
CREATE TABLE company_contact_cooldowns (
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    company_id     uuid NOT NULL REFERENCES companies (id) ON DELETE CASCADE,
    last_method    application_method NOT NULL,
    last_contacted_at timestamptz NOT NULL DEFAULT now(),
    application_id uuid REFERENCES applications (id) ON DELETE SET NULL,

    PRIMARY KEY (user_id, company_id)
);

CREATE INDEX company_cooldowns_time_idx
    ON company_contact_cooldowns (user_id, last_contacted_at DESC);
-- +goose StatementEnd

-- +goose StatementBegin
-- An application's CV, match and loop must all belong to the application's owner.
-- Three separate ownership paths converge on one row, so a single missed check in any
-- one of them would be a cross-tenant leak.
CREATE OR REPLACE FUNCTION applications_enforce_owner() RETURNS trigger AS $$
DECLARE
    cv_owner    uuid;
    match_owner uuid;
    loop_owner  uuid;
BEGIN
    SELECT user_id INTO cv_owner    FROM cv_versions WHERE id = NEW.cv_version_id;
    SELECT user_id INTO match_owner FROM job_matches WHERE id = NEW.job_match_id;
    SELECT user_id INTO loop_owner  FROM loops       WHERE id = NEW.loop_id;

    IF cv_owner IS DISTINCT FROM NEW.user_id THEN
        RAISE EXCEPTION 'application cv_version belongs to another user';
    END IF;
    IF match_owner IS DISTINCT FROM NEW.user_id THEN
        RAISE EXCEPTION 'application job_match belongs to another user';
    END IF;
    IF loop_owner IS DISTINCT FROM NEW.user_id THEN
        RAISE EXCEPTION 'application loop belongs to another user';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER applications_owner_check
    BEFORE INSERT OR UPDATE OF cv_version_id, job_match_id, loop_id, user_id ON applications
    FOR EACH ROW EXECUTE FUNCTION applications_enforce_owner();

CREATE TRIGGER applications_set_updated_at
    BEFORE UPDATE ON applications
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER saved_answers_set_updated_at
    BEFORE UPDATE ON saved_answers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose StatementBegin
-- The prepared artefacts and the authorisation are immutable once consent is given.
-- Allowing them to change afterwards would mean the candidate authorised one thing and
-- we sent another, which is exactly the abuse the authorisation record exists to rule
-- out.
CREATE OR REPLACE FUNCTION applications_freeze_authorised_artefacts() RETURNS trigger AS $$
BEGIN
    IF OLD.authorised_at IS NULL THEN
        RETURN NEW;
    END IF;

    IF NEW.prepared_cover_letter  IS DISTINCT FROM OLD.prepared_cover_letter
    OR NEW.prepared_email_subject IS DISTINCT FROM OLD.prepared_email_subject
    OR NEW.prepared_email_body    IS DISTINCT FROM OLD.prepared_email_body
    OR NEW.prepared_form_fields   IS DISTINCT FROM OLD.prepared_form_fields
    OR NEW.prepared_attachments   IS DISTINCT FROM OLD.prepared_attachments
    OR NEW.cv_version_id          IS DISTINCT FROM OLD.cv_version_id
    OR NEW.authorisation_id       IS DISTINCT FROM OLD.authorisation_id
    THEN
        RAISE EXCEPTION
            'application % is authorised; its prepared artefacts and authorisation are immutable',
            OLD.id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER applications_freeze_after_authorisation
    BEFORE UPDATE ON applications
    FOR EACH ROW EXECUTE FUNCTION applications_freeze_authorised_artefacts();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS applications_freeze_after_authorisation ON applications;
DROP FUNCTION IF EXISTS applications_freeze_authorised_artefacts();
DROP TRIGGER IF EXISTS saved_answers_set_updated_at ON saved_answers;
DROP TRIGGER IF EXISTS applications_set_updated_at ON applications;
DROP TRIGGER IF EXISTS applications_owner_check ON applications;
DROP FUNCTION IF EXISTS applications_enforce_owner();
DROP TABLE IF EXISTS company_contact_cooldowns;
DROP TABLE IF EXISTS saved_answers;
DROP TABLE IF EXISTS application_questions;
DROP TABLE IF EXISTS application_events;
ALTER TABLE applications DROP CONSTRAINT IF EXISTS applications_authorisation_fk;
DROP TABLE IF EXISTS application_authorisations;
DROP TABLE IF EXISTS applications;
DROP TABLE IF EXISTS standing_authorisations;
DROP TYPE IF EXISTS answer_source;
DROP TYPE IF EXISTS authorisation_kind;
DROP TYPE IF EXISTS application_status;
DROP TYPE IF EXISTS application_method;
-- +goose StatementEnd
