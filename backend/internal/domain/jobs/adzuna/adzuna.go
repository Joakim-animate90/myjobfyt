// Package adzuna implements the Adzuna job-search adapter.
//
// Adzuna is the platform's first source because it needs no partnership: documented
// public API, self-service credentials, broad country coverage and unusually good salary
// data. The API shape here was verified against developer.adzuna.com at implementation
// time rather than recalled.
//
// Two of its behaviours shaped this code and are easy to get wrong:
//
//   - The default response format is JSONP, not JSON. A request that does not ask for
//     JSON gets a JavaScript function call wrapping the payload, which fails to parse in
//     a way that looks like a malformed response rather than a missing header.
//   - Salaries may be predicted by Adzuna rather than stated by the employer, flagged by
//     salary_is_predicted. That flag is carried through, never dropped.
package adzuna

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// Slug is the source's identifier, matching job_sources.slug.
const Slug = "adzuna"

// defaultBaseURL is the documented API root.
const defaultBaseURL = "https://api.adzuna.com/v1/api"

// maxResultsPerPage bounds one request.
//
// Adzuna does not document a maximum. Fifty is used because it is the page size the
// seeded capability record claims and because a larger request is more likely to time
// out than to be served: the cost of being wrong downwards is one extra request.
const maxResultsPerPage = 50

// Config configures the adapter.
type Config struct {
	AppID   string
	AppKey  string
	BaseURL string
	Timeout time.Duration
	// Limiter paces requests. Required: the adapter owns its rate limit so a
	// scheduling bug cannot cause abuse.
	Limiter jobs.Limiter
	// Countries are the ISO codes this deployment will search, uppercase.
	Countries []string
	// HTTPClient is injectable for tests.
	HTTPClient *http.Client
}

// Adapter is the Adzuna adapter.
type Adapter struct {
	appID     string
	appKey    string
	baseURL   string
	countries []string
	limiter   jobs.Limiter
	client    *http.Client
}

// New builds the adapter.
func New(cfg Config) (*Adapter, error) {
	if strings.TrimSpace(cfg.AppID) == "" || strings.TrimSpace(cfg.AppKey) == "" {
		return nil, fmt.Errorf("adzuna: both an app id and an app key are required")
	}

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}

	limiter := cfg.Limiter
	if limiter == nil {
		// Never silently unlimited. A missing limiter is a wiring mistake, and the safe
		// reading of it is "pace conservatively", not "send as fast as you can".
		limiter = jobs.NewTokenLimiter(20)
	}

	countries := make([]string, 0, len(cfg.Countries))
	for _, c := range cfg.Countries {
		if trimmed := strings.ToUpper(strings.TrimSpace(c)); trimmed != "" {
			countries = append(countries, trimmed)
		}
	}
	if len(countries) == 0 {
		countries = defaultCountries()
	}

	return &Adapter{
		appID: cfg.AppID, appKey: cfg.AppKey, baseURL: baseURL,
		countries: countries, limiter: limiter, client: client,
	}, nil
}

// defaultCountries are the markets Adzuna documents.
func defaultCountries() []string {
	return []string{"AT", "AU", "BE", "BR", "CA", "CH", "DE", "ES", "FR", "GB",
		"IN", "IT", "MX", "NL", "NZ", "PL", "SG", "US", "ZA"}
}

// Slug identifies the source.
func (a *Adapter) Slug() string { return Slug }

// Capabilities declares what Adzuna can serve.
//
// Honest in the negative direction too. Adzuna has no work-mode field, so it cannot
// filter to remote roles: claiming otherwise would have the planner issue such a search
// and get back every onsite job in the country, which reads as a successful run with
// wrong results.
func (a *Adapter) Capabilities() jobs.Capabilities {
	return jobs.Capabilities{
		Keywords:             true,
		Location:             true,
		Radius:               true,
		RemoteFilter:         false,
		WorkModeFilter:       false,
		EmploymentTypeFilter: true,
		SalaryFilter:         true,
		PostedWithinFilter:   true,
		Pagination:           true,
		MaxPageSize:          maxResultsPerPage,
		Regions:              a.countries,
		// Search already returns the fields the platform needs, and the description is
		// a snippet either way, so a per-posting detail call would spend the rate limit
		// for nothing.
		DetailFetch: false,
	}
}

// Search returns one page of results.
func (a *Adapter) Search(ctx context.Context, spec jobs.SearchSpec, cursor string) (jobs.Page, error) {
	page, err := parseCursor(cursor)
	if err != nil {
		return jobs.Page{}, err
	}

	country := strings.ToLower(spec.Country)
	if country == "" {
		// Adzuna requires a country in the path; there is no global search. Defaulting
		// rather than erroring, because a loop with no country set is a common
		// configuration rather than a fault.
		country = strings.ToLower(a.countries[0])
	}

	endpoint := fmt.Sprintf("%s/jobs/%s/search/%d", a.baseURL, country, page)
	query := a.queryFor(spec)

	body, err := a.get(ctx, endpoint, query)
	if err != nil {
		return jobs.Page{}, err
	}

	var response searchResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return jobs.Page{}, fmt.Errorf("adzuna: decode search response: %w", err)
	}

	fetchedAt := time.Now().UTC()
	postings := make([]jobs.RawPosting, 0, len(response.Results))
	for _, result := range response.Results {
		id := result.identifier()
		if id == "" {
			// Without a stable identifier the posting cannot be deduplicated or
			// recognised on a later run, so it would be re-imported for ever. Dropping
			// one posting is better than that.
			continue
		}
		payload, err := json.Marshal(result)
		if err != nil {
			continue
		}
		postings = append(postings, jobs.RawPosting{
			ExternalID: id,
			Payload:    payload,
			URL:        result.RedirectURL,
			FetchedAt:  fetchedAt,
		})
	}

	next := ""
	// A full page implies there may be more. Adzuna's count is unreliable, so paging
	// stops when a page comes back short rather than when the count says so.
	if len(response.Results) >= a.pageSize(spec) && len(response.Results) > 0 {
		next = strconv.Itoa(page + 1)
	}

	return jobs.Page{Postings: postings, NextCursor: next, Total: response.Count}, nil
}

