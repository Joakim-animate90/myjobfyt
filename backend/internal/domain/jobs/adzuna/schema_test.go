package adzuna_test

import (
	"encoding/json"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/adzuna"
)

// decodeNth decodes the nth result from a fixture, as the normalisation stage would
// from a stored payload.
func decodeNth(t *testing.T, fixtureName string, n int) ([]byte, error) {
	t.Helper()
	var envelope struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal(fixture(t, fixtureName), &envelope); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if n >= len(envelope.Results) {
		t.Fatalf("fixture has %d results, wanted index %d", len(envelope.Results), n)
	}
	return envelope.Results[n], nil
}

func TestASalaryStatedByTheEmployerIsNotMarkedAnEstimate(t *testing.T) {
	payload, _ := decodeNth(t, "search_page.json", 0)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if posting.SalaryMin == nil || *posting.SalaryMin != 70000 {
		t.Errorf("SalaryMin = %v, want 70000", posting.SalaryMin)
	}
	if posting.SalaryMax == nil || *posting.SalaryMax != 85000 {
		t.Errorf("SalaryMax = %v, want 85000", posting.SalaryMax)
	}
	if posting.SalaryIsEstimate {
		t.Error("a salary the employer stated was marked as an estimate")
	}
	if posting.SalaryPeriod != "year" {
		t.Errorf("SalaryPeriod = %q, want year", posting.SalaryPeriod)
	}
}

func TestAPredictedSalaryIsMarkedAsAnEstimate(t *testing.T) {
	// The criterion that matters most in this mapping. Adzuna predicts a salary where
	// the employer stated none, and presenting a prediction as the employer's own
	// figure would have a candidate apply, negotiate or decline on a number nobody
	// offered.
	payload, _ := decodeNth(t, "search_page.json", 1)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !posting.SalaryIsEstimate {
		t.Error("a salary Adzuna predicted was presented as the employer's stated figure")
	}
}

func TestAnUnrecognisedPredictionFlagIsTreatedAsPredicted(t *testing.T) {
	// The fixture has salary_is_predicted: "yes", which is neither of the documented
	// values. Guessing "stated" would present an unknown-provenance figure as the
	// employer's; guessing "predicted" only understates our confidence.
	payload, _ := decodeNth(t, "search_malformed.json", 0)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !posting.SalaryIsEstimate {
		t.Error("an unrecognised prediction flag defaulted to 'stated by the employer'")
	}
}

func TestEmploymentTypeMapsAdzunasTwoIndependentFlags(t *testing.T) {
	// contract_time (full_time/part_time) and contract_type (permanent/contract)
	// describe different axes, and the platform's enum mixes both.
	cases := []struct {
		name        string
		payload     string
		want        string
		explanation string
	}{
		{
			name:    "permanent full time",
			payload: `{"id":"1","title":"T","contract_type":"permanent","contract_time":"full_time"}`,
			want:    "Full-time",
		},
		{
			name:    "part time",
			payload: `{"id":"1","title":"T","contract_time":"part_time"}`,
			want:    "Part-time",
			explanation: "hours win over engagement: part-time tells a candidate more " +
				"about whether the job suits them",
		},
		{
			name:    "fixed-term contract worked full time",
			payload: `{"id":"1","title":"T","contract_type":"contract","contract_time":"full_time"}`,
			want:    "Contract",
			explanation: "a six-month contract is not a permanent full-time job, and " +
				"reporting it as one would mislead a candidate filtering for stability",
		},
		{
			name:    "contract with no hours stated",
			payload: `{"id":"1","title":"T","contract_type":"contract"}`,
			want:    "Contract",
		},
		{
			name:    "permanent with no hours stated",
			payload: `{"id":"1","title":"T","contract_type":"permanent"}`,
			want:    "Full-time",
		},
		{
			name:        "neither field present",
			payload:     `{"id":"1","title":"T"}`,
			want:        "",
			explanation: "unknown rather than guessed: an invented type would be filtered on as though stated",
		},
		{
			name:    "both fields empty strings",
			payload: `{"id":"1","title":"T","contract_type":"","contract_time":""}`,
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			posting, err := adzuna.Decode([]byte(tc.payload))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if posting.EmploymentType != tc.want {
				t.Errorf("EmploymentType = %q, want %q. %s",
					posting.EmploymentType, tc.want, tc.explanation)
			}
		})
	}
}

