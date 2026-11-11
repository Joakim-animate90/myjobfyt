# Engineering Backlog

61 issues across 8 milestones, each specified against the template in the engineering
directive: problem, business impact, technical impact, proposed solution, acceptance
criteria, dependencies, priority, complexity, subsystem and testing requirements.

This file and the GitHub issues are generated from one definition in
`tools/backlog/`. A backlog that exists in two places is a backlog nobody trusts, so
edit the definition and regenerate rather than editing either output.

## Summary

| Priority | Count |
|---|---|
| critical | 30 |
| high | 24 |
| medium | 6 |
| low | 1 |

| Complexity | Count |
|---|---|
| S | 4 |
| M | 24 |
| L | 29 |
| XL | 4 |

| Milestone | Issues | Critical |
|---|---|---|
| M1 — CV Intelligence | 12 | 7 |
| M2 — Job Acquisition | 9 | 6 |
| M3 — Matching | 5 | 2 |
| M4 — Application Orchestration | 7 | 5 |
| M5 — Frontend | 7 | 5 |
| M6 — Outreach, Analytics & Admin | 7 | 0 |
| M7 — Infrastructure & GitOps | 6 | 2 |
| M8 — CI/CD & Hardening | 8 | 3 |

## Critical path

The issues that block the most other work. A delay here is a delay everywhere.

| Issue | Blocks |
|---|---|
| `MJF-005` Structured CV extraction with per-field confidence and source offsets | MJF-006, MJF-007, MJF-008, MJF-011, MJF-012 |
| `MJF-031` Explainable multi-dimensional match scoring | MJF-032, MJF-033, MJF-034, MJF-040, MJF-054 |
| `MJF-020` Adapter contract, registry and per-source circuit breaker | MJF-021, MJF-022, MJF-023, MJF-024 |
| `MJF-080` CI workflow: format, vet, lint, test, race, coverage, migration validation | MJF-081, MJF-083, MJF-084, MJF-086 |
| `MJF-004` Provider-agnostic AI client with structured output, retries and token accounting | MJF-005, MJF-009, MJF-044 |
| `MJF-026` Fingerprint deduplication with near-duplicate linking | MJF-027, MJF-030, MJF-060 |


---

# M1 — CV Intelligence

Turn an uploaded document into structured, matchable candidate data, and generate tailored materials that never fabricate anything.

## MJF-001 — Object storage client with magic-byte validation and AV scanning

`critical` · `backend` · complexity `M`

**Problem.** There is no object-storage layer. CV uploads cannot be accepted at all, and the upload endpoint is the single highest-risk input in the product: it accepts arbitrary binary documents from the internet.

**Business impact.** No CV upload means no product. Every downstream feature depends on it.

**Technical impact.** Needs an S3-compatible client, streaming size enforcement, content-type verification by magic bytes rather than extension or client-supplied MIME, per-tenant opaque key namespacing, presigned download URLs, and an orphaned-object reaper for uploads never committed.

**Proposed solution.** Implement internal/platform/storage over minio-go. Verify type by sniffing the first bytes, enforce the size cap during the stream so an oversized file is rejected mid-transfer rather than after being buffered, namespace keys under tenants/{user_id}/{uuid}, and issue short-TTL presigned URLs so the application is never on the bytes path. Add a ClamAV client and quarantine on a positive result.

**Acceptance criteria.**

- [ ] Only PDF and DOCX are accepted, verified by magic bytes; a .pdf-named ZIP is rejected
- [ ] A file over STORAGE_MAX_CV_BYTES is rejected during transfer, not after
- [ ] Keys are opaque UUIDs under a per-tenant prefix; no filename is used in the key
- [ ] Download URLs are presigned with a TTL of 5 minutes or less
- [ ] An AV-positive file is quarantined, the candidate is notified, and the object is deleted after retention
- [ ] The bucket is created on first use so no broad storage credentials are needed in the app

**Testing.** Unit tests for magic-byte detection across valid PDF/DOCX and hostile inputs (polyglot, ZIP renamed, empty, truncated). Integration test against MinIO covering upload, presign, download and delete.

**Depends on** none. **Blocks** `MJF-002`.

## MJF-002 — CV upload endpoint and cv_documents/cv_versions persistence

`critical` · `backend` · complexity `M`

**Problem.** No endpoint exists to accept a CV. The schema is ready but nothing writes to it.

**Business impact.** Step 4 of the primary onboarding journey. A candidate cannot proceed without it.

**Technical impact.** Multipart upload, a transaction creating the document and its first version, publishing cv.uploaded, and returning immediately with a parsing status so the client can poll.

**Proposed solution.** POST /v1/cvs accepting multipart, streamed through the storage layer. Create cv_documents plus cv_versions(version_no=1) in one transaction, then publish cv.uploaded after commit. Enforce the cv_slots quota before accepting the body.

**Acceptance criteria.**

- [ ] Upload returns 202 with a document id and parse_status=pending
- [ ] The cv_slots entitlement is checked before the body is read, not after the file is stored
- [ ] cv.uploaded carries cv_id, version_id, user_id, storage_key, content_type, size and checksum
- [ ] A storage failure leaves no database row
- [ ] Re-uploading the same file content creates a new version, not a duplicate document

**Testing.** API tests for success, oversized, wrong type, quota exceeded and storage-unavailable. Verify no orphaned rows after an induced storage failure.

**Depends on** `MJF-001`. **Blocks** `MJF-003`, `MJF-010`.

## MJF-003 — Deterministic text extraction from PDF and DOCX

`critical` · `backend` · complexity `M`

**Problem.** A CV must be converted to text before anything can parse it. Sending a PDF straight to an LLM wastes tokens and loses layout information we can extract reliably and for free.

**Business impact.** Parse quality determines match quality, which determines whether the product works at all.

**Technical impact.** PDF text extraction plus DOCX XML extraction, with page and character counts. Must detect the image-only PDF case explicitly.

**Proposed solution.** Implement extraction in the cv worker. For PDF use a pure-Go extractor; for DOCX read word/document.xml. Record char_count and page_count on the version. If char_count is zero, emit cv.parse_failed with reason=no_extractable_text and a clear candidate-facing message.

**Acceptance criteria.**

- [ ] Text, page count and character count are persisted on the cv_version
- [ ] A scanned image-only PDF produces an explicit, actionable failure rather than an empty profile
- [ ] An encrypted or password-protected PDF is rejected with a clear message
- [ ] Extraction completes within 30 seconds for a 10-page CV
- [ ] CV text never appears in any log line

**Testing.** Fixture corpus: text PDF, scanned PDF, encrypted PDF, DOCX, DOCX with tables, malformed file, 50-page document. Assert the failure reason for each bad case.

**Depends on** `MJF-002`. **Blocks** `MJF-005`, `MJF-007`.

## MJF-004 — Provider-agnostic AI client with structured output, retries and token accounting

`critical` · `ai` · complexity `L`

**Problem.** Every AI feature needs a shared client. Without one, each feature would reimplement schema validation, retries, timeouts and cost tracking, and they would diverge.

**Business impact.** AI is the product's core differentiator and its largest variable cost. Untracked spend is an unbounded liability.

**Technical impact.** Needs a provider interface, schema-constrained generation, bounded retries with a repair prompt on schema violation, per-call timeouts, output token caps, token accounting per purpose, and a prompt-injection-safe message construction helper.

**Proposed solution.** Implement internal/platform/ai with a Provider interface, an Anthropic implementation defaulting to the current Claude models, and a Generate[T] helper that validates against a JSON schema and retries at most twice with a repair prompt. Untrusted text is passed only through a delimited data block helper that cannot be bypassed.

**Acceptance criteria.**

- [ ] No domain package imports a provider SDK directly
- [ ] A schema violation retries twice with a repair prompt, then returns a terminal error
- [ ] prompt_tokens, completion_tokens and total_tokens are recorded per call, labelled by purpose
- [ ] The ai_tokens_month quota is enforced before the call
- [ ] Prompt and completion bodies are never logged
- [ ] Untrusted text can only be included via the delimited-data helper

**Testing.** Unit tests with a fake provider: valid response, schema violation then recovery, schema violation exhausted, timeout, rate limit. A test asserting an injection string inside the data block does not alter the instruction portion.

**Depends on** none. **Blocks** `MJF-005`, `MJF-009`, `MJF-044`.

## MJF-005 — Structured CV extraction with per-field confidence and source offsets

`critical` · `ai` · complexity `L`

**Problem.** Extracted text is unusable for matching until it is structured into experience, education, skills and certifications.

**Business impact.** Match quality depends entirely on extraction quality. A wrongly parsed CV produces irrelevant matches and the candidate blames the product.

**Technical impact.** Schema-constrained LLM extraction into cv_parsed_data, with a confidence score and a character offset per field so the UI can show provenance.

**Proposed solution.** Define a strict extraction schema. Chunk long CVs. Persist per-field confidence and offsets in the confidence column. Surface low-confidence fields for candidate review rather than trusting them silently. Candidate corrections are recorded in corrected_fields and are never overwritten by a re-parse.

**Acceptance criteria.**

- [ ] Experience, education, skills, certifications and languages are extracted into the documented schema
- [ ] Every field carries a confidence score and a character offset into the source text
- [ ] Fields below the confidence threshold are flagged for review
- [ ] A re-parse never overwrites a field listed in corrected_fields
- [ ] total_experience_months and detected_seniority are derived and stored
- [ ] An injection string in the CV does not alter the extraction instructions

