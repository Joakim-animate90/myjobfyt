// Package remotefeeds implements the three free, credential-free job feeds.
//
// Remotive, Remote OK and Arbeitnow are public JSON feeds published for third-party use.
// They cost nothing and need no partnership, which makes them the cheapest breadth
// available — but their terms impose real obligations, and those obligations are encoded
// here rather than left as something a UI is expected to remember:
//
//   - Remotive and Remote OK both state they will suspend API access without a visible
//     link back and a named credit. Every posting therefore carries its Attribution,
//     and Remote OK specifically requires the link be followable rather than nofollow.
//   - Remotive advises at most four requests a day and says excessive requests will be
//     blocked, so its limiter is expressed as an interval rather than a per-minute rate.
//   - Arbeitnow asks only that callers not abuse the feed and would appreciate a link.
//
// One correction to the issue's framing, found by reading the live feeds rather than
// their descriptions: Arbeitnow is not a remote-only source. Of 300 postings sampled,
// 286 were not remote — it is a general German job board carrying a remote flag. It
// therefore declares a remote filter it applies itself, not RemoteOnly.
package remotefeeds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// Config is shared by the three adapters.
type Config struct {
	// BaseURL overrides the feed's documented endpoint, for tests.
	BaseURL string
	Timeout time.Duration
	// Limiter paces requests. The adapter owns its rate limit, so a scheduling bug
	// cannot cause abuse — which for these feeds is the difference between working
	// access and being blocked.
	Limiter    jobs.Limiter
	HTTPClient *http.Client
}

// client is the shared HTTP plumbing.
type client struct {
	baseURL string
	limiter jobs.Limiter
	http    *http.Client
	slug    string
}

func newClient(slug, defaultURL string, cfg Config, defaultLimiter jobs.Limiter) *client {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultURL
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}
	limiter := cfg.Limiter
	if limiter == nil {
		// Never silently unlimited. For these feeds in particular, the stated
		// consequence of excessive requests is losing access entirely.
		limiter = defaultLimiter
	}
	return &client{baseURL: baseURL, limiter: limiter, http: httpClient, slug: slug}
}

// get fetches and returns a JSON body.
func (c *client) get(ctx context.Context, endpoint string, query url.Values) ([]byte, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	target := endpoint
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: build request: %w", c.slug, err)
	}
	req.Header.Set("Accept", "application/json")
	// A generic client name. The platform does not identify its users to a third party.
	req.Header.Set("User-Agent", "myjobfyt-job-discovery/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: request failed: %w", c.slug, err)
	}
	defer resp.Body.Close()

	// Bounded: Arbeitnow's single page is over two megabytes, and an error page from
	// any of these can be arbitrarily large.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("%s: read response: %w", c.slug, err)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &jobs.ErrRateLimited{Slug: c.slug, RetryAfter: retryAfter(resp.Header)}
	case resp.StatusCode == http.StatusForbidden:
		// These feeds suspend access for terms violations rather than returning 401, so
		// a 403 here is more likely a policy problem than a transient one and the
		// message has to say so.
		return nil, fmt.Errorf("%s: access refused (HTTP 403); these feeds suspend access "+
			"for excessive requests or missing attribution, so check the rate limit and "+
			"that postings are credited", c.slug)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d: %s", c.slug, resp.StatusCode, snippet(body))
	}

	return body, nil
}

func retryAfter(header http.Header) time.Duration {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	var seconds int
	if _, err := fmt.Sscanf(value, "%d", &seconds); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		if wait := time.Until(when); wait > 0 {
			return wait
		}
	}
	return 0
}

func snippet(body []byte) string {
	const max = 200
	text := strings.TrimSpace(string(body))
	if len(text) <= max {
		return text
	}
	return text[:max] + "…"
}

// matchesKeywords filters a posting client-side.
//
// None of these feeds supports server-side keyword search, so the adapters declare the
// capability and apply it themselves. That is honest: the planner's alternative is to
// skip these sources for every keyword search, which would exclude them from nearly
// every real loop.
func matchesKeywords(keywords []string, fields ...string) bool {
	if len(keywords) == 0 {
		return true
	}
	haystack := strings.ToLower(strings.Join(fields, " \n "))
	// Any keyword, not all: a candidate searching "go rust" wants either, and
	// requiring both would return almost nothing from a feed of a few hundred.
	for _, keyword := range keywords {
		if trimmed := strings.ToLower(strings.TrimSpace(keyword)); trimmed != "" {
			if strings.Contains(haystack, trimmed) {
				return true
			}
		}
	}
	return false
}

// rawPosting builds a RawPosting, preserving the source's own object as the payload.
func rawPosting(externalID string, source any, postingURL string, fetchedAt time.Time) (jobs.RawPosting, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return jobs.RawPosting{}, err
	}
	return jobs.RawPosting{
		ExternalID: externalID,
		Payload:    payload,
		URL:        postingURL,
		FetchedAt:  fetchedAt,
	}, nil
}

// parseUnixOrRFC3339 reads the timestamp forms these feeds actually use.
//
// Arbeitnow sends a unix timestamp as a JSON string, Remote OK sends RFC 3339 and a
// unix epoch, and Remotive sends RFC 3339 with no zone. An unparseable value is reported
// absent rather than defaulted to now: a stale posting ranked as fresh would be shown
// first and waste the candidate's application on a role that is probably filled.
func parseUnixOrRFC3339(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), true
		}
	}
	var seconds int64
	if _, err := fmt.Sscanf(trimmed, "%d", &seconds); err == nil && seconds > 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	return time.Time{}, false
}

// positiveOrNil returns nil for a zero or negative figure.
//
// Remote OK sends salary_min and salary_max as 0 when it does not know them — 68 of 98
// postings in a live sample. Carrying that through as a real salary would show a
// candidate a £0 job, and worse, would make it match a salary filter's lower bound.
func positiveOrNil(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	out := value
	return &out
}
