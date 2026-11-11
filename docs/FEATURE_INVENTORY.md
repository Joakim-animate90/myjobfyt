# Feature Inventory

Every feature discovered during Phase One reverse engineering, documented against the
mandated template. Each entry carries: purpose, journey, inputs, outputs, data, expected
behaviour, backend/frontend responsibilities, integrations, background processes, failure
scenarios, security considerations, and module dependencies.

**Legend — Tier:** F = free, P = paid, PR = premium, DFY = done-for-you.
**Legend — Mode:** `SYNC` = request-scoped, `ASYNC` = background, `HYBRID` = sync trigger + async completion.

---

## Domain A — Identity & Candidate Management

### A1. Email/password registration
- **Purpose:** Create a candidate account and establish the security principal everything else is scoped by.
- **Journey:** Landing → Sign up → email + password → verification email → verify → onboarding wizard.
- **Inputs:** email, password, accepted-terms flag, optional referral code.
- **Outputs:** `user` row, `candidate_profile` row, email-verification token, access + refresh tokens.
- **Data required:** users, candidate_profiles, email_verification_tokens, audit_logs.
- **Expected behaviour:** Idempotent on re-submit of the same email (no account enumeration — identical response whether or not the email exists). Unverified accounts may sign in but cannot start a Loop or submit an application.
- **Backend:** validate, normalise email (lowercase, trim), argon2id hash, insert in one transaction, emit `candidate.created`, enqueue verification email.
- **Frontend:** accessible form, Zod validation, password strength meter, inline server errors, "check your inbox" state.
- **Integrations:** transactional email provider (SMTP/Postmark).
- **Background:** verification-email send with retry; unverified-account reminder at 24h; purge of unverified accounts at 30d.
- **Failures:** email provider down (queue + retry, account still created); duplicate email (generic success response); weak password (422).
- **Security:** argon2id (64MB, t=3, p=2); per-IP and per-email rate limits; tokens single-use, 24h TTL, hashed at rest; no password in logs; timing-equalised responses.
- **Depends on:** nothing. **Depended on by:** every other domain.
- **Tier:** F · **Mode:** HYBRID

### A2. Session authentication (JWT access + rotating refresh)
- **Purpose:** Authenticate API calls without a server-side session lookup on the hot path.
- **Journey:** Sign in → tokens issued → silent refresh → sign out revokes.
- **Inputs:** email + password, or a refresh token.
- **Outputs:** 15-minute access JWT, 30-day refresh token (httpOnly, Secure, SameSite=Lax cookie).
- **Data required:** users, refresh_tokens (hashed, with family id), audit_logs.
- **Expected behaviour:** Refresh rotates: the presented token is revoked and a new one issued. Reuse of an already-rotated token revokes the entire token family and forces re-authentication (detects theft).
- **Backend:** verify credentials in constant time, sign JWT (RS256), persist refresh hash, middleware validating JWT and loading the principal into context.
- **Frontend:** React Query mutation, automatic refresh on 401 with single-flight dedupe, redirect to sign-in on refresh failure.
- **Integrations:** none.
- **Background:** expired-refresh-token reaper.
- **Failures:** clock skew (60s leeway); revoked family (401 + forced sign-in); Redis unavailable (JWT validation still works — it is stateless by design).
- **Security:** RS256 with rotatable keys served via JWKS; refresh tokens stored hashed; strict cookie flags; brute-force lockout after 10 failures in 15 min.
- **Depends on:** A1. **Depended on by:** all authenticated features.
- **Tier:** F · **Mode:** SYNC

### A3. OAuth sign-in (Google)
- **Purpose:** Reduce registration friction; obtain a verified email without a verification round-trip.
- **Journey:** "Continue with Google" → consent → callback → account created or linked → onboarding or dashboard.
- **Inputs:** OAuth authorisation code, PKCE verifier, state.
- **Outputs:** linked `oauth_identities` row, session tokens.
- **Data required:** users, oauth_identities, audit_logs.
- **Expected behaviour:** Links to an existing account only when the provider asserts the email is verified and it matches an existing verified account; otherwise creates a new account. Never silently merges on an unverified claim.
- **Backend:** PKCE + state validation (state in Redis, 10-min TTL, single use), provider token exchange, identity upsert.
- **Frontend:** provider button, loading state, error surface for denied consent.
- **Integrations:** Google OIDC.
- **Background:** none.
- **Failures:** provider outage (fall back to password); mismatched state (reject, audit); email already taken by a password account (prompt to sign in and link).
- **Security:** PKCE mandatory; state single-use; redirect URI allowlist; provider ID tokens verified against JWKS; no provider access tokens persisted beyond what is needed.
- **Depends on:** A1, A2. **Tier:** F · **Mode:** SYNC

