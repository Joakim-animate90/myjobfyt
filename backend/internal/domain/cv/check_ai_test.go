package cv_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/google/uuid"
)

// checkRepo serves one version and its content.
type checkRepo struct {
	cv.Repository
	version *cv.Version
	parsed  *cv.ParsedData
	rawText string
}

func (r *checkRepo) VersionByID(context.Context, uuid.UUID, uuid.UUID) (*cv.Version, error) {
	return r.version, nil
}
func (r *checkRepo) ParsedDataByVersion(context.Context, uuid.UUID, uuid.UUID) (*cv.ParsedData, error) {
	return r.parsed, nil
}
func (r *checkRepo) RawTextForVersion(context.Context, uuid.UUID, uuid.UUID) (string, error) {
	return r.rawText, nil
}

type stubAnalyser struct {
	analysis cv.CheckAnalysis
	err      error
	calls    int
}

func (s *stubAnalyser) AnalyseCV(context.Context, cv.CheckAnalysisInput) (cv.CheckAnalysis, error) {
	s.calls++
	if s.err != nil {
		return cv.CheckAnalysis{}, s.err
	}
	return s.analysis, nil
}

// memoryCache is an in-memory CheckCache.
type memoryCache struct {
	entries map[string]cv.CheckReport
	hits    int
	puts    int
}

func newMemoryCache() *memoryCache {
	return &memoryCache{entries: map[string]cv.CheckReport{}}
}

func (c *memoryCache) Get(_ context.Context, key string) (cv.CheckReport, bool) {
	r, ok := c.entries[key]
	if ok {
		c.hits++
	}
	return r, ok
}

func (c *memoryCache) Put(_ context.Context, key string, report cv.CheckReport) error {
	c.puts++
	c.entries[key] = report
	return nil
}

func newCheckRepo() *checkRepo {
	chars, pages := 1800, 2
	return &checkRepo{
		version: &cv.Version{ID: uuid.New(), CharCount: &chars, PageCount: &pages},
		parsed:  goodCV(),
		rawText: goodText().Text,
	}
}

func newCheckService(t *testing.T, repo *checkRepo, analyser cv.CheckAnalyser, cache cv.CheckCache) *cv.CheckService {
	t.Helper()
	return cv.NewCheckService(repo, analyser, cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestTheModelPassFillsInKeywordCoverage(t *testing.T) {
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{
			{Keyword: "Go", Location: "Senior Backend Engineer at Acme Corporation"},
			{Keyword: "payments", Location: "Senior Backend Engineer at Acme Corporation"},
			{Keyword: "PostgreSQL", Location: "Backend Engineer at Beta Payments Ltd"},
		},
		KeywordsMissing: []string{"Kafka"},
	}}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Go payments engineer")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	var keywords cv.CategoryScore
	for _, c := range report.Categories {
		if c.Category == cv.CategoryKeywords {
			keywords = c
		}
	}
	if !keywords.Assessed {
		t.Fatal("keyword coverage was not assessed despite a successful model pass")
	}
	// Three of four terms demonstrated.
	if keywords.Score != 75 {
		t.Errorf("keyword score = %d, want 75", keywords.Score)
	}
	if report.Degraded {
		t.Error("the report still claims to be incomplete after every category was assessed")
	}
	if report.Coverage != 100 {
		t.Errorf("coverage = %d, want 100", report.Coverage)
	}
}

func TestAProviderFailureDegradesRatherThanFailingTheCheck(t *testing.T) {
	// A partial assessment that says what is missing is far more use to a candidate
	// than an error, and this is a free-tier acquisition feature.
	analyser := &stubAnalyser{err: errors.New("provider unavailable")}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Go payments engineer")
	if err != nil {
		t.Fatalf("Check failed instead of degrading: %v", err)
	}
	if !report.Degraded {
		t.Error("the report does not declare itself incomplete after the model pass failed")
	}
	if report.Overall <= 0 {
		t.Error("no deterministic score was produced; the rule layer should stand alone")
	}

	var sawKeywords bool
	for _, missing := range report.Missing {
		if missing == cv.CategoryKeywords {
			sawKeywords = true
		}
	}
	if !sawKeywords {
		t.Errorf("Missing does not name keyword coverage: %v", report.Missing)
	}
	if report.Coverage >= 100 {
		t.Errorf("coverage = %d, want below 100 when a category was not assessed", report.Coverage)
	}
}

func TestNoScoreIsInventedWhenThereIsNothingToJudgeAgainst(t *testing.T) {
	// An analysis with no terms either way is no evidence. Scoring the category from it
	// would be inventing a result.
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{}}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Go engineer")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, c := range report.Categories {
		if c.Category == cv.CategoryKeywords && c.Assessed {
			t.Error("keyword coverage was scored from an analysis containing no terms")
		}
	}
}

func TestAKeywordClaimedWithNoLocationIsDiscarded(t *testing.T) {
	// Every recommendation must name a place in the CV. A keyword asserted with no
	// location cannot be shown to the candidate, so it cannot count in their favour.
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{
			{Keyword: "Go", Location: "Senior Backend Engineer at Acme Corporation"},
			{Keyword: "Kubernetes", Location: ""},
			{Keyword: "", Location: "somewhere"},
		},
		KeywordsMissing: []string{"Kafka"},
	}}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Go engineer")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, c := range report.Categories {
		if c.Category != cv.CategoryKeywords {
			continue
		}
		// One valid hit against one gap.
		if c.Score != 50 {
			t.Errorf("keyword score = %d, want 50; unlocatable claims were counted", c.Score)
		}
	}
}

