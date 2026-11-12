# Event Catalog

## Envelope

Every message on every stream shares one envelope. `payload` is the only part that varies.

```json
{
  "event_id": "01JC...",            // ULID, unique per publish
  "type": "cv.uploaded",
  "version": 1,
  "occurred_at": "2026-11-11T18:40:12.481Z",
  "correlation_id": "01JC...",      // constant across one logical workflow
  "causation_id": "01JC...",        // event_id of the direct cause
  "user_id": "uuid|null",
  "trace": { "traceparent": "00-..." },
  "payload": { }
}
```

`correlation_id` is created at the originating HTTP request and copied into every descendant
event, so one candidate action is one queryable chain across logs, traces, and the event store.
`traceparent` is carried explicitly because OTel context does not survive a message broker on
its own — without it, the admin's end-to-end trace (I2) silently breaks at every async hop.

## Streams

| Stream | Subjects | Retention | Max age | Replicas | Dead letter |
|---|---|---|---|---|---|
| `IDENTITY` | `myjobfyt.identity.>` | interest | 7d | 1 (3 in prod) | `myjobfyt.dlq.identity` |
| `CV` | `myjobfyt.cv.>` | interest | 7d | 1 (3) | `myjobfyt.dlq.cv` |
| `JOBS` | `myjobfyt.jobs.>` | interest | 3d | 1 (3) | `myjobfyt.dlq.jobs` |
| `MATCHING` | `myjobfyt.matching.>` | interest | 3d | 1 (3) | `myjobfyt.dlq.matching` |
| `APPLICATIONS` | `myjobfyt.applications.>` | interest | 30d | 1 (3) | `myjobfyt.dlq.applications` |
| `OUTREACH` | `myjobfyt.outreach.>` | interest | 30d | 1 (3) | `myjobfyt.dlq.outreach` |
| `BILLING` | `myjobfyt.billing.>` | interest | 90d | 1 (3) | `myjobfyt.dlq.billing` |
| `DLQ` | `myjobfyt.dlq.>` | limits | 90d | 1 (3) | — |

Every domain stream uses **interest** retention, because these are fan-out event streams
rather than work queues. A work-queue stream delivers each message to exactly one consumer
and therefore rejects a second consumer filtering the same subject — and `cv.parsed` alone
is consumed independently by skill normalisation, by embedding and (later) by matching.
Declaring these as work queues made the worker fail to start with `filtered consumer not
unique on workqueue stream` as soon as the second `cv.parsed` consumer was added. Interest
retention gives each consumer its own copy while still reclaiming storage once every
registered consumer has acked.

Several worker replicas competing for the same message is a *consumer*-level property, not
a stream-level one: a durable consumer shared between replicas already distributes work
among them, with no need for work-queue retention.

JetStream cannot change retention in place. `EnsureStreams` therefore reports a mismatch
and refuses to start rather than deleting a stream that may hold a candidate's CV
mid-pipeline — see TD-13.

`JOBS` and `MATCHING` have short retention because they are high-volume and fully
reconstructible from `raw_job_payloads` — replaying discovery is cheap. `APPLICATIONS` and
`OUTREACH` retain for 30 days because they carry consent-relevant history, and `BILLING` for
90 because payment disputes outlive everything else.

---

## Events

Legend: **Idem** = idempotency key · **MD** = `MaxDeliver` · **AW** = `AckWait`.

### identity

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `identity.candidate.created` | `user_id, email, source` | `send-verification-email`, `provision-defaults`, `analytics` | `event_id` | 5 / 30s |
| `identity.email.verified` | `user_id, verified_at` | `notifications`, `analytics` | `event_id` | 5 / 30s |
| `identity.profile.updated` | `user_id, changed_fields[], completeness_pct` | `recompute-form-readiness` | `user_id:updated_at` | 3 / 30s |
| `identity.account.deletion_requested` | `user_id, erase_after` | `schedule-erasure`, `halt-automation` | `user_id` | 5 / 1m |
| `identity.account.erased` | `user_id, erased_at, summary` | `analytics`, `audit` | `user_id` | 5 / 1m |

