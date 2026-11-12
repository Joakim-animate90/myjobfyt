package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
)

func TestRegisteringADuplicateSlugIsRefused(t *testing.T) {
	// Two adapters claiming one slug means one is silently never used, and discovering
	// that from missing postings is far harder than from a startup error.
	registry := jobs.NewRegistry()
	if err := registry.Register(&fakeAdapter{slug: "adzuna"}); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := registry.Register(&fakeAdapter{slug: "adzuna"}); err == nil {
		t.Fatal("a duplicate slug was accepted")
	}
}

func TestAnAdapterWithNoSlugIsRefused(t *testing.T) {
	registry := jobs.NewRegistry()
	for _, slug := range []string{"", "   "} {
		if err := registry.Register(&fakeAdapter{slug: slug}); err == nil {
			t.Errorf("an adapter with slug %q was registered; it could never be matched "+
				"to its job_sources row", slug)
		}
	}
	if err := registry.Register(nil); err == nil {
		t.Error("a nil adapter was registered")
	}
}

func TestSlugsAreSortedSoLogsAreStable(t *testing.T) {
	// Ranging a map is randomised, and this list appears in startup logs where a
	// changing order is noise that hides a real change.
	registry := jobs.NewRegistry()
	for _, slug := range []string{"remotive", "adzuna", "greenhouse", "lever"} {
		registry.MustRegister(&fakeAdapter{slug: slug})
	}

	want := []string{"adzuna", "greenhouse", "lever", "remotive"}
	for i := 0; i < 20; i++ {
		got := registry.Slugs()
		if len(got) != len(want) {
			t.Fatalf("got %d slugs, want %d", len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("slugs = %v, want %v", got, want)
			}
		}
	}
}

func TestAnUnknownSlugReportsNotFound(t *testing.T) {
	registry := jobs.NewRegistry()
	_, err := registry.Get("nonexistent")
	if apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("err = %v (kind %v), want not found", err, apperr.KindOf(err))
	}
}

func TestBindReportsAnEnabledSourceWithNoAdapter(ter *testing.T) {
	// The case that matters: this deployment is configured to use a source it cannot
	// talk to. Otherwise it shows up only as a source that silently returns nothing.
	registry := jobs.NewRegistry()
	registry.MustRegister(&fakeAdapter{slug: "adzuna"})

	records := []jobs.SourceRecord{
		{Slug: "adzuna", State: jobs.StateEnabled, LegalBasis: "api terms"},
		{Slug: "linkedin", State: jobs.StateEnabled, LegalBasis: "none"},
		{Slug: "indeed", State: jobs.StatePartnershipRequired},
	}

	bound, unbound := registry.Bind(records)

	if len(bound) != 1 || bound[0].Slug != "adzuna" {
		ter.Errorf("bound = %v, want just adzuna", bound)
	}
	if len(unbound) != 1 || unbound[0] != "linkedin" {
		ter.Errorf("unbound = %v, want just linkedin; an enabled source with no adapter "+
			"must be reported", unbound)
	}
}

func TestBindOrdersSourcesStably(t *testing.T) {
	registry := jobs.NewRegistry()
	for _, slug := range []string{"zeta", "alpha", "mid"} {
		registry.MustRegister(&fakeAdapter{slug: slug})
	}
	records := []jobs.SourceRecord{
		{Slug: "zeta", State: jobs.StateEnabled, LegalBasis: "x"},
		{Slug: "alpha", State: jobs.StateEnabled, LegalBasis: "x"},
		{Slug: "mid", State: jobs.StateEnabled, LegalBasis: "x"},
	}

	bound, _ := registry.Bind(records)
	for i := 1; i < len(bound); i++ {
		if bound[i-1].Slug > bound[i].Slug {
			t.Fatalf("sources are not sorted: %s before %s", bound[i-1].Slug, bound[i].Slug)
		}
	}
}

func TestUsableRejectsEverySourceThatMustNotBeQueried(t *testing.T) {
	adapter := &fakeAdapter{slug: "s"}
	cases := map[string]jobs.Source{
		"no adapter": jobs.SourceRecord{
			Slug: "s", State: jobs.StateEnabled, LegalBasis: "x",
		}.Bind(nil),
		"disabled": jobs.SourceRecord{
			Slug: "s", State: jobs.StateDisabled, LegalBasis: "x",
		}.Bind(adapter),
		"partnership required": jobs.SourceRecord{
			Slug: "s", State: jobs.StatePartnershipRequired, LegalBasis: "x",
		}.Bind(adapter),
		"no legal basis": jobs.SourceRecord{
			Slug: "s", State: jobs.StateEnabled, LegalBasis: "",
		}.Bind(adapter),
		"whitespace legal basis": jobs.SourceRecord{
			Slug: "s", State: jobs.StateEnabled, LegalBasis: "  \t ",
		}.Bind(adapter),
	}

	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			ok, reason := source.Usable()
			if ok {
				t.Fatal("the source was reported usable")
			}
			if reason == "" {
				t.Error("no reason was given, so the run record cannot explain the skip")
			}
		})
	}

	// And a properly configured source is usable, so this is not simply refusing
	// everything.
	good := jobs.SourceRecord{
		Slug: "s", State: jobs.StateEnabled, LegalBasis: "documented API terms",
	}.Bind(adapter)
	if ok, reason := good.Usable(); !ok {
		t.Errorf("a configured source was refused: %s", reason)
	}
}

