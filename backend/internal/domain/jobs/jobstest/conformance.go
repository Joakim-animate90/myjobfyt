// Package jobstest holds the conformance suite every job adapter must pass.
//
// A shared suite rather than per-adapter tests, because the properties that matter are
// the same for every source and the ones that break are always the same: an adapter that
// paginates for ever, one that returns postings with no stable identifier, one that
// honours a filter it declared it does not support. Each would corrupt discovery in a way
// that looks like a data problem rather than an integration bug.
//
// An adapter's own tests still cover its parsing against recorded fixtures. This covers
// the contract.
package jobstest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

// Subject is an adapter under test, with whatever the suite needs to exercise it.
type Subject struct {
	// Adapter is the adapter itself.
	Adapter jobs.Adapter
	// SampleSpec is a search this adapter can actually serve. The suite uses it for the
	// happy path, so each adapter supplies one appropriate to its capabilities rather
	// than the suite guessing.
	SampleSpec jobs.SearchSpec
	// SkipLive is true when the adapter talks to a real network service, so the
	// network-dependent checks are skipped rather than making the suite flaky. The
	// contract checks that need no network still run.
	SkipLive bool
}

// ValidateContract runs the checks that need no network, returning one problem string
// per violation.
//
// Separated from Run so the suite itself can be tested: a conformance suite that passes
// every adapter, including a broken one, is worse than no suite at all because it
// reports confidence it has not earned. See conformance_test.go.
func ValidateContract(subject Subject) []string {
	if subject.Adapter == nil {
		return []string{"no adapter supplied"}
	}
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	slug := subject.Adapter.Slug()
	switch {
	case strings.TrimSpace(slug) == "":
		add("Slug() is empty; the adapter cannot be matched to its job_sources row")
	case slug != strings.TrimSpace(slug) || strings.ToLower(slug) != slug:
		add("Slug() = %q, want lowercase with no surrounding whitespace: it is a database "+
			"key and a metric label", slug)
	}
	// Called twice, because a slug derived from configuration or a clock would make the
	// breaker state and the source record drift apart.
	if again := subject.Adapter.Slug(); again != slug {
		add("Slug() returned %q then %q; it must be constant", slug, again)
	}

	caps := subject.Adapter.Capabilities()
	if caps.Radius && !caps.Location {
		add("the adapter declares a search radius but not location filtering; a radius " +
			"around nothing is not a query it can serve")
	}
	if caps.MaxPageSize < 0 {
		add("MaxPageSize = %d; a negative page size would be sent to the source as a "+
			"request parameter, which returns either an error or an unbounded result set",
			caps.MaxPageSize)
	}
	for _, region := range caps.Regions {
		if len(region) != 2 || strings.ToUpper(region) != region {
			add("region %q is not an uppercase ISO 3166-1 alpha-2 code; the planner "+
				"compares it against a candidate's country", region)
		}
	}
	// The planner caches capabilities, so a value that varies makes its decisions
	// unreproducible.
	if !sameCapabilities(caps, subject.Adapter.Capabilities()) {
		add("Capabilities() is not constant between calls")
	}

	// The capability declaration has to be honest in the negative direction too. An
	// adapter claiming it can serve everything will be handed queries it silently
	// mishandles, and the run looks successful while the results are wrong.
	demanding := jobs.SearchSpec{
		Keywords: []string{"engineer"}, Location: "Nairobi", Country: "ZZ",
		RadiusKm: 50, RemoteOnly: true, WorkModes: []string{"remote"},
		EmploymentTypes: []string{"Full-time"}, MinSalary: 100000,
		PostedWithin: 24 * time.Hour,
	}
	if ok, reason := caps.Supports(demanding); ok {
		if len(caps.Regions) > 0 {
			add("the adapter accepted a search in country ZZ despite declaring specific regions")
		}
	} else if strings.TrimSpace(reason) == "" {
		add("an unsupported search was refused with no reason; the run record would show " +
			"a missing source and no explanation")
	}

	// Catches a mismatch between what an adapter says it does and what its own fixture
	// exercises, which is how a capability declaration rots.
	if ok, reason := caps.Supports(subject.SampleSpec); !ok {
		add("the adapter's own sample spec is unsupported: %s", reason)
	}

	return problems
}

