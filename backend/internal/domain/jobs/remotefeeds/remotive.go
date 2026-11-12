package remotefeeds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// RemotiveSlug identifies the source.
const RemotiveSlug = "remotive"

const remotiveURL = "https://remotive.com/api/remote-jobs"

// remotiveMinInterval honours Remotive's own stated guidance.
//
// Their legal notice advises at most four requests a day and says excessive requests
// will be blocked. Six hours between requests is that rate exactly. The data is
// deliberately delayed twenty-four hours at their end, so polling faster would gain
// nothing even if it were permitted.
const remotiveMinInterval = 6 * time.Hour

// Remotive adapts the Remotive feed.
type Remotive struct{ c *client }

// NewRemotive builds the adapter.
func NewRemotive(cfg Config) *Remotive {
	return &Remotive{c: newClient(RemotiveSlug, remotiveURL, cfg,
		jobs.NewIntervalLimiter(remotiveMinInterval))}
}

func (r *Remotive) Slug() string { return RemotiveSlug }

// Capabilities declares what Remotive can serve.
//
// Everything on Remotive is remote, so RemoteOnly is set and a search wanting onsite or
// hybrid work is refused rather than answered with remote jobs. Keyword and category
// filtering are supported server-side; nothing else is, and claiming otherwise would
// have the planner issue searches whose filters Remotive silently ignores.
func (r *Remotive) Capabilities() jobs.Capabilities {
	return jobs.Capabilities{
		Keywords:     true,
		RemoteFilter: true,
		RemoteOnly:   true,
		// No pagination: the feed returns everything it has in one response, and the
		// live sample was fourteen postings.
		Pagination:  false,
		MaxPageSize: 0,
		// No declared regions. Postings carry a candidate_required_location such as
		// "Worldwide" or "USA, Canada", which is a restriction on the candidate rather
		// than a country the source covers, so a region declaration would be wrong.
		Regions:     nil,
		DetailFetch: false,
	}
}

type remotiveEnvelope struct {
	LegalNotice string          `json:"0-legal-notice"`
	JobCount    int             `json:"job-count"`
	Total       int             `json:"total-job-count"`
	Jobs        []remotiveJob   `json:"jobs"`
	Raw         json.RawMessage `json:"-"`
}

type remotiveJob struct {
	ID       json.Number `json:"id"`
	URL      string      `json:"url"`
	Title    string      `json:"title"`
	Company  string      `json:"company_name"`
	Category string      `json:"category"`
	Tags     []string    `json:"tags"`
	// JobType is Remotive's own vocabulary: full_time, part_time, contract, freelance.
	JobType         string `json:"job_type"`
	PublicationDate string `json:"publication_date"`
	// CandidateRequiredLocation is a restriction on the candidate ("USA, Canada"), not
	// the employer's office. Carried as location text because that is what a candidate
	// needs to read, but never treated as a place to geocode.
	CandidateRequiredLocation string `json:"candidate_required_location"`
	// Salary is free text: "$14 - $20/hour", "OTE $25k - $35k", or empty.
	Salary      string `json:"salary"`
	Description string `json:"description"`
}

// Search returns the feed, filtered client-side.
func (r *Remotive) Search(ctx context.Context, spec jobs.SearchSpec, cursor string) (jobs.Page, error) {
	if strings.TrimSpace(cursor) != "" {
		// The feed is a single response. A cursor means a caller believes otherwise,
		// which is worth failing on rather than silently refetching page one for ever.
		return jobs.Page{}, fmt.Errorf("remotive: the feed is not paginated but a cursor %q was supplied", cursor)
	}

	query := url.Values{}
	// Remotive supports a server-side search parameter, which is better than filtering
	// its whole feed locally.
	if len(spec.Keywords) > 0 {
		query.Set("search", strings.Join(spec.Keywords, " "))
	}
	if spec.Limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", spec.Limit))
	}

	body, err := r.c.get(ctx, r.c.baseURL, query)
	if err != nil {
		return jobs.Page{}, err
	}

	var envelope remotiveEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return jobs.Page{}, fmt.Errorf("remotive: decode feed: %w", err)
	}

	fetchedAt := time.Now().UTC()
	postings := make([]jobs.RawPosting, 0, len(envelope.Jobs))
	for _, job := range envelope.Jobs {
		id := strings.TrimSpace(job.ID.String())
		if id == "" || strings.TrimSpace(job.Title) == "" {
			continue
		}
		// The server-side search is applied by Remotive, but a spec may carry keywords
		// alongside a limit that truncated the result, so this is belt and braces
		// rather than the primary filter.
		if !matchesKeywords(spec.Keywords, job.Title, job.Company, job.Category,
			strings.Join(job.Tags, " "), job.Description) {
			continue
		}
		posting, err := rawPosting(id, job, job.URL, fetchedAt)
		if err != nil {
			continue
		}
		postings = append(postings, posting)
	}

	return jobs.Page{Postings: postings, Total: envelope.Total}, nil
}

