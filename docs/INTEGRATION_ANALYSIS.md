# Integration Analysis

LoopCV's knowledge base names roughly 25 job boards. That list is a *product* claim, not an
*engineering* plan: most of those platforms have no lawful programmatic access for a
third-party applicant tool. This document classifies every named source plus additional
candidates by how we can legitimately integrate, and states what MyJobFyt will and will not do.

**Governing constraints** (from `CLAUDE.md`, non-negotiable):
- No bypassing CAPTCHAs, authentication, access controls, or anti-bot protections.
- No scraping of credential-gated content.
- `robots.txt` honoured; published user agent; conservative rate limits.
- Credentials in a secret manager, never in Git.
- All third-party content is untrusted input (prompt-injection and XSS boundary).

Each adapter declares a `legal_basis` and a `terms_url` in `job_sources`, and the admin UI
shows it. An integration without a recorded basis cannot be enabled.

---

## Tier 1 — Official public job-search APIs (implement first)

These publish documented APIs with free or self-service keys for third-party job-search tools.
Exact endpoint shapes and current terms are verified at implementation time per adapter; the
adapter's conformance test is the proof.

| Source | Access | Auth | Notes |
|---|---|---|---|
| **Adzuna** | Public API, self-service | `app_id` + `app_key` | Broad coverage across many countries; good salary data. Best first adapter. |
| **Jooble** | Public API, self-service key | API key (per-country endpoint) | Large aggregate index, simple POST search. |
| **USAJobs** | Official US government API | API key + registered email in header | Authoritative for US federal roles; generous terms. |
| **Reed.co.uk** | Official API, self-service | API key (HTTP Basic) | Strong UK coverage. |
| **Careerjet** | Public search API, affiliate | affiliate id | Wide international coverage. |
| **The Muse** | Public API | optional key | Curated, good company metadata. |
| **Remotive / RemoteOK / Arbeitnow / Jobicy / Himalayas** | Public JSON feeds | none | Remote-only; cheap breadth for remote searches, which is a large share of demand. |

**Implementation order:** Adzuna → Jooble → Remotive/RemoteOK → USAJobs → Reed → Careerjet.
Adzuna alone validates the whole pipeline end-to-end with real data and no partnership
negotiation, which is why it is first.

## Tier 2 — ATS public job-board APIs (highest data quality)

These are the endpoints a company's own careers page calls to render its public job list. They
are public by design, return clean structured data, and crucially expose a **real application
path** — which makes them the only sources where fully server-side submission is legitimate.

| Provider | Board endpoint shape | Submission |
|---|---|---|
| **Greenhouse** | `boards-api.greenhouse.io/v1/boards/{token}/jobs` (+`?content=true`) | Public job-board POST endpoint with documented fields |
| **Lever** | `api.lever.co/v0/postings/{company}` | Public postings apply endpoint |
| **Ashby** | `api.ashbyhq.com/posting-api/job-board/{name}` | Public application endpoint |
| **Workable** | public account jobs endpoint per subdomain | Public apply endpoint |
| **SmartRecruiters** | `api.smartrecruiters.com/v1/companies/{id}/postings` | Public posting-apply endpoint |
| **Recruitee / Teamtailor / Personio** | public careers API per account | Varies; form-based |

**Why this tier matters most.** It inverts the usual trade-off. Aggregators give breadth with
a weak apply path; ATS boards give depth with a strong one. A company allowlist, seeded from
the ATS provider detected on each company's careers page, turns "apply to 200 jobs" from a
scraping problem into an API problem. `D4` (career-page discovery) is implemented as ATS
detection plus these endpoints — not as generic HTML crawling.

**Application submission is implemented here only**, plus authorised email and the
extension-assisted channel. Every submission still requires a recorded candidate authorisation.

## Tier 3 — Partnership or licence required (not implemented without an agreement)

| Source | Why blocked | Path if ever needed |
|---|---|---|
| **LinkedIn** | No public job-search API for this use case; ToS forbids automated access; strong anti-bot | Candidate-session extension channel only (F6) |
| **Indeed** | Publisher/search API closed to new third parties; partner-only | Formal partner application |
| **Glassdoor** | API closed | Partner agreement |
| **SimplyHired, CareerAddict, Skywalker, Coroflot** | No documented public API | Authorised feed agreement |
| **StepStone, Xing, InfoJobs, CV Library** | Partner APIs only | Regional partnership |
| **Seek, Naukri, NaukriGulf, Instahyre, GulfTalent** | Partner APIs only | Regional partnership |
| **Dice** | Partner API | Partner application |