func TestAnImpactNoteWithNoLocationIsDiscarded(t *testing.T) {
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{{Keyword: "Go", Location: "Acme"}},
		ImpactNotes: []cv.ImpactNote{
			{Location: "Senior Backend Engineer at Acme Corporation",
				Message:    "The first bullet describes a responsibility, not an outcome.",
				Suggestion: "Say what changed as a result."},
			{Location: "", Message: "Something vague", Suggestion: "Be specific"},
		},
	}}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Go engineer")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	for _, f := range report.Findings {
		if strings.TrimSpace(f.Location) == "" {
			t.Errorf("a finding with no location reached the report: %q", f.Message)
		}
		if f.Message == "Something vague" {
			t.Error("the unlocatable note was not discarded")
		}
	}
}

func TestAnUnchangedCVIsServedFromCache(t *testing.T) {
	// This is a free-tier feature and candidates refresh it while editing, so a re-check
	// of an unchanged CV must not cost a model call.
	cache := newMemoryCache()
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{{Keyword: "Go", Location: "Acme"}},
	}}
	repo := newCheckRepo()
	svc := newCheckService(t, repo, analyser, cache)
	userID, versionID := uuid.New(), uuid.New()

	for i := 0; i < 3; i++ {
		if _, err := svc.Check(context.Background(), userID, versionID, "Go engineer"); err != nil {
			t.Fatalf("Check %d: %v", i, err)
		}
	}

	if analyser.calls != 1 {
		t.Errorf("the model was called %d times for three checks of one unchanged CV", analyser.calls)
	}
	if cache.hits != 2 {
		t.Errorf("cache hits = %d, want 2", cache.hits)
	}
}

func TestADifferentTargetRoleIsADifferentCheck(t *testing.T) {
	// Keyword coverage is relative to the role, so the cache must not serve a report
	// computed against a different one.
	cache := newMemoryCache()
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{{Keyword: "Go", Location: "Acme"}},
	}}
	svc := newCheckService(t, newCheckRepo(), analyser, cache)
	userID, versionID := uuid.New(), uuid.New()

	if _, err := svc.Check(context.Background(), userID, versionID, "Go engineer"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	if _, err := svc.Check(context.Background(), userID, versionID, "Platform engineer"); err != nil {
		t.Fatalf("Check: %v", err)
	}

	if analyser.calls != 2 {
		t.Errorf("the model was called %d times for two different target roles", analyser.calls)
	}
}

func TestWithoutATargetRoleTheModelIsNotCalled(t *testing.T) {
	// There is nothing to assess coverage against, and calling anyway would spend the
	// candidate's tokens on a question with no subject.
	analyser := &stubAnalyser{}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if analyser.calls != 0 {
		t.Errorf("the model was called %d times with no target role", analyser.calls)
	}
	if !report.Degraded {
		t.Error("the report does not declare keyword coverage missing")
	}
}

func TestWithoutAnAnalyserTheDeterministicReportStillStands(t *testing.T) {
	svc := newCheckService(t, newCheckRepo(), nil, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Go engineer")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if report.Overall <= 0 {
		t.Error("no score was produced without an analyser")
	}
	if !report.Degraded {
		t.Error("the report does not declare itself incomplete")
	}
}

func TestApplyingAnAnalysisIsDeterministic(t *testing.T) {
	// The merged report has to be as reproducible as the deterministic one, or caching
	// it would serve a different answer from the one just computed.
	analysis := cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{{Keyword: "Go", Location: "Acme"}},
		KeywordsMissing: []string{"Kafka", "Terraform", "gRPC", "Redis"},
		ImpactNotes: []cv.ImpactNote{
			{Location: "Acme", Message: "a", Suggestion: "x"},
			{Location: "Beta", Message: "b", Suggestion: "y"},
		},
	}

	first, err := json.Marshal(cv.ApplyAnalysis(cv.CheckCV(goodCV(), goodText()), analysis))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 30; i++ {
		next, err := json.Marshal(cv.ApplyAnalysis(cv.CheckCV(goodCV(), goodText()), analysis))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("run %d differed:\n%s\n%s", i, first, next)
		}
	}
}

func TestAGapFindingNeverTellsTheCandidateToClaimTheSkill(t *testing.T) {
	// The product must not undo elsewhere what the containment validator refuses.
	analyser := &stubAnalyser{analysis: cv.CheckAnalysis{
		KeywordsPresent: []cv.KeywordHit{{Keyword: "Go", Location: "Acme"}},
		KeywordsMissing: []string{"Kubernetes", "Kafka"},
	}}
	svc := newCheckService(t, newCheckRepo(), analyser, nil)

	report, err := svc.Check(context.Background(), uuid.New(), uuid.New(), "Platform engineer")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}

	for _, f := range report.Findings {
		if f.Category != cv.CategoryKeywords {
			continue
		}
		lower := strings.ToLower(f.Suggestion)
		if strings.Contains(lower, "add these") || strings.Contains(lower, "include them") {
			t.Errorf("the suggestion tells the candidate to claim skills they lack: %q", f.Suggestion)
		}
		if !strings.Contains(lower, "if you have not") {
			t.Errorf("the suggestion does not distinguish a gap from something to add: %q", f.Suggestion)
		}
		return
	}
	t.Error("no keyword finding was produced for a CV missing half the role's terms")
}
