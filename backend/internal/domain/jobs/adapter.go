// Package jobs acquires job postings from external sources.
//
// Every source sits behind the Adapter interface, so a new board is a new adapter rather
// than a new set of rate-limiting, retry and failure-handling decisions. Job supply is
// the product's raw material, and the design assumption throughout is that sources break
// constantly and independently: one failing board must degrade a discovery run, never
// fail it.
package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SourceKind records how a source is accessed, which determines whether it may be
// enabled at all.
type SourceKind string

const (
	// KindOfficialAPI is a documented API with terms we comply with.
	KindOfficialAPI SourceKind = "official_api"
	// KindAuthorisedFeed is a feed published for aggregation.
	KindAuthorisedFeed SourceKind = "authorised_feed"
	// KindATSAPI is an applicant tracking system's public board API.
	KindATSAPI SourceKind = "ats_api"
	// KindCareerPage is an employer's own careers page. Deliberately present and
	// deliberately hard to enable: it is the kind most likely to be scraping rather
	// than integrating, so it needs a legal basis recorded like every other.
	KindCareerPage SourceKind = "career_page"
)

// Valid reports whether k is a known kind.
func (k SourceKind) Valid() bool {
	switch k {
	case KindOfficialAPI, KindAuthorisedFeed, KindATSAPI, KindCareerPage:
		return true
	default:
		return false
	}
}

// SourceState is a source's operational state.
type SourceState string

const (
	// StateEnabled means the source is in use.
	StateEnabled SourceState = "enabled"
	// StateDisabled means it is configured but not in use.
	StateDisabled SourceState = "disabled"
	// StatePartnershipRequired means access needs an agreement we do not have. Distinct
	// from disabled so the backlog records why, rather than someone re-enabling it and
	// discovering the reason the hard way.
	StatePartnershipRequired SourceState = "partnership_required"
	// StateDegraded means it is in use but failing often.
	StateDegraded SourceState = "degraded"
)

// Capabilities declares what an adapter can actually do.
//
// The planner consults this and skips a query the adapter cannot serve, rather than
// issuing it and handling the failure. That distinction matters: a board that silently
// ignores an unsupported filter returns everything, and the run looks successful while
// the results are wrong.
type Capabilities struct {
	// Keywords means free-text search is supported.
	Keywords bool
	// Location means location filtering is supported.
	Location bool
	// Radius means a distance around a location is supported.
	Radius bool
	// RemoteFilter means filtering to remote roles is supported.
	RemoteFilter bool
	// WorkModeFilter means filtering by onsite/hybrid/remote is supported.
	WorkModeFilter bool
	// EmploymentTypeFilter means filtering by contract type is supported.
	EmploymentTypeFilter bool
	// SalaryFilter means filtering by salary is supported.
	SalaryFilter bool
	// PostedWithinFilter means restricting to recent postings is supported.
	PostedWithinFilter bool
	// Pagination means results can be paged beyond the first response.
	Pagination bool
	// MaxPageSize bounds one request. Zero means the adapter decides.
	MaxPageSize int
	// Regions lists ISO country codes the source covers. Empty means global or
	// unknown, which the planner treats as "try it".
	Regions []string
	// DetailFetch means Fetch returns more than Search already provided. False for a
	// feed that gives everything up front, and the planner then skips a useless call.
	DetailFetch bool
}

// Supports reports whether these capabilities can serve a search specification.
//
// Returns the reason when they cannot, because that reason is worth recording on the
// run: "skipped, source cannot filter by salary" is diagnosable, while a missing result
// is not.
func (c Capabilities) Supports(spec SearchSpec) (bool, string) {
	switch {
	case len(spec.Keywords) > 0 && !c.Keywords:
		return false, "the source does not support keyword search"
	case spec.Location != "" && !c.Location:
		return false, "the source does not support location filtering"
	case spec.RadiusKm > 0 && !c.Radius:
		return false, "the source does not support a search radius"
	case spec.RemoteOnly && !c.RemoteFilter && !c.WorkModeFilter:
		return false, "the source cannot filter to remote roles"
	case len(spec.WorkModes) > 0 && !c.WorkModeFilter:
		return false, "the source does not support work-mode filtering"
	case len(spec.EmploymentTypes) > 0 && !c.EmploymentTypeFilter:
		return false, "the source does not support employment-type filtering"
	case spec.MinSalary > 0 && !c.SalaryFilter:
		return false, "the source does not support salary filtering"
	case spec.PostedWithin > 0 && !c.PostedWithinFilter:
		return false, "the source does not support filtering by posting age"
	}

	if len(c.Regions) > 0 && spec.Country != "" {
		for _, region := range c.Regions {
			if strings.EqualFold(region, spec.Country) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("the source does not cover %s", spec.Country)
	}
	return true, ""
}

// SearchSpec is what to look for. Produced from a candidate's loop configuration.
type SearchSpec struct {
	Keywords []string
	// Location is free text as the candidate wrote it, passed through to sources that
	// accept it. Normalisation happens on the way back, not on the way out: a source
	// knows its own geography better than we do.
	Location string
	// Country is an ISO 3166-1 alpha-2 code, used for region capability checks.
	Country         string
	RadiusKm        int
	RemoteOnly      bool
	WorkModes       []string
	EmploymentTypes []string
	MinSalary       int
	// PostedWithin restricts to postings newer than this. Zero means no restriction.
	PostedWithin time.Duration
	// Limit bounds how many postings this search should yield in total, so one loop
	// cannot consume a whole run's budget.
	Limit int
}

// RawPosting is a posting exactly as a source returned it.
//
// Raw on purpose. Normalisation is a separate stage, and keeping the original means a
// schema change upstream can be diagnosed from the stored payload and the affected
// postings re-normalised, rather than having to re-fetch from a source that may no
// longer have them.
type RawPosting struct {
	// ExternalID is the source's own identifier. Must be stable: it is half of the
	// identity used to recognise a posting we have already seen.
	ExternalID string
	// Payload is the source's response for this posting, as JSON.
	Payload []byte
	// URL is where a human can see the posting.
	URL string
	// FetchedAt is when this was retrieved.
	FetchedAt time.Time
}

// Page is one page of search results.
type Page struct {
	Postings []RawPosting
	// NextCursor is an opaque string the adapter understands. Empty means no more
	// results. Persisted on the discovery run, so a worker that crashes resumes rather
	// than restarting and re-spending the source's rate limit.
	NextCursor string
	// Total is the source's claimed result count, where it provides one. Advisory:
	// sources routinely over-report.
	Total int
}

// Adapter is the contract every job source implements.
type Adapter interface {
	// Slug identifies the source. Matches job_sources.slug.
	Slug() string
	// Capabilities declares what this source can serve.
	Capabilities() Capabilities
	// Search returns one page of results. cursor is empty for the first page.
	Search(ctx context.Context, spec SearchSpec, cursor string) (Page, error)
	// Fetch returns one posting's full detail. Only called when Capabilities reports
	// DetailFetch.
	Fetch(ctx context.Context, externalID string) (RawPosting, error)
	// HealthCheck reports whether the source is reachable and behaving. Called before a
	// run and by the breaker's half-open probe.
	HealthCheck(ctx context.Context) error
}
