# Implementation Roadmap

Eight milestones, sequenced so that each one ends with something verifiable rather than
something merely written. The ordering is driven by two constraints: the critical path
through the backlog's dependency graph, and the principle that risk should surface as
early as it can be afforded.

## Sequencing decision

**A thin end-to-end slice first, then depth.**

The alternative — completing each domain to full production depth before starting the
next — produces better individual domains but hides integration risk until very late. In
a pipeline with eight lossy asynchronous stages, the integration risk *is* the risk. A
candidate's journey crosses every domain, and a design mistake at the matching/
application boundary is cheap to fix now and expensive to fix after both are finished.

So M1–M4 build the narrowest viable path through the whole pipeline, M5 makes it usable,
and M6–M8 add the breadth, operations and hardening. Depth comes back to each domain
after the whole chain has carried real data at least once.

## Milestones

### M1 — CV Intelligence (12 issues, 7 critical)

Turn a document into structured, matchable data.

**Critical path:** `MJF-001` storage → `MJF-002` upload → `MJF-003` extraction →
`MJF-004` AI client → `MJF-005` structured extraction → `MJF-007` worker pipeline.

`MJF-005` is the single most blocking issue in the backlog: five other issues wait on it,
and match quality is bounded by extraction quality. `MJF-011` (tailoring with factual
containment) is the product's most serious safety requirement and deliberately comes
after the pipeline works, so the validator can be tested against real parsed CVs rather
than fixtures.

**Done when:** a real CV uploads, parses, and produces structured experience and skills
visible in the database, with the pipeline traceable end to end and its failure modes —
scanned PDF, encrypted PDF, schema violation — each producing a clear, actionable outcome.

### M2 — Job Acquisition (9 issues, 6 critical)

Real postings from lawful sources, normalised and deduplicated.

**Critical path:** `MJF-020` adapter contract → `MJF-021` Adzuna → `MJF-025`
normalisation → `MJF-026` deduplication → `MJF-024` scheduler. `MJF-028` (loops) runs in
parallel and gates matching.

Adzuna first because it needs no partnership and immediately validates the whole
pipeline with real data. The ATS adapters (`MJF-023`) matter disproportionately: they are
the only sources where fully server-side submission is legitimate, which makes them the
difference between job discovery and genuine application automation.

**Done when:** a configured loop produces real, deduplicated postings from at least two
sources, with the same job from multiple boards collapsing to one row with several
attributions, and one source failing degrades a run without failing it.

### M3 — Matching (5 issues, 2 critical)

Explainable scoring, and self-diagnosable rejection.

**Critical path:** `MJF-030` hard filters → `MJF-031` scoring. `MJF-031` blocks five
later issues.

`MJF-032` (prefilter) is scheduled here rather than deferred because retrofitting a
prefilter after the scoring interface has callers is more work than building it alongside.

**Done when:** matches carry per-dimension sub-scores and reasons a candidate can act on,
rejections are browsable by category, and a filter change rescores recent postings within
minutes rather than at the next daily run.

### M4 — Application Orchestration (7 issues, 5 critical)

Prepare, authorise, submit — with consent recorded and duplicates impossible.

**Critical path:** `MJF-040` method selection → `MJF-041` preparation → `MJF-042`
authorisation → `MJF-043` submission → `MJF-045` event log.

This is the milestone where the product can do real harm if built carelessly. `MJF-042`
and `MJF-043` each carry a release-gating test suite: one attempting to submit without
authorisation through every code path, one attempting to produce a duplicate submission
under concurrency and redelivery. The database constraints are already in place; these
milestones build the features that must respect them.

**Done when:** an application can be prepared, explicitly authorised, and submitted
through the email channel, with the exact artefacts recorded, a full event history, and
a simulated lost response producing `outcome_unknown` rather than a guess or a retry.

### M5 — Frontend (7 issues, 5 critical)

Make it usable, honestly.

**Critical path:** `MJF-050` foundation → `MJF-051` auth and onboarding → `MJF-054`
matches → `MJF-055` applications and authorisation.