func TestADegradedSourceIsStillUsed(t *testing.T) {
	// Degraded means failing often, not unusable. Excluding it would turn a flaky
	// source into no source at all, which is worse for the candidate than some results.
	source := jobs.SourceRecord{
		Slug: "flaky", State: jobs.StateDegraded, LegalBasis: "api terms",
	}.Bind(&fakeAdapter{slug: "flaky"})

	if ok, reason := source.Usable(); !ok {
		t.Errorf("a degraded source was excluded: %s", reason)
	}
}

func TestCapabilitiesRefuseAnUnservableSpecWithAReason(t *testing.T) {
	keywordsOnly := jobs.Capabilities{Keywords: true}

	cases := map[string]jobs.SearchSpec{
		"location":        {Keywords: []string{"go"}, Location: "Nairobi"},
		"radius":          {Keywords: []string{"go"}, RadiusKm: 25},
		"remote":          {Keywords: []string{"go"}, RemoteOnly: true},
		"work mode":       {Keywords: []string{"go"}, WorkModes: []string{"hybrid"}},
		"employment type": {Keywords: []string{"go"}, EmploymentTypes: []string{"Contract"}},
		"salary":          {Keywords: []string{"go"}, MinSalary: 90000},
		"posted within":   {Keywords: []string{"go"}, PostedWithin: 48 * time.Hour},
	}

	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			ok, reason := keywordsOnly.Supports(spec)
			if ok {
				t.Fatal("an unservable spec was accepted")
			}
			if reason == "" {
				t.Error("no reason given")
			}
		})
	}
}

func TestARegionLimitedSourceRefusesOtherCountries(t *testing.T) {
	uk := jobs.Capabilities{Keywords: true, Location: true, Regions: []string{"GB", "IE"}}

	if ok, _ := uk.Supports(jobs.SearchSpec{Keywords: []string{"go"}, Country: "GB"}); !ok {
		t.Error("a GB search was refused by a GB source")
	}
	ok, reason := uk.Supports(jobs.SearchSpec{Keywords: []string{"go"}, Country: "KE"})
	if ok {
		t.Error("a Kenyan search was accepted by a UK-only source")
	}
	if reason == "" {
		t.Error("no reason given for the region mismatch")
	}
}

func TestAGlobalSourceAcceptsAnyCountry(t *testing.T) {
	// No declared regions means global or unknown, which the planner treats as "try it"
	// rather than excluding the source from every search.
	global := jobs.Capabilities{Keywords: true, Location: true}
	if ok, reason := global.Supports(jobs.SearchSpec{Keywords: []string{"go"}, Country: "KE"}); !ok {
		t.Errorf("a source with no declared regions refused a search: %s", reason)
	}
}

func TestTheRateLimiterSpacesRequests(t *testing.T) {
	// Enforced inside the adapter so a scheduling bug cannot cause abuse. A burst of
	// sixty at the top of a minute is how an integration gets a 429 and then a ban.
	limiter := jobs.NewTokenLimiter(60)
	if got := limiter.Interval(); got != time.Second {
		t.Errorf("interval for 60/min = %s, want 1s", got)
	}

	// The first request is immediate; measuring the second would mean sleeping.
	start := time.Now()
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("the first request waited %s", elapsed)
	}
}

func TestAZeroRateLimitDoesNotMeanUnlimited(t *testing.T) {
	// Reading a misconfigured zero as "no limit" is how a source gets hammered.
	for _, rate := range []int{0, -1, -100} {
		limiter := jobs.NewTokenLimiter(rate)
		if limiter.Interval() <= 0 {
			t.Errorf("rate %d produced interval %s, which is unlimited", rate, limiter.Interval())
		}
	}
}

func TestTheLimiterHonoursCancellation(t *testing.T) {
	// One request per hour, so the second would block for a long time.
	limiter := jobs.NewTokenLimiter(1)
	if err := limiter.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Wait(ctx); err == nil {
		t.Error("Wait ignored a cancelled context, so a deploy would block on the rate limit")
	}
}

func TestConcurrentWaitersQueueRatherThanFiringTogether(t *testing.T) {
	// If each caller measured the same gap independently they would all fire at once,
	// which defeats the limiter precisely under the load it exists for.
	limiter := jobs.NewTokenLimiter(600) // 100ms apart
	ctx := context.Background()

	start := time.Now()
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			_ = limiter.Wait(ctx)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}

	// Four requests at 100ms spacing: the first is immediate, so at least 300ms.
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Errorf("four waiters completed in %s; they fired together rather than queueing", elapsed)
	}
}