### A4. Candidate profile
- **Purpose:** Hold the identity and contact facts used to fill application forms and personalise outreach.
- **Journey:** Onboarding step 1, then editable in Settings.
- **Inputs:** full name, phone, city, country, work-authorisation notes, LinkedIn/portfolio URLs, languages, salary expectation, notice period.
- **Outputs:** completed `candidate_profiles` row; profile-completeness percentage.
- **Data required:** candidate_profiles, candidate_languages.
- **Expected behaviour:** Phone and city are required before form auto-fill can be enabled, because the knowledge base identifies them as the required additional form fields. The UI enforces this as a precondition rather than failing later in the pipeline.
- **Backend:** validation (E.164 phone, ISO country), completeness computation, emit `candidate.profile_updated`.
- **Frontend:** sectioned form, autosave per section, completeness ring, deep links from blocked features.
- **Integrations:** none. **Background:** none.
- **Failures:** partial save (per-section transactions); invalid phone (422 with field path).
- **Security:** PII — encrypted at rest at column level for phone; redacted in logs; export and deletion via A7.
- **Depends on:** A1. **Depended on by:** C (job prefs), F (applications), G (outreach).
- **Tier:** F · **Mode:** SYNC

### A5. Role-based access control
- **Purpose:** Separate candidate, support, and admin capability.
- **Inputs:** authenticated principal, requested resource. **Outputs:** allow/deny + audit entry.
- **Expected behaviour:** Default deny. Roles: `candidate`, `support` (read-only, PII-masked), `admin`, `system`. Ownership checks are enforced in the repository layer, not only in middleware, so a missing handler check cannot leak data.
- **Backend:** policy middleware + mandatory `user_id` scoping in every candidate-data query; tests assert cross-tenant reads return zero rows.
- **Security:** privilege escalation is the primary threat; role changes are admin-only, audited, and never self-service.
- **Depends on:** A2. **Tier:** F · **Mode:** SYNC

### A6. Entitlements & quotas
- **Purpose:** Enforce plan limits without scattering tier logic through the codebase.
- **Inputs:** user's subscription, quota key, requested amount. **Outputs:** allow/deny + remaining allowance.
- **Data required:** subscriptions, plans, quota_usage (Redis counters + daily Postgres rollup).
- **Expected behaviour:** Quotas are named keys resolved from config: `daily_applications`, `active_loops`, `cv_slots`, `ai_tokens_month`, plus boolean features (`worldwide_remote`, `smtp_custom`, `cv_tailoring`, `ai_question_answering`). Counters reset on a UTC day boundary. Exceeding a quota degrades gracefully: matches still accumulate, submission queues rather than fails.
- **Backend:** single `entitlements.Check(ctx, userID, key, n)`; Redis `INCR` with expiry for the hot path, reconciled nightly against Postgres.
- **Frontend:** remaining-allowance indicators, upgrade prompts at the point of friction, never a dead-end.
- **Failures:** Redis loss (fail open to the Postgres rollup with a conservative cap, alert).
- **Security:** quota checks are server-side only; never trust a client-supplied tier.
- **Depends on:** A2, I3. **Depended on by:** C, D, E, F.
- **Tier:** all · **Mode:** SYNC

### A7. Data export & account deletion (GDPR)
- **Purpose:** Legal obligation and trust.
- **Journey:** Settings → Privacy → Export my data / Delete my account → confirm by typing email → 7-day grace → irreversible erase.
- **Outputs:** signed, expiring download of a ZIP (JSON + original CV files); or full erasure.
- **Expected behaviour:** Export covers every table holding the candidate's data. Deletion erases PII and CV objects, and anonymises rows that must be retained for aggregate integrity (billing records, de-identified funnel counters). A 7-day grace window allows recovery; after it, erasure is irreversible.
- **Background:** export assembly job; scheduled erasure job; object-storage purge with verification.
- **Failures:** partial erasure (job is resumable and idempotent; completion is asserted by a verification pass that fails loudly).
- **Security:** export links are single-use, 1-hour TTL, bound to the requesting session; deletion requires re-authentication.
- **Depends on:** every domain. **Tier:** F (deletion for all tiers; LoopCV restricting deletion to its basic tier is a defect we do not copy) · **Mode:** ASYNC

---

## Domain B — CV Intelligence

