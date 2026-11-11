-- +goose Up
-- +goose StatementBegin

CREATE TYPE cv_source       AS ENUM ('uploaded', 'builder');
CREATE TYPE av_status       AS ENUM ('pending', 'clean', 'infected', 'error');
CREATE TYPE cv_parse_status AS ENUM ('pending', 'scanning', 'extracting', 'parsing', 'parsed', 'failed');

-- Canonical skill taxonomy. Normalising to a shared vocabulary is what makes skill
-- overlap comparable across CVs and job descriptions; comparing raw strings would
-- treat "JS", "Javascript" and "JavaScript" as three unrelated skills.
CREATE TABLE skills (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    canonical_name text NOT NULL,
    category       text,
    aliases        text[] NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT skills_canonical_uniq UNIQUE (canonical_name)
);

CREATE INDEX skills_aliases_idx   ON skills USING gin (aliases);
CREATE INDEX skills_trgm_idx      ON skills USING gin (canonical_name gin_trgm_ops);

CREATE TABLE cv_documents (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    source            cv_source NOT NULL DEFAULT 'uploaded',
    label             text,
    original_filename text,
    content_type      text NOT NULL,
    size_bytes        bigint NOT NULL,
    is_default        boolean NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    deleted_at        timestamptz,

    CONSTRAINT cv_documents_size_positive CHECK (size_bytes > 0),
    CONSTRAINT cv_documents_content_type_allowed CHECK (
        content_type IN (
            'application/pdf',
            'application/vnd.openxmlformats-officedocument.wordprocessingml.document'
        )
    )
);

CREATE INDEX cv_documents_user_idx ON cv_documents (user_id) WHERE deleted_at IS NULL;
-- At most one default CV per candidate, enforced in the database so a concurrent
-- "set default" on two CVs cannot leave both marked.
CREATE UNIQUE INDEX cv_documents_one_default_per_user
    ON cv_documents (user_id) WHERE is_default AND deleted_at IS NULL;

CREATE TABLE cv_versions (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cv_document_id   uuid NOT NULL REFERENCES cv_documents (id) ON DELETE CASCADE,
    -- Denormalised from the parent so every tenant-scoped query can filter without a
    -- join. The trigger below keeps it honest.
    user_id          uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    version_no       int  NOT NULL,
    storage_key      text NOT NULL,
    checksum_sha256  bytea NOT NULL,
    size_bytes       bigint NOT NULL,
    av_status        av_status NOT NULL DEFAULT 'pending',
    av_scanned_at    timestamptz,
    parse_status     cv_parse_status NOT NULL DEFAULT 'pending',
    parse_error      text,
    parse_stage      text,
    parsed_at        timestamptz,
    raw_text         text,
    char_count       int,
    page_count       int,
    -- Tailoring lineage. A tailored version must record both where it came from and
    -- what it was tailored for, or its provenance is unverifiable.
    derived_from_version_id   uuid REFERENCES cv_versions (id) ON DELETE SET NULL,
    tailored_for_job_posting_id uuid,
    containment_passed boolean,
    created_at       timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT cv_versions_no_per_document UNIQUE (cv_document_id, version_no),
    CONSTRAINT cv_versions_storage_key_uniq UNIQUE (storage_key),
    CONSTRAINT cv_versions_version_positive CHECK (version_no > 0),
    -- A tailored version without a containment verdict must never be usable: that
    -- verdict is the only thing standing between the candidate and a fabricated CV.
    CONSTRAINT cv_versions_tailored_requires_containment CHECK (
        tailored_for_job_posting_id IS NULL OR containment_passed IS NOT NULL
    ),
    CONSTRAINT cv_versions_tailored_requires_base CHECK (
        tailored_for_job_posting_id IS NULL OR derived_from_version_id IS NOT NULL
    )
);

CREATE INDEX cv_versions_document_idx ON cv_versions (cv_document_id, version_no DESC);
CREATE INDEX cv_versions_user_idx     ON cv_versions (user_id);
CREATE INDEX cv_versions_parse_status_idx ON cv_versions (parse_status)
    WHERE parse_status NOT IN ('parsed', 'failed');
CREATE INDEX cv_versions_tailored_for_idx ON cv_versions (tailored_for_job_posting_id)
    WHERE tailored_for_job_posting_id IS NOT NULL;

