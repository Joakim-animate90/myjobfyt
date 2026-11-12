package adzuna

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// searchResponse is the search endpoint's envelope.
type searchResponse struct {
	Count   int          `json:"count"`
	Results []jobResult  `json:"results"`
	Mean    *json.Number `json:"mean,omitempty"`
}

// jobResult is one posting as Adzuna returns it.
//
// Every field is a pointer or tolerant type, because Adzuna's own examples disagree with
// each other: contract_time is absent from some results, latitude and longitude from
// others, and id arrives as a string in some responses and a number in others. A struct
// that insisted on a shape would quarantine valid postings, and the whole point of
// keeping the raw payload is that a schema surprise can be diagnosed and re-normalised
// rather than lost.
type jobResult struct {
	ID          flexibleID  `json:"id"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Company     *named      `json:"company,omitempty"`
	Location    *location   `json:"location,omitempty"`
	Category    *categoried `json:"category,omitempty"`

	SalaryMin *float64 `json:"salary_min,omitempty"`
	SalaryMax *float64 `json:"salary_max,omitempty"`
	// SalaryIsPredicted arrives as "0" or "1" — a string, not a boolean.
	SalaryIsPredicted *flexibleBool `json:"salary_is_predicted,omitempty"`

	// ContractType is permanent or contract; ContractTime is full_time or part_time.
	// Two independent fields, and either may be absent.
	ContractType string `json:"contract_type,omitempty"`
	ContractTime string `json:"contract_time,omitempty"`

	Created     string   `json:"created,omitempty"`
	RedirectURL string   `json:"redirect_url,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
}

type named struct {
	DisplayName string `json:"display_name"`
}

type location struct {
	DisplayName string   `json:"display_name"`
	Area        []string `json:"area,omitempty"`
}

type categoried struct {
	Label string `json:"label"`
	Tag   string `json:"tag"`
}

// identifier returns the posting's stable id.
func (r jobResult) identifier() string { return strings.TrimSpace(string(r.ID)) }

// flexibleID accepts an id given as either a JSON string or a number.
type flexibleID string

func (f *flexibleID) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" || trimmed == "" {
		*f = ""
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*f = flexibleID(s)
		return nil
	}
	// A bare number. Kept as its literal text rather than parsed into an int: these ids
	// exceed what a float64 holds exactly, and round-tripping through one silently
	// changes the last digits — which would make the posting a new one on every run.
	*f = flexibleID(trimmed)
	return nil
}

// flexibleBool accepts a boolean given as true/false, "1"/"0" or 1/0.
type flexibleBool bool

func (f *flexibleBool) UnmarshalJSON(data []byte) error {
	trimmed := strings.Trim(strings.TrimSpace(string(data)), `"`)
	switch strings.ToLower(trimmed) {
	case "true", "1":
		*f = true
	case "false", "0", "null", "":
		*f = false
	default:
		// Unrecognised. Treated as "predicted", because the consequence of guessing
		// wrong in the other direction is presenting a predicted salary as the
		// employer's stated figure.
		*f = true
	}
	return nil
}

// Decode turns a stored Adzuna payload into the canonical shape.
//
// Exported because the normalisation stage needs it: only this package knows Adzuna's
// schema, and the raw payload is kept precisely so a posting can be re-decoded after a
// mapping fix without re-fetching from a source that may no longer have it.
func Decode(payload []byte) (jobs.CanonicalPosting, error) {
	var result jobResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return jobs.CanonicalPosting{}, fmt.Errorf("adzuna: decode payload: %w", err)
	}
	return result.canonical()
}

func (r jobResult) canonical() (jobs.CanonicalPosting, error) {
	id := r.identifier()
	if id == "" {
		return jobs.CanonicalPosting{}, fmt.Errorf("adzuna: payload has no id")
	}
	if strings.TrimSpace(r.Title) == "" {
		// A posting with no title cannot be matched, shown or applied to. Quarantining
		// it is correct; storing it would put an empty row in front of a candidate.
		return jobs.CanonicalPosting{}, fmt.Errorf("adzuna: posting %s has no title", id)
	}

	out := jobs.CanonicalPosting{
		ExternalID:  id,
		Title:       strings.TrimSpace(r.Title),
		Description: strings.TrimSpace(r.Description),
		SalaryMin:   r.SalaryMin,
		SalaryMax:   r.SalaryMax,
		Latitude:    r.Latitude,
		Longitude:   r.Longitude,
		ApplyURL:    strings.TrimSpace(r.RedirectURL),
		SourceURL:   strings.TrimSpace(r.RedirectURL),
	}

	if r.Company != nil {
		out.CompanyName = strings.TrimSpace(r.Company.DisplayName)
	}
	if r.Location != nil {
		out.LocationText = strings.TrimSpace(r.Location.DisplayName)
		for _, area := range r.Location.Area {
			if trimmed := strings.TrimSpace(area); trimmed != "" {
				out.LocationParts = append(out.LocationParts, trimmed)
			}
		}
	}
	if r.Category != nil {
		out.Category = strings.TrimSpace(r.Category.Label)
	}

	// Carried through, never flattened away. Adzuna predicts salaries where an employer
	// did not state one, and presenting a prediction as the employer's figure would
	// have a candidate apply, negotiate or decline on a number nobody offered.
	if r.SalaryIsPredicted != nil {
		out.SalaryIsEstimate = bool(*r.SalaryIsPredicted)
	}
	if out.SalaryMin != nil || out.SalaryMax != nil {
		// Adzuna reports annual figures in the country's own currency and does not name
		// it in the result, so the period is known and the currency is left for
		// normalisation to resolve from the country.
		out.SalaryPeriod = "year"
	}

	out.EmploymentType = employmentType(r.ContractTime, r.ContractType)

	if posted, ok := parseCreated(r.Created); ok {
		out.PostedAt = &posted
	}

	return out, nil
}

// employmentType maps Adzuna's two independent flags onto the platform's single enum.
//
// contract_time (full_time/part_time) and contract_type (permanent/contract) describe
// different axes, and the platform's employment_type enum mixes both. Hours win where
// both are present: "part-time" tells a candidate more about whether the job suits them
// than "permanent" does.
func employmentType(contractTime, contractType string) string {
	switch strings.ToLower(strings.TrimSpace(contractTime)) {
	case "part_time":
		return "Part-time"
	case "full_time":
		// Only meaningful on its own if the engagement is not a fixed contract, which
		// the platform records as Contract instead.
		if strings.EqualFold(strings.TrimSpace(contractType), "contract") {
			return "Contract"
		}
		return "Full-time"
	}

	switch strings.ToLower(strings.TrimSpace(contractType)) {
	case "contract":
		return "Contract"
	case "permanent":
		return "Full-time"
	}
	// Unknown rather than guessed. An employment type the platform invented would be
	// filtered on by candidates as though the employer had stated it.
	return ""
}

// parseCreated reads Adzuna's created timestamp.
func parseCreated(value string) (time.Time, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return time.Time{}, false
	}
	// Adzuna returns RFC 3339, sometimes without a zone.
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
	// A unix timestamp, seen in some responses.
	if seconds, err := strconv.ParseInt(trimmed, 10, 64); err == nil && seconds > 0 {
		return time.Unix(seconds, 0).UTC(), true
	}
	// Unparseable. Reported as absent rather than defaulted to now: a posting dated
	// today that is actually six months old would be ranked as fresh and shown first.
	return time.Time{}, false
}
