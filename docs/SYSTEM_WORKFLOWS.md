# System Workflows

Each workflow names its trigger, stages, events, idempotency key, retry policy, failure
handling, and observability. These are the contracts the worker implementations must satisfy.

Conventions used throughout:
- **At-least-once delivery.** Every consumer must be idempotent; redelivery is normal, not exceptional.
- **Idempotency key.** A deterministic key persisted in a `processed_messages` table (or as a
  Redis SETNX with a TTL for hot, low-value paths). A duplicate is acknowledged and dropped.
- **Retry.** Exponential backoff with full jitter, bounded attempts, then dead-letter.
- **Dead-letter.** A durable stream plus an admin requeue path. Dead-letter depth > 0 alerts.
- **Correlation.** Every message carries `correlation_id`, `causation_id`, `user_id` (where
  applicable), and `occurred_at`. These propagate into logs and OpenTelemetry spans.

---

## W1. CV upload → parsed profile

```
POST /v1/cvs (multipart)
  │  sync: magic-byte sniff, size cap during stream, object-storage PUT,
  │        insert cv_documents(status=parsing)
  ├─> publish cv.uploaded {cv_id, user_id, storage_key, content_type}
  │
  ├─ consumer: av-scan        → clean | quarantine
  ├─ consumer: text-extract   → raw_text (pdftotext / docx XML), page count, char count
  │     └─ if char_count == 0 → cv.parse_failed{reason: no_extractable_text}  [terminal, user-notified]
  ├─ consumer: structure      → schema-constrained LLM call → cv_parsed_data + per-field confidence
  ├─ consumer: skill-normalise→ map to canonical skill taxonomy, persist cv_skills
  ├─ consumer: embed          → skill/title embeddings for semantic matching
  └─> publish cv.parsed {cv_id, user_id, version}
        └─ fan-out: rescore existing matches for Loops using this CV; notify candidate
```

- **Idempotency:** `cv.parse:{cv_id}:{version}`. Re-running overwrites the same version's derived rows inside one transaction.
- **Retry:** AV 3×/30s; extract 3×/1m; structure 3× with a repair prompt on schema violation; embed 5×/5m.
- **Timeouts:** AV 60s, extract 30s, structure 120s, embed 30s.
- **Failure modes:** AV positive → quarantine object, notify, delete after retention. Encrypted PDF → terminal, clear message. LLM schema violation after retries → dead-letter, candidate told parsing needs attention, manual-entry path offered.
- **Observability:** one trace spanning the whole chain; metrics `cv_parse_duration_seconds`, `cv_parse_failures_total{reason}`, `ai_tokens_total{purpose="cv_extract"}`.
- **Security:** CV text never logged. Text enters the prompt only inside a delimited untrusted-data block. Output schema-validated before persistence.

---

## W2. Scheduled discovery → normalised postings

```
cron (per search-spec, distributed across the day)
  │  planner: resolve distinct search specs across all ACTIVE loops
  │           (spec = titles × locations × work mode × employment type × seniority)
  │  acquire distributed lock  lock:discovery:{spec_hash}:{source}   (TTL > max run)
  ├─> publish discovery.requested {spec_hash, source, cursor}
  │
  ├─ consumer: source-fetch (per adapter)
  │     ├─ circuit breaker per source; per-source rate limiter
  │     ├─ paginate with a persisted cursor (resumable after a crash)
  │     └─> publish job.discovered {source, external_id, raw_payload_ref}   (one per posting)
  │
  ├─ consumer: normalise
  │     ├─ company identity normalisation, geocode location, parse salary,
  │     │  map employment type + seniority + work mode, detect language
  │     ├─ compute fingerprint = hash(norm_company, norm_title, geo_key, shingle(description))
  │     ├─ dedupe: exact fingerprint → attach source attribution to the existing posting
  │     │          near match (similarity ≥ threshold) → link as related, do NOT merge
  │     └─> publish job.normalised {job_posting_id, is_new, changed_fields[]}
  │
  └─ nightly: liveness re-check of postings seen in the last 30 days
        └─> job.expired  → cascades matches to status `expired`
```