These are **not** scraped. Where LoopCV reaches LinkedIn, Indeed, Dice, Reed, StepStone, and
GulfTalent it does so through its Chrome extension, in the candidate's own authenticated
session — which is exactly the boundary we adopt in `F6`. The adapter registry carries entries
for these sources in a `partnership_required` state so the admin UI explains *why* a board the
candidate expects is unavailable, rather than silently omitting it.

## Tier 4 — Company career pages (constrained)

Generic HTML crawling is a last resort and is tightly bounded:
- ATS provider detection first; if a Tier-2 provider is found, use its API instead.
- `robots.txt` fetched, cached, and obeyed per host.
- Published user agent identifying the platform with a contact URL.
- ≤ 1 request per 5 seconds per host; nightly at most.
- Response size capped; no JavaScript execution; no login; no CAPTCHA interaction.
- Any host returning a challenge is disabled and recorded.

---

## Non-job integrations

| Capability | Choice | Rationale |
|---|---|---|
| **Object storage** | S3-compatible; MinIO locally, S3/R2/Spaces in production | CVs must not live in Postgres; presigned URLs keep the app off the download path |
| **AI provider** | Provider-abstracted; Anthropic Claude as the default | Structured outputs, long context for CVs + job descriptions, strong instruction adherence for the containment constraints in B6/F4 |
| **Embeddings** | Pluggable; a self-hosted sentence-transformer is viable | Semantic title/skill matching at volume; cheap to run locally, and the lexical fallback keeps matching alive if it is down |
| **Transactional email** | SMTP abstraction: Postmark/SES in production, Mailpit locally | Deliverability for verification and digests; separate from candidate outreach so a reputation problem in one cannot poison the other |
| **Candidate SMTP** | User-supplied host/port/credentials (Gmail/Yahoo app passwords), envelope-encrypted | Mirrors the observed LoopCV feature; improves deliverability and puts the mail in the candidate's own sent box |
| **Reply ingestion** | Per-application reply address + inbound webhook; IMAP as a fallback | Threads replies to applications without needing mailbox access |
| **Geocoding** | Nominatim self-hosted or a licensed provider | Location normalisation is required for dedupe and location scoring |
| **Antivirus** | ClamAV | Mandatory for an endpoint that accepts arbitrary documents from the internet |
| **Payments** | Stripe (webhook-authoritative, signature-verified, idempotent) | No card data touches our systems |
| **PDF rendering** | Headless Chromium in a sandboxed, network-isolated container | CV builder output; SSRF and local-file-inclusion risk demands isolation |
| **Observability** | OpenTelemetry → Collector → Prometheus/Grafana/Loki/Tempo | Required for the end-to-end trace in I2 |
| **Secrets** | SOPS + age for GitOps; External Secrets Operator in-cluster | Never plaintext in Git |

---

## Adapter contract

```go
type Adapter interface {
    Slug() string
    Capabilities() Capabilities      // supported filters, pagination, max page size, regions
    Search(ctx context.Context, spec SearchSpec, cursor string) (Page, error)
    Fetch(ctx context.Context, externalID string) (RawPosting, error)
    HealthCheck(ctx context.Context) error
}
```

Rules every adapter obeys:
1. **Declare capabilities.** The planner skips a query an adapter cannot serve instead of
   issuing it and failing — an adapter that cannot filter by work mode says so.
2. **Own your rate limit.** Enforced inside the adapter, so a scheduling bug cannot cause abuse.
3. **Return raw payloads.** Normalisation is a separate stage; the raw payload is retained for
   replay and for diagnosing schema drift.
4. **Resumable cursors.** Opaque strings persisted on the `DiscoveryRun`, so a worker crash
   resumes rather than restarts.
5. **Fail soft.** An adapter error degrades a run; it never fails it. Repeated failure opens a
   circuit breaker and alerts.
6. **Conformance-tested.** One shared test suite runs against every adapter using recorded
   fixtures, so a schema change upstream fails CI rather than corrupting production data.

## Submission adapter contract

```go
type Submitter interface {
    Method() ApplicationMethod
    CanSubmit(posting JobPosting) (bool, string)         // reason recorded on the application
    Prepare(ctx context.Context, in PrepareInput) (PreparedArtefacts, error)
    Submit(ctx context.Context, in SubmitInput) (Outcome, error)   // idempotency key required
}
```

`Submit` must be callable twice with the same idempotency key without producing two
applications. An outcome that cannot be determined returns `outcome_unknown`; it never guesses,
and the caller never retries it.
