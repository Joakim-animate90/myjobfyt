-- +goose Up
-- +goose StatementBegin

CREATE TYPE job_source_kind AS ENUM ('official_api', 'authorised_feed', 'ats_api', 'career_page');
CREATE TYPE source_state    AS ENUM ('enabled', 'disabled', 'partnership_required', 'degraded');
CREATE TYPE work_mode       AS ENUM ('onsite', 'hybrid', 'remote');
CREATE TYPE employment_type AS ENUM ('Full-time', 'Contract', 'Part-time', 'Temporary', 'Internship');
CREATE TYPE seniority_level AS ENUM ('Junior', 'Associate', 'Senior', 'Executive', 'All');
CREATE TYPE posting_status  AS ENUM ('active', 'expired', 'removed');
CREATE TYPE payload_state   AS ENUM ('pending', 'normalised', 'quarantined');

-- Every source records the basis on which we access it. A source without a recorded
-- legal basis cannot be enabled: that check is the difference between an integration
-- and a scraping incident.
CREATE TABLE job_sources (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug               text NOT NULL,
    name               text NOT NULL,
    kind               job_source_kind NOT NULL,
    state              source_state NOT NULL DEFAULT 'disabled',
    regions            text[] NOT NULL DEFAULT '{}',
    rate_limit_per_min int NOT NULL DEFAULT 30,
    capabilities       jsonb NOT NULL DEFAULT '{}'::jsonb,
    legal_basis        text,
    terms_url          text,
    -- Circuit-breaker state, persisted so a restart does not immediately hammer a
    -- source that was already failing.
    consecutive_failures int NOT NULL DEFAULT 0,
    breaker_open_until timestamptz,
    last_success_at    timestamptz,
    last_error         text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT job_sources_slug_uniq UNIQUE (slug),
    CONSTRAINT job_sources_rate_limit_positive CHECK (rate_limit_per_min > 0),
    CONSTRAINT job_sources_enabled_requires_legal_basis CHECK (
        state <> 'enabled' OR (legal_basis IS NOT NULL AND length(trim(legal_basis)) > 0)
    )
);

CREATE TABLE companies (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Normalised identity ("acme" from "Acme, Inc."), so company exclusion and the
    -- per-candidate cooldown match on identity rather than on punctuation.
    normalised_name  text NOT NULL,
    display_name     text NOT NULL,
    domain           text,
    ats_provider     text,
    ats_board_token  text,
    logo_url         text,
    industry         text,
    size_bucket      text,
    careers_url      text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT companies_normalised_uniq UNIQUE (normalised_name)
);

CREATE INDEX companies_domain_idx ON companies (domain) WHERE domain IS NOT NULL;
CREATE INDEX companies_trgm_idx   ON companies USING gin (normalised_name gin_trgm_ops);
CREATE INDEX companies_ats_idx    ON companies (ats_provider) WHERE ats_provider IS NOT NULL;

-- Raw payloads are retained so normalisation can be replayed after a bug fix and so
-- upstream schema drift is diagnosable rather than merely fatal.
CREATE TABLE raw_job_payloads (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id         uuid NOT NULL REFERENCES job_sources (id) ON DELETE CASCADE,
    external_id       text NOT NULL,
    content_hash      bytea NOT NULL,
    payload           jsonb NOT NULL,
    fetched_at        timestamptz NOT NULL DEFAULT now(),
    processing_status payload_state NOT NULL DEFAULT 'pending',
    quarantine_reason text,
    job_posting_id    uuid,

    -- The same posting fetched again with unchanged content is a no-op, which is what
    -- stops a daily re-sighting from re-triggering the whole match pipeline.
    CONSTRAINT raw_payload_identity_uniq UNIQUE (source_id, external_id, content_hash)
);

CREATE INDEX raw_payloads_pending_idx ON raw_job_payloads (processing_status, fetched_at)
    WHERE processing_status = 'pending';
CREATE INDEX raw_payloads_quarantined_idx ON raw_job_payloads (source_id, fetched_at DESC)
    WHERE processing_status = 'quarantined';

CREATE TABLE job_postings (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Deterministic over normalised company, normalised title, geo key and a shingled
    -- description hash. Uniqueness here IS the deduplication mechanism.
    fingerprint      bytea NOT NULL,
    company_id       uuid NOT NULL REFERENCES companies (id) ON DELETE RESTRICT,
    title            text NOT NULL,
    normalised_title text NOT NULL,
    description      text,
    description_html text,
    requirements     text[] NOT NULL DEFAULT '{}',
    benefits         text[] NOT NULL DEFAULT '{}',
    location         jsonb NOT NULL DEFAULT '{}'::jsonb,
    geo_key          text,
    work_mode        work_mode,
    employment_type  employment_type,
    seniority        seniority_level,
    salary_min       numeric(12,2),
    salary_max       numeric(12,2),
    salary_currency  char(3),
    salary_period    text,
    salary_is_estimate boolean NOT NULL DEFAULT false,
    language         char(2),
    apply_method_hint text,
    apply_url        text,
    apply_email      citext,
    ats_provider     text,
    ats_job_id       text,
    posted_at        timestamptz,
    expires_at       timestamptz,
    first_seen_at    timestamptz NOT NULL DEFAULT now(),
    last_seen_at     timestamptz NOT NULL DEFAULT now(),
    status           posting_status NOT NULL DEFAULT 'active',
    content_hash     bytea NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT job_postings_fingerprint_uniq UNIQUE (fingerprint),
    CONSTRAINT job_postings_salary_order CHECK (
        salary_min IS NULL OR salary_max IS NULL OR salary_min <= salary_max
    ),
    CONSTRAINT job_postings_apply_hint_valid CHECK (
        apply_method_hint IS NULL
        OR apply_method_hint IN ('ats_api', 'form', 'email', 'external', 'unknown')
    )
);

