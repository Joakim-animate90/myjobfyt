package remotefeeds

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// ArbeitnowSlug identifies the source.
const ArbeitnowSlug = "arbeitnow"

const arbeitnowURL = "https://www.arbeitnow.com/api/job-board-api"

// Arbeitnow adapts the Arbeitnow job board.
type Arbeitnow struct{ c *client }

// NewArbeitnow builds the adapter.
func NewArbeitnow(cfg Config) *Arbeitnow {
	// Arbeitnow asks only that callers not abuse the feed. Once every ten minutes is
	// well inside that, and its pages are 300 postings each.
	return &Arbeitnow{c: newClient(ArbeitnowSlug, arbeitnowURL, cfg,
		jobs.NewIntervalLimiter(10*time.Minute))}
}

func (a *Arbeitnow) Slug() string { return ArbeitnowSlug }

// Capabilities declares what Arbeitnow can serve.
//
// Deliberately NOT RemoteOnly, which the issue's framing assumed. Reading the live feed
// settled it: 286 of 300 postings were not remote. Arbeitnow is a general German job
// board that happens to carry a remote flag, so it declares a remote filter it applies
// itself and remains available for location-bound searches — which is most of what it
// actually holds.
func (a *Arbeitnow) Capabilities() jobs.Capabilities {
	return jobs.Capabilities{
		Keywords: true,
		// Applied client-side against the posting's location text.
		Location:     true,
		RemoteFilter: true,
		RemoteOnly:   false,
		Pagination:   true,
		MaxPageSize:  300,
		// Overwhelmingly German, with some Austrian and Swiss listings. Declared so the
		// planner does not spend a request asking a German board about Nairobi.
		Regions:     []string{"DE", "AT", "CH"},
		DetailFetch: false,
	}
}

type arbeitnowEnvelope struct {
	Data  []arbeitnowJob `json:"data"`
	Links struct {
		Next string `json:"next"`
	} `json:"links"`
	Meta struct {
		CurrentPage int `json:"current_page"`
		PerPage     int `json:"per_page"`
	} `json:"meta"`
}

type arbeitnowJob struct {
	// Slug is the identifier: the feed has no numeric id.
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Company     string   `json:"company_name"`
	Description string   `json:"description"`
	Location    string   `json:"location"`
	Remote      bool     `json:"remote"`
	JobTypes    []string `json:"job_types"`
	Tags        []string `json:"tags"`
	// CreatedAt is a unix timestamp, sent as a JSON string.
	CreatedAt json.Number `json:"created_at"`
	URL       string      `json:"url"`
}

// Search returns one page, filtered client-side.
func (a *Arbeitnow) Search(ctx context.Context, spec jobs.SearchSpec, cursor string) (jobs.Page, error) {
	page := 1
	if trimmed := strings.TrimSpace(cursor); trimmed != "" {
		parsed, err := strconv.Atoi(trimmed)
		if err != nil || parsed < 1 {
			return jobs.Page{}, fmt.Errorf("arbeitnow: unusable cursor %q", cursor)
		}
		page = parsed
	}

	query := url.Values{}
	query.Set("page", strconv.Itoa(page))

	body, err := a.c.get(ctx, a.c.baseURL, query)
	if err != nil {
		return jobs.Page{}, err
	}

	var envelope arbeitnowEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return jobs.Page{}, fmt.Errorf("arbeitnow: decode feed: %w", err)
	}

	fetchedAt := time.Now().UTC()
	postings := make([]jobs.RawPosting, 0, len(envelope.Data))
	for _, job := range envelope.Data {
		if strings.TrimSpace(job.Slug) == "" || strings.TrimSpace(job.Title) == "" {
			continue
		}
		// The feed offers no server-side filtering, so the spec is applied here.
		if spec.RemoteOnly && !job.Remote {
			continue
		}
		if !matchesKeywords(spec.Keywords, job.Title, job.Company,
			strings.Join(job.Tags, " "), strings.Join(job.JobTypes, " "), job.Description) {
			continue
		}
		if spec.Location != "" && !strings.Contains(
			strings.ToLower(job.Location), strings.ToLower(strings.TrimSpace(spec.Location))) {
			// Remote postings have no useful location, so a location-bound search must
			// not silently discard them when the candidate also accepts remote work.
			if !(job.Remote && spec.RemoteOnly) {
				continue
			}
		}
		posting, err := rawPosting(strings.TrimSpace(job.Slug), job, job.URL, fetchedAt)
		if err != nil {
			continue
		}
		postings = append(postings, posting)
	}

	// Paged from the feed's own next link rather than inferred from the page size: the
	// filtering above means a page can come back nearly empty while more pages remain,
	// and stopping on a short page would cut the run off after the first one.
	next := ""
	if strings.TrimSpace(envelope.Links.Next) != "" {
		next = strconv.Itoa(page + 1)
	}

	return jobs.Page{Postings: postings, NextCursor: next, Total: len(postings)}, nil
}

