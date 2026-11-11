package worker_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/worker"
)

type stubProvider struct{ regs []events.Registration }

func (s stubProvider) Registrations() []events.Registration { return s.regs }

func reg(name, group string) events.Registration {
	return events.Registration{
		Name:    name,
		Group:   group,
		Stream:  "CV",
		Handler: func(context.Context, *events.Envelope) error { return nil },
	}
}

func TestCollectMergesAndSortsRegistrations(t *testing.T) {
	regs, err := worker.Collect(
		stubProvider{regs: []events.Registration{reg("z-last", "cv"), reg("a-first", "cv")}},
		stubProvider{regs: []events.Registration{reg("m-middle", "jobs")}},
	)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(regs) != 3 {
		t.Fatalf("got %d registrations, want 3", len(regs))
	}
	// Deterministic order so startup logs are comparable between deploys.
	if regs[0].Name != "a-first" || regs[2].Name != "z-last" {
		t.Fatalf("registrations are not sorted: %v", []string{regs[0].Name, regs[1].Name, regs[2].Name})
	}
}

func TestCollectRejectsDuplicateConsumerNames(t *testing.T) {
	// Two handlers on one durable name compete for the same consumer, so each message
	// reaches only one of them at random. That presents as "sometimes the pipeline
	// skips a stage" and is very hard to find, so it must refuse to start.
	_, err := worker.Collect(
		stubProvider{regs: []events.Registration{reg("cv-av-scan", "cv")}},
		stubProvider{regs: []events.Registration{reg("cv-av-scan", "cv")}},
	)
	if err == nil {
		t.Fatal("Collect accepted a duplicate consumer name")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v, want it to mention the duplicate", err)
	}
}

func TestCollectRejectsIncompleteRegistrations(t *testing.T) {
	cases := map[string]events.Registration{
		"no name":    {Group: "cv", Handler: func(context.Context, *events.Envelope) error { return nil }},
		"no handler": {Name: "x", Group: "cv"},
		// A consumer with no group is never selected by any worker, so it would be
		// silently dead code that looks registered.
		"no group": {Name: "x", Handler: func(context.Context, *events.Envelope) error { return nil }},
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := worker.Collect(stubProvider{regs: []events.Registration{r}}); err == nil {
				t.Fatal("Collect accepted an incomplete registration")
			}
		})
	}
}

func TestGroupsReturnsDistinctSortedGroups(t *testing.T) {
	regs := []events.Registration{
		reg("a", "cv"), reg("b", "cv"), reg("c", "jobs"), reg("d", "applications"),
	}
	got := worker.Groups(regs)
	want := []string{"applications", "cv", "jobs"}
	if len(got) != len(want) {
		t.Fatalf("Groups() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Groups() = %v, want %v", got, want)
		}
	}
}

func TestValidateGroupsReportsUnmatchedWithoutFailing(t *testing.T) {
	// Configuring a group before its consumers exist is normal while a domain is being
	// built, and the default WORKER_GROUPS lists every planned group. It must be
	// reported, not fatal.
	regs := []events.Registration{reg("cv-av-scan", "cv")}

	unmatched, err := worker.ValidateGroups(regs, []string{"cv", "jobs", "matching"})
	if err != nil {
		t.Fatalf("ValidateGroups = %v, want nil when at least one group matches", err)
	}
	if len(unmatched) != 2 {
		t.Fatalf("unmatched = %v, want jobs and matching", unmatched)
	}
}

func TestValidateGroupsFailsWhenNothingMatches(t *testing.T) {
	// This is the failure the check exists for: a worker that starts, reports healthy,
	// and processes nothing, because WORKER_GROUPS has a typo.
	regs := []events.Registration{reg("cv-av-scan", "cv")}

	_, err := worker.ValidateGroups(regs, []string{"cvs", "job"})
	if err == nil {
		t.Fatal("ValidateGroups accepted a configuration that matches no consumer")
	}
	// The message must name what IS available, or the operator is left guessing.
	if !strings.Contains(err.Error(), "cv") {
		t.Fatalf("err = %v, want it to name the available groups", err)
	}
}

func TestValidateGroupsIgnoresBlankEntries(t *testing.T) {
	// A trailing comma in WORKER_GROUPS is a common typo and must not be reported as
	// an unmatched group.
	regs := []events.Registration{reg("cv-av-scan", "cv")}

	unmatched, err := worker.ValidateGroups(regs, []string{"cv", "", "  "})
	if err != nil {
		t.Fatalf("ValidateGroups = %v", err)
	}
	if len(unmatched) != 0 {
		t.Fatalf("unmatched = %v, want none", unmatched)
	}
}