**Testing.** A labelled corpus of at least 20 real-shaped CVs with expected extractions, asserting a minimum field-level accuracy. Adversarial CVs containing prompt-injection text.

**Depends on** `MJF-003`, `MJF-004`. **Blocks** `MJF-006`, `MJF-007`, `MJF-008`, `MJF-011`, `MJF-012`.

## MJF-006 — Skill normalisation against a canonical taxonomy

`high` · `ai` · complexity `M`

**Problem.** Raw skill strings are not comparable. 'JS', 'Javascript' and 'JavaScript' would be treated as three unrelated skills, making skill-overlap scoring meaningless.

**Business impact.** Skill overlap is one of the heaviest match dimensions. If it is noise, matching is noise.

**Technical impact.** Map extracted labels onto the skills table via canonical name, aliases and trigram similarity, persisting cv_skills with the raw label retained.

**Proposed solution.** Seed the taxonomy with a substantial skill set and alias list. Normalise by exact match, then alias, then trigram similarity above a threshold. Retain the raw label so nothing is lost, and leave skill_id null rather than forcing a bad match.

**Acceptance criteria.**

- [ ] Common aliases resolve to one canonical skill
- [ ] An unrecognised skill is retained with a null skill_id rather than force-matched
- [ ] The raw label is always preserved
- [ ] Normalisation is deterministic for a given input

**Testing.** A table test of alias resolution, including near-misses that must NOT match (e.g. 'Java' must not resolve to 'JavaScript').

**Depends on** `MJF-005`. **Blocks** `MJF-007`.

## MJF-007 — CV parse worker pipeline with idempotency and dead-lettering

`critical` · `backend` · complexity `M`

**Problem.** The parse pipeline described in docs/SYSTEM_WORKFLOWS.md W1 has no consumers. Uploaded CVs would stay in pending forever.

**Business impact.** A CV that never parses is a candidate who cannot use the product, with no explanation.

**Technical impact.** Chained consumers: av-scan, text-extract, structure, skill-normalise, embed. Each idempotent, with the documented AckWait, MaxDeliver and timeouts.

**Proposed solution.** Register the consumers in internal/worker under the cv group. Idempotency key cv.parse:{cv_version_id}. InFlightRetry throughout, since re-parsing is harmless. Publish cv.parsed on success and cv.parse_failed with the stage on terminal failure.

**Acceptance criteria.**

- [ ] All five stages run in order and are individually idempotent
- [ ] A redelivery after a crash does not duplicate derived rows
- [ ] A terminal failure dead-letters with the stage recorded and notifies the candidate
- [ ] Dead-letter depth above zero raises an alert
- [ ] The whole chain appears as one OpenTelemetry trace

**Testing.** Integration test against real Postgres, NATS and MinIO: upload to parsed, crash-and-resume mid-pipeline, and a poison message reaching the dead-letter queue.

**Depends on** `MJF-003`, `MJF-005`, `MJF-006`. **Blocks** `MJF-064`.

## MJF-008 — AI CV checker with deterministic rules plus an LLM pass

`high` · `ai` · complexity `L`

**Problem.** Candidates need concrete, actionable feedback on their CV. Generic advice is worthless and an opaque score is worse.

**Business impact.** A free-tier acquisition feature and a genuine differentiator, but only if the advice is specific.

**Technical impact.** Deterministic checks (length, sections present, contact detectable, date-format consistency, bullet density, ATS-parseability) combined with an LLM pass for impact and keyword coverage, feeding one documented scoring rubric.

**Proposed solution.** Implement the rule layer first so a score exists without the AI. Add the LLM pass for impact and missing keywords. Every recommendation is anchored to a specific location in the CV. Cache by version hash plus target role.

**Acceptance criteria.**

- [ ] The same CV always produces the same score
- [ ] Sub-scores are reported per category, not just an overall number
- [ ] Every recommendation names a specific location in the CV
- [ ] With the AI provider down, the deterministic subset is returned and the response states what is missing
- [ ] A fabricated score is never returned

**Testing.** Rubric unit tests per category. A snapshot test proving determinism. A degraded-mode test asserting the response declares its own incompleteness.

**Depends on** `MJF-005`. **Blocks** `MJF-052`.

## MJF-009 — Embeddings with a lexical fallback

`high` · `ai` · complexity `M`

**Problem.** Semantic title and skill matching needs embeddings, and matching must keep working when the embedding provider does not.

**Business impact.** Semantic matching is what makes a 'Go developer' CV match a 'Backend Engineer (Golang)' posting.

**Technical impact.** Generate title, skills and summary embeddings per CV version; store in cv_embeddings; provide cosine similarity and a lexical fallback.

**Proposed solution.** Add an Embedder to the AI platform package. Persist vectors with their model and dimensions so a model change is detectable. When unavailable, fall back to trigram/token similarity and mark affected matches scoring_mode=degraded for later rescoring.

**Acceptance criteria.**

- [ ] Embeddings are generated for each CV version and stored with model and dims
- [ ] Cosine similarity is correct against known vectors
- [ ] With the provider down, matching continues lexically and matches are marked degraded
- [ ] Degraded matches are rescored when the provider recovers
- [ ] A match is never dropped because scoring was degraded

**Testing.** Unit tests for cosine similarity and the fallback. An integration test forcing provider failure and asserting matches are still produced and marked.

**Depends on** `MJF-004`. **Blocks** `MJF-011`, `MJF-031`.

## MJF-010 — CV library, versioning and soft-delete protection

`high` · `backend` · complexity `S`

**Problem.** Candidates need several CVs and version history, and a CV referenced by a submitted application must never disappear.

**Business impact.** Multiple CVs enable the A/B testing feature. Losing the CV behind a submitted application destroys the audit trail.

**Technical impact.** List, rename, set-default, soft-delete endpoints; version chain; RESTRICT on the application foreign key is already in the schema.

**Proposed solution.** Implement the CRUD endpoints with user_id scoping. Soft-delete only; refuse hard deletion of a version referenced by any application and explain why.

**Acceptance criteria.**

- [ ] A candidate sees only their own CVs
- [ ] At most one default CV per candidate, enforced by the existing partial unique index
- [ ] A version used by a submitted application cannot be deleted
- [ ] Version history is preserved and browsable

**Testing.** Tenant-isolation test asserting another user's CVs are invisible and unmodifiable. A test proving a referenced version resists deletion.

**Depends on** `MJF-002`. **Blocks** `MJF-028`.

## MJF-011 — CV tailoring with mandatory factual containment validation

`critical` · `ai` · complexity `L`

**Problem.** Tailoring is the highest-value AI feature and the highest-risk: an LLM asked to improve a CV for a job will invent experience unless actively prevented.

**Business impact.** A fabricated line on a CV is a lie told in the candidate's name to an employer. It can cost them the role and their reputation. This is the product's most serious safety requirement.

**Technical impact.** Generate a tailored version constrained to the parsed CV as the sole factual source, then run a post-generation validator that diffs against the source and rejects any new employer, title, date, certification or skill.

**Proposed solution.** Implement the containment validator as a hard gate, independent of the prompt. Prompt compliance is necessary but insufficient, and a job description can carry an injection requesting exactly the fabrication we forbid. On rejection, retry once with a stricter prompt, then report that tailoring could not be completed safely. Persist containment_passed; the schema already requires it to be non-null for a tailored version.

**Acceptance criteria.**

- [ ] Output introducing any employer, title, date, certification or skill absent from the source is rejected
- [ ] Unvalidated output is never surfaced to the candidate or attached to an application
- [ ] The candidate sees a diff against the source before the tailored CV is used
- [ ] containment_passed is recorded on every tailored version
- [ ] A prompt-injection attempt in the job description cannot defeat the validator

**Testing.** Adversarial suite: job descriptions demanding skills the candidate lacks; descriptions containing explicit injection instructions; a mocked provider returning fabricated content, asserting rejection. This suite is a release gate.

**Depends on** `MJF-005`, `MJF-009`. **Blocks** `MJF-041`.

## MJF-012 — CV builder with a sandboxed, network-isolated PDF renderer

`medium` · `backend` · complexity `L`

**Problem.** Candidates need to create or restructure a CV in the product, and rendering user content to PDF is an SSRF and local-file-inclusion risk.

**Business impact.** Removes the dependency on external CV tools and makes ATS-parseable output the default.

**Technical impact.** Template registry, server-side HTML-to-PDF rendering, a preview that uses the same renderer as the final output.

**Proposed solution.** Render in a sandboxed container with no network egress and no filesystem access beyond bundled fonts. Allowlist templates strictly. Use one renderer for both preview and output so what is previewed is what is produced.

**Acceptance criteria.**

- [ ] Output is real text, single column by default, and ATS-parseable
- [ ] The renderer has no network access and cannot read local files
- [ ] Only allowlisted templates can be rendered
- [ ] Preview and final PDF are byte-identical for the same input
- [ ] A renderer timeout returns a clear failure, never a corrupt PDF

