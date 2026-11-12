//go:build adzunalive

// This test calls the real Adzuna API and is therefore behind its own build tag: it is
// never part of `make check`, because a third party being slow or down must not fail the
// build.
//
// Run it when changing the adapter, to confirm the live schema still matches the
// recorded fixtures:
//
//	ADZUNA_APP_ID=... ADZUNA_APP_KEY=... \
//	  go test -tags=adzunalive -v -run TestLive ./internal/domain/jobs/adzuna/
//
// It spends real rate limit, so it makes one search and one health check and no more.

package adzuna_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/adzuna"
)

func liveAdapter(t *testing.T) *adzuna.Adapter {
	t.Helper()
	appID, appKey := os.Getenv("ADZUNA_APP_ID"), os.Getenv("ADZUNA_APP_KEY")
	if appID == "" || appKey == "" {
		t.Skip("ADZUNA_APP_ID and ADZUNA_APP_KEY are not set")
	}
	adapter, err := adzuna.New(adzuna.Config{
		AppID: appID, AppKey: appKey,
		// The real documented limit, not the test's unlimited: this is live traffic.
		Limiter:   jobs.NewTokenLimiter(20),
		Countries: []string{"GB"},
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

func TestLiveSearchReturnsRealPostings(t *testing.T) {
	adapter := liveAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	page, err := adapter.Search(ctx, jobs.SearchSpec{
		Keywords: []string{"backend engineer"},
		Location: "London", Country: "GB",
		Limit: 10,
	}, "")
	if err != nil {
		t.Fatalf("live search: %v", err)
	}
	if len(page.Postings) == 0 {
		t.Fatal("no postings returned for a search that should match thousands")
	}

	t.Logf("returned %d postings, total claimed %d", len(page.Postings), page.Total)

	// The point of running this: confirm the live schema still decodes, so a change
	// upstream is caught here rather than by quarantined postings in production.
	for i, posting := range page.Postings {
		decoded, err := adzuna.Decode(posting.Payload)
		if err != nil {
			t.Errorf("posting %d (%s) no longer decodes: %v\npayload: %s",
				i, posting.ExternalID, err, posting.Payload)
			continue
		}
		if decoded.Title == "" {
			t.Errorf("posting %d decoded with no title", i)
		}
		if i == 0 {
			pretty, _ := json.MarshalIndent(decoded, "", "  ")
			t.Logf("first posting decoded as:\n%s", pretty)
		}
	}
}

func TestLiveHealthCheck(t *testing.T) {
	adapter := liveAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := adapter.HealthCheck(ctx); err != nil {
		t.Fatalf("live health check: %v", err)
	}
}