func (r *Remotive) Fetch(_ context.Context, externalID string) (jobs.RawPosting, error) {
	return jobs.RawPosting{}, fmt.Errorf(
		"remotive: no detail endpoint; DetailFetch is false so this should not be called (id %q)", externalID)
}

// HealthCheck fetches the feed with a limit of one.
//
// There is no cheaper endpoint, and this one consumes a request from a budget of four a
// day — so a half-open probe here is genuinely expensive and the breaker's cooldown
// should stay long for this source.
func (r *Remotive) HealthCheck(ctx context.Context) error {
	query := url.Values{}
	query.Set("limit", "1")
	_, err := r.c.get(ctx, r.c.baseURL, query)
	return err
}

// remotiveAttribution is the credit Remotive's terms require.
//
// Their notice is explicit: link back to the Remotive URL and name Remotive as the
// source, or access is terminated.
func remotiveAttribution(postingURL string) jobs.Attribution {
	return jobs.Attribution{SourceName: "Remotive", SourceURL: postingURL}
}

// DecodeRemotive turns a stored Remotive payload into the canonical shape.
func DecodeRemotive(payload []byte) (jobs.CanonicalPosting, error) {
	var job remotiveJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return jobs.CanonicalPosting{}, fmt.Errorf("remotive: decode payload: %w", err)
	}

	id := strings.TrimSpace(job.ID.String())
	if id == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("remotive: payload has no id")
	}
	if strings.TrimSpace(job.Title) == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("remotive: posting %s has no title", id)
	}

	out := jobs.CanonicalPosting{
		ExternalID:  id,
		Title:       strings.TrimSpace(job.Title),
		CompanyName: strings.TrimSpace(job.Company),
		Description: strings.TrimSpace(job.Description),
		Category:    strings.TrimSpace(job.Category),
		// Every Remotive posting is remote by construction.
		WorkMode:       "remote",
		EmploymentType: remotiveEmploymentType(job.JobType),
		// The candidate-side restriction, which is what a candidate needs to read. Not
		// geocoded: "Worldwide" and "USA, Canada, USA timezones" are eligibility rules,
		// not places, and resolving them to coordinates would put a posting on a map
		// where it does not belong.
		LocationText: strings.TrimSpace(job.CandidateRequiredLocation),
		ApplyURL:     strings.TrimSpace(job.URL),
		SourceURL:    strings.TrimSpace(job.URL),
		Attribution:  remotiveAttribution(strings.TrimSpace(job.URL)),
	}

	if posted, ok := parseUnixOrRFC3339(job.PublicationDate); ok {
		out.PostedAt = &posted
	}

	// Salary is deliberately not parsed into numbers. Remotive sends free text —
	// "$14 - $20/hour", "OTE $25k - $35k", "" — and guessing a figure from it would
	// produce a salary the employer never stated, filterable as though they had. The
	// text is kept where a candidate can read it and judge for themselves.
	if salary := strings.TrimSpace(job.Salary); salary != "" {
		out.Description = strings.TrimSpace(out.Description + "\n\nSalary as stated by the employer: " + salary)
	}

	return out, nil
}

// remotiveEmploymentType maps Remotive's vocabulary onto the platform's enum.
func remotiveEmploymentType(jobType string) string {
	switch strings.ToLower(strings.TrimSpace(jobType)) {
	case "full_time":
		return "Full-time"
	case "part_time":
		return "Part-time"
	case "contract", "freelance":
		// The platform has no Freelance value, and Contract is the honest nearest: both
		// mean a fixed engagement rather than employment.
		return "Contract"
	case "internship":
		return "Internship"
	case "temporary":
		return "Temporary"
	default:
		// Unknown rather than guessed.
		return ""
	}
}
