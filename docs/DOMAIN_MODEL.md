# Domain Model

The authoritative entity model. `DATABASE_DESIGN.md` carries the physical schema, indexes,
and retention rules; this document defines the domains, aggregates, invariants, and the
relationships between them.

## Bounded contexts

| Context | Aggregate roots | Owns | Never writes |
|---|---|---|---|
| **identity** | `User` | users, profiles, oauth identities, sessions, roles | anything outside identity |
| **cv** | `CVDocument` | documents, versions, parsed data, skills, analyses | job or application tables |
| **jobs** | `JobPosting` | sources, raw payloads, postings, attributions | candidate tables |
| **matching** | `JobMatch` | matches, rejections, score breakdowns | postings, CVs |
| **applications** | `Application` | applications, authorisations, events, questions, answers | postings, CVs |
| **outreach** | `OutreachMessage` | contacts, messages, delivery events, suppressions | applications (reads only) |
| **billing** | `Subscription` | plans, subscriptions, invoices, quota usage | — |
| **platform** | — | audit logs, notifications, background jobs, processed messages | — |

A `JobPosting` is **global and shared**; a `JobMatch` is the **per-candidate, per-loop**
evaluation of a posting. Collapsing these two — as LoopCV's public API appears to, using
`jobId` as the match identifier — would make shared posting storage and cross-candidate
deduplication impossible. Keeping them separate is the single most important modelling
decision in the system.

---

## identity

```
User
  id uuid pk · email citext unique · password_hash (nullable: OAuth-only accounts)
  email_verified_at · status (active|suspended|pending_deletion|deleted)
  role (candidate|support|admin|system) · created_at · updated_at · deleted_at
  ├─1:1─ CandidateProfile
  │        full_name · phone (encrypted) · city · country (ISO-3166-1 alpha-2)
  │        timezone · linkedin_url · portfolio_url · work_authorisation
  │        salary_expectation_{min,max,currency,period} · notice_period_days
  │        open_to_relocation · completeness_pct (derived)
  ├─1:N─ CandidateLanguage   (language, proficiency)
  ├─1:N─ OAuthIdentity       (provider, provider_user_id, email, linked_at)  unique(provider, provider_user_id)
  ├─1:N─ RefreshToken        (token_hash, family_id, expires_at, revoked_at, user_agent, ip)
  └─1:N─ EmailVerificationToken (token_hash, purpose, expires_at, consumed_at)
```

**Invariants.** Email is unique, case-insensitive. At least one credential must exist
(`password_hash` or an `OAuthIdentity`). A refresh token is single-use: presenting a rotated
token revokes the whole `family_id`. `pending_deletion` halts all automation immediately while
keeping sign-in available during the grace period.

## cv

```
CVDocument
  id · user_id fk · source (uploaded|builder) · original_filename · content_type
  size_bytes · storage_key · checksum_sha256 · av_status (pending|clean|infected)
  is_default · created_at · deleted_at (soft)
  └─1:N─ CVVersion
           version_no · storage_key · raw_text (nullable) · page_count
           parse_status (pending|parsing|parsed|failed) · parse_error
           derived_from_version_id (self fk — tailoring lineage)
           tailored_for_job_posting_id (nullable fk)
           ├─1:1─ CVParsedData   (summary, contact jsonb, experience jsonb[], education jsonb[],
           │                      certifications jsonb[], confidence jsonb, corrected_fields[])
           ├─1:N─ CVSkill        (skill_id fk→Skill, raw_label, years, confidence, source)
           ├─1:N─ CVAnalysis     (target_role, overall_score, subscores jsonb,
           │                      missing_keywords[], recommendations jsonb[], engine_version)
           └─1:N─ CVEmbedding    (kind: title|skills|summary, vector, model, dims)

Skill  (canonical taxonomy)
  id · canonical_name unique · category · aliases[]
```

**Invariants.** A `CVVersion` referenced by a submitted `Application` is never hard-deleted —
this is what makes the audit trail real. Candidate corrections in `CVParsedData.corrected_fields`
are permanently authoritative over AI output and are never overwritten by a re-parse. A tailored
version must record both `derived_from_version_id` and `tailored_for_job_posting_id`, and must
pass factual containment against its source before it can be used.

## jobs

```
JobSource
  id · slug unique · name · kind (official_api|authorised_feed|ats_api|career_page)
  regions[] · enabled · rate_limit_per_min · capabilities jsonb · health_status
  legal_basis · terms_url · last_checked_at

RawJobPayload
  id · source_id fk · external_id · fetched_at · payload jsonb
  content_hash · processing_status (pending|normalised|quarantined) · quarantine_reason
  unique(source_id, external_id, content_hash)

JobPosting                                     -- global, shared, deduplicated
  id · fingerprint unique · title · normalised_title · company_id fk
  description · description_html · requirements[] · benefits[]
  location jsonb {raw, city, region, country, lat, lon, geo_key}
  work_mode (onsite|hybrid|remote) · employment_type · seniority
  salary {min, max, currency, period, is_estimate} · language (ISO-639-1)
  apply_method_hint (ats_api|form|email|external|unknown)
  apply_url · apply_email · ats_provider · ats_job_id
  posted_at · expires_at · first_seen_at · last_seen_at
  status (active|expired|removed) · content_hash
  ├─1:N─ JobPostingSource  (source_id, external_id, source_url, first_seen_at, last_seen_at)
  └─N:N─ JobPostingRelation (related_posting_id, relation: near_duplicate|same_company_role,
                             similarity, resolved_by)

Company
  id · normalised_name unique · display_name · domain · ats_provider
  logo_url · industry · size_bucket
```