// Run executes the conformance suite.
func Run(t *testing.T, subject Subject) {
	t.Helper()
	if subject.Adapter == nil {
		t.Fatal("no adapter supplied to the conformance suite")
	}

	for _, problem := range ValidateContract(subject) {
		t.Errorf("contract violation: %s", problem)
	}

	if subject.SkipLive {
		t.Log("skipping the result-shape and health checks: this adapter needs a live service")
		return
	}

	t.Run("search returns usable postings", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		page, err := subject.Adapter.Search(ctx, subject.SampleSpec, "")
		if err != nil {
			t.Fatalf("Search on the sample spec failed: %v", err)
		}

		seen := map[string]bool{}
		for i, posting := range page.Postings {
			if strings.TrimSpace(posting.ExternalID) == "" {
				t.Errorf("posting %d has no ExternalID; deduplication and re-sighting both "+
					"key on it, so a posting without one is re-imported on every run", i)
			}
			if seen[posting.ExternalID] {
				t.Errorf("ExternalID %q appeared twice in one page", posting.ExternalID)
			}
			seen[posting.ExternalID] = true

			if len(posting.Payload) == 0 {
				t.Errorf("posting %d has an empty payload; normalisation is a separate "+
					"stage and has nothing to work from", i)
				continue
			}
			if !json.Valid(posting.Payload) {
				t.Errorf("posting %d's payload is not valid JSON; it is stored in a jsonb "+
					"column and would be rejected", i)
			}
			if posting.FetchedAt.IsZero() {
				t.Errorf("posting %d has no FetchedAt", i)
			}
		}

		if page.NextCursor != "" && !subject.Adapter.Capabilities().Pagination {
			t.Error("the adapter returned a next cursor but declares it does not paginate; " +
				"the executor would ignore it and silently drop the rest of the results")
		}
		if page.NextCursor != "" && len(page.Postings) == 0 {
			t.Error("the adapter returned a next cursor with no postings, which makes a " +
				"discovery run spin")
		}
	})

	t.Run("a cursor is resumable", func(t *testing.T) {
		caps := subject.Adapter.Capabilities()
		if !caps.Pagination {
			t.Skip("the adapter does not paginate")
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		first, err := subject.Adapter.Search(ctx, subject.SampleSpec, "")
		if err != nil {
			t.Fatalf("first page: %v", err)
		}
		if first.NextCursor == "" {
			t.Skip("only one page of results for the sample spec")
		}

		second, err := subject.Adapter.Search(ctx, subject.SampleSpec, first.NextCursor)
		if err != nil {
			t.Fatalf("resuming from a cursor failed: %v; a worker crash would restart "+
				"rather than resume, re-spending the source's rate limit", err)
		}

		// The second page must not simply repeat the first, which is what an ignored
		// cursor looks like and which would make a run loop until maxPages.
		firstIDs := map[string]bool{}
		for _, p := range first.Postings {
			firstIDs[p.ExternalID] = true
		}
		var overlap int
		for _, p := range second.Postings {
			if firstIDs[p.ExternalID] {
				overlap++
			}
		}
		if len(second.Postings) > 0 && overlap == len(second.Postings) {
			t.Error("the second page repeats the first exactly; the cursor is being ignored")
		}
	})

	t.Run("health check reports reachability", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		if err := subject.Adapter.HealthCheck(ctx); err != nil {
			t.Fatalf("HealthCheck failed: %v", err)
		}
	})

	t.Run("a cancelled context is honoured", func(t *testing.T) {
		// A run has to be able to shut down. An adapter that ignores cancellation holds
		// a deploy open for as long as its HTTP timeout.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := subject.Adapter.Search(ctx, subject.SampleSpec, ""); err == nil {
			t.Error("Search succeeded with an already-cancelled context")
		}
	})
}

// sameCapabilities compares two declarations.
//
// Compared by serialising rather than field by field, so a capability added later is
// covered automatically. A hand-written comparison would quietly stop checking the new
// field, which is the failure this whole test is guarding against.
func sameCapabilities(a, b jobs.Capabilities) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(left) == string(right)
}
