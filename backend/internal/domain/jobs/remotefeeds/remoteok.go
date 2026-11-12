package remotefeeds

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// RemoteOKSlug identifies the source.
const RemoteOKSlug = "remoteok"

const remoteOKURL = "https://remoteok.com/api"

// RemoteOK adapts the Remote OK feed.
type RemoteOK struct{ c *client }

// NewRemoteOK builds the adapter.
func NewRemoteOK(cfg Config) *RemoteOK {
	// Remote OK publishes no figure. Twice an hour is comfortably below anything that
	// could be called excessive, and the feed changes slowly.
	return &RemoteOK{c: newClient(RemoteOKSlug, remoteOKURL, cfg,
		jobs.NewIntervalLimiter(30*time.Minute))}
}

func (r *RemoteOK) Slug() string { return RemoteOKSlug }

// Capabilities declares what Remote OK can serve.
func (r *RemoteOK) Capabilities() jobs.Capabilities {
	return jobs.Capabilities{
		// Applied client-side over the whole feed, which is honest: the alternative is
		// the planner skipping this source for every keyword search, which would
		// exclude it from nearly every real loop.
		Keywords:     true,
		RemoteFilter: true,
		RemoteOnly:   true,
		// Salary is present on about a third of postings and filterable locally. Not
		// declared, because filtering on it would silently drop the two thirds with no
		// salary, and a candidate who asked for £60k+ would never learn that most of
		// the feed simply does not say.
		SalaryFilter: false,
		Pagination:   false,
		Regions:      nil,
		DetailFetch:  false,
	}
}

// remoteOKEntry is one element of the feed.
//
// The feed is a JSON array whose FIRST element is a legal notice rather than a posting.
// Both shapes are decoded into this one struct so the notice can be recognised and
// skipped: treating it as a job would create a posting with no title and no id on every
// single run.
type remoteOKEntry struct {
	// Legal is set only on the metadata element.
	Legal       string `json:"legal,omitempty"`
	LastUpdated int64  `json:"last_updated,omitempty"`

	ID   json.Number `json:"id,omitempty"`
	Slug string      `json:"slug,omitempty"`
	// Position, not title. Getting this wrong yields a feed of untitled postings.
	Position    string      `json:"position,omitempty"`
	Company     string      `json:"company,omitempty"`
	Description string      `json:"description,omitempty"`
	Location    string      `json:"location,omitempty"`
	Tags        []string    `json:"tags,omitempty"`
	SalaryMin   json.Number `json:"salary_min,omitempty"`
	SalaryMax   json.Number `json:"salary_max,omitempty"`
	Date        string      `json:"date,omitempty"`
	Epoch       json.Number `json:"epoch,omitempty"`
	URL         string      `json:"url,omitempty"`
	ApplyURL    string      `json:"apply_url,omitempty"`
	Verified    bool        `json:"verified,omitempty"`
}

// isPosting reports whether this element is a job rather than the legal notice.
func (e remoteOKEntry) isPosting() bool {
	return strings.TrimSpace(e.Legal) == "" &&
		strings.TrimSpace(e.ID.String()) != "" &&
		strings.TrimSpace(e.Position) != ""
}

// Search returns the feed, filtered client-side.
func (r *RemoteOK) Search(ctx context.Context, spec jobs.SearchSpec, cursor string) (jobs.Page, error) {
	if strings.TrimSpace(cursor) != "" {
		return jobs.Page{}, fmt.Errorf("remoteok: the feed is not paginated but a cursor %q was supplied", cursor)
	}

	body, err := r.c.get(ctx, r.c.baseURL, nil)
	if err != nil {
		return jobs.Page{}, err
	}

	var entries []remoteOKEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return jobs.Page{}, fmt.Errorf("remoteok: decode feed: %w", err)
	}

	fetchedAt := time.Now().UTC()
	postings := make([]jobs.RawPosting, 0, len(entries))
	for _, entry := range entries {
		// Skips the legal-notice element, which is the first thing the feed returns.
		if !entry.isPosting() {
			continue
		}
		if !matchesKeywords(spec.Keywords, entry.Position, entry.Company,
			strings.Join(entry.Tags, " "), entry.Description) {
			continue
		}
		posting, err := rawPosting(strings.TrimSpace(entry.ID.String()), entry, entry.URL, fetchedAt)
		if err != nil {
			continue
		}
		postings = append(postings, posting)
		if spec.Limit > 0 && len(postings) >= spec.Limit {
			break
		}
	}

	return jobs.Page{Postings: postings, Total: len(postings)}, nil
}

func (r *RemoteOK) Fetch(_ context.Context, externalID string) (jobs.RawPosting, error) {
	return jobs.RawPosting{}, fmt.Errorf(
		"remoteok: no detail endpoint; DetailFetch is false so this should not be called (id %q)", externalID)
}

// HealthCheck fetches the feed. There is no lighter endpoint.
func (r *RemoteOK) HealthCheck(ctx context.Context) error {
	_, err := r.c.get(ctx, r.c.baseURL, nil)
	return err
}

// DecodeRemoteOK turns a stored Remote OK payload into the canonical shape.
func DecodeRemoteOK(payload []byte) (jobs.CanonicalPosting, error) {
	var entry remoteOKEntry
	if err := json.Unmarshal(payload, &entry); err != nil {
		return jobs.CanonicalPosting{}, fmt.Errorf("remoteok: decode payload: %w", err)
	}
	if strings.TrimSpace(entry.Legal) != "" {
		// The feed's metadata element reached storage. Worth a distinct message: it
		// means the search-time filter was bypassed.
		return jobs.CanonicalPosting{}, fmt.Errorf("remoteok: payload is the feed's legal notice, not a posting")
	}

	id := strings.TrimSpace(entry.ID.String())
	if id == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("remoteok: payload has no id")
	}
	if strings.TrimSpace(entry.Position) == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("remoteok: posting %s has no position", id)
	}

	out := jobs.CanonicalPosting{
		ExternalID:  id,
		Title:       strings.TrimSpace(entry.Position),
		CompanyName: strings.TrimSpace(entry.Company),
		Description: strings.TrimSpace(entry.Description),
		WorkMode:    "remote",
		ApplyURL:    firstNonEmpty(entry.ApplyURL, entry.URL),
		SourceURL:   strings.TrimSpace(entry.URL),
		// Remote OK's terms require a followable link and a named credit, and say
		// access is suspended without them.
		Attribution: jobs.Attribution{
			SourceName:         "Remote OK",
			SourceURL:          strings.TrimSpace(entry.URL),
			NoFollowProhibited: true,
		},
	}

	if location := strings.TrimSpace(entry.Location); location != "" {
		out.LocationText = location
	}

	// Zero means "not stated", not "unpaid". Two thirds of a live sample had zero, and
	// carrying that through would show a candidate a £0 job and match it against the
	// lower bound of any salary filter.
	out.SalaryMin = positiveOrNil(numberOrZero(entry.SalaryMin))
	out.SalaryMax = positiveOrNil(numberOrZero(entry.SalaryMax))
	if out.SalaryMin != nil || out.SalaryMax != nil {
		// Remote OK quotes annual USD figures.
		out.SalaryCurrency = "USD"
		out.SalaryPeriod = "year"
	}

	if posted, ok := parseUnixOrRFC3339(entry.Date); ok {
		out.PostedAt = &posted
	} else if posted, ok := parseUnixOrRFC3339(entry.Epoch.String()); ok {
		out.PostedAt = &posted
	}

	return out, nil
}

func numberOrZero(n json.Number) float64 {
	value, err := n.Float64()
	if err != nil {
		return 0
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
