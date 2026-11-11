

# Product Analysis — Reverse Engineering of LoopCV's Observable Behaviour

**Date of investigation:** 2026-11-11
**Sources:** `https://www.loopcv.pro/`, `https://www.loopcv.pro/pricing`,
`https://loopcv.freshdesk.com/support/solutions/articles/103000399849-knowledge-base`,
and the publicly exposed LoopCV MCP connector tool schemas.

**Scope and method.** Only publicly observable behaviour was studied: marketing pages,
the public knowledge base, and the published tool schemas of LoopCV's own MCP connector.
No proprietary source code, private API, credential, or protected implementation detail
was obtained or reproduced. Everything below is a description of *observed product
behaviour*, from which MyJobFyt derives an original implementation. Where the public
material is silent or inconsistent, this document says so explicitly and records the
design decision MyJobFyt makes instead.

---

## 1. What the product is

LoopCV is a candidate-side job-application automation platform. A candidate uploads a CV,
describes the roles they want, and the platform continuously discovers matching postings
across a large set of job boards, evaluates each posting against the candidate's CV, and
then applies on the candidate's behalf through whichever channel the posting supports.
Everything else in the product — analytics, CV tooling, recruiter outreach, screening-question
handling — exists to feed or to measure that core loop.

The central product abstraction is the **Loop**: a persistent, named, automated job-search
cycle owned by a candidate. The knowledge base defines it as "an automated job search cycle
that finds, evaluates, and applies to postings." A candidate may own several Loops, each with
its own CV, filters, email template, automation settings, and statistics. Loops run on a daily
cadence, can be edited, duplicated, or deleted — and notably **cannot be paused** in LoopCV,
which is a genuine usability defect MyJobFyt will not reproduce (see §6).

## 2. The observable pipeline

Reconstructed from the knowledge base's description of Loop creation, match statuses,
rejected-match categories, and application methods:

```
candidate registers
  -> profile created
  -> CV uploaded                       (PDF/DOCX, multiple CVs per account)
  -> CV parsed into structured data    (skills, experience, seniority)
  -> Loop configured                   (titles, locations, boards, filters, template, automation)
  -> scheduled discovery               (daily, per source adapter, per Loop)
  -> normalisation                     (one canonical posting shape across ~25 boards)
  -> deduplication                     (same posting from several boards collapses)
  -> matching / rejection              (title, location, work mode, language, keywords,
                                        company exclusions, CV relevance)
  -> match surfaced to candidate       (All Matches, with a status)
  -> optional CV tailoring             (per-job emphasis of existing content)
  -> application prepared              (method selected: email / form / extension / manual)
  -> screening questions resolved      (manually, or by AI on higher tiers)
  -> application submitted             (or handed to candidate for manual submission)
  -> status tracked                    (11 terminal and non-terminal statuses)
  -> analytics                          (opens, replies, per-CV performance, A/B tests)
```

Two properties of this pipeline matter architecturally:

**It is overwhelmingly asynchronous.** Discovery, normalisation, deduplication, matching,
CV parsing, tailoring, and submission are all background work measured in minutes to hours.
The knowledge base's "Timing of activity" section and the daily collection cadence confirm
the candidate is not waiting on any of it. Only authentication, Loop CRUD, match browsing,
question answering, and the apply *authorisation* click are synchronous.

**It is a funnel with lossy stages.** Postings scanned ≫ matches ≫ applications submitted
≫ responses. The knowledge base devotes an entire section ("Understanding Results") to
explaining this to confused users — few matches, broader-than-expected matches, matches vs.
applications. That tells us funnel visibility is a real product requirement, not a nice-to-have.

## 3. Application methods — the most important observed mechanic

LoopCV has three automated channels and one manual fallback, and it "automatically chooses
the most efficient method available for each posting." The selection inputs are: the job
platform, whether an employer email was found, whether a form exists, whether the platform
requires an authenticated session, and which automation toggles the candidate enabled.

| Method | Precondition | What happens |
|---|---|---|
| Automatic email | "Automatically send emails" on **and** an employer email was found | Candidate's template is sent with the CV attached |
| Automatic form | "Auto-fill application forms" on **and** the posting exposes a form | Required fields (phone, city) filled, CV attached where supported |
| Extension-supported | Platform requires a logged-in session (LinkedIn, Indeed, Dice, Reed, StepStone, GulfTalent) | Candidate applies in bulk via the Chrome extension, in their own browser session |
| Manual | Employer requires an account LoopCV cannot access | Candidate is handed the job link |

The extension channel is the key legal and architectural insight. LoopCV does **not** attempt
to automate authenticated job boards server-side. It pushes that work into the candidate's own
authenticated browser, under the candidate's own session. MyJobFyt adopts the same boundary as
a deliberate policy, not merely a limitation: server-side automation only where a public form
or a published ATS API exists; anything requiring the candidate's session happens client-side
with the candidate present.

## 4. Match statuses

Eleven observed statuses, which together form the application state machine:

`Applied Automatically`, `Email Sent`, `Applied Externally`, `Ready to Apply`,
`Questions Pending`, `Extension Supported`, `Already Applied / Email Already Sent`,
`External Apply Required`, `Submitting`, `Failed`, `Expired`.

Several details are load-bearing:

- **`Already Applied` is company-scoped with a six-month window.** LoopCV suppresses a second
  application to the same company within six months. This is a per-candidate, per-company
  dedupe rule distinct from posting-level deduplication.