CREATE TABLE cv_parsed_data (
    cv_version_id   uuid PRIMARY KEY REFERENCES cv_versions (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    summary         text,
    contact         jsonb NOT NULL DEFAULT '{}'::jsonb,
    experience      jsonb NOT NULL DEFAULT '[]'::jsonb,
    education       jsonb NOT NULL DEFAULT '[]'::jsonb,
    certifications  jsonb NOT NULL DEFAULT '[]'::jsonb,
    languages       jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Per-field confidence plus the character offsets the value was extracted from, so
    -- the review UI can show provenance instead of asking the candidate to trust it.
    confidence      jsonb NOT NULL DEFAULT '{}'::jsonb,
    -- Fields the candidate corrected. These are permanently authoritative over AI
    -- output and are never overwritten by a re-parse.
    corrected_fields text[] NOT NULL DEFAULT '{}',
    total_experience_months int,
    detected_seniority text,
    engine_version  text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT parsed_experience_is_array CHECK (jsonb_typeof(experience) = 'array'),
    CONSTRAINT parsed_education_is_array  CHECK (jsonb_typeof(education) = 'array'),
    CONSTRAINT parsed_months_sane CHECK (
        total_experience_months IS NULL OR total_experience_months BETWEEN 0 AND 1200
    )
);

CREATE TABLE cv_skills (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cv_version_id uuid NOT NULL REFERENCES cv_versions (id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    skill_id      uuid REFERENCES skills (id) ON DELETE SET NULL,
    raw_label     text NOT NULL,
    years         numeric(4,1),
    confidence    numeric(3,2),
    source        text NOT NULL DEFAULT 'ai',

    CONSTRAINT cv_skills_confidence_range CHECK (confidence IS NULL OR confidence BETWEEN 0 AND 1),
    CONSTRAINT cv_skills_source_valid CHECK (source IN ('ai', 'candidate', 'taxonomy')),
    CONSTRAINT cv_skills_unique_per_version UNIQUE (cv_version_id, raw_label)
);

CREATE INDEX cv_skills_version_idx ON cv_skills (cv_version_id);
CREATE INDEX cv_skills_skill_idx   ON cv_skills (skill_id);

CREATE TABLE cv_analyses (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cv_version_id   uuid NOT NULL REFERENCES cv_versions (id) ON DELETE CASCADE,
    user_id         uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    target_role     text,
    overall_score   smallint NOT NULL,
    subscores       jsonb NOT NULL DEFAULT '{}'::jsonb,
    missing_keywords text[] NOT NULL DEFAULT '{}',
    recommendations jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Set when the AI pass was unavailable and only deterministic checks ran. The UI
    -- must say so rather than present a partial score as complete.
    degraded        boolean NOT NULL DEFAULT false,
    engine_version  text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT cv_analyses_score_range CHECK (overall_score BETWEEN 0 AND 100),
    CONSTRAINT cv_analyses_recommendations_is_array CHECK (jsonb_typeof(recommendations) = 'array')
);

CREATE INDEX cv_analyses_version_idx ON cv_analyses (cv_version_id, created_at DESC);

-- Embeddings live in their own table so the vector column type can be swapped for
-- pgvector later without rewriting cv_versions. Stored as float4[] to avoid making
-- pgvector a hard dependency for local development.
CREATE TABLE cv_embeddings (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cv_version_id uuid NOT NULL REFERENCES cv_versions (id) ON DELETE CASCADE,
    kind          text NOT NULL,
    model         text NOT NULL,
    dims          int  NOT NULL,
    vector        real[] NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT cv_embeddings_kind_valid CHECK (kind IN ('title', 'skills', 'summary', 'full')),
    CONSTRAINT cv_embeddings_dims_match CHECK (dims = array_length(vector, 1)),
    CONSTRAINT cv_embeddings_unique_per_kind UNIQUE (cv_version_id, kind, model)
);

-- +goose StatementEnd

-- +goose StatementBegin
-- Keep the denormalised user_id on cv_versions consistent with its parent document.
-- Without this, a bug that set the wrong user_id would create a cross-tenant leak that
-- every tenant-scoped query would happily serve.
CREATE OR REPLACE FUNCTION cv_versions_enforce_owner() RETURNS trigger AS $$
DECLARE
    owner uuid;
BEGIN
    SELECT user_id INTO owner FROM cv_documents WHERE id = NEW.cv_document_id;
    IF owner IS NULL THEN
        RAISE EXCEPTION 'cv_document % does not exist', NEW.cv_document_id;
    END IF;
    IF NEW.user_id IS DISTINCT FROM owner THEN
        RAISE EXCEPTION 'cv_version user_id % does not match owning document user_id %',
            NEW.user_id, owner;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER cv_versions_owner_check
    BEFORE INSERT OR UPDATE ON cv_versions
    FOR EACH ROW EXECUTE FUNCTION cv_versions_enforce_owner();

CREATE TRIGGER cv_documents_set_updated_at
    BEFORE UPDATE ON cv_documents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TRIGGER cv_parsed_data_set_updated_at
    BEFORE UPDATE ON cv_parsed_data
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS cv_parsed_data_set_updated_at ON cv_parsed_data;
DROP TRIGGER IF EXISTS cv_documents_set_updated_at ON cv_documents;
DROP TRIGGER IF EXISTS cv_versions_owner_check ON cv_versions;
DROP FUNCTION IF EXISTS cv_versions_enforce_owner();
DROP TABLE IF EXISTS cv_embeddings;
DROP TABLE IF EXISTS cv_analyses;
DROP TABLE IF EXISTS cv_skills;
DROP TABLE IF EXISTS cv_parsed_data;
DROP TABLE IF EXISTS cv_versions;
DROP TABLE IF EXISTS cv_documents;
DROP TABLE IF EXISTS skills;
DROP TYPE IF EXISTS cv_parse_status;
DROP TYPE IF EXISTS av_status;
DROP TYPE IF EXISTS cv_source;
-- +goose StatementEnd
