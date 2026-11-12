package jobstest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/jobstest"
)

// stub is a minimal adapter whose contract behaviour each test bends in one way.
type stub struct {
	slug      string
	slugCalls int
	// varyingSlug returns a different slug on each call.
	varyingSlug bool
	caps        jobs.Capabilities
	varyingCaps bool
	capsCalls   int
}

func (s *stub) Slug() string {
	s.slugCalls++
	if s.varyingSlug {
		return s.slug + string(rune('a'+s.slugCalls))
	}
	return s.slug
}

func (s *stub) Capabilities() jobs.Capabilities {
	s.capsCalls++
	caps := s.caps
	if s.varyingCaps {
		caps.MaxPageSize = s.capsCalls * 10
	}
	return caps
}

func (s *stub) Search(ctx context.Context, _ jobs.SearchSpec, _ string) (jobs.Page, error) {
	// Honoured, because a well-behaved adapter must: one that ignores cancellation holds
	// a deploy open for as long as its HTTP timeout. The suite checks this, and caught
	// this stub when it did not.
	if err := ctx.Err(); err != nil {
		return jobs.Page{}, err
	}
	return jobs.Page{}, nil
}
func (s *stub) Fetch(context.Context, string) (jobs.RawPosting, error) {
	return jobs.RawPosting{}, nil
}
func (s *stub) HealthCheck(context.Context) error { return nil }

func goodSubject() jobstest.Subject {
	return jobstest.Subject{
		Adapter: &stub{
			slug: "good",
			caps: jobs.Capabilities{Keywords: true, Location: true, Pagination: true, MaxPageSize: 50},
		},
		SampleSpec: jobs.SearchSpec{Keywords: []string{"engineer"}, Location: "Nairobi"},
	}
}

func TestTheSuiteAcceptsAWellBehavedAdapter(t *testing.T) {
	// If this failed, every adapter would be blocked by the suite itself.
	if problems := jobstest.ValidateContract(goodSubject()); len(problems) != 0 {
		t.Fatalf("a well-behaved adapter was rejected: %v", problems)
	}
}

// TestTheSuiteCatchesEachContractViolation is the test that makes the conformance suite
// worth having. A suite that passes every adapter, including a broken one, reports
// confidence it has not earned — which is worse than no suite, because nobody looks
// again.
func TestTheSuiteCatchesEachContractViolation(t *testing.T) {
	cases := map[string]struct {
		subject jobstest.Subject
		expect  string
	}{
		"empty slug": {
			subject: jobstest.Subject{
				Adapter:    &stub{slug: "", caps: jobs.Capabilities{Keywords: true}},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "Slug() is empty",
		},
		"slug with whitespace": {
			subject: jobstest.Subject{
				Adapter:    &stub{slug: " adzuna ", caps: jobs.Capabilities{Keywords: true}},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "want lowercase",
		},
		"uppercase slug": {
			subject: jobstest.Subject{
				Adapter:    &stub{slug: "Adzuna", caps: jobs.Capabilities{Keywords: true}},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "want lowercase",
		},
		"slug varies between calls": {
			subject: jobstest.Subject{
				Adapter:    &stub{slug: "drift", varyingSlug: true, caps: jobs.Capabilities{Keywords: true}},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "must be constant",
		},
		"capabilities vary between calls": {
			subject: jobstest.Subject{
				Adapter: &stub{
					slug: "drifty", varyingCaps: true,
					caps: jobs.Capabilities{Keywords: true, Pagination: true},
				},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "not constant between calls",
		},
		"radius without location": {
			subject: jobstest.Subject{
				Adapter: &stub{
					slug: "radius-only",
					caps: jobs.Capabilities{Keywords: true, Radius: true},
				},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "radius around nothing",
		},
		"lowercase region code": {
			subject: jobstest.Subject{
				Adapter: &stub{
					slug: "lower-region",
					caps: jobs.Capabilities{Keywords: true, Regions: []string{"gb"}},
				},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "ISO 3166-1",
		},
		"three-letter region code": {
			subject: jobstest.Subject{
				Adapter: &stub{
					slug: "bad-region",
					caps: jobs.Capabilities{Keywords: true, Regions: []string{"GBR"}},
				},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "ISO 3166-1",
		},
		"negative page size": {
			subject: jobstest.Subject{
				Adapter: &stub{
					slug: "negative",
					caps: jobs.Capabilities{Keywords: true, MaxPageSize: -5},
				},
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
			},
			expect: "MaxPageSize",
		},
		"sample spec the adapter cannot serve": {
			subject: jobstest.Subject{
				Adapter: &stub{
					slug: "mismatch",
					caps: jobs.Capabilities{Keywords: true},
				},
				// Asks for salary filtering the adapter does not declare, which is how a
				// capability declaration rots away from what the adapter actually does.
				SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}, MinSalary: 50000},
			},
			expect: "sample spec is unsupported",
		},
		"no adapter at all": {
			subject: jobstest.Subject{},
			expect:  "no adapter supplied",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			problems := jobstest.ValidateContract(tc.subject)
			if len(problems) == 0 {
				t.Fatalf("the suite accepted a broken adapter; it should have reported %q", tc.expect)
			}
			joined := strings.Join(problems, " | ")
			if !strings.Contains(joined, tc.expect) {
				t.Errorf("problems = %q, want one mentioning %q", joined, tc.expect)
			}
		})
	}
}

func TestEveryProblemExplainsTheConsequence(t *testing.T) {
	// A conformance failure is read by whoever is writing an adapter, who needs to know
	// why the rule exists rather than only that they broke it.
	subject := jobstest.Subject{
		Adapter: &stub{
			slug: "Bad Slug",
			caps: jobs.Capabilities{Radius: true, MaxPageSize: -1, Regions: []string{"gbr"}},
		},
		SampleSpec: jobs.SearchSpec{Keywords: []string{"x"}},
	}

	problems := jobstest.ValidateContract(subject)
	if len(problems) < 3 {
		t.Fatalf("expected several problems, got %v", problems)
	}
	for _, problem := range problems {
		if len(problem) < 30 {
			t.Errorf("problem %q is too terse to act on", problem)
		}
	}
}

// TestTheSuiteRunsAgainstAReferenceAdapter proves Run itself works, using the stub as a
// stand-in adapter. Real adapters call this from their own test files.
func TestTheSuiteRunsAgainstAReferenceAdapter(t *testing.T) {
	jobstest.Run(t, goodSubject())
}