**Testing.** SSRF attempt tests (remote image, remote CSS, file:// reference) asserting each is blocked. A determinism test. An ATS-parseability check extracting text back out of the generated PDF.

**Depends on** `MJF-005`. **Blocks** none.


---

# M2 — Job Acquisition

Bring real postings in from lawfully accessible sources, normalised and deduplicated, with loops as the search abstraction.

## MJF-020 — Adapter contract, registry and per-source circuit breaker

`critical` · `job-acquisition` · complexity `M`

**Problem.** There is no abstraction over job sources. Without one, each integration would hand-roll its own rate limiting and failure handling and a single bad source could stall all discovery.

**Business impact.** Job supply is the product's raw material. One failing board must not stop the others.

**Technical impact.** The Adapter interface from docs/INTEGRATION_ANALYSIS.md, a registry, per-source rate limiting enforced inside the adapter, a persisted circuit breaker, and capability declaration.

**Proposed solution.** Implement internal/domain/jobs/adapter with the documented interface. Capabilities drive planner behaviour so an unsupported query is skipped rather than issued and failed. Breaker state persists on job_sources so a restart does not immediately hammer a failing source.

**Acceptance criteria.**

- [ ] Every adapter implements Slug, Capabilities, Search, Fetch and HealthCheck
- [ ] Rate limiting is enforced inside the adapter so a scheduling bug cannot cause abuse
- [ ] A failing source degrades a run and never fails it
- [ ] Repeated failure opens the breaker, alerts, and persists across restart
- [ ] A source cannot be enabled without a recorded legal_basis

**Testing.** A shared conformance suite every adapter must pass. Breaker tests for open, half-open and close transitions.

**Depends on** none. **Blocks** `MJF-021`, `MJF-022`, `MJF-023`, `MJF-024`.

## MJF-021 — Adzuna adapter

`critical` · `integrations` · complexity `M`

**Problem.** No real job data enters the system. Adzuna is the right first source: a documented public API with self-service credentials, broad country coverage and good salary data.

**Business impact.** Validates the entire pipeline end to end with real postings and no partnership negotiation.

**Technical impact.** Paginated search across countries, mapping to the canonical raw shape, honouring the documented rate limit.

**Proposed solution.** Implement the adapter against the current documented API, verified at implementation time rather than from memory. Record fixtures for the conformance suite.

**Acceptance criteria.**

- [ ] Search returns real postings for a title and location across supported countries
- [ ] Pagination is resumable from a persisted cursor
- [ ] Salary, employment type and location are mapped to the canonical shape
- [ ] 429 responses honour Retry-After
- [ ] Credentials come from config and are never committed

**Testing.** Fixture-based conformance tests: normal page, empty result, malformed field, 429, 500. One manually-run live test behind a build tag.

**Depends on** `MJF-020`. **Blocks** `MJF-025`.

## MJF-022 — Remote-only feed adapters (Remotive, RemoteOK, Arbeitnow)

`high` · `integrations` · complexity `S`

**Problem.** Remote roles are a large share of candidate demand and these feeds are public, free and require no credentials.

**Business impact.** Cheap breadth for the most-requested search type.

**Technical impact.** Three simple JSON feed adapters with no authentication.

**Proposed solution.** Implement against the public feeds, declaring remote_only in capabilities so the planner does not waste calls on location-bound searches.

**Acceptance criteria.**

- [ ] Each adapter returns postings and maps them to the canonical shape
- [ ] Capabilities correctly declare remote_only
- [ ] A feed outage degrades the run only

**Testing.** Fixture conformance tests per adapter.

**Depends on** `MJF-020`. **Blocks** none.

## MJF-023 — ATS board adapters (Greenhouse, Lever, Ashby)

`high` · `integrations` · complexity `L`

**Problem.** Aggregators give breadth with a weak apply path. ATS board APIs give clean structured data AND a real, lawful application path, which is the only place fully server-side submission is legitimate.

**Business impact.** This tier is what makes genuine application automation possible rather than just job discovery.

**Technical impact.** Per-company board endpoints, company allowlist seeded by ATS detection, and the submission endpoints each provider documents.

**Proposed solution.** Implement the three board adapters plus ATS-provider detection for company careers pages. Maintain a company allowlist. These are the sources where the submitter in MJF-043 can act server-side.

**Acceptance criteria.**

- [ ] Each adapter lists a company's public postings
- [ ] ATS provider and job id are recorded on the posting so submission can find its way back
- [ ] Capabilities declare supports_submission
- [ ] The company allowlist is configurable and auditable

**Testing.** Fixture conformance tests per provider. ATS-detection tests against recorded careers-page HTML.

**Depends on** `MJF-020`. **Blocks** none.

## MJF-024 — Discovery scheduler keyed by search specification

`critical` · `backend` · complexity `L`

**Problem.** Discovery must run daily per loop, but running it per candidate would multiply upstream API cost by the number of candidates searching for the same thing.

**Business impact.** Upstream rate limits and costs are the binding constraint on how many candidates the platform can serve.

**Technical impact.** Resolve distinct search specs across all active loops, distribute runs across the day, hold a distributed lock per spec and source, and persist resumable cursors on discovery_runs.

**Proposed solution.** Implement the scheduler binary with Redis leader election. Plan by spec hash, not by candidate, so N candidates searching identically cost one upstream query. Spread runs to smooth load and respect upstream limits.

**Acceptance criteria.**

- [ ] Two loops with identical specs produce one upstream query
- [ ] Only one scheduler instance plans at a time, via a Redis leader lock
- [ ] A worker crash resumes a run from its cursor rather than restarting it
- [ ] Runs are distributed across the day rather than bursting
- [ ] Every run records start, end, counts and errors, visible in the admin UI

**Testing.** Integration test with two identical loops asserting a single fetch. Crash-and-resume test. Leader-election test with two schedulers.

**Depends on** `MJF-020`. **Blocks** none.

## MJF-025 — Normalisation: company identity, location, salary, work mode, language

`critical` · `backend` · complexity `L`

**Problem.** Raw postings from 20 boards have incompatible shapes. Without normalisation, deduplication and location scoring are impossible.

**Business impact.** Deduplication depends on normalised identity. A candidate seeing the same job five times loses trust immediately.

**Technical impact.** Company name normalisation, location geocoding to a structured form with a geo key, salary parsing to currency/period/range, employment type and seniority mapping, language detection.

**Proposed solution.** Implement the normalise consumer. Normalise company identity so 'Acme, Inc.' and 'Acme Inc' resolve to one company. Geocode locations; on geocoder failure persist the raw string and backfill later rather than failing the posting.

**Acceptance criteria.**

- [ ] Company variants resolve to one companies row
- [ ] Locations produce a stable geo_key for matching
- [ ] Salaries are normalised to min/max/currency/period with is_estimate recorded
- [ ] Language is detected and stored
- [ ] Geocoder unavailability does not lose the posting
- [ ] Schema drift quarantines the payload rather than corrupting the dataset

**Testing.** Table tests for company, location and salary normalisation with real-world messy inputs. A quarantine test with a malformed payload.

**Depends on** `MJF-021`. **Blocks** `MJF-026`.

## MJF-026 — Fingerprint deduplication with near-duplicate linking

`critical` · `backend` · complexity `L`

**Problem.** The same job appears on many boards. Collapsing them wrongly merges distinct jobs; not collapsing them floods the candidate with duplicates.

**Business impact.** Duplicate matches make the product feel broken. A wrongly merged pair silently hides a real opportunity.

**Technical impact.** A deterministic fingerprint over normalised company, normalised title, geo key and a shingled description hash. Exact collision attaches a source attribution; near matches are linked, never merged.

**Proposed solution.** Implement fingerprinting and the dedupe path. On exact fingerprint match, add a job_posting_sources row to the existing posting. On near match above the similarity threshold, insert both and link them as near_duplicate — merging two distinct jobs is unrecoverable, a link is cheap and reversible.

**Acceptance criteria.**

- [ ] The same posting from three boards yields one job_postings row with three attributions
- [ ] Near-duplicates are linked with a similarity score, not merged
- [ ] The fingerprint is deterministic for identical input
- [ ] jobs.normalised is published only when content actually changed
- [ ] The UI can show 'also on Indeed, LinkedIn' from the attributions

**Testing.** A corpus of known duplicate and near-duplicate pairs with expected outcomes, including hard negatives that must not collapse.

**Depends on** `MJF-025`. **Blocks** `MJF-027`, `MJF-030`, `MJF-060`.

## MJF-027 — Posting liveness re-check and expiry cascade

`high` · `backend` · complexity `S`

**Problem.** Postings disappear upstream. Applying to a dead posting wastes a candidate's daily quota and looks incompetent.

**Business impact.** Quota is scarce on the free tier. Spending it on an expired job is a direct harm.

**Technical impact.** A nightly re-check of postings seen in the last 30 days, publishing jobs.expired and cascading to matches and applications.

**Proposed solution.** Scheduler job re-checking liveness per source, batched and rate-limited. Set status=expired and cascade match and application status.

**Acceptance criteria.**

- [ ] A posting removed upstream becomes expired within 24 hours
- [ ] Matches and un-submitted applications cascade to expired
- [ ] A submitted application is NOT retroactively changed
- [ ] The re-check respects per-source rate limits

**Testing.** Integration test: posting disappears from a fixture source, assert the cascade and that submitted applications are untouched.

**Depends on** `MJF-026`. **Blocks** none.

## MJF-028 — Loops domain: wizard, lifecycle with pause, and config versioning

`critical` · `backend` · complexity `L`

**Problem.** Loops are the product's central abstraction and nothing implements them.

**Business impact.** Without loops there is no automated search. Pause is the specific defect we identified in LoopCV and must not reproduce.

**Technical impact.** Four-step resumable wizard, draft persistence, the draft/active/paused/archived state machine, config versioning, and a Redis kill-flag so pause takes effect in under a second.

**Proposed solution.** Implement the loops domain. Pause sets a Redis flag that submission workers check immediately before every send, so pausing is near-instant rather than effective at the next scheduler tick. Every config change bumps config_version and snapshots the config, so a historical match can be explained by the settings in force when it was created.

**Acceptance criteria.**

- [ ] The wizard is resumable; a partial loop persists as draft
- [ ] Pause halts discovery and pending submissions within one second
- [ ] Editing an active loop takes effect from the next run and is recorded as a config version
- [ ] Duplicating copies configuration but no history
- [ ] Preconditions are enforced at the step where they apply, not at submit
- [ ] The active_loops entitlement is enforced

**Testing.** State-machine tests for every transition including illegal ones. A race test pausing a loop while a submission is in flight, asserting the pre-send gate catches it.

**Depends on** `MJF-010`. **Blocks** `MJF-030`, `MJF-053`.


---

# M3 — Matching

Score postings against candidates explainably, and retain rejections with reasons so a thin funnel is self-diagnosable.

## MJF-030 — Hard filters with a persisted rejection taxonomy

`critical` · `backend` · complexity `M`

**Problem.** Most postings must be rejected before scoring, and a candidate who sees no matches currently has no way to find out why.

**Business impact.** 'Why am I getting no matches?' is the single largest support burden in this product category. Retained rejections turn it into a self-service screen.

**Technical impact.** Ordered hard filters — excluded company, excluded keyword, required keyword missing, location, work mode, language, employment type, seniority floor — each persisting a categorised rejection.

**Proposed solution.** Implement the filter layer, persisting match_rejections with the category and detail. Required keywords use OR semantics (at least one must appear), matching the documented behaviour. Company exclusion matches normalised identity, not raw strings.

**Acceptance criteria.**

- [ ] Each filter rejects correctly and records its category
- [ ] Required keywords are OR semantics; excluded keywords are any-match
- [ ] Company exclusion matches 'Acme, Inc.' and 'Acme Inc' alike
- [ ] A rejected posting remains directly applicable by the candidate
- [ ] Rejections are browsable and filterable by category

**Testing.** A table test per filter including boundary cases. A test asserting a rejected posting can still be applied to.

**Depends on** `MJF-026`, `MJF-028`. **Blocks** `MJF-031`.

## MJF-031 — Explainable multi-dimensional match scoring

`critical` · `backend` · complexity `XL`

**Problem.** Matching is the product's core intelligence, and an unexplainable score is a support ticket. LoopCV's opaque Low/Middle/High knob is the defect we are replacing.

**Business impact.** Candidates act on these scores. If they cannot see why a job scored 82, they cannot trust or tune the system.

**Technical impact.** Independent dimension scores — title similarity, required and nice-to-have skill overlap, years of experience, seniority fit, location fit, employment type fit — combined with versioned weights, producing per-dimension sub-scores and human-readable reasons.

**Proposed solution.** Implement scoring with each dimension independently testable. Build the reason text from the structured sub-scores rather than from free-form LLM output, so an explanation cannot leak prompt content. Weights live in versioned config so historical scores stay interpretable.

**Acceptance criteria.**

- [ ] Every match carries per-dimension sub-scores and reasons
- [ ] The numeric threshold plus the low/middle/high presets both work and agree
- [ ] Weights are versioned; config_version is recorded on each match
- [ ] Scoring is deterministic for identical inputs
- [ ] Explanation text is generated from structured data, never raw model output
- [ ] Degraded scoring is marked and rescored later, never dropped

**Testing.** Per-dimension unit tests. A golden-set regression suite of CV/posting pairs with expected score ranges, acting as a guard against scoring drift.

**Depends on** `MJF-009`, `MJF-030`. **Blocks** `MJF-032`, `MJF-033`, `MJF-034`, `MJF-040`, `MJF-054`.

## MJF-032 — Match fan-out with an inverted index prefilter

`high` · `performance` · complexity `M`

**Problem.** Scoring every new posting against every active loop is quadratic and will not scale past a few thousand candidates.

**Business impact.** Determines how many candidates the platform can serve per unit of compute.

**Technical impact.** An inverted index over title tokens, geo keys and work mode to prefilter candidate loops before scoring.

**Proposed solution.** Implement match-fanout with a prefilter, then score only plausible pairs. Measure the reduction and alert if the prefilter stops being effective.

**Acceptance criteria.**

- [ ] A new posting fans out only to loops that could plausibly admit it
- [ ] The prefilter never excludes a pair that full scoring would have matched
- [ ] Matching lag stays within target at 10k active loops
- [ ] Prefilter effectiveness is a published metric

**Testing.** A correctness test asserting the prefilter is a superset of true matches. A load test at 10k loops and 50k postings.

**Depends on** `MJF-031`. **Blocks** none.

## MJF-033 — Per-candidate company cooldown

`high` · `backend` · complexity `S`

**Problem.** Applying repeatedly to the same company embarrasses the candidate and annoys the employer.

**Business impact.** Reputational harm to the candidate is a direct product harm.

**Technical impact.** A configurable per-candidate, per-company suppression window, defaulting to the observed six months, surfaced with the prior contact date.

**Proposed solution.** Implement using company_contact_cooldowns. Surface the suppression as already_applied or email_already_sent WITH the prior date, so the candidate understands rather than suspects a bug.

**Acceptance criteria.**

- [ ] A second application to the same company inside the window is suppressed
- [ ] The suppression is surfaced with the prior contact date and method
- [ ] The window is configurable
- [ ] Cooldown is per candidate, independent of other candidates

**Testing.** Tests at the window boundary, and a tenant test asserting one candidate's cooldown does not affect another's.

**Depends on** `MJF-031`. **Blocks** none.

## MJF-034 — Backfill rescoring on config or CV change

`high` · `backend` · complexity `M`

**Problem.** A candidate who loosens a filter should not wait a day to see the effect.

**Business impact.** The filter-tuning journey is how a candidate recovers from a too-narrow search. A day's delay makes it unusable.

**Technical impact.** Republish matching.requested for recent postings when a loop's config_version or cv_version changes.

**Proposed solution.** Implement a backfill planner triggered by config and CV changes, bounded to the last N days, with the idempotency key including config_version so unchanged pairs are not rescored.

**Acceptance criteria.**

- [ ] A filter change rescores recent postings within minutes
- [ ] A new CV version rescores the loop's matches
- [ ] Unchanged pairs are not rescored
- [ ] Backfill cannot starve live matching

**Testing.** Integration test: tighten then loosen a filter, assert match counts change appropriately and no duplicate matches appear.

**Depends on** `MJF-031`. **Blocks** none.


---

# M4 — Application Orchestration

Prepare, authorise and submit applications, with consent recorded and duplicates structurally impossible.

## MJF-040 — Deterministic submission method selection with recorded reasoning

`critical` · `backend` · complexity `M`

**Problem.** Each posting supports a different application channel and the choice must be explainable to both the candidate and an administrator.

**Business impact.** A candidate needs to know why a job requires manual application rather than being applied to automatically.

**Technical impact.** Ordered selection — ats_api, public_form, authorised_email, extension, manual — driven by the posting, the loop's automation settings and entitlements, with the choice and its reason persisted.

**Proposed solution.** Implement method selection as a pure, ordered function so it is exhaustively testable, persisting both the method and the human-readable reason on the application.

**Acceptance criteria.**

- [ ] Selection is deterministic and the reason is persisted
- [ ] A posting requiring an authenticated session routes to the extension channel, never to server-side automation
- [ ] Disabled automation settings route to manual rather than failing
- [ ] The reason is shown to the candidate

**Testing.** An exhaustive table test over posting shapes and automation settings, asserting the chosen method and reason for each.

**Depends on** `MJF-031`. **Blocks** `MJF-041`.

## MJF-041 — Application preparation materialising immutable artefacts

`critical` · `backend` · complexity `L`

**Problem.** What gets sent must be known and fixed before authorisation, or the audit trail is decorative and consent is meaningless.

**Business impact.** The candidate is consenting to specific content. If the content can change after consent, the consent is worthless.

**Technical impact.** Resolve the CV version, render the cover letter and email body from the template, map form fields from the profile, collect unknown screening questions, and persist all artefacts immutably.

**Proposed solution.** Implement preparation writing every artefact to the application row. The applications_freeze_authorised_artefacts trigger already makes them immutable post-authorisation. A missing required field moves the application to blocked with a precise reason and a deep link to fix it, rather than failing at submit time.

**Acceptance criteria.**

- [ ] All artefacts are persisted and are byte-identical to what will be sent
- [ ] Template rendering uses only the allowlisted placeholders; an unknown placeholder is a validation error
- [ ] A missing required field blocks with an actionable reason
- [ ] Preparation never sends anything
- [ ] Artefacts cannot be modified after authorisation

**Testing.** Tests asserting artefact immutability post-authorisation, template placeholder validation, and the blocked path for each required field.

**Depends on** `MJF-040`, `MJF-011`. **Blocks** `MJF-042`, `MJF-044`, `MJF-046`.

## MJF-042 — Authorisation: per-application and bounded standing consent

`critical` · `security` · complexity `L`

**Problem.** No application may be submitted without recorded candidate authorisation. This is the product's integrity boundary.

**Business impact.** Applying on someone's behalf without their consent is the worst thing this product could do. It must be structurally impossible, not merely unlikely.

**Technical impact.** Per-application approval, plus a standing authorisation scoped to one loop with a daily cap, a minimum score and an expiry, capturing a snapshot of what was agreed. Immutable records, fully audited.

**Proposed solution.** Implement both paths writing immutable application_authorisations rows. The existing applications_sent_requires_authorisation constraint is the backstop. Re-check validity, cap and the loop pause flag immediately before every dispatch.

**Acceptance criteria.**

- [ ] No application reaches a sent state without a valid authorisation
- [ ] A standing authorisation has a daily cap, a minimum score and a mandatory expiry
- [ ] Every authorisation stores a snapshot of exactly what was agreed
- [ ] Revocation takes effect before the next send
- [ ] Every grant and revocation is audited
- [ ] A submission completing just after a pause is recorded as authorised_before_pause

**Testing.** A dedicated security test suite attempting to submit without authorisation through every code path. Cap-exhaustion, expiry and revocation-race tests. This suite is a release gate.

**Depends on** `MJF-041`. **Blocks** `MJF-043`, `MJF-055`.

## MJF-043 — Submission adapters with idempotency and honest outcome reporting

`critical` · `backend` · complexity `XL`

**Problem.** Submission is where an error becomes irreversible. A duplicate application cannot be recalled.

**Business impact.** Applying twice to one job damages the candidate's standing with that employer.

**Technical impact.** Submitters per method, an idempotency key persisted before dispatch, bounded retries for transient failures, and a first-class outcome_unknown state when a request was sent but the response was lost.

**Proposed solution.** Implement the Submitter contract with the key written before dispatch and completed after. On redelivery with a key in_flight, report outcome_unknown and dead-letter rather than repeating a possibly-successful send. Never auto-retry an unknown outcome.

**Acceptance criteria.**

- [ ] Submit is safe to call twice with the same idempotency key
- [ ] A lost response produces outcome_unknown, never a guess or a silent retry
- [ ] Transient failures retry with backoff up to a cap, then fail with a reason
- [ ] Permanent failures are terminal with a reason
- [ ] Per-destination-domain rate limits are enforced
- [ ] The pre-send gate re-checks authorisation, cap, pause flag, suppression and cooldown

**Testing.** Idempotency tests per method. A simulated lost-response test asserting outcome_unknown and no retry. A concurrency test with two workers on one application asserting a single submission.

**Depends on** `MJF-042`. **Blocks** `MJF-045`, `MJF-061`, `MJF-065`.

## MJF-044 — Screening questions with saved answers and sensitive-category refusal

`high` · `ai` · complexity `L`

**Problem.** Forms ask screening questions that block submission, and some of those questions must never be answered by a machine.

**Business impact.** An AI-answered disability or visa question is a discrimination-exposure problem for the candidate and the platform.

**Technical impact.** A questions queue, similarity-matched saved answers, AI drafting for non-sensitive questions on higher tiers, and hard refusal for sensitive categories.

**Proposed solution.** Implement the queue and saved-answer reuse by normalised question similarity. AI answers draw only on the parsed CV, profile and previous answers; anything unsupported is escalated, never guessed. Sensitive categories are always escalated — the app_questions_sensitive_never_ai constraint enforces it in the database too.

**Acceptance criteria.**

- [ ] Nothing is submitted while required questions are pending
- [ ] Saved answers are reused across applications by question similarity
- [ ] Sensitive categories are never auto-answered on any tier
- [ ] An AI answer unsupported by profile data escalates instead of guessing
- [ ] Saved answers are viewable and editable
- [ ] Question text is treated as untrusted input in prompts

**Testing.** A sensitive-category detection suite across phrasings and languages. A test asserting an unsupported claim escalates. Injection tests on question text.

**Depends on** `MJF-041`, `MJF-004`. **Blocks** `MJF-056`.

## MJF-045 — Application status derived from an append-only event log

`critical` · `backend` · complexity `M`

**Problem.** Status mutated in place destroys history, and the history is what makes the audit trail and the analytics trustworthy.

**Business impact.** A candidate must be able to see exactly what happened to each application, and the analytics numbers must reconcile with it.

**Technical impact.** Append-only application_events, with status derived from the log rather than written directly.

**Proposed solution.** Implement the event log and derive status from it. Every transition appends an event with its actor, payload and correlation id. The timeline view reconstructs from the log.

**Acceptance criteria.**

- [ ] Every status change appends an event; no status is written without one
- [ ] The full history is reconstructible from the log
- [ ] The timeline shows the artefacts that were actually sent
- [ ] Analytics derived from the log reconcile exactly with the per-application timeline

**Testing.** A test replaying an event log and asserting the derived status matches at every step. A reconciliation test between analytics aggregates and per-application histories.

**Depends on** `MJF-043`. **Blocks** `MJF-063`, `MJF-064`.

## MJF-046 — Browser extension API with no credential relay

`high` · `security` · complexity `L`

**Problem.** Authenticated boards can only be reached legitimately through the candidate's own session, and the server must never hold their job-board credentials.

**Business impact.** This is the boundary between legitimate assistance and account compromise. Crossing it would expose candidates to losing their LinkedIn or Indeed accounts.

**Technical impact.** A queue endpoint returning extension-eligible applications with prepared field values, authenticated with a short-lived extension-audience token and a strict origin allowlist.

**Proposed solution.** Implement the extension API surface behind the extension audience, which is already rejected against the main API. The server never receives credentials or session cookies. If a CAPTCHA or any anti-bot challenge appears, control is handed to the candidate; nothing is solved or bypassed.

**Acceptance criteria.**

- [ ] Extension tokens are short-lived and rejected against the main API
- [ ] No endpoint accepts a job-board credential or session cookie
- [ ] The origin allowlist is enforced
- [ ] Results report back and append application events
- [ ] No CAPTCHA or anti-bot handling exists anywhere in the codebase

**Testing.** Audience-isolation tests. A grep-based test asserting no credential-relay fields exist in any extension request type.

**Depends on** `MJF-041`. **Blocks** none.


---

# M5 — Frontend

A polished interface where every visible feature is wired to real backend behaviour and no number is presented more confidently than the data supports.

## MJF-050 — Next.js foundation: App Router, Tailwind, shadcn/ui, React Query, Zod

`critical` · `frontend` · complexity `M`

**Problem.** There is no frontend. The API is unusable by a candidate.

**Business impact.** Without a UI there is no product, only an API.

**Technical impact.** Next.js App Router with TypeScript, Tailwind and shadcn/ui, a React Query provider, a typed API client with Zod validation, and design tokens.

**Proposed solution.** Scaffold the application with the conventions in CLAUDE.md: server components by default, all server state through React Query, every payload validated by a Zod schema so a contract mismatch surfaces as a clear error rather than rendering as blank.

**Acceptance criteria.**

- [ ] make fe-typecheck and make fe-lint pass
- [ ] Design tokens are defined; no hardcoded colours in components
- [ ] The API client attaches the access token and refreshes once on 401 with single-flight dedupe
- [ ] A Zod validation failure produces a visible, actionable error
- [ ] Dark mode works

**Testing.** Component tests for the API client's refresh path, including concurrent 401s collapsing into one refresh.

**Depends on** none. **Blocks** `MJF-051`.

## MJF-051 — Authentication and onboarding screens

`critical` · `frontend` · complexity `L`

**Problem.** A candidate cannot sign up, verify, sign in or complete onboarding.

**Business impact.** The first five minutes decide whether a candidate ever reaches a match.

**Technical impact.** Sign-up, sign-in, email verification, and a resumable onboarding wizard covering profile, CV upload and first loop.

**Proposed solution.** Build the flows against the live API. Accessible forms with Zod validation and inline server errors. The onboarding wizard is resumable, with completeness shown as progress and the specific missing fields named.

**Acceptance criteria.**

- [ ] Sign-up, verification and sign-in all work against the running API
- [ ] Form validation matches the server's rules and shows field-level errors
- [ ] Onboarding is resumable after a reload
- [ ] The password field shows the real policy, not invented composition rules
- [ ] Keyboard navigation and screen-reader labels are correct throughout

**Testing.** End-to-end journey test from sign-up through to a configured loop. Accessibility tests on every form.

**Depends on** `MJF-050`. **Blocks** `MJF-052`, `MJF-053`, `MJF-054`.

## MJF-052 — CV library, upload, parse-review and CV checker screens

`high` · `frontend` · complexity `L`

**Problem.** CV management is the candidate's main lever on match quality and there is no interface for it.

**Business impact.** A badly parsed CV produces bad matches. The review screen is how a candidate fixes that themselves.

**Technical impact.** Drag-and-drop upload with progress, live parse status, a field-level parse review with provenance, the CV checker dashboard, and per-CV performance stats.

**Proposed solution.** Build the library grid, the upload flow with pre-upload type and size feedback, and the parse-review screen showing confidence and letting the candidate correct fields — corrections being permanently authoritative. The checker shows sub-scores and anchored recommendations.

**Acceptance criteria.**

- [ ] Upload shows progress and rejects wrong types before uploading
- [ ] Parse status is live, naming the current stage rather than showing a bare spinner
- [ ] Low-confidence fields are visibly flagged and editable
- [ ] The checker shows per-category sub-scores and location-anchored recommendations
- [ ] Per-CV stats withhold a verdict below the minimum sample and say so

**Testing.** Component tests for each upload failure mode. A test asserting the 'not enough data' state appears below the sample threshold.

**Depends on** `MJF-051`, `MJF-008`. **Blocks** none.

## MJF-053 — Loop configuration wizard and loop management

`critical` · `frontend` · complexity `L`

**Problem.** Loops cannot be created or managed without a UI.

**Business impact.** The loop wizard is where a candidate's search intent is captured. Getting it wrong produces irrelevant matches.

**Technical impact.** The four-step wizard, draft resume, a live email-template preview with real substitutions, a filter impact preview, and lifecycle controls.

**Proposed solution.** Build the stepper with per-step validation and preconditions enforced where they apply. Show the filter impact preview ('this would have rejected N of your last 240 postings') so filter tuning is informed. Pause is a prominent, first-class control.

**Acceptance criteria.**

- [ ] All four steps validate independently and the draft resumes
- [ ] The email template preview shows real substituted values
- [ ] The filter editor shows the impact of a change before saving
- [ ] Pause is prominent and takes effect visibly
- [ ] An edit shows a clear 'takes effect next run' notice
- [ ] Entitlement limits are explained at the point of friction with an upgrade path, never a dead end

**Testing.** Wizard tests for each step's validation and for draft resume. A test asserting the pause control reflects server state.

**Depends on** `MJF-051`, `MJF-028`. **Blocks** none.

## MJF-054 — Matches, rejected-matches browser and match detail

`critical` · `frontend` · complexity `L`

**Problem.** Matches are the product's output and there is nowhere to see them.

**Business impact.** This is the screen a candidate opens every day. It is the product.

**Technical impact.** A virtualised, server-paginated, filterable match list; a detail view with the full score breakdown; the rejected-matches browser filtered by category; and bulk actions.

**Proposed solution.** Build the list with saved filters and keyboard navigation, the detail view showing every sub-score and reason, and the rejected browser that turns 'why no matches?' into self-service. Empty states name the current pipeline stage.

**Acceptance criteria.**

- [ ] The list is server-paginated and stays responsive at thousands of matches
- [ ] The detail view shows every dimension's sub-score and reason
- [ ] Rejected matches are browsable by category and can still be applied to
- [ ] Bulk actions are optimistic and roll back on failure
- [ ] The empty state names the pipeline stage and expected timing, never a bare spinner

**Testing.** Virtualisation performance test at 5000 rows. A test asserting the empty state renders stage information.

**Depends on** `MJF-051`, `MJF-031`. **Blocks** `MJF-055`.

## MJF-055 — Application management, Kanban board and the authorisation flow

`critical` · `frontend` · complexity `XL`

**Problem.** Applications cannot be reviewed, authorised or tracked, and the authorisation screen is where informed consent actually happens.

**Business impact.** If the candidate cannot see exactly what will be sent, their consent is not informed.

**Technical impact.** An application list and Kanban board, a per-application timeline, the authorisation screen showing the real artefacts, and standing-authorisation setup with its cap, expiry and plain-language summary.

**Proposed solution.** Build the authorisation screen to display the actual CV version, rendered cover letter and email body before the approve action. Standing authorisation shows a plain-language summary of exactly what it permits, with a visible revoke control. The timeline shows what was sent.

**Acceptance criteria.**

- [ ] The authorisation screen shows the exact artefacts that will be sent
- [ ] Standing authorisation setup states its cap, expiry and minimum score in plain language
- [ ] Revoke is visible and takes effect immediately
- [ ] The Kanban board reflects real statuses including questions_pending and outcome_unknown
- [ ] outcome_unknown is shown honestly as 'we could not confirm', never as success or failure
- [ ] The timeline shows every event with its timestamp

**Testing.** A test asserting the approve action is unavailable until artefacts have loaded. A test asserting outcome_unknown renders as uncertainty.

**Depends on** `MJF-054`, `MJF-042`. **Blocks** `MJF-056`.

## MJF-056 — Questions queue, analytics dashboard and notifications

`high` · `frontend` · complexity `L`

**Problem.** Screening questions block submissions, and the funnel is invisible without an analytics view.

**Business impact.** Pending questions silently stall applications. Funnel visibility is what lets a candidate improve.

**Technical impact.** The questions queue with sensitive-question marking, the funnel dashboard, per-CV comparison with confidence intervals, and the notification centre.

**Proposed solution.** Build the queue showing which questions need the candidate specifically and why. The analytics dashboard shows the funnel from the event log, labels small samples, and declines to name a winner below the minimum sample. Open tracking is labelled as indicative, not fact.

**Acceptance criteria.**

- [ ] Sensitive questions are marked as requiring the candidate, with the reason
- [ ] The funnel reconciles with the per-application timelines
- [ ] Small samples are labelled and no verdict is shown below the threshold
- [ ] Open rates are presented as indicative, with the reason stated
- [ ] Notification preferences work per category

**Testing.** A test asserting no winner is declared below the sample threshold. A test asserting sensitive questions cannot be AI-answered from the UI.

**Depends on** `MJF-055`, `MJF-044`. **Blocks** `MJF-082`.


---

# M6 — Outreach, Analytics & Admin

Lawful recruiter outreach, analytics that reconcile with the event log, operational visibility, billing and GDPR obligations.

## MJF-060 — Recruiter contact discovery with recorded provenance

`medium` · `integrations` · complexity `M`

**Problem.** The email application channel needs recruiter addresses, and obtaining them unlawfully would be both illegal and reputationally fatal.

**Business impact.** Email is a real application channel, but a GDPR violation or a spam reputation would end the product.

**Technical impact.** Contacts only from the posting itself, the company's published careers address, or a licensed provider. Every contact records provenance, lawful basis and verification state.

**Proposed solution.** Implement discovery from lawful sources only. No email-pattern guessing — the contact_provenance enum has no value for it, so it is structurally impossible. Unverified addresses are never used for automated sending.

**Acceptance criteria.**

- [ ] Every contact records provenance and a lawful basis
- [ ] No code path constructs an address from a name-and-domain pattern
- [ ] Unverified addresses are excluded from automated sending
- [ ] Provenance is auditable per contact

**Testing.** A test asserting no pattern-guessing code path exists. Provenance-recording tests per source.

**Depends on** `MJF-026`. **Blocks** `MJF-061`.

## MJF-061 — Outreach sending with suppression, SMTP support and bounce handling

`medium` · `backend` · complexity `L`

**Problem.** Sending application emails without suppression and bounce handling destroys deliverability for every candidate on the platform.

**Business impact.** One candidate's spam complaints must not poison delivery for everyone else.

**Technical impact.** Platform sending with the candidate in Reply-To, optional candidate SMTP with envelope-encrypted credentials, per-recipient-domain rate limits, global suppression checked before every send, and bounce and complaint handling.

**Proposed solution.** Implement the send path with the suppression check as an unconditional pre-send gate. SMTP credentials are write-only through the API, never returned and never logged. Hard bounces suppress permanently; complaints suppress and alert.

**Acceptance criteria.**

- [ ] The suppression list is checked before every send without exception
- [ ] SMTP credentials are never returned by any endpoint and never appear in logs
- [ ] Per-recipient-domain rate limits are enforced
- [ ] A hard bounce suppresses permanently; a complaint suppresses and alerts
- [ ] An SMTP auth failure disables the config and notifies the candidate

**Testing.** A test asserting no send path bypasses the suppression check. Credential-exposure tests across every endpoint and log line.

**Depends on** `MJF-043`, `MJF-060`. **Blocks** `MJF-062`.

## MJF-062 — Open and reply tracking with an inbox

`low` · `backend` · complexity `M`

**Problem.** Candidates cannot tell whether their applications were read or replied to.

**Business impact.** Reply detection drives the status pipeline and the analytics funnel.

**Technical impact.** A disclosed, disableable tracking pixel; per-application reply addresses; inbound ingestion threaded against applications.

**Proposed solution.** Implement tracking and reply ingestion. Inbound mail is untrusted: attachments are scanned, HTML sanitised, and no inbound content ever reaches a prompt instruction position. Open tracking is presented as indicative because image blocking makes it unreliable.

**Acceptance criteria.**

- [ ] Opens are recorded and the pixel can be disabled by the candidate
- [ ] Replies are threaded to the correct application
- [ ] Inbound attachments are AV-scanned and HTML is sanitised
- [ ] No inbound content reaches a prompt instruction position
- [ ] The UI labels open data as indicative

**Testing.** Injection tests on inbound email content. Threading tests with malformed reply addresses.

**Depends on** `MJF-061`. **Blocks** none.

## MJF-063 — Analytics rollups reconciling with the event log

`medium` · `backend` · complexity `M`

**Problem.** Funnel analytics computed independently of the event log will disagree with the per-application history, and then neither is trusted.

**Business impact.** A funnel number that contradicts the detail view destroys confidence in all reporting.

**Technical impact.** Nightly rollups aggregating application_events into per-user, per-loop and per-CV summaries, plus live counters for today.

**Proposed solution.** Derive every aggregate from the event log, so the dashboard and the timeline cannot disagree. Compute per-CV reply and interview rates with confidence intervals and an explicit minimum-sample gate.

**Acceptance criteria.**

- [ ] Every aggregate derives from application_events
- [ ] Dashboard totals reconcile exactly with per-application histories
- [ ] Confidence intervals and sample sizes are computed and exposed
- [ ] Below the minimum sample, no verdict is produced
- [ ] Rollups are idempotent and re-runnable

**Testing.** A reconciliation test comparing aggregates against a replay of the event log. An idempotency test re-running a rollup.

**Depends on** `MJF-045`. **Blocks** none.

## MJF-064 — Admin dashboard with end-to-end workflow tracing

`high` · `observability` · complexity `XL`

**Problem.** There is no operational visibility. A failed CV parse or a stuck submission cannot be diagnosed.

**Business impact.** Without this, every production problem is a guess, and candidate-reported issues cannot be investigated.

**Technical impact.** Platform metrics, queue and stream health, per-source discovery health, AI spend, worker execution, and a trace view reconstructing one workflow from a correlation id with timestamps, durations and errors at every hop.

**Proposed solution.** Build the admin surface with every number linking through to its underlying records. The trace view correlates OpenTelemetry traces with the application event log and Loki logs. PII is masked by default and unmasking is a separately audited action.

**Acceptance criteria.**

- [ ] An administrator can trace a CV upload through every stage with timestamps and durations
- [ ] Dead-letter queues are visible with the full envelope and a requeue action
- [ ] AI token spend is visible per purpose and per user
- [ ] PII is masked by default; unmasking is audited
- [ ] No CV content, password, token or prompt body appears anywhere in the admin UI or its logs

**Testing.** A test asserting a correlation id reconstructs a full chain. A redaction test across every admin view.

**Depends on** `MJF-045`, `MJF-007`. **Blocks** none.

## MJF-065 — Subscriptions, entitlements and webhook-authoritative billing

`high` · `backend` · complexity `L`

**Problem.** Entitlements are defined in the plans table but nothing enforces them, and there is no billing integration.

**Business impact.** No revenue, and no protection against a single user consuming unbounded AI spend.

**Technical impact.** Stripe checkout, signature-verified idempotent webhooks as the source of truth, entitlement resolution, Redis quota counters with a nightly Postgres reconciliation, and graceful degradation at the limit.

**Proposed solution.** Implement entitlements.Check as the single enforcement point. Webhooks are keyed on the provider's event id because providers deliver duplicates and out-of-order events routinely. Exceeding a quota degrades gracefully: matches keep accumulating and submissions queue rather than fail.

**Acceptance criteria.**

- [ ] Quotas are enforced server-side only; a client-supplied tier is never trusted
- [ ] Webhook signatures are verified and processing is idempotent on the provider event id
- [ ] Out-of-order webhooks do not corrupt subscription state
- [ ] A lost webhook is caught by nightly reconciliation
- [ ] Exceeding a quota queues submissions rather than losing them
- [ ] A Redis outage falls back to the Postgres rollup with a conservative cap

**Testing.** Webhook replay, duplicate and out-of-order tests. Quota boundary tests. A Redis-failure test asserting the conservative fallback.

**Depends on** `MJF-043`. **Blocks** `MJF-066`.

## MJF-066 — GDPR data export and account erasure

`high` · `security` · complexity `L`

**Problem.** There is no way for a candidate to export or erase their data, which is a legal obligation for a platform holding CVs and application history.

**Business impact.** A non-negotiable legal requirement, and a trust signal.

**Technical impact.** An export job assembling per-table JSON plus original CV files; a 7-day grace period; an idempotent resumable erasure job with a verification pass.

**Proposed solution.** Implement both as resumable idempotent jobs. Deletion halts automation immediately while leaving sign-in available during the grace period, so a candidate can cancel. The erasure verification pass fails loudly rather than reporting a partial erasure as complete.

**Acceptance criteria.**

- [ ] Export covers every table holding the candidate's data plus their CV files
- [ ] Export links are single-use with a one-hour TTL and bound to the requesting session
- [ ] Automation halts immediately on a deletion request; sign-in continues during grace
- [ ] Erasure is resumable and idempotent
- [ ] A verification pass confirms completeness and fails loudly if not
- [ ] Deletion is available on every tier

**Testing.** An export completeness test enumerating every table with a user_id column. An erasure test asserting the verification pass catches an induced partial erasure.

**Depends on** `MJF-065`. **Blocks** none.


---

# M7 — Infrastructure & GitOps

Containerise, chart, and deploy to a VPS Kubernetes cluster reconciled from Git, with observability and a tested restore path.

## MJF-070 — Production Dockerfiles for api, worker, scheduler and migrate

`critical` · `infrastructure` · complexity `M`

**Problem.** Nothing is containerised, so nothing can be deployed.

**Business impact.** Blocks every deployment.

**Technical impact.** Multi-stage builds, non-root users, minimal base images, embedded migrations, and reproducible builds.

**Proposed solution.** One multi-stage Dockerfile per binary with a distroless or minimal runtime, running as non-root with a read-only root filesystem. Migrations are embedded so the migration job provably runs the SQL built from that commit.

**Acceptance criteria.**

- [ ] Images run as a non-root user with a read-only root filesystem
- [ ] Final image size is under 50MB per binary
- [ ] Builds are reproducible for a given commit
- [ ] The migrate image contains the embedded migrations
- [ ] A container security scan reports no high or critical findings

**Testing.** Build and run each image. Trivy scan in CI as a gate.

**Depends on** none. **Blocks** `MJF-071`.

## MJF-071 — Helm chart with development, staging and production values

`critical` · `infrastructure` · complexity `L`

**Problem.** There is no Kubernetes deployment definition.

**Business impact.** Blocks deployment to the VPS cluster and any later migration to managed Kubernetes.

**Technical impact.** Deployments for api, worker (per group) and scheduler; services; ingress; configmaps; secret references; probes; resource requests and limits; HPA; network policies; pod security contexts; and a migration job with correct ordering.

**Proposed solution.** One chart with three value files. Worker deployments are split by consumer group so an AI outage stalls cv consumers while applications keeps submitting. The migration job is a pre-install and pre-upgrade hook so the schema is always ahead of the code.

**Acceptance criteria.**

- [ ] One chart deploys to all three environments via values alone
- [ ] Workers are separately scalable per consumer group
- [ ] Liveness, readiness and startup probes are wired to the real endpoints
- [ ] Resource requests and limits are set on every container
- [ ] Network policies default to deny
- [ ] Pod security contexts drop all capabilities and disallow privilege escalation
- [ ] The migration job runs before the application starts
- [ ] helm lint and a template render pass for all three environments

**Testing.** helm lint, helm template, and kubeconform validation in CI. A deploy to a local kind or k3s cluster as an integration gate.

**Depends on** `MJF-070`. **Blocks** `MJF-072`, `MJF-074`.

## MJF-072 — FluxCD GitOps repository structure with SOPS-encrypted secrets

`high` · `devops` · complexity `L`

**Problem.** No GitOps. Deployment would be manual and undocumented.

**Business impact.** Manual deployment is unrepeatable and unauditable.

**Technical impact.** Cluster bootstrap, GitRepository and HelmRepository sources, HelmReleases per environment, Kustomizations, image update automation, health checks and a documented rollback.

**Proposed solution.** Structure the Flux repository with clean environment separation. Secrets are SOPS-encrypted with age; nothing is plaintext in Git. Image update automation is scoped to patch tags only, so a major version never deploys itself.

**Acceptance criteria.**

- [ ] Bootstrap is documented and repeatable from scratch
- [ ] Environments are separated and cannot cross-contaminate
- [ ] No plaintext secret exists anywhere in Git history
- [ ] HelmRelease health checks gate the rollout
- [ ] Rollback is documented and tested
- [ ] Image automation is limited to patch versions

**Testing.** A bootstrap test against a kind cluster. A git-history secret scan as a CI gate.

**Depends on** `MJF-071`. **Blocks** `MJF-073`, `MJF-083`.

## MJF-073 — VPS bootstrap and hardening scripts

`high` · `devops` · complexity `L`

**Problem.** There is no path from a bare VPS to a running cluster.

**Business impact.** Blocks the initial deployment target.

**Technical impact.** Idempotent scripts for SSH hardening, firewall, users and permissions, unattended security updates, Docker, k3s, Flux bootstrap, backups and cluster health checks.

**Proposed solution.** Write each script to be safe, documented and idempotent, with a dry-run mode. SSH hardening disables password authentication and root login. Backups cover Postgres and object storage with a tested restore procedure.

**Acceptance criteria.**

- [ ] Every script is idempotent and safe to re-run
- [ ] A dry-run mode shows what would change without changing it
- [ ] SSH hardening disables password and root login
- [ ] The firewall defaults to deny inbound except SSH, HTTP and HTTPS
- [ ] Backups are scheduled and the restore procedure is documented and tested
- [ ] No secret is written to disk in plaintext

**Testing.** Run each script twice against a fresh VM asserting the second run is a no-op. Perform an actual restore from backup.

**Depends on** `MJF-072`. **Blocks** none.

## MJF-074 — Observability stack: OpenTelemetry Collector, Prometheus, Grafana, Loki, alerting

`high` · `observability` · complexity `L`

**Problem.** Metrics and traces are emitted but nothing collects, stores or alerts on them.

**Business impact.** Without alerting, failures are discovered by candidates rather than by the team.

**Technical impact.** Collector deployment, Prometheus with recording and alerting rules, Grafana dashboards, Loki with log shipping, and alert routing.

**Proposed solution.** Deploy the stack via Helm. Build dashboards for the pipeline funnel, consumer lag, AI spend, submission outcomes and per-source health. Alert on dead-letter depth above zero, consumer lag, submission failure rate, AI error rate, p99 latency and database saturation.

**Acceptance criteria.**

- [ ] Traces span the full asynchronous chain as one trace
- [ ] Dashboards exist for the funnel, queues, AI spend and source health
- [ ] Dead-letter depth above zero alerts immediately
- [ ] Logs are queryable by request_id, correlation_id and trace_id
- [ ] No alert fires on a healthy system for 24 hours

**Testing.** Induce each alert condition and confirm it fires and routes. Verify a correlation id resolves across traces, logs and metrics.

**Depends on** `MJF-071`. **Blocks** none.

## MJF-075 — Terraform for VPS, DNS, object storage and backups

`medium` · `infrastructure` · complexity `M`

**Problem.** Infrastructure is provisioned by hand, so it is undocumented and unreproducible.

**Business impact.** A lost server would be a manual, error-prone rebuild.

**Technical impact.** Modules for the VPS, DNS records, S3-compatible storage buckets with lifecycle rules, and backup storage, with remote state.

**Proposed solution.** Write modules with remote state and locking. Keep the module boundaries aligned with the migration path to managed Kubernetes, so moving Postgres to a managed instance is a variable change rather than a rewrite.

**Acceptance criteria.**

- [ ] terraform plan is clean against live infrastructure
- [ ] State is remote and locked
- [ ] Buckets have lifecycle and retention rules
- [ ] No credential is committed
- [ ] The managed-Kubernetes migration path is documented

**Testing.** terraform validate and plan in CI. A full apply into a scratch project.

**Depends on** none. **Blocks** none.


---

# M8 — CI/CD & Hardening

Separate CI from CD, make the safety invariants continuously verified rather than verified once, and document the security posture.

## MJF-080 — CI workflow: format, vet, lint, test, race, coverage, migration validation

`critical` · `devops` · complexity `M`

**Problem.** There is no CI. Nothing stops a broken commit reaching main.

**Business impact.** Every defect that reaches main costs more to find than one caught in CI.

**Technical impact.** GitHub Actions running gofmt check, go vet, golangci-lint, unit tests with -race, coverage reporting, migration validation, frontend lint, typecheck and tests.

**Proposed solution.** Implement CI with caching, running the backend and frontend jobs in parallel. Migration validation runs without a database so it needs no service container.

**Acceptance criteria.**

- [ ] Every push and pull request runs the full suite
- [ ] Tests run with -race
- [ ] A formatting or vet failure blocks the merge
- [ ] Coverage is reported per package
- [ ] The pipeline completes in under 10 minutes
- [ ] Migration validation requires no database

**Testing.** Verify each check fails the build when deliberately broken.

**Depends on** none. **Blocks** `MJF-081`, `MJF-083`, `MJF-084`, `MJF-086`.

## MJF-081 — Integration test suite with testcontainers

`critical` · `testing` · complexity `L`

**Problem.** Repository SQL, worker pipelines and the database invariants are only verified by hand. A regression in any of them would reach production.

**Business impact.** The invariants protecting candidates from unauthorised submissions and fabricated CVs must be continuously verified, not verified once.

**Technical impact.** Testcontainers for Postgres, Redis, NATS and MinIO, behind the integration build tag, covering repositories, migrations, workers, event processing and the database constraints.

**Proposed solution.** Build the harness and port the manual verifications already performed into permanent tests: every database invariant attempting its violation, the full CV parse pipeline, discovery through matching, and the authorisation gate.

**Acceptance criteria.**

- [ ] Every repository method is covered against real Postgres
- [ ] Every database CHECK and trigger has a test attempting to violate it
- [ ] Migrations are tested up, down and up again with no residue
- [ ] Worker pipelines are tested including crash-and-resume and dead-lettering
- [ ] Tenant isolation is asserted per repository
- [ ] The suite runs in CI on every pull request

**Testing.** This issue is the testing. Each invariant test must be seen to fail when its constraint is removed.

**Depends on** `MJF-080`. **Blocks** `MJF-082`, `MJF-085`.

## MJF-082 — End-to-end journey tests

`high` · `testing` · complexity `L`

**Problem.** No test covers a complete candidate journey, so an integration gap between working components would go unnoticed.

**Business impact.** Components can each pass their tests while the journey is broken.

**Technical impact.** Playwright against a real stack, covering register through to analytics.

**Proposed solution.** Implement the journeys from docs/USER_JOURNEYS.md: register, onboard, upload a CV, parse it, configure a loop, discover from a fixture source, match, tailor, prepare, authorise, submit, update status and view analytics.

**Acceptance criteria.**

- [ ] The full primary journey passes against a real stack
- [ ] The diagnose-no-matches journey passes
- [ ] Accessibility assertions run on every screen visited
- [ ] The suite is stable enough to gate merges, with no flaky retries masking failures

**Testing.** Run 20 consecutive times and require zero flakes before enabling as a gate.

**Depends on** `MJF-081`, `MJF-056`. **Blocks** none.

## MJF-083 — CD workflow with environment promotion

`high` · `devops` · complexity `M`

**Problem.** There is no deployment pipeline, and CI and CD must be separate so untested code cannot reach production.

**Business impact.** An untested production deployment is the most expensive kind of mistake.

**Technical impact.** Image build and publish on a green CI run, a deployment-configuration update, Flux reconciliation, health gating, and documented rollback.

**Proposed solution.** Separate CD from CI. Images are built only from a commit whose CI passed. Staging deploys automatically; production requires a manual approval. Failed health checks block promotion.

**Acceptance criteria.**

- [ ] CD runs only after CI passes on the same commit
- [ ] Images are tagged with the commit SHA, never only latest
- [ ] Staging deploys automatically; production requires approval
- [ ] A failed health check blocks promotion and alerts
- [ ] Rollback is one documented, tested action
- [ ] Failed checks make production deployment impossible, not merely discouraged

**Testing.** A deliberately failing deployment asserting promotion is blocked and rollback succeeds.

**Depends on** `MJF-072`, `MJF-080`. **Blocks** none.

## MJF-084 — Security scanning: dependencies, containers, secrets, SAST

`critical` · `security` · complexity `M`

**Problem.** No automated security scanning exists, so a vulnerable dependency or a committed secret would go unnoticed.

**Business impact.** This platform holds CVs, phone numbers, application history and SMTP credentials. A disclosure would be material.

**Technical impact.** govulncheck, npm audit, Trivy for containers, gitleaks across history, CodeQL, and Dependabot.

**Proposed solution.** Add all scanners to CI as gates. Secret scanning covers the full history, not just the diff, since the repository is public.

**Acceptance criteria.**

- [ ] A dependency vulnerability of high severity or above fails the build
- [ ] A container vulnerability of high severity or above fails the build
- [ ] Secret scanning covers the entire git history
- [ ] SAST runs on every pull request
- [ ] Dependabot is configured for Go, npm and Actions
- [ ] Every finding is triaged, not merely reported

**Testing.** Introduce a known-vulnerable dependency and a fake secret and confirm each is caught.

**Depends on** `MJF-080`. **Blocks** none.

## MJF-085 — Load, concurrency and failure-recovery testing

`medium` · `performance` · complexity `L`

**Problem.** Nothing establishes the platform's capacity or its behaviour under dependency failure.

**Business impact.** Capacity limits discovered in production are discovered by candidates.

**Technical impact.** Load tests for the API and the pipeline, concurrency tests on the authorisation and quota paths, and failure-recovery tests for each dependency.

**Proposed solution.** Build load profiles for the API and for discovery-through-matching. Concurrency tests target the paths where a race would be serious: quota reservation and application authorisation. Failure tests kill each dependency in turn and assert the documented degradation.

**Acceptance criteria.**

- [ ] API p99 latency stays within target at the documented concurrency
- [ ] Matching throughput is measured and documented
- [ ] No double submission occurs under concurrent authorisation attempts
- [ ] No quota over-consumption occurs under concurrent requests
- [ ] Each dependency failure produces the documented degradation, not an outage
- [ ] Workers resume correctly after a restart mid-pipeline
- [ ] Event replay is verified

**Testing.** This issue is the testing. Results are recorded in docs as the capacity baseline.

**Depends on** `MJF-081`. **Blocks** none.

## MJF-086 — OpenAPI specification generated from and verified against the implementation

`high` · `documentation` · complexity `M`

**Problem.** There is no API specification, so clients have nothing to generate from and no contract to rely on.

**Business impact.** The frontend and the extension both need a reliable contract.

**Technical impact.** An OpenAPI 3.1 document covering every endpoint, request and response, verified against the running API so it cannot drift.

**Proposed solution.** Write the specification and add a contract test that exercises it against the live API, so a divergence fails CI. A specification nobody verifies is worse than none, because it is believed.

**Acceptance criteria.**

- [ ] Every endpoint, request schema and response schema is documented
- [ ] Error responses and their codes are documented
- [ ] A contract test verifies the specification against the running API in CI
- [ ] A client generated from the specification works against the real API

**Testing.** The contract test is the verification. It must fail when an endpoint changes without the specification being updated.

**Depends on** `MJF-080`. **Blocks** none.

## MJF-087 — Security documentation: SECURITY.md, THREAT_MODEL.md, SECURITY_CHECKLIST.md

`high` · `documentation` · complexity `M`

**Problem.** The security decisions are embedded in code comments and commit messages rather than stated anywhere a reviewer can read them.

**Business impact.** Required for any serious customer conversation, and for the team to reason about risk consistently.

**Technical impact.** A threat model, a security overview, and a release checklist.

**Proposed solution.** Document the eight security boundaries from docs/ARCHITECTURE.md as a threat model with assets, actors, threats and controls. SECURITY.md covers reporting and the supported versions. The checklist is a release gate.

**Acceptance criteria.**

- [ ] The threat model covers every boundary with assets, threats and controls
- [ ] The upload, prompt-injection, tenant, submission, credential and secret boundaries are each documented
- [ ] The common-password list's limitations are stated, with the recommended production approach
- [ ] SECURITY.md states how to report a vulnerability
- [ ] The checklist is used as a release gate, not written and forgotten

**Testing.** Review against the invariant-auditor agent's findings; any gap it reports that the documents do not cover is a defect in the documents.

**Depends on** none. **Blocks** none.
