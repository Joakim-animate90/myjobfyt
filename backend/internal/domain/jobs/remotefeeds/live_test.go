//go:build feedslive

// These tests call the real feeds and are therefore behind their own build tag: they are
// never part of `make check`, because a third party being slow or down must not fail the
// build.
//
//	go test -tags=feedslive -v ./internal/domain/jobs/remotefeeds/
//
// They make one request per feed and no more. Remotive's own terms advise at most four
// requests a day and say excessive requests will be blocked, so this is not a test to
// run in a loop.

package remotefeeds_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/remotefeeds"
)

// TestLiveFeedsStillMatchTheFixtures is the check the recorded fixtures cannot make: it
// confirms the live schema still decodes, so a change upstream is caught here rather
// than by quarantined postings in production.
func TestLiveFeedsStillMatchTheFixtures(t *testing.T) {
	cases := []struct {
		name    string
		adapter jobs.Adapter
		decode  func([]byte) (jobs.CanonicalPosting, error)
		spec    jobs.SearchSpec
	}{
		{
			name:    "remotive",
			adapter: remotefeeds.NewRemotive(remotefeeds.Config{Limiter: jobs.Unlimited()}),
			decode:  remotefeeds.DecodeRemotive,
			spec:    jobs.SearchSpec{Limit: 5},
		},
		{
			name:    "remoteok",
			adapter: remotefeeds.NewRemoteOK(remotefeeds.Config{Limiter: jobs.Unlimited()}),
			decode:  remotefeeds.DecodeRemoteOK,
			spec:    jobs.SearchSpec{Limit: 5},
		},
		{
			name:    "arbeitnow",
			adapter: remotefeeds.NewArbeitnow(remotefeeds.Config{Limiter: jobs.Unlimited()}),
			decode:  remotefeeds.DecodeArbeitnow,
			spec:    jobs.SearchSpec{Country: "DE"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			page, err := tc.adapter.Search(ctx, tc.spec, "")
			if err != nil {
				t.Fatalf("live search: %v", err)
			}
			if len(page.Postings) == 0 {
				t.Fatal("the live feed returned no postings")
			}
			t.Logf("%d postings returned", len(page.Postings))

			var withSalary, withDate, withAttribution int
			for i, posting := range page.Postings {
				decoded, derr := tc.decode(posting.Payload)
				if derr != nil {
					t.Errorf("posting %d (%s) no longer decodes: %v\npayload: %s",
						i, posting.ExternalID, derr, posting.Payload)
					continue
				}
				if decoded.Title == "" {
					t.Errorf("posting %d decoded with no title", i)
				}
				if decoded.SalaryMin != nil {
					withSalary++
				}
				if decoded.PostedAt != nil {
					withDate++
				}
				if decoded.Attribution.Required() {
					withAttribution++
				}
				if i == 0 {
					pretty, _ := json.MarshalIndent(decoded, "", "  ")
					t.Logf("first posting decoded as:\n%s", pretty)
				}
			}

			// Every posting must carry its credit: these feeds suspend access without one.
			if withAttribution != len(page.Postings) {
				t.Errorf("%d of %d postings carry attribution; all of them must",
					withAttribution, len(page.Postings))
			}
			t.Logf("with a structured salary: %d, with a posting date: %d",
				withSalary, withDate)
		})
	}
}

func TestLiveFeedHealthChecks(t *testing.T) {
	adapters := map[string]jobs.Adapter{
		"remotive":  remotefeeds.NewRemotive(remotefeeds.Config{Limiter: jobs.Unlimited()}),
		"remoteok":  remotefeeds.NewRemoteOK(remotefeeds.Config{Limiter: jobs.Unlimited()}),
		"arbeitnow": remotefeeds.NewArbeitnow(remotefeeds.Config{Limiter: jobs.Unlimited()}),
	}
	for name, adapter := range adapters {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			if err := adapter.HealthCheck(ctx); err != nil {
				t.Fatalf("live health check: %v", err)
			}
		})
	}
}