**Invariants.** `fingerprint` is unique and deterministic over
(normalised company, normalised title, geo key, shingled description hash). Exact fingerprint
collisions **add a `JobPostingSource` attribution** to the existing posting rather than
inserting a row — that is the deduplication mechanism. Near-duplicates are **linked, never
merged**: silently merging two distinct jobs is unrecoverable, while a link is cheap and
reversible. `status = expired` is driven by the nightly liveness re-check and cascades to matches.

## loops (job search configuration)

```
Loop
  id · user_id fk · name · status (draft|active|paused|archived)
  cv_version_id fk · email_template_id fk (nullable)
  config_version int                         -- bumped on every edit
  titles[] · locations jsonb[] · remote_only · worldwide_remote · hybrid_only
  seniority (Junior|Associate|Senior|Executive|All)
  employment_types[] (Full-time|Contract|Part-time|Temporary|Internship)
  source_slugs[] · career_page_search
  required_keywords[] · excluded_keywords[] · excluded_companies[]
  match_threshold int (0-100) · match_preset (low|middle|high|custom)
  auto_email · auto_form · auto_ai_answers · extension_auto_apply
  form_phone · form_city · cover_letter_mode (none|default|custom) · cover_letter_template
  activated_at · paused_at · last_discovery_at · next_discovery_at
  ├─1:N─ LoopConfigVersion    (config_version, snapshot jsonb, changed_at, changed_by)
  ├─1:N─ DiscoveryRun         (source_id, started_at, finished_at, cursor,
  │                            postings_seen, postings_new, errors jsonb, outcome)
  └─1:N─ StandingAuthorisation (see applications)

EmailTemplate
  id · user_id fk · name · subject · body · is_system · placeholders_used[]
```

**Invariants.** Only an `active` Loop is scheduled for discovery. `config_version` is immutable
once used: every `JobMatch` records the `config_version` and `cv_version` it was scored under,
so any historical match can be explained by the configuration in force at the time. The
`match_preset` values map onto `match_threshold` — the named presets stay familiar while the
numeric threshold stays inspectable, replacing LoopCV's opaque Low/Middle/High knob.

## matching

```
JobMatch
  id · user_id fk · loop_id fk · job_posting_id fk · cv_version_id fk
  config_version · score int · score_breakdown jsonb
  reasons jsonb[]              -- {dimension, score, weight, explanation}
  status (new|viewed|dismissed|saved|application_created|rejected|suppressed_cooldown|expired)
  scoring_mode (full|degraded)
  matched_at · viewed_at · dismissed_at
  unique(loop_id, job_posting_id)

MatchRejection
  id · user_id · loop_id · job_posting_id · category · detail jsonb · rejected_at
  -- category: job_title | location | work_mode | language | required_keyword_missing
  --         | excluded_keyword | excluded_company | cv_relevance | employment_type | seniority
```

**Invariants.** One match per (loop, posting). Rejections are **retained** with their category
so the "why am I getting no matches?" screen is self-service and so the matching engine has a
labelled evaluation set. A rejected posting remains applicable to directly. `scoring_mode =
degraded` marks matches scored without embeddings, for later rescoring — a match is never
dropped because scoring was degraded.

## applications

```
Application
  id · user_id fk · job_match_id fk unique · job_posting_id fk · loop_id fk
  cv_version_id fk · method (ats_api|public_form|authorised_email|extension|manual)
  method_reason text
  status (prepared|blocked|questions_pending|awaiting_authorisation|ready|submitting
          |submitted|email_sent|applied_externally|extension_pending|suppressed_cooldown
          |external_required|failed|outcome_unknown|expired|interview|offer|rejected)
  blocked_reason · failure_reason · retry_count
  -- IMMUTABLE PREPARED ARTEFACTS: exactly what was/will be sent
  prepared_cover_letter · prepared_email_subject · prepared_email_body
  prepared_form_fields jsonb · prepared_attachments jsonb
  authorisation_id fk (nullable)
  prepared_at · authorised_at · submitted_at · confirmed_at
  ├─1:N─ ApplicationEvent     (append-only: type, payload jsonb, actor, occurred_at,
  │                            correlation_id)  -- status is DERIVED from this log
  ├─1:N─ ApplicationQuestion  (question_text, normalised_text, kind, options[],
  │                            answer, answer_source (candidate|saved|ai),
  │                            is_sensitive, answered_at)
  └─1:1─ ApplicationAuthorisation
           kind (per_application|standing) · granted_at · granted_by · ip · user_agent
           standing_id fk (nullable) · snapshot jsonb   -- settings at grant time

StandingAuthorisation
  id · user_id · loop_id · daily_cap · min_score · expires_at
  granted_at · revoked_at · snapshot jsonb

SavedAnswer
  id · user_id · normalised_question · question_embedding · answer · kind
  use_count · last_used_at · is_sensitive
```