func (a *Arbeitnow) Fetch(_ context.Context, externalID string) (jobs.RawPosting, error) {
	return jobs.RawPosting{}, fmt.Errorf(
		"arbeitnow: no detail endpoint; DetailFetch is false so this should not be called (id %q)", externalID)
}

// HealthCheck fetches the first page.
func (a *Arbeitnow) HealthCheck(ctx context.Context) error {
	query := url.Values{}
	query.Set("page", "1")
	_, err := a.c.get(ctx, a.c.baseURL, query)
	return err
}

// DecodeArbeitnow turns a stored Arbeitnow payload into the canonical shape.
func DecodeArbeitnow(payload []byte) (jobs.CanonicalPosting, error) {
	var job arbeitnowJob
	if err := json.Unmarshal(payload, &job); err != nil {
		return jobs.CanonicalPosting{}, fmt.Errorf("arbeitnow: decode payload: %w", err)
	}

	id := strings.TrimSpace(job.Slug)
	if id == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("arbeitnow: payload has no slug")
	}
	if strings.TrimSpace(job.Title) == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("arbeitnow: posting %s has no title", id)
	}

	out := jobs.CanonicalPosting{
		ExternalID:   id,
		Title:        strings.TrimSpace(job.Title),
		CompanyName:  strings.TrimSpace(job.Company),
		Description:  strings.TrimSpace(job.Description),
		LocationText: strings.TrimSpace(job.Location),
		ApplyURL:     strings.TrimSpace(job.URL),
		SourceURL:    strings.TrimSpace(job.URL),
		// Arbeitnow asks only for a link back, so the credit is recorded without the
		// follow requirement the other two impose.
		Attribution: jobs.Attribution{
			SourceName: "Arbeitnow",
			SourceURL:  strings.TrimSpace(job.URL),
		},
	}

	if job.Remote {
		out.WorkMode = "remote"
	}
	// Not set to onsite otherwise: the flag's absence means the posting did not say it
	// was remote, which is not the same as the employer stating it is office-based.

	out.EmploymentType = arbeitnowEmploymentType(job.JobTypes)

	if posted, ok := parseUnixOrRFC3339(job.CreatedAt.String()); ok {
		out.PostedAt = &posted
	}

	return out, nil
}

// arbeitnowEmploymentType maps Arbeitnow's German job_types onto the platform's enum.
//
// The feed's vocabulary is free text in German, so this recognises the forms that
// actually appear and returns empty for anything else rather than guessing. An invented
// employment type would be filtered on by candidates as though the employer had stated it.
func arbeitnowEmploymentType(jobTypes []string) string {
	for _, raw := range jobTypes {
		normalised := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case strings.Contains(normalised, "vollzeit"), strings.Contains(normalised, "full-time"),
			strings.Contains(normalised, "full time"):
			return "Full-time"
		case strings.Contains(normalised, "teilzeit"), strings.Contains(normalised, "part-time"),
			strings.Contains(normalised, "part time"):
			return "Part-time"
		case strings.Contains(normalised, "praktikum"), strings.Contains(normalised, "internship"),
			strings.Contains(normalised, "werkstudent"):
			return "Internship"
		case strings.Contains(normalised, "befristet"), strings.Contains(normalised, "temporary"),
			strings.Contains(normalised, "zeitarbeit"):
			// "unbefristet" means permanent and contains "befristet", so it has to be
			// excluded explicitly or every permanent role is reported as temporary.
			if strings.Contains(normalised, "unbefristet") {
				return "Full-time"
			}
			return "Temporary"
		case strings.Contains(normalised, "freelance"), strings.Contains(normalised, "freiberuf"):
			return "Contract"
		}
	}
	return ""
}
