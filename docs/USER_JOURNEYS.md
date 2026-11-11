# User Journeys

Journeys are written as the sequence a real person experiences, with the system's
synchronous and asynchronous responsibilities separated, because the gap between
"I clicked" and "something happened" is where this product lives or dies.

---

## J1. New candidate: sign-up to first match

**Actor:** unauthenticated visitor. **Goal:** see relevant jobs. **Success:** at least one scored match.

| # | Candidate does | System does synchronously | System does asynchronously | Candidate sees |
|---|---|---|---|---|
| 1 | Submits email + password | Validates, hashes (argon2id), creates `user` + `candidate_profile`, issues tokens | Emits `candidate.created`; queues verification email | "Check your inbox" + onboarding step 1 |
| 2 | Clicks verification link | Marks verified, single-use token consumed | — | "Email verified" |
| 3 | Fills profile (name, phone, city, country) | Validates, saves per section, recomputes completeness | — | Completeness ring advances |
| 4 | Uploads CV (PDF) | Magic-byte check, size cap, stores to object storage, creates `cv_documents` as `parsing` | AV scan → text extraction → structured LLM extraction → `cv.parsed` | "Analysing your CV…" with live status |
| 5 | Reviews parsed CV | Saves candidate corrections as authoritative | Re-embeds skills on change | Editable experience/skills with confidence flags |
| 6 | Creates first Loop (wizard, 4 steps) | Per-step validation, entitlement checks, draft persistence | On activation: `loop.activated`, first discovery scheduled immediately | Review summary, then "Your Loop is running" |
| 7 | Waits | — | Discovery → normalisation → dedupe → matching per Loop | Empty state that *explains the pipeline stage*, with expected timing |
| 8 | Returns | Serves paginated matches | Continuing discovery | Match cards with score and top reasons |