- **Idempotency:** `discover:{source}:{external_id}:{content_hash}` — the same posting with unchanged content is a no-op. `job.normalised` is only published when something actually changed, which prevents a daily re-match storm.
- **Retry:** fetch 5× with backoff; 429 honours `Retry-After`; auth failure disables the adapter and alerts (no retry storm).
- **Failure modes:** schema drift → quarantine the payload, alert, do not corrupt the dataset. Geocoder down → store the raw location string, backfill later. One adapter failing degrades the run; it never fails it.
- **Observability:** `discovery_runs_total{source,outcome}`, `postings_discovered_total{source}`, `dedupe_collapse_ratio`, `source_health{source}`, run records with start/end/counts/errors visible in the admin UI.
- **Cost control:** discovery is keyed by *spec*, not candidate — N candidates with the same search cost one upstream query.

---

## W3. Matching

```
job.normalised {job_posting_id}
  │
  ├─ consumer: match-fanout
  │     └─ for each ACTIVE loop whose spec could admit this posting
  │          (pre-filtered by an inverted index on title tokens / geo / work mode)
  │          → publish match.requested {loop_id, job_posting_id}
  │
  └─ consumer: match-score  (idempotent per (loop_id, job_posting_id, config_version, cv_version))
        ├─ HARD FILTERS (reject + persist category, do not score):
        │    excluded_company · excluded_keyword · required_keyword_missing ·
        │    location · work_mode · language · employment_type · seniority_floor
        ├─ company cooldown: prior contact with this company < 180d
        │    → status already_applied / email_already_sent  (surfaced, with the prior date)
        ├─ DIMENSION SCORES (0-100 each, weights from versioned config):
        │    title_similarity · skill_overlap_required · skill_overlap_nice ·
        │    years_experience_fit · seniority_fit · location_fit · employment_type_fit
        ├─ overall = Σ(weight_i × score_i); reasons[] built from the structured sub-scores
        └─> job.matched {match_id, score} | job.rejected {category}
              └─ if loop has a standing authorisation and score ≥ its threshold
                   → publish application.preparation_requested
```

- **Backfill trigger:** a Loop config change or a new CV version republishes `match.requested` for postings from the last N days, so the candidate does not wait a day to see the effect of a filter change.
- **Degraded mode:** embedding provider down → lexical similarity fallback, match flagged `degraded_scoring`, re-scored when the provider recovers. A match is never dropped because scoring was degraded.
- **Observability:** `match_scored_total{outcome}`, `match_score_histogram`, `match_rejections_total{category}`, `matching_lag_seconds`.

---

## W4. Application preparation → authorisation → submission

```
application.preparation_requested {match_id}
  │
  ├─ consumer: method-select   (deterministic, ordered, reason recorded)
  │     ats_api → public_form → authorised_email → extension → manual
  │
  ├─ consumer: prepare
  │     ├─ resolve CV version (tailored if requested and containment-validated)
  │     ├─ render cover letter + email body from the template (allowlisted placeholders only)
  │     ├─ map form fields from profile; collect unknown screening questions
  │     ├─ persist ALL artefacts immutably on the application row
  │     └─ state: prepared | blocked{missing_field} | questions_pending
  │
  ├─> application.prepared
  │     ├─ standing authorisation valid, within daily cap, score ≥ threshold
  │     │     → write authorisation reference → application.authorised
  │     └─ otherwise → state awaiting_authorisation, notify candidate
  │
  ├─ POST /v1/applications/{id}/authorise   (sync, explicit consent)
  │     └─ immutable application_authorisations row → publish application.authorised
  │
  └─ consumer: submit   (idempotency key = sha256(application_id | method))
        ├─ PRE-SEND GATE — all must pass, re-checked immediately before the send:
        │    loop not paused (Redis kill-flag) · authorisation valid & unexpired ·
        │    daily cap not exhausted · recipient not suppressed · cooldown still clear
        ├─ per-destination-domain rate limit
        ├─ dispatch via the method adapter
        └─> application.submitted | application.failed{reason} | application.outcome_unknown
```

