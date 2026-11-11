-- +goose Up
-- +goose StatementBegin

CREATE TYPE loop_status       AS ENUM ('draft', 'active', 'paused', 'archived');
CREATE TYPE match_preset      AS ENUM ('low', 'middle', 'high', 'custom');
CREATE TYPE cover_letter_mode AS ENUM ('none', 'default', 'custom');
CREATE TYPE discovery_outcome AS ENUM ('running', 'succeeded', 'partial', 'failed');
CREATE TYPE match_status AS ENUM (
    'new', 'viewed', 'dismissed', 'saved', 'application_created',
    'suppressed_cooldown', 'expired'
);
CREATE TYPE rejection_category AS ENUM (
    'job_title', 'location', 'work_mode', 'language', 'required_keyword_missing',
    'excluded_keyword', 'excluded_company', 'cv_relevance', 'employment_type', 'seniority'
);

CREATE TABLE email_templates (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid REFERENCES users (id) ON DELETE CASCADE,
    name            text NOT NULL,
    subject         text NOT NULL,
    body            text NOT NULL,
    is_system       boolean NOT NULL DEFAULT false,
    placeholders_used text[] NOT NULL DEFAULT '{}',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    -- A system template is shared and has no owner; a user template must have one.
    CONSTRAINT templates_owner_matches_kind CHECK (
        (is_system AND user_id IS NULL) OR (NOT is_system AND user_id IS NOT NULL)
    )
);

CREATE INDEX email_templates_user_idx ON email_templates (user_id) WHERE user_id IS NOT NULL;

CREATE TABLE loops (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name               text NOT NULL,
    status             loop_status NOT NULL DEFAULT 'draft',
    cv_version_id      uuid REFERENCES cv_versions (id) ON DELETE RESTRICT,
    email_template_id  uuid REFERENCES email_templates (id) ON DELETE SET NULL,
    -- Bumped on every edit. Each match records the config_version it was scored
    -- under, so a historical match can always be explained by the configuration in
    -- force at the time rather than by today's settings.
    config_version     int NOT NULL DEFAULT 1,

    titles             text[] NOT NULL DEFAULT '{}',
    locations          jsonb  NOT NULL DEFAULT '[]'::jsonb,
    remote_only        boolean NOT NULL DEFAULT false,
    worldwide_remote   boolean NOT NULL DEFAULT false,
    hybrid_only        boolean NOT NULL DEFAULT false,
    seniority          seniority_level NOT NULL DEFAULT 'All',
    employment_types   employment_type[] NOT NULL DEFAULT '{}',
    source_slugs       text[] NOT NULL DEFAULT '{}',
    career_page_search boolean NOT NULL DEFAULT false,

    required_keywords  text[] NOT NULL DEFAULT '{}',
    excluded_keywords  text[] NOT NULL DEFAULT '{}',
    excluded_companies text[] NOT NULL DEFAULT '{}',
    -- Numeric threshold plus the named preset it came from: the preset keeps the
    -- control familiar, the number keeps it inspectable.
    match_threshold    smallint NOT NULL DEFAULT 60,
    match_preset       match_preset NOT NULL DEFAULT 'middle',

    auto_email         boolean NOT NULL DEFAULT false,
    auto_form          boolean NOT NULL DEFAULT false,
    auto_ai_answers    boolean NOT NULL DEFAULT false,
    extension_auto_apply boolean NOT NULL DEFAULT false,
    form_phone         text,
    form_city          text,
    cover_letter_mode  cover_letter_mode NOT NULL DEFAULT 'none',
    cover_letter_template text,

    activated_at       timestamptz,
    paused_at          timestamptz,
    archived_at        timestamptz,
    last_discovery_at  timestamptz,
    next_discovery_at  timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT loops_threshold_range CHECK (match_threshold BETWEEN 0 AND 100),
    CONSTRAINT loops_config_version_positive CHECK (config_version > 0),
    -- An active loop must have something to search for and a CV to apply with;
    -- without these it would run and silently produce nothing.
    CONSTRAINT loops_active_requires_titles CHECK (
        status <> 'active' OR array_length(titles, 1) >= 1
    ),
    CONSTRAINT loops_active_requires_cv CHECK (
        status <> 'active' OR cv_version_id IS NOT NULL
    ),
    -- Remote-only and hybrid-only are mutually exclusive; accepting both would match
    -- nothing and look like a system fault.
    CONSTRAINT loops_work_mode_coherent CHECK (NOT (remote_only AND hybrid_only))
);

CREATE INDEX loops_user_idx      ON loops (user_id) WHERE status <> 'archived';
-- Drives the discovery scheduler.
CREATE INDEX loops_due_idx       ON loops (next_discovery_at)
    WHERE status = 'active';
CREATE INDEX loops_status_idx    ON loops (status);

CREATE TABLE loop_config_versions (
    loop_id        uuid NOT NULL REFERENCES loops (id) ON DELETE CASCADE,
    config_version int  NOT NULL,
    snapshot       jsonb NOT NULL,
    changed_by     uuid REFERENCES users (id) ON DELETE SET NULL,
    changed_at     timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (loop_id, config_version)
);

CREATE TABLE discovery_runs (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    loop_id        uuid REFERENCES loops (id) ON DELETE CASCADE,
    source_id      uuid NOT NULL REFERENCES job_sources (id) ON DELETE CASCADE,
    spec_hash      text NOT NULL,
    -- Opaque, resumable cursor persisted so a worker crash resumes the run rather
    -- than restarting it and re-spending the upstream rate limit.
    cursor         text,
    started_at     timestamptz NOT NULL DEFAULT now(),
    finished_at    timestamptz,
    postings_seen  int NOT NULL DEFAULT 0,
    postings_new   int NOT NULL DEFAULT 0,
    errors         jsonb NOT NULL DEFAULT '[]'::jsonb,
    outcome        discovery_outcome NOT NULL DEFAULT 'running',

    CONSTRAINT discovery_counts_non_negative CHECK (postings_seen >= 0 AND postings_new >= 0)
);

