-- +goose Up
-- +goose StatementBegin

-- Plans. Quota values are OUR conservative choices, not LoopCV's: its public pricing
-- page contradicts itself on the free-tier limit, so guessing would be worse than
-- choosing deliberately and documenting it (see docs/PRODUCT_ANALYSIS.md section 7).
INSERT INTO plans (slug, name, price_cents, currency, interval, sort_order, quotas, features) VALUES
('free', 'Free', 0, 'EUR', 'none', 0,
 '{"daily_applications": 5, "active_loops": 1, "cv_slots": 2, "ai_tokens_month": 50000, "daily_matches": 25}'::jsonb,
 '{"worldwide_remote": false, "smtp_custom": false, "cv_tailoring": false, "ai_question_answering": false, "career_page_search": false, "analytics_full": false}'::jsonb),

('pro', 'Pro', 999, 'EUR', 'month', 1,
 '{"daily_applications": 25, "active_loops": 3, "cv_slots": 10, "ai_tokens_month": 500000, "daily_matches": 200}'::jsonb,
 '{"worldwide_remote": true, "smtp_custom": true, "cv_tailoring": true, "ai_question_answering": false, "career_page_search": true, "analytics_full": true}'::jsonb),

('premium', 'Premium', 2999, 'EUR', 'month', 2,
 '{"daily_applications": 100, "active_loops": 10, "cv_slots": 25, "ai_tokens_month": 2000000, "daily_matches": 1000}'::jsonb,
 '{"worldwide_remote": true, "smtp_custom": true, "cv_tailoring": true, "ai_question_answering": true, "career_page_search": true, "analytics_full": true}'::jsonb),

('done_for_you', 'Done For You', 9999, 'EUR', 'month', 3,
 '{"daily_applications": 250, "active_loops": 25, "cv_slots": 50, "ai_tokens_month": 10000000, "daily_matches": 5000}'::jsonb,
 '{"worldwide_remote": true, "smtp_custom": true, "cv_tailoring": true, "ai_question_answering": true, "career_page_search": true, "analytics_full": true, "managed_service": true}'::jsonb);

-- Job sources. Only Tier-1 and Tier-2 integrations from docs/INTEGRATION_ANALYSIS.md
-- start life enabled; everything requiring a partnership is recorded as
-- 'partnership_required' so the admin UI can explain WHY a board the candidate expects
-- is unavailable, instead of silently omitting it.
INSERT INTO job_sources (slug, name, kind, state, regions, rate_limit_per_min, legal_basis, terms_url, capabilities) VALUES
('adzuna', 'Adzuna', 'official_api', 'disabled', '{GB,US,DE,FR,NL,AU,CA,IN,ZA}', 20,
 'Public API with self-service application credentials', 'https://developer.adzuna.com/',
 '{"filters": ["title","location","salary","employment_type","distance"], "pagination": "page", "max_page_size": 50}'::jsonb),

('jooble', 'Jooble', 'official_api', 'disabled', '{GB,US,DE,FR,PL,IN}', 20,
 'Public API with self-service key', 'https://jooble.org/api/about',
 '{"filters": ["title","location","salary"], "pagination": "page", "max_page_size": 20}'::jsonb),

('usajobs', 'USAJobs', 'official_api', 'disabled', '{US}', 30,
 'Official US government open API', 'https://developer.usajobs.gov/',
 '{"filters": ["title","location","seniority","employment_type"], "pagination": "page", "max_page_size": 500}'::jsonb),

('reed', 'Reed.co.uk', 'official_api', 'disabled', '{GB}', 20,
 'Official API with self-service key', 'https://www.reed.co.uk/developers',
 '{"filters": ["title","location","salary","employment_type"], "pagination": "offset", "max_page_size": 100}'::jsonb),

('careerjet', 'Careerjet', 'official_api', 'disabled', '{GB,US,DE,FR,ES,IT,NL,IN,ZA}', 20,
 'Public search API under affiliate terms', 'https://www.careerjet.com/partners/api/',
 '{"filters": ["title","location","employment_type"], "pagination": "page", "max_page_size": 99}'::jsonb),

('remotive', 'Remotive', 'authorised_feed', 'disabled', '{WORLDWIDE}', 10,
 'Public JSON feed offered for third-party use', 'https://remotive.com/api-documentation',
 '{"filters": ["title","category"], "pagination": "none", "remote_only": true}'::jsonb),

('remoteok', 'RemoteOK', 'authorised_feed', 'disabled', '{WORLDWIDE}', 10,
 'Public JSON feed offered for third-party use', 'https://remoteok.com/api',
 '{"filters": ["title"], "pagination": "none", "remote_only": true}'::jsonb),