func TestLocationKeepsBothTheTextAndTheHierarchy(t *testing.T) {
	// The display name is what a candidate reads; the area hierarchy is what geo
	// normalisation resolves against. Keeping only one would lose either readability or
	// the ability to match on region.
	payload, _ := decodeNth(t, "search_page.json", 0)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	if posting.LocationText != "Shoreditch, East London" {
		t.Errorf("LocationText = %q", posting.LocationText)
	}
	want := []string{"UK", "London", "East London", "Shoreditch"}
	if len(posting.LocationParts) != len(want) {
		t.Fatalf("LocationParts = %v, want %v", posting.LocationParts, want)
	}
	for i := range want {
		if posting.LocationParts[i] != want[i] {
			t.Errorf("LocationParts[%d] = %q, want %q", i, posting.LocationParts[i], want[i])
		}
	}
	// Broadest first, which is the order geo resolution walks.
	if posting.LocationParts[0] != "UK" {
		t.Error("the hierarchy is not broadest-first")
	}

	if posting.Latitude == nil || posting.Longitude == nil {
		t.Error("coordinates were dropped")
	}
}

func TestMissingCoordinatesAreNilNotZero(t *testing.T) {
	// Zero is a real place in the Gulf of Guinea. A posting defaulted to it would match
	// a radius search around nothing and appear in results it has no business in.
	payload, _ := decodeNth(t, "search_page.json", 1)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.Latitude != nil || posting.Longitude != nil {
		t.Errorf("absent coordinates became %v,%v", posting.Latitude, posting.Longitude)
	}
}

func TestACreatedTimestampIsParsed(t *testing.T) {
	payload, _ := decodeNth(t, "search_page.json", 0)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.PostedAt == nil {
		t.Fatal("PostedAt is nil for a posting with a valid created date")
	}
	if got := posting.PostedAt.Format("2006-01-02"); got != "2026-03-01" {
		t.Errorf("PostedAt = %s, want 2026-03-01", got)
	}
}

func TestAnUnparseableDateIsAbsentRatherThanNow(t *testing.T) {
	// Defaulting to now would rank a six-month-old posting as fresh and show it first,
	// which wastes the candidate's application on a role that is probably filled.
	payload, _ := decodeNth(t, "search_malformed.json", 0)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.PostedAt != nil {
		t.Errorf("an unparseable date became %s", posting.PostedAt)
	}
}

func TestAPostingWithNoIDIsRejected(t *testing.T) {
	// Without a stable identifier it cannot be deduplicated or recognised on a later
	// run, so it would be re-imported for ever.
	payload, _ := decodeNth(t, "search_malformed.json", 1)
	if _, err := adzuna.Decode(payload); err == nil {
		t.Error("a posting with no id was accepted")
	}
}

func TestAPostingWithNoTitleIsRejected(t *testing.T) {
	// It cannot be matched, shown or applied to, so quarantining it is correct;
	// storing it would put an empty row in front of a candidate.
	payload, _ := decodeNth(t, "search_malformed.json", 2)
	if _, err := adzuna.Decode(payload); err == nil {
		t.Error("a posting with an empty title was accepted")
	}
}

func TestAPostingWithOnlyAnIDAndTitleIsAccepted(t *testing.T) {
	// Those are the only genuinely required fields. Insisting on more would quarantine
	// usable postings, and job boards omit things constantly.
	payload, _ := decodeNth(t, "search_malformed.json", 3)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("a sparse but usable posting was rejected: %v", err)
	}
	if posting.ExternalID == "" || posting.Title == "" {
		t.Error("the two required fields did not survive")
	}
	if posting.CompanyName != "" || posting.LocationText != "" {
		t.Error("absent fields were filled with something")
	}
	if posting.SalaryPeriod != "" {
		t.Error("a salary period was set for a posting with no salary")
	}
}

func TestANumericIDKeepsEveryDigit(t *testing.T) {
	// Adzuna ids exceed what a float64 holds exactly. Round-tripping one through a
	// float silently changes its last digits, which makes the posting new on every run
	// and re-triggers the whole match pipeline for it.
	payload := []byte(`{"id":9007199254740993,"title":"T"}`)
	posting, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.ExternalID != "9007199254740993" {
		t.Errorf("ExternalID = %q, want 9007199254740993 exactly", posting.ExternalID)
	}
}

func TestDecodeRejectsContentThatIsNotJSON(t *testing.T) {
	if _, err := adzuna.Decode([]byte(`not json`)); err == nil {
		t.Error("non-JSON was decoded")
	}
}

func TestDecodingIsStableForTheSamePayload(t *testing.T) {
	// A stored payload is re-decoded whenever the mapping changes, so the same bytes
	// must always produce the same posting.
	payload, _ := decodeNth(t, "search_page.json", 0)

	first, err := adzuna.Decode(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for i := 0; i < 10; i++ {
		next, err := adzuna.Decode(payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		a, _ := json.Marshal(first)
		b, _ := json.Marshal(next)
		if string(a) != string(b) {
			t.Fatalf("decode %d differed", i)
		}
	}
}