CREATE INDEX job_postings_company_idx    ON job_postings (company_id);
CREATE INDEX job_postings_active_idx     ON job_postings (status, last_seen_at DESC)
    WHERE status = 'active';
CREATE INDEX job_postings_geo_idx        ON job_postings (geo_key) WHERE status = 'active';
CREATE INDEX job_postings_title_trgm_idx ON job_postings USING gin (normalised_title gin_trgm_ops);
-- Supports the nightly liveness re-check, which drives the Expired status.
CREATE INDEX job_postings_liveness_idx   ON job_postings (last_seen_at)
    WHERE status = 'active';
CREATE INDEX job_postings_ats_idx        ON job_postings (ats_provider, ats_job_id)
    WHERE ats_provider IS NOT NULL;

-- One row per board carrying a posting, so the UI can say "also on Indeed, LinkedIn"
-- and attribution survives deduplication.
CREATE TABLE job_posting_sources (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_posting_id uuid NOT NULL REFERENCES job_postings (id) ON DELETE CASCADE,
    source_id      uuid NOT NULL REFERENCES job_sources (id) ON DELETE CASCADE,
    external_id    text NOT NULL,
    source_url     text,
    first_seen_at  timestamptz NOT NULL DEFAULT now(),
    last_seen_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT job_posting_source_uniq UNIQUE (source_id, external_id)
);

CREATE INDEX job_posting_sources_posting_idx ON job_posting_sources (job_posting_id);

-- Near-duplicates are linked, never merged. Silently merging two distinct jobs is
-- unrecoverable; a link is cheap and reversible.
CREATE TABLE job_posting_relations (
    job_posting_id      uuid NOT NULL REFERENCES job_postings (id) ON DELETE CASCADE,
    related_posting_id  uuid NOT NULL REFERENCES job_postings (id) ON DELETE CASCADE,
    relation            text NOT NULL,
    similarity          numeric(4,3),
    resolved_by         text,
    created_at          timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (job_posting_id, related_posting_id),
    CONSTRAINT job_relation_valid CHECK (relation IN ('near_duplicate', 'same_company_role')),
    CONSTRAINT job_relation_not_self CHECK (job_posting_id <> related_posting_id),
    CONSTRAINT job_relation_similarity_range CHECK (
        similarity IS NULL OR similarity BETWEEN 0 AND 1
    )
);

-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE raw_job_payloads
    ADD CONSTRAINT raw_payloads_posting_fk
    FOREIGN KEY (job_posting_id) REFERENCES job_postings (id) ON DELETE SET NULL;

ALTER TABLE cv_versions
    ADD CONSTRAINT cv_versions_tailored_for_fk
    FOREIGN KEY (tailored_for_job_posting_id) REFERENCES job_postings (id) ON DELETE SET NULL;

CREATE TRIGGER job_sources_set_updated_at
    BEFORE UPDATE ON job_sources
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER companies_set_updated_at
    BEFORE UPDATE ON companies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER job_postings_set_updated_at
    BEFORE UPDATE ON job_postings
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS job_postings_set_updated_at ON job_postings;
DROP TRIGGER IF EXISTS companies_set_updated_at ON companies;
DROP TRIGGER IF EXISTS job_sources_set_updated_at ON job_sources;
ALTER TABLE cv_versions DROP CONSTRAINT IF EXISTS cv_versions_tailored_for_fk;
ALTER TABLE raw_job_payloads DROP CONSTRAINT IF EXISTS raw_payloads_posting_fk;
DROP TABLE IF EXISTS job_posting_relations;
DROP TABLE IF EXISTS job_posting_sources;
DROP TABLE IF EXISTS job_postings;
DROP TABLE IF EXISTS raw_job_payloads;
DROP TABLE IF EXISTS companies;
DROP TABLE IF EXISTS job_sources;
DROP TYPE IF EXISTS payload_state;
DROP TYPE IF EXISTS posting_status;
DROP TYPE IF EXISTS seniority_level;
DROP TYPE IF EXISTS employment_type;
DROP TYPE IF EXISTS work_mode;
DROP TYPE IF EXISTS source_state;
DROP TYPE IF EXISTS job_source_kind;
-- +goose StatementEnd