- **`Submitting` is a visible in-flight state**, so submission is a durable, observable job
  rather than a request-scoped action.
- **`Failed` is retryable** by the candidate, and lists its causes as temporary errors,
  timeouts, or the employer no longer accepting applications.
- **`Expired` is terminal** and driven by the posting disappearing upstream, which implies a
  recurring liveness check on known postings.

## 5. Rejection taxonomy

Rejected matches are retained, categorised, and browsable — the candidate can still apply to
a rejected posting. The categories are: job title, location, remote/hybrid preference,
language (LoopCV covers English, French, German, Spanish only), required/excluded keywords,
company exclusions, and CV relevance.

Retaining rejections with a reason is an unusually good design choice and MyJobFyt keeps it.
It converts "why am I getting no matches?" from a support ticket into a self-service screen,
and it gives the matching engine a labelled evaluation set for free.

## 6. Observed weaknesses MyJobFyt will not reproduce

| Observed in LoopCV | Problem | MyJobFyt decision |
|---|---|---|
| Loops cannot be paused | Candidate's only options are delete or let it keep applying | Loops have an explicit lifecycle: `draft`, `active`, `paused`, `archived` |
| Matching level is a 3-way Low/Middle/High knob | Opaque; candidate cannot tell why a job scored as it did | Explainable scores: per-dimension sub-scores and human-readable reasons on every match |
| Pricing/limit documentation is self-contradictory | Two FAQ answers give different free-tier limits | Limits are a single source of truth in config, surfaced in the UI and the API |
| CV tailoring works for manual applications only | The highest-value AI feature is cut off from the automated path | Tailoring is a first-class pipeline stage available to every submission channel |
| Automation is opt-in-then-forget | Applications can be sent without per-application awareness | Every submission requires a recorded authorisation: per-application, or an explicit, bounded standing authorisation with a daily cap and full audit trail |
| No visible provenance for a given application | Candidate cannot see what was actually sent | Immutable application-event log storing the exact rendered artefacts |

## 7. Plan tiers (observed, incomplete)

The public pricing page does not expose a full matrix. Confirmed: a free forever plan with no
time limit; paid plans "from €9.99/month"; tiers named *Pro*, *Premium*, and *Done For You*
appear in the knowledge base and FAQ. Gated behind paid tiers, per the knowledge base:
worldwide remote search, SMTP configuration, CV tailoring. Gated behind Premium or
Done-For-You: AI screening-question answering.

Numeric limits are not reliably published, so MyJobFyt does not guess them. Entitlements are
modelled as named, config-driven quotas (`daily_applications`, `active_loops`, `cv_slots`,
`ai_tokens_month`, feature flags) with the free-tier values set conservatively and documented
in `docs/ENTITLEMENTS.md`.

## 8. Confirmed external data surface

Supported boards named in the knowledge base, by region:

- **Global:** LinkedIn, Indeed, Glassdoor, CareerJet, SimplyHired, Adzuna, Jooble, CareerAddict
- **United States:** USAJobs, Dice
- **Europe:** Reed.co.uk, CV Library, Xing, Workable, InfoJobs, StepStone, Skywalker
- **Asia-Pacific & Oceania:** Naukri, Instahyre, Seek
- **Middle East:** GulfTalent, NaukriGulf
- **Specialised:** Coroflot

Plus career-page search as a separate, tier-dependent source. MyJobFyt's source-adapter
selection is driven by legal accessibility rather than by this list — see
`INTEGRATION_ANALYSIS.md`, which classifies each of these into official API, authorised feed,
partner ATS integration, or not-implementable.

## 9. Evidence from the public MCP connector schemas

LoopCV publishes an MCP connector whose tool schemas leak a little of the public API's shape
and confirm the Loop model. `create_job_search` accepts `title` (required), `location`,
`remote`, `hybrid`, `keywords[]`, `experience` (`Junior|Associate|Senior|Executive|All`), and
`employmentType` (`Full-time|Contract|Part-time|Temporary|Internship`). It returns a search
identifier. `get_job_results` pages over matches by `searchId` with `limit`/`offset`
(max 100). `get_match_details` fetches one match by `matchId`, described as the `jobId`.

Two useful conclusions: the enumerations above are the real public vocabulary for seniority
and employment type, so MyJobFyt adopts compatible enums; and match identity is posting
identity in LoopCV's model. MyJobFyt separates them — a `job_posting` is global and shared,
a `job_match` is the per-candidate, per-loop evaluation of it — because collapsing the two
makes shared posting storage and cross-candidate deduplication impossible.

## 10. What this means for MyJobFyt's architecture

1. **Event-driven core.** The pipeline is a chain of lossy asynchronous stages with
   independent failure and retry characteristics. NATS JetStream with per-stage consumers,
   idempotency keys, and dead-letter handling is the right backbone.
2. **Shared postings, per-candidate matches.** Deduplicate globally; evaluate per candidate.
3. **Explainability is a feature, not debug output.** Match scores and rejection reasons are
   persisted, user-visible data.
4. **Authorisation is the product's integrity boundary.** Submission requires recorded consent.
5. **Funnel observability is a user-facing requirement**, which conveniently makes it an
   operational one too — the same event log serves the candidate's analytics and the admin's
   end-to-end trace.
6. **The authenticated-board boundary is a policy.** Server-side automation stops where a
   candidate's own session begins.