### B1. CV upload
- **Purpose:** Get the candidate's source document into the system safely.
- **Journey:** Onboarding step 2 or CV Library → drag/drop → validation → upload → parse status.
- **Inputs:** PDF or DOCX, ≤ 10 MB, 1 file per request.
- **Outputs:** `cv_documents` row, object-storage key, `cv.uploaded` event.
- **Data required:** cv_documents, object storage.
- **Expected behaviour:** Content type is verified by magic bytes, not by extension or client-supplied MIME. Upload returns immediately with a `parsing` status; parsing is asynchronous.
- **Backend:** presigned-PUT issuance or streamed multipart, magic-byte sniffing, size cap enforced during streaming, antivirus scan, per-user key namespace.
- **Frontend:** drag-and-drop with progress, file-type and size feedback before upload, live parse status via polling.
- **Integrations:** S3-compatible object storage (MinIO locally), ClamAV.
- **Background:** AV scan; parse job; orphaned-object reaper for uploads never committed.
- **Failures:** storage unavailable (503, no DB row); AV positive (quarantine, notify, delete); oversized (413 during stream, not after).
- **Security:** **This is the highest-risk input in the product.** No execution of uploaded content; keys are opaque UUIDs under `tenants/{user_id}/`; downloads are presigned and short-lived; never served from the app origin; parse workers run with no network egress except the AI provider.
- **Depends on:** A1, A4. **Depended on by:** all of B, D, E, F.
- **Tier:** F (slots by tier) · **Mode:** HYBRID

### B2. CV parsing & structured extraction
- **Purpose:** Convert an unstructured document into queryable, matchable structure.
- **Inputs:** stored CV object. **Outputs:** `cv_parsed_data` (contact, summary, experience[], education[], skills[], certifications[], languages[]), extracted raw text, `cv.parsed` event.
- **Expected behaviour:** Deterministic text extraction first (pdftotext/docx XML), then a schema-constrained LLM pass for structure. Every extracted field carries a confidence score and a character offset into the source text, so the UI can show provenance and the candidate can correct it. Low-confidence fields are surfaced for review rather than silently trusted.
- **Backend:** text extraction, chunking, structured-output LLM call with a strict JSON schema, validation, persistence, skill normalisation against a canonical taxonomy.
- **Frontend:** parsed-CV review screen with field-level edit and a "looks wrong?" correction path. Candidate corrections are authoritative over AI output, permanently.
- **Integrations:** AI provider.
- **Background:** parse consumer with retry/backoff, a dead-letter queue, and a 120s timeout per attempt.
- **Failures:** scanned image-only PDF (detect zero extractable text → ask for a text PDF, do not silently produce an empty profile); encrypted PDF (reject with a clear message); LLM schema violation (retry twice with repair prompt, then dead-letter and notify); partial extraction (persist what validated, flag the rest).
- **Security:** **Prompt-injection boundary.** CV text is untrusted: it is passed as data in a delimited user block, never concatenated into instructions; output is schema-validated before use. CV text is never logged.
- **Depends on:** B1. **Depended on by:** D, E, F.
- **Tier:** F · **Mode:** ASYNC

### B3. CV library (multiple CVs, versions, A/B testing)
- **Purpose:** Let candidates keep several CVs and learn which performs best.
- **Inputs:** CV set, per-CV application outcomes. **Outputs:** library list with per-CV funnel stats.
- **Expected behaviour:** Each CV is independently versioned; a Loop references a specific CV version so historical applications remain reproducible. Per-CV analytics report applications sent, opens, replies, and interview rate with sample sizes — and explicitly withhold a verdict below a minimum sample, instead of presenting noise as insight.
- **Backend:** version chain, soft delete (a CV referenced by a submitted application is never hard-deleted), stats aggregation.
- **Frontend:** library grid, default-CV selection, per-CV stats card with confidence caveat.
- **Depends on:** B1, B2, H. **Tier:** F (slots by tier) · **Mode:** SYNC

### B4. AI CV checker (ATS analysis)
- **Purpose:** Tell the candidate concretely how to improve the CV.
- **Inputs:** parsed CV, optional target role. **Outputs:** overall score, per-category sub-scores (formatting, keyword coverage, impact/quantification, length, contactability, ATS-parseability), missing-keyword list, ranked prioritised recommendations.
- **Expected behaviour:** Every recommendation is specific and actionable, anchored to a concrete location in the CV. No generic advice. Scores are computed from explicit, documented rubrics so the same CV always scores the same.
- **Backend:** deterministic rule checks (length, sections present, contact detectable, date-format consistency, bullet density) combined with an LLM pass for impact and keyword analysis; both feed one scoring function.
- **Frontend:** score dashboard, per-category breakdown, prioritised recommendation list, before/after re-check.
- **Background:** analysis job, cached by CV version hash + target role.
- **Failures:** AI unavailable (return the deterministic subset with an explicit note on what is missing, never a fabricated score).
- **Security:** injection boundary as B2; results cached per user, never shared.
- **Depends on:** B2. **Tier:** F (limited runs) / P · **Mode:** ASYNC