// pageSize is how many results this search asks for.
func (a *Adapter) pageSize(spec jobs.SearchSpec) int {
	size := maxResultsPerPage
	if spec.Limit > 0 && spec.Limit < size {
		size = spec.Limit
	}
	return size
}

// queryFor builds the query string for a search.
func (a *Adapter) queryFor(spec jobs.SearchSpec) url.Values {
	query := url.Values{}
	query.Set("app_id", a.appID)
	query.Set("app_key", a.appKey)
	// Explicit, because the API's default is JSONP: without this the response is a
	// JavaScript function call and decoding fails in a way that looks like a malformed
	// payload rather than a missing parameter.
	query.Set("content-type", "application/json")
	query.Set("results_per_page", strconv.Itoa(a.pageSize(spec)))

	if len(spec.Keywords) > 0 {
		query.Set("what", strings.Join(spec.Keywords, " "))
	}
	if spec.Location != "" {
		query.Set("where", spec.Location)
	}
	if spec.RadiusKm > 0 {
		query.Set("distance", strconv.Itoa(spec.RadiusKm))
	}
	if spec.MinSalary > 0 {
		query.Set("salary_min", strconv.Itoa(spec.MinSalary))
	}
	if spec.PostedWithin > 0 {
		// Rounded up, so "within 12 hours" asks for one day rather than zero, which
		// Adzuna would read as no filter at all.
		days := int((spec.PostedWithin + 24*time.Hour - time.Nanosecond) / (24 * time.Hour))
		if days < 1 {
			days = 1
		}
		query.Set("max_days_old", strconv.Itoa(days))
	}

	// Adzuna expresses employment type as independent flags rather than one field, so
	// each requested type sets its own.
	for _, employment := range spec.EmploymentTypes {
		switch strings.ToLower(strings.TrimSpace(employment)) {
		case "full-time", "full_time", "fulltime":
			query.Set("full_time", "1")
		case "part-time", "part_time", "parttime":
			query.Set("part_time", "1")
		case "contract":
			query.Set("contract", "1")
		case "permanent":
			query.Set("permanent", "1")
		}
	}

	return query
}

// get performs a rate-limited request.
func (a *Adapter) get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	// Paced before the request is built, so every path out of this adapter is limited.
	if err := a.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("adzuna: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// A generic client name. The platform does not identify its users to a third party.
	req.Header.Set("User-Agent", "myjobfyt-job-discovery/1.0")

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("adzuna: request failed: %w", err)
	}
	defer resp.Body.Close()

	// Bounded, because an error page can be arbitrarily large and this is a third
	// party's response body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("adzuna: read response: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &jobs.ErrRateLimited{Slug: Slug, RetryAfter: retryAfter(resp.Header)}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		// Distinguished because the fix is ours — wrong or revoked credentials — not a
		// matter of waiting for Adzuna to recover.
		return nil, fmt.Errorf("adzuna: credentials rejected (HTTP %d); check ADZUNA_APP_ID and ADZUNA_APP_KEY",
			resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("adzuna: HTTP %d: %s", resp.StatusCode, snippet(body))
	}

	return body, nil
}

// retryAfter reads the Retry-After header, in either of its documented forms.
func retryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	// The header may also be an HTTP date.
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}

// snippet bounds an error body included in a message.
func snippet(body []byte) string {
	const max = 200
	text := strings.TrimSpace(string(body))
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// Fetch is not supported: search already returns everything the platform needs.
func (a *Adapter) Fetch(_ context.Context, externalID string) (jobs.RawPosting, error) {
	return jobs.RawPosting{}, fmt.Errorf(
		"adzuna: no detail endpoint; Capabilities().DetailFetch is false so this should not be called (id %q)",
		externalID)
}

// HealthCheck confirms the API is reachable and the credentials work.
//
// Uses the version endpoint rather than a search: it is the cheapest call that still
// exercises authentication, so a half-open probe costs almost nothing.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	query := url.Values{}
	query.Set("app_id", a.appID)
	query.Set("app_key", a.appKey)
	query.Set("content-type", "application/json")

	if _, err := a.get(ctx, a.baseURL+"/version", query); err != nil {
		return err
	}
	return nil
}

// parseCursor reads a page number from an opaque cursor.
func parseCursor(cursor string) (int, error) {
	if strings.TrimSpace(cursor) == "" {
		// Adzuna pages are 1-indexed.
		return 1, nil
	}
	page, err := strconv.Atoi(cursor)
	if err != nil || page < 1 {
		return 0, fmt.Errorf("adzuna: unusable cursor %q", cursor)
	}
	return page, nil
}