`MJF-055` is the most important screen in the product: it is where consent becomes
informed, because it shows the candidate exactly what will be sent. The recurring
constraint across this milestone is honesty — empty states that name the pipeline stage,
no verdict below the minimum sample, `outcome_unknown` shown as uncertainty, and no
placeholder that implies a feature exists.

**Done when:** a candidate can complete the entire journey in a browser, from sign-up to
an authorised submission, and the diagnose-no-matches journey works without support.

### M6 — Outreach, Analytics & Admin (7 issues)

Breadth, operations, revenue and legal obligations.

`MJF-064` (admin tracing) is the issue that makes the platform operable: without it every
production problem is a guess. `MJF-066` (GDPR) is non-negotiable before any real
candidate data exists. `MJF-065` (billing) gates revenue and, equally, protects against
one user consuming unbounded AI spend.

**Done when:** an administrator can trace any workflow end to end from a correlation id,
analytics reconcile exactly with per-application histories, and a candidate can export
or erase their data.

### M7 — Infrastructure & GitOps (6 issues, 2 critical)

Deployable, and recoverable.

**Critical path:** `MJF-070` Dockerfiles → `MJF-071` Helm → `MJF-072` FluxCD →
`MJF-073` VPS scripts.

Helm must be installed first (`ENGINEERING_AUDIT.md` finding E1). The worker deployments
are split per consumer group so an AI-provider outage stalls CV parsing while application
submission continues — the operational payoff for the group separation built in M1.

**Done when:** a push to main reconciles to a running cluster, alerts fire on the
conditions that matter, and a restore from backup has actually been performed rather than
documented.

### M8 — CI/CD & Hardening (8 issues, 3 critical)

Make the guarantees continuous rather than one-off.

**Critical path:** `MJF-080` CI → `MJF-081` integration tests → `MJF-083` CD.

`MJF-081` is the most valuable issue in this milestone and arguably in the backlog's
second half. Everything verified by hand during the foundation work — the database
invariants, the migration round-trip, the pipeline crash-and-resume — is currently
verified *once*. Until it is automated, a refactor can remove a constraint protecting a
candidate from an unauthorised submission and nothing will notice.

**Done when:** CI gates every merge, CD cannot deploy untested code, every database
invariant has a test that has been seen to fail when its constraint is removed, and the
security posture is documented.

## Parallelism

Work that can proceed independently:

- **M5 frontend foundation** (`MJF-050`) needs only the identity API and can start now.
- **M7 infrastructure** depends on nothing in the product and can proceed throughout.
- **M8 CI** (`MJF-080`) should be built early rather than last; the roadmap position
  reflects the milestone grouping, not a scheduling constraint.
- **`MJF-087` security documentation** depends on nothing and improves every review.

Work that genuinely serialises: the M1 CV pipeline, the M2 acquisition pipeline, the M3
scoring chain, and the M4 authorisation chain. Each is a dependency chain where the next
stage needs the previous one's output shape to exist.

## Risks

| Risk | Mitigation |
|---|---|
| Extraction quality bounds match quality | `MJF-005` ships with a labelled accuracy corpus, not spot checks |
| An LLM invents CV content | `MJF-011`'s containment validator is a hard gate independent of the prompt, with an adversarial release-gating suite |
| A duplicate application cannot be recalled | Idempotency keys written before dispatch, `InFlightUnknown` policy, and `outcome_unknown` as a first-class state |
| Upstream rate limits cap platform capacity | Discovery keyed by search specification, so N identical searches cost one query |
| AI spend is unbounded | Per-call accounting, monthly quotas, caching by content hash, and a per-request ceiling (TD-10) |
| Invariants verified once, not continuously | `MJF-081`, flagged as the highest-priority debt item in `TECHNICAL_DEBT.md` |
| A job board closes its API | The adapter registry degrades per source; `partnership_required` sources are modelled so their absence is explained rather than silent |