**Invariants — the product's integrity boundary.**
1. An `Application` cannot reach `submitted` / `email_sent` without a non-null
   `authorisation_id` pointing at a valid, unexpired, uncapped authorisation. This is enforced
   in the repository layer and asserted by tests.
2. Prepared artefacts are **immutable** once authorised. What is stored is byte-for-byte what
   was sent; otherwise the audit trail is decorative.
3. `status` is **derived** from the `ApplicationEvent` log, never mutated in place — so the
   history can always be reconstructed.
4. `outcome_unknown` is a real, first-class state. When a request was dispatched but the
   response was lost, the system records uncertainty rather than guessing, and never retries a
   possibly-successful submission.
5. Questions in a sensitive category are never auto-answered, on any tier.

## outreach

```
RecruiterContact
  id · company_id fk · job_posting_id fk (nullable) · email · name · title
  provenance (job_posting|company_careers_page|licensed_provider) · legal_basis
  verification_status (unverified|verified|invalid) · verified_at
  -- NO pattern-guessed addresses, ever

OutreachMessage
  id · user_id · application_id fk · contact_id fk · direction (outbound|inbound)
  subject · body · message_id · reply_to_address · tracking_token
  sent_via (platform_smtp|user_smtp) · status (queued|sent|delivered|bounced|complained|failed)
  sent_at · opened_at · replied_at
  └─1:N─ OutreachEvent (type: sent|delivered|opened|replied|bounced|complained, payload, occurred_at)

SuppressionEntry
  id · email_or_domain · scope (global|user) · reason (bounce|complaint|unsubscribe|manual)
  created_at                                  -- checked before EVERY send

UserSMTPConfig
  id · user_id unique · host · port · username
  password_ciphertext · key_id              -- envelope encryption; write-only via API
  from_name · verified_at · last_error · disabled_at
```

**Invariants.** The suppression list is checked before every send, without exception. SMTP
credentials are write-only through the API, never returned to a client, never logged. Every
contact records provenance and a lawful basis; unverified addresses are not used for automated
sending.

## billing & platform

```
Plan            slug unique · name · price_cents · currency · interval · quotas jsonb · features jsonb
Subscription    user_id · plan_id · provider · provider_subscription_id
                status (trialing|active|past_due|canceled|incomplete)
                current_period_{start,end} · cancel_at_period_end · canceled_at
QuotaUsage      user_id · quota_key · period_start · used · limit_snapshot   unique(user_id,quota_key,period_start)
Invoice         user_id · provider_invoice_id · amount_cents · status · issued_at · paid_at

AuditLog        id · user_id (nullable) · actor_id · actor_role · action · resource_type
                resource_id · ip · user_agent · metadata jsonb · occurred_at   -- append-only
Notification    id · user_id · category · title · body · link · read_at · created_at
BackgroundJob   id · kind · status · attempts · max_attempts · payload jsonb
                last_error · scheduled_for · started_at · finished_at · idempotency_key unique
ProcessedMessage idempotency_key pk · stream · consumer · state (in_flight|done)
                 result jsonb · processed_at · expires_at
```

`ProcessedMessage` is the backbone of at-least-once safety. A key in `in_flight` on redelivery
means the previous attempt's outcome is unknown — the consumer must report `outcome_unknown`
rather than retrying a side effect that may already have happened.

---

## Cross-context relationship summary

```
User 1─N Loop 1─N JobMatch N─1 JobPosting N─N JobSource
                      │
                      1
                      │
                      1
                  Application 1─N ApplicationEvent
                      │ 1─N ApplicationQuestion
                      │ 1─1 ApplicationAuthorisation N─1 StandingAuthorisation
                      └ 1─N OutreachMessage N─1 RecruiterContact

User 1─N CVDocument 1─N CVVersion ──┬─1 CVParsedData
                                    ├─N CVSkill N─1 Skill
                                    ├─N CVAnalysis
                                    └─N CVEmbedding
       ▲
       └── Loop.cv_version_id, JobMatch.cv_version_id, Application.cv_version_id
           (pinned, never hard-deleted once used in a submission)

User 1─1 Subscription N─1 Plan ; User 1─N QuotaUsage
```

## Tenant isolation rule

Every table holding candidate data carries `user_id`, and **every** query touching such a table
is scoped by it in the repository layer — not only in middleware. A missing handler-level check
must not be able to leak data. Integration tests assert that cross-tenant reads return zero
rows for each such repository.