### cv

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `cv.uploaded` | `cv_id, version_id, user_id, storage_key, content_type, size_bytes, checksum` | `av-scan` → `text-extract` | `cv_version_id` | 3 / 2m |
| `cv.scanned` | `cv_version_id, av_status` | `text-extract` | `cv_version_id` | 3 / 1m |
| `cv.text_extracted` | `cv_version_id, char_count, page_count` | `structure-extract` | `cv_version_id` | 3 / 3m |
| `cv.parsed` | `cv_id, version_id, user_id, skill_count, confidence_avg` | `embed`, `rescore-matches`, `notify`, `analytics` | `cv_version_id` | 5 / 2m |
| `cv.embedded` | `cv_id, version_id, user_id, model, kinds, dims, backfilled` | *(none yet — see TD-12)* | `cv_version_id` | 4 / 2m |
| `cv.parse_failed` | `cv_version_id, reason, stage` | `notify`, `admin-alert` | `cv_version_id:stage` | 3 / 1m |
| `cv.analysis_requested` | `cv_version_id, target_role` | `cv-checker` | `cv_version_id:target_role` | 3 / 3m |
| `cv.analysis_completed` | `cv_version_id, analysis_id, overall_score` | `notify` | `analysis_id` | 3 / 1m |
| `cv.tailoring_requested` | `base_version_id, job_posting_id, user_id` | `tailor-cv` | `base_version_id:job_posting_id` | 3 / 3m |
| `cv.tailored` | `new_version_id, base_version_id, job_posting_id, containment_passed` | `notify` | `new_version_id` | 3 / 1m |

`cv.tailored` carries `containment_passed` explicitly. A tailored version that fails factual
containment is never published as tailored — it emits `cv.parse_failed`-style failure instead,
because surfacing unvalidated tailored content would breach the no-fabrication rule.

### jobs

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `jobs.discovery.requested` | `spec_hash, source_slug, cursor, loop_ids[]` | `source-fetch` | `spec_hash:source:cursor` | 5 / 5m |
| `jobs.discovered` | `source_slug, external_id, raw_payload_id, content_hash` | `normalise` | `source_slug:external_id:content_hash` | 5 / 2m |
| `jobs.normalised` | `job_posting_id, is_new, changed_fields[], fingerprint` | `match-fanout`, `analytics` | `job_posting_id:content_hash` | 5 / 2m |
| `jobs.updated` | `job_posting_id, changed_fields[]` | `rescore-affected-matches` | `job_posting_id:content_hash` | 3 / 2m |
| `jobs.expired` | `job_posting_id, detected_at` | `expire-matches`, `expire-applications` | `job_posting_id` | 3 / 1m |
| `jobs.source.degraded` | `source_slug, error_class, consecutive_failures` | `admin-alert`, `circuit-breaker` | `source_slug:window` | 3 / 30s |

`jobs.normalised` is published **only when content actually changed**. Publishing on every
daily re-sighting would re-match the entire corpus every day — the single most expensive
mistake available in this design.

### matching

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `matching.requested` | `loop_id, job_posting_id, reason (new_posting\|config_change\|cv_change\|rescore)` | `match-score` | `loop_id:job_posting_id:config_version:cv_version` | 5 / 2m |
| `matching.matched` | `match_id, user_id, loop_id, job_posting_id, score, scoring_mode` | `maybe-prepare-application`, `notify`, `analytics` | `match_id` | 5 / 1m |
| `matching.rejected` | `loop_id, job_posting_id, category, detail` | `persist-rejection`, `analytics` | `loop_id:job_posting_id:config_version` | 3 / 1m |
| `matching.backfill_requested` | `loop_id, since, reason` | `backfill-planner` | `loop_id:config_version` | 3 / 10m |

The idempotency key includes `config_version` and `cv_version`: the *same* loop/posting pair
legitimately needs rescoring when either changes, and must not be rescored otherwise.

