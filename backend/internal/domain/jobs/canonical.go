package jobs

import "time"

// CanonicalPosting is a posting decoded from one source's own schema into the shape the
// rest of the platform understands.
//
// Produced by an adapter, because only the adapter knows its source's schema. The
// generic work — resolving company identity, geocoding, currency handling — is the
// normalisation stage's job; this is the translation step that has to happen per source
// and nowhere else.
//
// Every field is optional except ExternalID and Title. Job boards omit things
// constantly, and a decoder that required a field would quarantine perfectly usable
// postings.
type CanonicalPosting struct {
	ExternalID string
	Title      string
	// CompanyName is the employer as the source wrote it. Identity resolution happens
	// later; this is the raw display name.
	CompanyName string
	Description string
	// LocationText is the source's own rendering, e.g. "Shoreditch, East London".
	LocationText string
	// LocationParts is the source's hierarchy, broadest first, where it gives one.
	LocationParts []string
	Latitude      *float64
	Longitude     *float64

	SalaryMin      *float64
	SalaryMax      *float64
	SalaryCurrency string
	SalaryPeriod   string
	// SalaryIsEstimate is true when the source predicted the figure rather than the
	// employer stating it. Carried separately and never flattened away: presenting a
	// predicted salary as the employer's own would mislead a candidate into applying,
	// negotiating, or declining on a number nobody offered.
	SalaryIsEstimate bool

	// WorkMode is onsite, hybrid or remote, where the source says.
	WorkMode string
	// EmploymentType is Full-time, Contract, Part-time, Temporary or Internship.
	EmploymentType string
	Category       string

	PostedAt  *time.Time
	ApplyURL  string
	SourceURL string
}