### B5. CV builder
- **Purpose:** Create or restructure a CV inside the product.
- **Journey:** CV Builder → pick template → edit content → live preview → finalise → added to library.
- **Inputs:** structured CV content, template choice, theme options. **Outputs:** rendered PDF, new `cv_documents` entry.
- **Expected behaviour:** Content editor and designer are separate concerns; the live preview is the same renderer used for the final PDF, so what is previewed is what is produced. Output must be ATS-parseable: real text, single column by default, no text in images.
- **Backend:** server-side PDF rendering (HTML→PDF in a sandboxed, network-isolated renderer), template registry, deterministic output for a given input.
- **Frontend:** split-pane editor with live preview, section reordering, undo, autosave.
- **Failures:** renderer timeout (retry once, then return a clear failure — never a corrupt PDF); font missing (bundled fonts only).
- **Security:** user content is rendered in a sandboxed process with no network and no filesystem access; a strict template allowlist prevents SSRF and local-file inclusion through the renderer.
- **Depends on:** B1. **Tier:** F · **Mode:** HYBRID

### B6. CV tailoring (per-job)
- **Purpose:** Raise relevance for a specific posting without fabricating anything.
- **Journey:** A match → tailor icon → review diff → accept → finalise into library or attach to this application.
- **Inputs:** base CV version, job posting. **Outputs:** tailored CV version with a recorded diff and the posting it was tailored for.
- **Expected behaviour:** **Reorder, re-emphasise, and re-word existing content only.** A post-generation validator diffs the tailored content against the source and rejects the output if it introduces any employer, title, date, certification, or skill not present in the source. The candidate always sees the diff before it is used.
- **Backend:** tailoring prompt constrained to the parsed CV as the sole factual source; mandatory factual-containment validation; versioned result linked to base version and job.
- **Frontend:** side-by-side diff with additions/removals highlighted, explicit accept step.
- **Integrations:** AI provider. **Background:** tailoring job.
- **Failures:** containment validation fails (do not surface the output; retry once with a stricter prompt, then report that tailoring could not be completed safely).
- **Security:** job descriptions are untrusted (injection boundary); the containment validator is the last line of defence against both hallucination and injection-driven content insertion.
- **Depends on:** B2, D. **Tier:** P (available to automated and extension channels, unlike LoopCV) · **Mode:** ASYNC

---

## Domain C — Job Search Configuration (Loops)

### C1. Loop creation wizard
- **Purpose:** Capture everything an automated search cycle needs.
- **Journey:** Four steps — Search info → Email template → Settings → Review → Start.
- **Inputs:**
  - *Step 1:* job titles[], locations[], remote-only, worldwide-remote (paid), hybrid-only, source selection, career-page search, seniority (`Junior|Associate|Senior|Executive|All`), employment type (`Full-time|Contract|Part-time|Temporary|Internship`), CV selection.
  - *Step 2:* template (predefined or custom) with `{{job_title}}`, `{{company}}`, `{{candidate_name}}` placeholders, test send.
  - *Step 3:* automation toggles (auto-email, auto-form, AI answering, extension auto-apply), form fields (phone, city), cover-letter mode, required keywords[], excluded keywords[], excluded companies[], match threshold.
  - *Step 4:* review and confirm.
- **Outputs:** `loops` row plus child config rows; `loop.activated` event; first discovery run scheduled.
- **Expected behaviour:** The wizard is resumable — a partially completed Loop persists as `draft`. Preconditions are checked at the step where they apply, not at submit (e.g. auto-form requires phone + city; worldwide remote requires a paid plan). Activation is explicit.
- **Backend:** per-step validation, draft persistence, entitlement checks, template placeholder validation, schedule registration.
- **Frontend:** stepper with per-step validation, draft resume, live preview of the email template with real substituted values, review summary.
- **Failures:** entitlement exceeded (block at the relevant step with a clear upgrade path); invalid placeholder (reject with the list of valid placeholders).
- **Security:** template bodies are escaped on render; no arbitrary expressions in templates — a fixed placeholder allowlist only.
- **Depends on:** A4, A6, B1. **Depended on by:** D, E, F.
- **Tier:** F (count by tier) · **Mode:** SYNC