### applications

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `applications.preparation_requested` | `match_id, user_id, tailor_cv (bool)` | `method-select` → `prepare` | `match_id` | 5 / 5m |
| `applications.method_selected` | `application_id, method, reason` | `prepare` | `application_id` | 3 / 1m |
| `applications.prepared` | `application_id, method, has_pending_questions, blocked_reason` | `maybe-auto-authorise`, `notify` | `application_id:prepared_at` | 5 / 2m |
| `applications.questions_pending` | `application_id, question_ids[], sensitive_count` | `ai-answer` (PR/DFY), `notify` | `application_id` | 3 / 2m |
| `applications.questions_answered` | `application_id, answered_by` | `resume-preparation` | `application_id:answered_at` | 3 / 2m |
| `applications.authorised` | `application_id, authorisation_id, kind` | `submit` | `application_id` | 5 / 10m |
| `applications.submitted` | `application_id, method, external_ref, submitted_at` | `track-status`, `notify`, `analytics`, `cooldown-register` | `application_id` | 5 / 2m |
| `applications.failed` | `application_id, reason, retryable, attempt` | `notify`, `admin-alert`, `analytics` | `application_id:attempt` | 3 / 1m |
| `applications.outcome_unknown` | `application_id, method, dispatched_at` | `notify`, `admin-alert` | `application_id` | 3 / 1m |
| `applications.status_updated` | `application_id, from, to, source (ats_poll\|email_reply\|candidate)` | `notify`, `analytics` | `application_id:to:occurred_at` | 3 / 1m |

`applications.authorised` has `AckWait=10m` because submission involves third-party I/O that can
be slow. `applications.outcome_unknown` has no automatic consumer that retries — by design. It
exists to make uncertainty visible, not to be resolved automatically.

### outreach

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `outreach.send_requested` | `application_id, contact_id, template_id` | `send-email` | `application_id:contact_id` | 4 / 5m |
| `outreach.sent` | `message_id, application_id, provider_message_id, sent_via` | `analytics` | `message_id` | 3 / 1m |
| `outreach.opened` | `message_id, opened_at, user_agent_class` | `analytics` | `message_id:first_open` | 3 / 30s |
| `outreach.replied` | `message_id, application_id, reply_id` | `thread-inbox`, `notify`, `status-update` | `reply_id` | 5 / 2m |
| `outreach.bounced` | `message_id, bounce_type, recipient` | `suppress`, `notify` | `message_id` | 5 / 1m |
| `outreach.complained` | `message_id, recipient` | `suppress`, `admin-alert` | `message_id` | 5 / 1m |

### billing

| Type | Payload | Consumers | Idem | MD / AW |
|---|---|---|---|---|
| `billing.subscription.activated` | `user_id, plan_slug, period_end` | `refresh-entitlements`, `resume-queued-submissions` | `provider_event_id` | 5 / 1m |
| `billing.subscription.updated` | `user_id, plan_slug, status, period_end` | `refresh-entitlements` | `provider_event_id` | 5 / 1m |
| `billing.subscription.canceled` | `user_id, effective_at` | `schedule-downgrade`, `notify` | `provider_event_id` | 5 / 1m |
| `billing.payment.failed` | `user_id, attempt, next_retry_at` | `notify`, `grace-period` | `provider_event_id` | 5 / 1m |
| `billing.quota.exhausted` | `user_id, quota_key, limit, resets_at` | `notify`, `queue-submissions` | `user_id:quota_key:period` | 3 / 30s |

Billing idempotency is keyed on the **provider's** event ID, never ours. Payment providers
deliver duplicates and out-of-order events as a matter of routine, and the provider's ID is the
only stable identity available.

---

## Dead-letter handling

A message exceeding `MaxDeliver` is republished to `myjobfyt.dlq.<domain>` with the original
envelope plus `{ original_subject, delivery_count, last_error, first_failed_at }`.

- `dead_letter_depth > 0` alerts immediately. A dead letter is always a bug, a bad input, or a
  dependency failure — never routine.
- The admin UI lists dead letters with the full envelope and error, and offers requeue.
- Requeue is safe because every consumer is idempotent; this is the practical payoff for the
  `processed_messages` discipline.

## Versioning rules

1. Payload changes are additive. Never remove or retype a field in the same version.
2. A breaking change publishes `version: 2` alongside `version: 1` until all consumers handle
   both; only then is v1 retired.
3. Consumers ignore unknown fields.
4. `type` strings are permanent. A renamed concept gets a new type and a transitional consumer.