**Critical UX constraint:** step 7 is the product's most dangerous moment — the candidate has
done work and has nothing yet. The empty state must name the current stage ("searching 9
sources", "scoring 240 postings") rather than showing a generic spinner. First matches should
arrive in minutes, not on the next daily cycle, which is why activation triggers an immediate
discovery run rather than waiting for the scheduler.

**Failure branches:** image-only PDF → explicit "we can't read this, please upload a text PDF"
at step 4, not an empty profile at step 5. No matches at step 8 → rejection-reason breakdown
plus one-click filter loosening, not a blank page.

---

## J2. Reviewing and authorising an application

**Actor:** verified candidate with matches. **Success:** application submitted with recorded consent.

1. Opens **All Matches**, sorts by score.
2. Opens a match → sees the full score breakdown (title, skills, experience, seniority,
   location, employment type), the posting, and the detected submission method with its reason.
3. Optionally clicks **Tailor CV** → async tailoring → reviews the **diff** → accepts.
   The containment validator has already rejected any output introducing a new employer,
   title, date, certification, or skill.
4. Clicks **Prepare application** → async preparation materialises the exact artefacts:
   CV version, rendered cover letter, rendered email body or mapped form fields.
5. If the form has screening questions → status `questions_pending`, candidate answers in the
   **Questions** tab (or AI drafts them on PR/DFY, escalating anything unsupported by profile data).
6. Reviews the **exact artefacts that will be sent** and clicks **Authorise & submit**.
   An immutable `application_authorisations` row is written.
7. Status goes `submitting` → `submitted` / `email_sent`. Timeline shows every hop.
8. On `failed`, the candidate sees the reason and a retry affordance; on `outcome_unknown`
   the system explicitly says it cannot confirm, and offers manual verification — it never guesses.

**Standing authorisation variant:** in Loop settings the candidate grants a bounded standing
authorisation — one Loop, a daily cap, an expiry, a minimum match score — and sees a
plain-language summary of exactly what that permits. Every submission still records a reference
to that authorisation. A visible **Revoke** control, and **Pause Loop**, both take effect before
the next send.

---

## J3. Extension-assisted apply (authenticated boards)

1. Matches on LinkedIn/Indeed/Dice/Reed/StepStone/GulfTalent get status `extension_pending`.
2. Candidate installs the extension and signs in to it with a short-lived, narrowly-scoped token.
3. Extension pulls its queue of prepared applications with pre-mapped field values.
4. In the candidate's **own** browser, under the candidate's **own** session, it fills each form.
5. Screening questions appear inline — answered from saved answers, or by the candidate.
6. **The candidate submits.** If a CAPTCHA or any anti-bot challenge appears, the extension
   stops and hands over control; it never attempts to solve it.
7. Results report back; the backend appends `application_events` and updates status.

**Boundary:** the server never receives job-board credentials or session cookies. This is the
line between legitimate assistance and account compromise, and it is not crossed.

---

## J4. Diagnosing "I'm not getting matches"

1. Loop detail shows the funnel for the Loop: scanned → rejected (by category) → matched.
2. **Rejected matches** browser, filtered by reason: title, location, work mode, language,
   required keyword missing, excluded keyword, excluded company, CV relevance.
3. Candidate sees, for example, that 180 of 240 postings were rejected on "required keyword
   missing" — and that the culprit is a single over-narrow keyword.
4. Filter editor shows a live preview: "this change would have matched 44 more of your last
   240 postings."
5. Candidate loosens the filter; a backfill re-scores recent postings against the new config
   rather than making them wait a day.
6. Any rejected posting can still be applied to directly.

This journey is why rejections are persisted with reasons. It converts the single largest
support burden into self-service.

---

## J5. Improving the CV

1. **CV Checker** → async analysis → overall score plus sub-scores for formatting, keyword
   coverage, impact/quantification, length, contactability, ATS-parseability.
2. Each recommendation is anchored to a specific location in the CV and is specific enough to act on.
3. Candidate edits in the **CV Builder** (split-pane: content editor, live preview that is the
   same renderer as the final PDF) or uploads a revised file.
4. Re-check shows before/after. A new CV **version** is created; prior versions are retained
   because submitted applications reference them.
5. Candidate sets the new version as the Loop's CV; later applications use it, historical ones
   remain reproducible.
6. After sufficient volume, **CV performance** compares versions on reply and interview rate —
   with confidence intervals, and an explicit "not enough data yet" when the sample is thin.

---

## J6. Admin traces a failed CV parse

1. Alert fires: CV parse dead-letter depth > 0.
2. Admin opens **Admin → Workflows**, filters `cv.parse` failures in the last hour.
3. Picks one; the trace view reconstructs, from the correlation ID: upload request → validation →
   AV scan → object-storage write → `cv.uploaded` publish → consumer receipt (with redelivery
   count) → text extraction → AI call (model, latency, tokens, finish reason) → schema
   validation failure → dead-letter, with timestamps and durations at every hop.
4. Root cause is visible: the document produced zero extractable text (scanned image).
5. Admin sees the candidate was correctly notified; no PII, CV text, or prompt body appears
   anywhere in the trace or logs.
6. Admin requeues the dead-lettered message after the fix; idempotency makes the replay safe.

---

## J7. Subscription upgrade at the point of friction

1. Candidate hits a quota — e.g. the daily application cap.
2. Matches **keep accumulating**; submissions **queue** rather than fail. Nothing is lost.
3. The quota indicator explains exactly what is capped, what their current limit is, and when
   it resets.
4. Upgrade → checkout → provider webhook (signature-verified, idempotent) updates the subscription.
5. Entitlements refresh; queued submissions resume automatically within the new cap.
6. Downgrade and cancellation take effect at period end, and the UI states precisely what will
   change and when.

---

## J8. Account deletion

1. Settings → Privacy → **Delete account**; confirm by typing the account email; re-authenticate.
2. Account enters a 7-day grace period: all automation halts immediately, sign-in still works,
   and a single-click **Cancel deletion** is offered.
3. After 7 days, an idempotent, resumable erasure job removes PII and CV objects from object
   storage, anonymises rows required for aggregate integrity, and runs a verification pass that
   fails loudly rather than reporting a partial erasure as complete.
4. Candidate receives written confirmation of what was erased and what was retained (and why).
5. **Data export** is offered before deletion: a single-use, 1-hour signed download containing
   every table's data plus the original CV files.

Deletion is available on **every** tier. LoopCV restricting it to its basic tier is a defect we
do not reproduce.