### C2. Loop lifecycle (activate / pause / edit / duplicate / archive)
- **Purpose:** Give the candidate real control over automation.
- **Expected behaviour:** States are `draft → active ⇄ paused → archived`. **Pause is a first-class operation** (LoopCV's omission of it is a defect): pausing stops discovery and halts all pending submissions for that Loop within seconds, without deleting history. Editing an active Loop takes effect from the next discovery run and is recorded as a config version, so a historical match can be explained by the config in force when it was created. Duplicating copies configuration but no history.
- **Backend:** state machine with guarded transitions; config versioning; pause propagates by setting a Redis kill-flag the submission workers check immediately before each send.
- **Frontend:** prominent pause control, state badge, edit-takes-effect-next-run notice, confirm on archive.
- **Failures:** pause racing an in-flight submission (the pre-send flag check makes the race window sub-second; any submission that completes after a pause is recorded with `authorised_before_pause` for a full audit trail).
- **Depends on:** C1. **Tier:** F · **Mode:** SYNC

### C3. Email template management
- **Purpose:** Personalised recruiter outreach at scale.
- **Inputs:** subject, body, placeholders, attachment policy. **Outputs:** stored template, rendered test send.
- **Expected behaviour:** Test send to the candidate's own verified address only. Rendering is pure substitution over an allowlist; unknown placeholders are a validation error rather than being silently emitted as literals.
- **Security:** no HTML injection — bodies are rendered as text and sanitised HTML; SPF/DKIM alignment handled in G.
- **Depends on:** C1. **Tier:** F (custom templates), P (SMTP) · **Mode:** SYNC

### C4. Filters — required/excluded keywords, excluded companies, match threshold
- **Purpose:** Precision control over what is matched.
- **Expected behaviour:** Required keywords are OR-semantics — a posting must contain **at least one** (matching the documented LoopCV behaviour). Excluded keywords are any-match exclusion. Company exclusion matches on a normalised company identity, not a raw string, so "Acme Inc." and "Acme, Inc" both exclude. Match threshold replaces LoopCV's opaque Low/Middle/High with a numeric 0–100 threshold **plus** the three named presets mapped onto it, so the control is both familiar and inspectable.
- **Backend:** filters are applied in the matching stage and the reason for each rejection is persisted.
- **Frontend:** chip inputs, live "this would have rejected N of your last 100 matches" preview.
- **Depends on:** C1, E. **Tier:** F · **Mode:** SYNC

---

## Domain D — Job Acquisition

### D1. Source adapters
- **Purpose:** One uniform interface over many heterogeneous job sources.
- **Inputs:** per-source credentials/config, a search specification. **Outputs:** raw postings in a canonical shape, with source attribution.
- **Expected behaviour:** Every adapter implements the same interface (`Search`, `Fetch`, `HealthCheck`, `Capabilities`) and declares its capabilities so the scheduler can skip unsupported queries rather than failing them. Each adapter enforces its own rate limit and honours the source's terms. A failing adapter degrades the run; it never fails it.
- **Integrations:** see `INTEGRATION_ANALYSIS.md` — official APIs (Adzuna, Jooble, USAJobs, Reed, Careerjet), ATS APIs (Greenhouse, Lever, Workable, Ashby, SmartRecruiters), and authorised feeds only. **No credential-gated or anti-bot-protected source is scraped.**
- **Background:** scheduled runs per source per Loop; circuit breaker per source; exponential backoff with jitter.
- **Failures:** rate limited (429 → backoff, resume); auth failure (disable adapter, alert admin); schema drift (validation failure quarantines the payload for inspection rather than corrupting the dataset).
- **Security:** source credentials in a secret manager, never in Git; all adapter output is untrusted input; strict response-size caps.
- **Depends on:** C1. **Depended on by:** D2, D3, E.
- **Tier:** all · **Mode:** ASYNC

### D2. Scheduled discovery
- **Purpose:** Keep matches flowing daily without duplicating work across candidates.
- **Expected behaviour:** Discovery is keyed by *search specification*, not by candidate, so N candidates searching "Go developer, Berlin, remote" cost one upstream query. Runs are distributed across the day to smooth load and respect upstream limits. Each run is a tracked job with a start, end, counts, and errors — visible to the admin and summarised to the candidate.
- **Backend:** cron-driven planner → per-source fetch jobs → `job.discovered` events; distributed lock per spec+source to prevent duplicate concurrent runs.
- **Failures:** worker crash mid-run (job is resumable from its cursor; at-least-once delivery with idempotent downstream handling).
- **Depends on:** D1. **Tier:** all · **Mode:** ASYNC

### D3. Normalisation & deduplication
- **Purpose:** One canonical posting per real-world job, regardless of how many boards carry it.
- **Inputs:** raw postings. **Outputs:** `job_postings` rows with a content fingerprint and all contributing source attributions.
- **Expected behaviour:** Normalise company identity, location (to a geocoded structure), employment type, seniority, salary (to a currency + period + range), and work mode. Deduplicate on a composite fingerprint (normalised company + normalised title + location + a shingled description hash) with a similarity threshold; near-duplicates are linked rather than discarded, so the candidate can see "also on Indeed, LinkedIn".
- **Background:** normalisation consumer; nightly liveness re-check driving `Expired`; change detection raising `job.updated` when a posting's description or salary changes.
- **Failures:** ambiguous dedupe (keep both, link as candidates, flag for review — never silently merge two distinct jobs); geocoder down (persist the raw location string, backfill later).
- **Security:** description text is untrusted and is stored separately from any field used in a prompt instruction position.
- **Depends on:** D1, D2. **Depended on by:** E. **Tier:** all · **Mode:** ASYNC

### D4. Career-page / ATS discovery
- **Purpose:** Reach postings that never appear on aggregators.
- **Expected behaviour:** Company career pages are discovered through their ATS provider's public API (Greenhouse/Lever/Workable/Ashby board endpoints) where one exists. Generic HTML crawling is limited to pages whose `robots.txt` permits it, with a published user agent, conservative rate limits, and no circumvention of any access control.
- **Security/legal:** explicit allowlist of ATS providers with public board APIs; `robots.txt` respected; no CAPTCHA solving; no authenticated crawling.
- **Depends on:** D1. **Tier:** P · **Mode:** ASYNC

---

## Domain E — Job Matching

### E1. Match scoring
- **Purpose:** Decide, and explain, whether a posting fits a candidate.
- **Inputs:** parsed CV, loop configuration, normalised posting. **Outputs:** `job_matches` row with overall score, per-dimension sub-scores, and human-readable reasons.
- **Expected behaviour:** Dimensions are scored independently and combined with documented weights: title similarity, skill overlap (required vs. nice-to-have), years-of-experience fit, seniority fit, location/work-mode fit, employment-type fit, language, and keyword rules. **Every score is explainable** — the match detail view shows each sub-score and the reason for it. Hard filters (location, work mode, language, excluded company, excluded keyword, missing required keyword) reject before scoring and record the category.
- **Backend:** deterministic rule layer for hard filters and structured dimensions; embedding similarity for semantic title/skill matching; weights in config, versioned so historical scores stay interpretable.
- **Frontend:** match card with score ring and top reasons; detail view with the full breakdown; rejected-matches browser filtered by category, with apply-anyway available.
- **Background:** matching consumer, fan-out per active Loop on `job.normalised`, plus backfill when a Loop's config or CV changes.
- **Failures:** embedding provider down (fall back to lexical similarity, mark the match as `degraded_scoring` so it can be rescored later, never drop it).
- **Security:** scoring never exposes another candidate's data; the explanation text is generated from structured sub-scores, not free-form LLM output, so it cannot leak prompt content.
- **Depends on:** B2, C1, D3. **Depended on by:** F. **Tier:** all · **Mode:** ASYNC

### E2. Per-candidate company cooldown
- **Purpose:** Avoid embarrassing the candidate with repeat applications.
- **Expected behaviour:** Reproduces the observed six-month, per-candidate, per-company suppression window, as a configurable policy. Suppressed matches are surfaced with `Already Applied` / `Email Already Sent` and the date of the prior contact, so the candidate understands why.
- **Depends on:** E1, F. **Tier:** all · **Mode:** ASYNC

### E3. Match review & bulk actions
- **Purpose:** Candidate control over the funnel.
- **Expected behaviour:** Sortable, filterable match list with server-side pagination; bulk select to apply, dismiss, or save; a Kanban board view over application status. Dismissals feed back as a negative signal into future scoring for that Loop.
- **Frontend:** virtualised table, saved filters, optimistic bulk actions with rollback on failure, keyboard navigation.
- **Depends on:** E1. **Tier:** F · **Mode:** SYNC

---

## Domain F — Application Orchestration

### F1. Method selection
- **Purpose:** Choose the right submission channel per posting.
- **Inputs:** posting (source, employer email presence, form availability, auth requirement), loop automation settings, entitlements. **Outputs:** chosen method and the reason it was chosen.
- **Expected behaviour:** Deterministic, ordered, and recorded: ATS API (if an authorised integration exists and the posting came from it) → public form auto-fill (if enabled and a form exists) → authorised email (if enabled and an employer email was found) → extension-assisted (if the platform requires the candidate's session) → manual (hand over the link). The chosen method **and the reason** are persisted on the application so the candidate and the admin can both see why.
- **Depends on:** D3, E1, C1. **Tier:** all · **Mode:** ASYNC

### F2. Application preparation
- **Purpose:** Assemble exactly what will be sent, before anything is sent.
- **Outputs:** `applications` row in `prepared` state holding the resolved CV version, rendered cover letter, rendered email body, mapped form-field values, and resolved screening answers.
- **Expected behaviour:** Preparation is fully materialised and immutable once authorised — the artefacts stored are byte-for-byte what gets submitted. This is what makes the audit trail meaningful. Preparation never sends.
- **Failures:** missing required field (application moves to `blocked` with a precise reason and a deep link to fix it, rather than failing at submit time).
- **Depends on:** B6, F1, F4. **Tier:** all · **Mode:** ASYNC

### F3. Authorisation & submission
- **Purpose:** Submit, with provable candidate consent.
- **Expected behaviour:** **No application is submitted without a recorded authorisation.** Two forms: per-application approval, or a standing authorisation scoped to one Loop, with a daily cap, an expiry, and the exact settings captured at the time it was granted. Every submission stores the authorisation reference. Submission is a durable job with a visible `submitting` state, bounded retries with backoff, and idempotency keyed on (application, method) so a redelivery cannot double-submit.
- **Backend:** authorisation records, pre-send kill-flag check (for C2 pause), per-domain rate limiting, submission adapters per method.
- **Frontend:** explicit approval UI showing the actual artefacts; standing-authorisation setup with cap, expiry, and a plain-language summary of what it permits; a visible revoke control.
- **Failures:** transient error (retry with backoff, cap attempts, then `failed` with a retry affordance); ambiguous outcome — request sent but response lost — (record `outcome_unknown` rather than guessing; never silently retry a possibly-successful submission).
- **Security:** authorisation is the integrity boundary of the entire product. Authorisation records are immutable and audited. Submission workers cannot act without a valid, unexpired, uncapped authorisation.
- **Depends on:** F2, A6. **Tier:** all · **Mode:** ASYNC

### F4. Screening questions & saved answers
- **Purpose:** Get past question-gated forms without fabricating answers.
- **Expected behaviour:** Questions encountered during preparation are collected into a Questions queue. Candidates answer manually (all tiers); AI answering (PR/DFY) drafts answers from the CV, profile, and previously saved answers — and **never invents a factual claim**: anything not supported by profile data is escalated to the candidate instead of guessed. Answers are saved and reused, normalised by question similarity. Saved answers are viewable and editable.
- **Failures:** unanswerable question (application moves to `questions_pending`, candidate notified, nothing is submitted).
- **Security:** injection boundary — question text comes from third-party forms; AI answers are schema-validated and length-capped; sensitive categories (disability, ethnicity, visa, salary history) are **never** auto-answered and always require the candidate.
- **Depends on:** B2, A4. **Tier:** F (manual) / PR (AI) · **Mode:** HYBRID

### F5. Application tracking & status sync
- **Purpose:** One place showing the true state of every application.
- **Outputs:** status timeline per application, built from an immutable `application_events` log.
- **Expected behaviour:** Statuses mirror the observed vocabulary plus explicit internal states: `prepared`, `blocked`, `questions_pending`, `awaiting_authorisation`, `ready`, `submitting`, `submitted`, `email_sent`, `applied_externally`, `extension_pending`, `suppressed_cooldown`, `external_required`, `failed`, `outcome_unknown`, `expired`, `interview`, `offer`, `rejected`. Status is derived from events, never mutated in place, so the history can always be reconstructed.
- **Backend:** append-only event log; ATS status polling where an integration supports it; email-reply detection for the email channel.
- **Frontend:** list, Kanban board, per-application timeline with the artefacts that were actually sent.
- **Depends on:** F3. **Tier:** F · **Mode:** HYBRID

### F6. Browser-extension assisted apply
- **Purpose:** Handle authenticated boards legitimately.
- **Expected behaviour:** The extension works in the candidate's own browser under the candidate's own session. The backend provides the queue of extension-eligible applications with prepared field values; the extension fills and the candidate submits. The server never receives or stores the candidate's job-board credentials or session cookies. No CAPTCHA is solved and no anti-bot measure is circumvented; if a platform presents either, control is handed to the candidate.
- **Security:** extension authenticates to our API with a short-lived, narrowly-scoped token; a strict origin allowlist; no credential relay of any kind.
- **Depends on:** F2. **Tier:** F · **Mode:** HYBRID

---

## Domain G — Recruiter Outreach

### G1. Recruiter contact discovery
- **Purpose:** Enable the email channel.
- **Expected behaviour:** Contacts come only from lawful sources: the posting itself, the company's published careers/contact address, and licensed enrichment providers where a lawful basis exists. **No email-pattern guessing and no scraping of personal addresses.** Every contact carries its provenance and a verification state; unverified addresses are not used for automated sending.
- **Security/legal:** GDPR lawful basis recorded per contact; suppression list honoured globally; unsubscribe handling; provenance is auditable.
- **Depends on:** D3. **Tier:** P · **Mode:** ASYNC

### G2. Outreach sending (platform or candidate SMTP)
- **Purpose:** Deliver applications and follow-ups by email.
- **Expected behaviour:** Default sending is from the platform's authenticated domain with the candidate's address in Reply-To. Paid tiers may configure their own SMTP (Gmail/Yahoo app passwords, as observed), which improves deliverability and places the mail in the candidate's own sent box. Per-recipient-domain rate limits, suppression-list checks, and bounce/complaint handling are mandatory before any send.
- **Failures:** SMTP auth failure (disable, notify the candidate, fall back to platform sending only with consent); hard bounce (suppress permanently); spam complaint (suppress and alert).
- **Security:** SMTP credentials encrypted with envelope encryption, write-only through the API, never returned to the client, never logged. SPF/DKIM/DMARC configured for the platform domain.
- **Depends on:** C3, G1. **Tier:** P (custom SMTP) · **Mode:** ASYNC

### G3. Open/reply tracking & inbox
- **Purpose:** Measure outreach and surface responses.
- **Expected behaviour:** Opens via a tracking pixel the candidate can disable (and which is disclosed); replies via a per-application reply address or IMAP connection. The Inbox threads replies against their application. Open tracking is explicitly acknowledged as unreliable (image blocking) in the UI rather than presented as fact.
- **Security:** reply ingestion is untrusted input — attachments are scanned, HTML sanitised, and no inbound content is ever placed in a prompt instruction position.
- **Depends on:** G2. **Tier:** P · **Mode:** ASYNC

---

## Domain H — Analytics

### H1. Candidate funnel analytics
- **Purpose:** Show the candidate what is working.
- **Outputs:** postings scanned → matched → prepared → submitted → opened → replied → interview → offer, with per-Loop and per-CV breakdowns, time series, and rejection-reason distribution.
- **Expected behaviour:** Computed from the immutable event log, so the numbers always reconcile with the per-application history. Small samples are labelled as such; no metric is presented as significant when it is not. A/B comparison between CVs reports the difference *and* whether the sample supports a conclusion.
- **Backend:** nightly rollups into summary tables plus live counters for today; all queries scoped by `user_id`.
- **Depends on:** F5. **Tier:** F (basic) / P (full) · **Mode:** SYNC

### H2. CV performance comparison (A/B)
- **Purpose:** Answer "which CV performs best?" honestly.
- **Expected behaviour:** Compares CVs on reply and interview rate with confidence intervals and an explicit minimum-sample gate. Below the gate, the UI says there is not enough data instead of showing a winner.
- **Depends on:** B3, H1. **Tier:** P · **Mode:** SYNC

---

## Domain I — Administration & Platform

### I1. Admin dashboard
- **Purpose:** Operate the platform.
- **Outputs:** registered/active users, CV uploads and parse outcomes, discovery runs per source, matching throughput, AI requests and token spend, worker execution, JetStream health, queue depth and lag, submissions and failures, API error rates, DB and Redis health, infrastructure and resource usage.
- **Expected behaviour:** Every number links through to the underlying records. PII is masked by default and unmasking is a separately-audited action.
- **Depends on:** all. **Tier:** admin · **Mode:** SYNC

### I2. End-to-end workflow tracing
- **Purpose:** Let an admin follow one candidate action through every stage.
- **Expected behaviour:** Given a correlation ID (or a CV, match, or application ID), reconstruct the full chain — request → validation → object storage → event publish → consumer execution → AI call → persistence → outcome — with timestamps, durations, status, and error details at every hop. Backed by OpenTelemetry traces correlated with the application event log and structured logs in Loki.
- **Security:** traces and logs must never contain CV contents, passwords, tokens, or AI prompt bodies containing personal data. This is enforced by a redaction layer in the logging package and asserted by tests.
- **Depends on:** I1. **Tier:** admin · **Mode:** SYNC

### I3. Subscription & billing
- **Purpose:** Monetise and enforce entitlements.
- **Expected behaviour:** Checkout, upgrade, downgrade, cancellation (effective at period end, as observed), refunds per policy. Webhooks are the source of truth for subscription state, verified by signature, idempotent on event ID, and tolerant of out-of-order delivery.
- **Failures:** webhook lost (nightly reconciliation against the provider); payment failed (grace period, then downgrade, never silent data loss).
- **Security:** no card data touches our systems; webhook signature verification mandatory.
- **Depends on:** A6. **Tier:** all · **Mode:** HYBRID

### I4. Notifications
- **Purpose:** Tell candidates what happened without being noisy.
- **Expected behaviour:** In-app notification centre plus digest email. Events: CV parsed, questions pending, authorisation needed, application submitted/failed, reply received, quota nearly exhausted, Loop paused by the system. Per-category preferences; digests are batched rather than per-event.
- **Depends on:** F5, G3. **Tier:** F · **Mode:** ASYNC

### I5. Audit log
- **Purpose:** Accountability.
- **Expected behaviour:** Append-only record of every security- and consent-relevant action: authentication, role change, authorisation granted/revoked, submission, SMTP change, data export, deletion, admin PII access. Immutable, retained per policy, queryable by admin.
- **Depends on:** A5. **Tier:** admin · **Mode:** SYNC

---

## Deliberately excluded

| Feature | Reason |
|---|---|
| Server-side automation of authenticated job boards | Requires circumventing access controls; handled client-side via F6 instead |
| CAPTCHA solving / anti-bot evasion | Prohibited |
| Email-pattern guessing for recruiter addresses | No lawful basis; damages deliverability and candidate reputation |
| Fully unattended applying with no recorded consent | Violates the product's integrity boundary (F3) |
| AI mock interview, career coach | Observed in LoopCV's marketing but adjacent to the core loop; deferred, not part of the initial product |