- **Idempotency:** the submit key is persisted **before** dispatch and marked complete after. On redelivery, a key in the `in_flight` state yields `outcome_unknown` — the system never re-sends something that may already have been sent.
- **Retry:** transient (network, 5xx, timeout) 4× with backoff; permanent (4xx, closed posting) terminal with a reason; never retry when the outcome is unknown.
- **Pause semantics:** the pre-send gate makes the pause race window sub-second. A submission completing after a pause is recorded with `authorised_before_pause` for a clean audit trail.
- **Observability:** `applications_submitted_total{method,outcome}`, `submission_duration_seconds{method}`, `submission_retries_total`, `authorisations_total{kind}`. Submission spans are children of the preparation trace.
- **Security:** this workflow is the product's integrity boundary. A submission without a valid authorisation reference is impossible by construction, and that invariant is enforced in the repository and asserted by tests.

---

## W5. Screening questions

```
prepare → unknown question encountered
  ├─ normalise question text → similarity lookup against saved_answers
  │     ├─ hit  → reuse the saved answer
  │     └─ miss → insert into the Questions queue
  │
  ├─ SENSITIVE CATEGORY (disability · ethnicity · gender · visa/sponsorship ·
  │   salary history · criminal record · age)
  │     → NEVER auto-answered. Always escalated to the candidate.
  │
  ├─ AI answering (PR/DFY only)
  │     ├─ sources: parsed CV + profile + previously saved answers ONLY
  │     ├─ schema-validated, length-capped output
  │     └─ any claim not supported by profile data → escalate, do not guess
  │
  └─ candidate answers → saved_answers upsert → resume preparation
```

- Nothing is submitted while questions are pending.
- **Security:** question text comes from third-party forms and is untrusted — it is delimited as data, and answers are validated against a schema before use.

---

## W6. Outreach, delivery and reply ingestion

```
application.authorised (method = authorised_email)
  ├─ pre-send: recipient verified · not on the global suppression list · domain rate limit ok
  ├─ send via platform SMTP (candidate in Reply-To) or the candidate's own SMTP
  ├─ record message_id, tracking token, per-application reply address
  └─> outreach.sent
        ├─ tracking pixel fetched → outreach.opened   (disclosed; candidate can disable)
        ├─ reply received (reply-address routing or IMAP) → outreach.replied → Inbox thread
        ├─ hard bounce → suppress permanently
        └─ spam complaint → suppress + alert
```

- Open tracking is presented in the UI as indicative, never as fact — image blocking makes it unreliable and saying so is more useful than a confident wrong number.
- **Security:** inbound mail is untrusted. Attachments are AV-scanned, HTML sanitised, and no inbound content ever reaches a prompt instruction position.

---

## W7. Analytics rollup

```
nightly (02:00 UTC, per timezone bucket)
  ├─ replay/aggregate application_events into daily per-user, per-loop, per-cv summaries
  ├─ recompute funnel: scanned → matched → prepared → submitted → opened → replied → interview → offer
  ├─ recompute per-CV reply/interview rates with confidence intervals and sample sizes
  └─ refresh materialised views backing the dashboards
```

Live counters in Redis cover "today"; everything historical comes from the rollups. Because
both derive from the same immutable event log, dashboard numbers always reconcile with the
per-application timeline — which is the only way a funnel metric is ever trustworthy.

---

## W8. Data export and erasure

```
export:  request → assemble per-table JSON + original CV objects → zip → single-use signed URL (1h)
erasure: request → 7-day grace (automation halted immediately, cancellable)
         → idempotent resumable job: erase PII columns, delete object-storage keys,
           anonymise rows needed for aggregate integrity
         → verification pass asserts erasure is complete (fails loudly if not)
         → written confirmation of what was erased and what was retained
```

Both jobs are resumable and idempotent. The verification pass exists because a partially
completed erasure that reports success is worse than one that reports failure.