CREATE INDEX discovery_runs_loop_idx   ON discovery_runs (loop_id, started_at DESC);
CREATE INDEX discovery_runs_source_idx ON discovery_runs (source_id, started_at DESC);
CREATE INDEX discovery_runs_spec_idx   ON discovery_runs (spec_hash, source_id, started_at DESC);

CREATE TABLE job_matches (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    loop_id         uuid NOT NULL REFERENCES loops (id) ON DELETE CASCADE,
    job_posting_id  uuid NOT NULL REFERENCES job_postings (id) ON DELETE CASCADE,
    cv_version_id   uuid NOT NULL REFERENCES cv_versions (id) ON DELETE RESTRICT,
    config_version  int  NOT NULL,
    score           smallint NOT NULL,
    -- Per-dimension sub-scores and human-readable reasons. These are user-visible
    -- product data, not debug output: an unexplainable score is a support ticket.
    score_breakdown jsonb NOT NULL DEFAULT '{}'::jsonb,
    reasons         jsonb NOT NULL DEFAULT '[]'::jsonb,
    status          match_status NOT NULL DEFAULT 'new',
    -- 'degraded' marks a match scored without embeddings, for later rescoring. A match
    -- is never dropped merely because scoring was degraded.
    scoring_mode    text NOT NULL DEFAULT 'full',
    matched_at      timestamptz NOT NULL DEFAULT now(),
    viewed_at       timestamptz,
    dismissed_at    timestamptz,

    CONSTRAINT job_matches_score_range CHECK (score BETWEEN 0 AND 100),
    CONSTRAINT job_matches_scoring_mode_valid CHECK (scoring_mode IN ('full', 'degraded')),
    CONSTRAINT job_matches_reasons_is_array CHECK (jsonb_typeof(reasons) = 'array'),
    -- One match per loop per posting.
    CONSTRAINT job_matches_loop_posting_uniq UNIQUE (loop_id, job_posting_id)
);

CREATE INDEX job_matches_user_score_idx ON job_matches (user_id, score DESC, matched_at DESC);
CREATE INDEX job_matches_loop_idx       ON job_matches (loop_id, matched_at DESC);
CREATE INDEX job_matches_posting_idx    ON job_matches (job_posting_id);
CREATE INDEX job_matches_actionable_idx ON job_matches (user_id, status, score DESC)
    WHERE status IN ('new', 'viewed', 'saved');

-- Rejections are retained with their reason so "why am I getting no matches?" is a
-- self-service screen rather than a support ticket, and so the matching engine has a
-- labelled evaluation set.
CREATE TABLE match_rejections (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id        uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    loop_id        uuid NOT NULL REFERENCES loops (id) ON DELETE CASCADE,
    job_posting_id uuid NOT NULL REFERENCES job_postings (id) ON DELETE CASCADE,
    config_version int  NOT NULL,
    category       rejection_category NOT NULL,
    detail         jsonb NOT NULL DEFAULT '{}'::jsonb,
    rejected_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT match_rejections_uniq UNIQUE (loop_id, job_posting_id, config_version)
);

CREATE INDEX match_rejections_loop_category_idx
    ON match_rejections (loop_id, category, rejected_at DESC);
CREATE INDEX match_rejections_user_idx ON match_rejections (user_id, rejected_at DESC);

-- +goose StatementEnd

-- +goose StatementBegin
-- A loop's CV must belong to the loop's owner. Enforced in the database because this
-- is a cross-tenant leak if it is ever wrong, and an application-level check can be
-- bypassed by a direct SQL fix or a future code path that forgets it.
CREATE OR REPLACE FUNCTION loops_enforce_cv_owner() RETURNS trigger AS $$
DECLARE
    cv_owner uuid;
BEGIN
    IF NEW.cv_version_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT user_id INTO cv_owner FROM cv_versions WHERE id = NEW.cv_version_id;
    IF cv_owner IS DISTINCT FROM NEW.user_id THEN
        RAISE EXCEPTION 'loop % references cv_version owned by a different user', NEW.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER loops_cv_owner_check
    BEFORE INSERT OR UPDATE OF cv_version_id, user_id ON loops
    FOR EACH ROW EXECUTE FUNCTION loops_enforce_cv_owner();

CREATE TRIGGER loops_set_updated_at
    BEFORE UPDATE ON loops
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER email_templates_set_updated_at
    BEFORE UPDATE ON email_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS email_templates_set_updated_at ON email_templates;
DROP TRIGGER IF EXISTS loops_set_updated_at ON loops;
DROP TRIGGER IF EXISTS loops_cv_owner_check ON loops;
DROP FUNCTION IF EXISTS loops_enforce_cv_owner();
DROP TABLE IF EXISTS match_rejections;
DROP TABLE IF EXISTS job_matches;
DROP TABLE IF EXISTS discovery_runs;
DROP TABLE IF EXISTS loop_config_versions;
DROP TABLE IF EXISTS loops;
DROP TABLE IF EXISTS email_templates;
DROP TYPE IF EXISTS rejection_category;
DROP TYPE IF EXISTS match_status;
DROP TYPE IF EXISTS discovery_outcome;
DROP TYPE IF EXISTS cover_letter_mode;
DROP TYPE IF EXISTS match_preset;
DROP TYPE IF EXISTS loop_status;
-- +goose StatementEnd