('arbeitnow', 'Arbeitnow', 'authorised_feed', 'disabled', '{DE,EU}', 10,
 'Public JSON feed offered for third-party use', 'https://documenter.getpostman.com/view/18545278/UVkmp8A8',
 '{"filters": ["title","location"], "pagination": "page", "max_page_size": 100}'::jsonb),

('greenhouse', 'Greenhouse Job Boards', 'ats_api', 'disabled', '{WORLDWIDE}', 60,
 'Public job-board API that powers employers'' own careers pages', 'https://developers.greenhouse.io/job-board.html',
 '{"filters": [], "pagination": "none", "per_company": true, "supports_submission": true}'::jsonb),

('lever', 'Lever Postings', 'ats_api', 'disabled', '{WORLDWIDE}', 60,
 'Public postings API that powers employers'' own careers pages', 'https://github.com/lever/postings-api',
 '{"filters": ["team","location","commitment"], "pagination": "none", "per_company": true, "supports_submission": true}'::jsonb),

('ashby', 'Ashby Job Board', 'ats_api', 'disabled', '{WORLDWIDE}', 60,
 'Public job-board API that powers employers'' own careers pages', 'https://developers.ashbyhq.com/',
 '{"filters": [], "pagination": "none", "per_company": true, "supports_submission": true}'::jsonb),

('workable', 'Workable', 'ats_api', 'disabled', '{WORLDWIDE}', 60,
 'Public account jobs endpoint per employer subdomain', 'https://workable.readme.io/',
 '{"filters": [], "pagination": "cursor", "per_company": true, "supports_submission": true}'::jsonb),

('smartrecruiters', 'SmartRecruiters', 'ats_api', 'disabled', '{WORLDWIDE}', 60,
 'Public postings API per employer', 'https://developers.smartrecruiters.com/',
 '{"filters": ["location"], "pagination": "offset", "per_company": true, "supports_submission": true}'::jsonb),

-- Partnership-gated. Present so the product can explain their absence; never scraped.
('linkedin', 'LinkedIn', 'official_api', 'partnership_required', '{WORLDWIDE}', 1, NULL,
 'https://www.linkedin.com/legal/user-agreement',
 '{"note": "No public job-search API for this use case. Reachable only through the candidate-session browser extension."}'::jsonb),

('indeed', 'Indeed', 'official_api', 'partnership_required', '{WORLDWIDE}', 1, NULL,
 'https://www.indeed.com/legal',
 '{"note": "Publisher/search API closed to new third parties; partner-only."}'::jsonb),

('glassdoor', 'Glassdoor', 'official_api', 'partnership_required', '{WORLDWIDE}', 1, NULL,
 'https://www.glassdoor.com/about/terms.htm',
 '{"note": "API closed; partner agreement required."}'::jsonb),

('stepstone', 'StepStone', 'official_api', 'partnership_required', '{DE,AT,BE,NL}', 1, NULL, NULL,
 '{"note": "Partner API only."}'::jsonb),

('dice', 'Dice', 'official_api', 'partnership_required', '{US}', 1, NULL, NULL,
 '{"note": "Partner API only; extension channel otherwise."}'::jsonb),

('seek', 'Seek', 'official_api', 'partnership_required', '{AU,NZ}', 1, NULL, NULL,
 '{"note": "Partner API only."}'::jsonb),

('naukri', 'Naukri', 'official_api', 'partnership_required', '{IN}', 1, NULL, NULL,
 '{"note": "Partner API only."}'::jsonb),

('gulftalent', 'GulfTalent', 'official_api', 'partnership_required', '{AE,SA,QA,KW}', 1, NULL, NULL,
 '{"note": "Partner API only; extension channel otherwise."}'::jsonb);

-- System email templates. Placeholders are a fixed allowlist, validated on render;
-- there is deliberately no expression language, so a template can never be a
-- server-side injection vector.
INSERT INTO email_templates (user_id, name, subject, body, is_system, placeholders_used) VALUES
(NULL, 'Standard application',
 'Application for {{job_title}} at {{company}}',
 E'Dear Hiring Team,\n\nI am writing to apply for the {{job_title}} position at {{company}}. My CV is attached for your consideration.\n\nI would welcome the opportunity to discuss how my experience fits the role.\n\nKind regards,\n{{candidate_name}}',
 true, '{job_title,company,candidate_name}'),

(NULL, 'Brief and direct',
 '{{job_title}} — application from {{candidate_name}}',
 E'Hello,\n\nI would like to be considered for the {{job_title}} role at {{company}}. My CV is attached.\n\nThank you for your time.\n\n{{candidate_name}}',
 true, '{job_title,company,candidate_name}');

-- Notification categories, defaulting on for the ones a candidate must not miss
-- (something needs their authorisation, something failed) and digest-only for
-- high-volume informational ones.
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM email_templates WHERE is_system;
DELETE FROM job_sources;
DELETE FROM plans;
-- +goose StatementEnd
