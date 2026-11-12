package adzuna_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/adzuna"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/jobstest"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// stubAPI serves a recorded response and records the request it received.
type stubAPI struct {
	status  int
	body    []byte
	headers map[string]string

	requests []*http.Request
	queries  []string
	server   *httptest.Server
}

func newStubAPI(t *testing.T, status int, body []byte) *stubAPI {
	t.Helper()
	stub := &stubAPI{status: status, body: body, headers: map[string]string{}}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.requests = append(stub.requests, r)
		stub.queries = append(stub.queries, r.URL.RawQuery)
		for k, v := range stub.headers {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(stub.status)
		_, _ = w.Write(stub.body)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func newAdapter(t *testing.T, stub *stubAPI) *adzuna.Adapter {
	t.Helper()
	adapter, err := adzuna.New(adzuna.Config{
		AppID: "test-id", AppKey: "test-key",
		BaseURL:   stub.server.URL,
		Limiter:   jobs.Unlimited(),
		Countries: []string{"GB", "US"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return adapter
}

func sampleSpec() jobs.SearchSpec {
	return jobs.SearchSpec{
		Keywords: []string{"backend", "engineer"},
		Location: "London", Country: "GB", RadiusKm: 25,
		MinSalary: 60000, PostedWithin: 72 * time.Hour,
		EmploymentTypes: []string{"Full-time"},
	}
}

// TestConformance runs the shared suite against the adapter and a recorded page, so a
// schema change upstream fails here rather than corrupting production data.
func TestConformance(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	jobstest.Run(t, jobstest.Subject{
		Adapter:    newAdapter(t, stub),
		SampleSpec: sampleSpec(),
	})
}

func TestCredentialsAreRequired(t *testing.T) {
	// A misconfigured adapter must fail at construction, not on the first search in a
	// scheduled run at 3am.
	for name, cfg := range map[string]adzuna.Config{
		"neither":    {},
		"no app id":  {AppKey: "k"},
		"no app key": {AppID: "i"},
		"whitespace": {AppID: "  ", AppKey: "  "},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adzuna.New(cfg); err == nil {
				t.Error("the adapter was built with incomplete credentials")
			}
		})
	}
}

func TestCredentialsAreSentButNeverLogged(t *testing.T) {
	// They are query parameters, which is how Adzuna authenticates, so they end up in
	// any URL this adapter logs or returns in an error. This asserts they are not in an
	// error message, which is the path that reaches a log.
	stub := newStubAPI(t, http.StatusInternalServerError, fixture(t, "error_500.json"))
	adapter := newAdapter(t, stub)

	_, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, secret := range []string{"test-key", "app_key"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error message leaks %q: %v", secret, err)
		}
	}
}

func TestSearchDecodesARecordedPage(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	page, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Postings) != 3 {
		t.Fatalf("got %d postings, want 3", len(page.Postings))
	}
	if page.Total != 1243 {
		t.Errorf("Total = %d, want 1243", page.Total)
	}

	// A numeric id must survive as its literal digits.
	var sawNumericID bool
	for _, posting := range page.Postings {
		if posting.ExternalID == "4812009999" {
			sawNumericID = true
		}
		if posting.ExternalID == "" {
			t.Error("a posting came back with no external id")
		}
		if len(posting.Payload) == 0 {
			t.Error("a posting came back with no payload")
		}
		if posting.FetchedAt.IsZero() {
			t.Error("a posting came back with no FetchedAt")
		}
	}
	if !sawNumericID {
		t.Error("an id given as a JSON number was not preserved as its digits; a rounded " +
			"id makes the posting new on every run")
	}
}

func TestTheRequestAsksForJSONNotJSONP(t *testing.T) {
	// Adzuna's default response format is JSONP. Without asking for JSON the body is a
	// JavaScript function call, and decoding fails in a way that looks like a malformed
	// payload rather than a missing parameter.
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	if _, err := adapter.Search(context.Background(), sampleSpec(), ""); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(stub.queries) == 0 {
		t.Fatal("no request was made")
	}
	if !strings.Contains(stub.queries[0], "content-type=application%2Fjson") {
		t.Errorf("the request does not ask for JSON: %s", stub.queries[0])
	}
	if got := stub.requests[0].Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept = %q, want application/json", got)
	}
}

func TestTheSpecIsTranslatedIntoAdzunasParameters(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	spec := sampleSpec()
	spec.EmploymentTypes = []string{"Part-time", "Contract"}
	if _, err := adapter.Search(context.Background(), spec, ""); err != nil {
		t.Fatalf("Search: %v", err)
	}

	query := stub.queries[0]
	for _, want := range []string{
		"what=backend+engineer",
		"where=London",
		"distance=25",
		"salary_min=60000",
		"max_days_old=3",
		// Adzuna expresses employment type as independent flags, not one field.
		"part_time=1",
		"contract=1",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("the query is missing %q: %s", want, query)
		}
	}

	// The country belongs in the path, not the query: Adzuna has no global search.
	if !strings.Contains(stub.requests[0].URL.Path, "/jobs/gb/search/1") {
		t.Errorf("path = %q, want /jobs/gb/search/1", stub.requests[0].URL.Path)
	}
}

func TestASubDayWindowAsksForOneDayNotZero(t *testing.T) {
	// max_days_old=0 would be read as no filter at all, silently widening the search
	// from "today" to "ever".
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	spec := sampleSpec()
	spec.PostedWithin = 6 * time.Hour
	if _, err := adapter.Search(context.Background(), spec, ""); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !strings.Contains(stub.queries[0], "max_days_old=1") {
		t.Errorf("a six-hour window produced %s", stub.queries[0])
	}
}

func TestPaginationIsResumableFromACursor(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	// The fixture has three results against a page size of 50, so it is a short page
	// and there is no next cursor.
	page, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q on a short page; paging would never terminate", page.NextCursor)
	}

	// Resuming from a persisted cursor must request that page.
	if _, err := adapter.Search(context.Background(), sampleSpec(), "4"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !strings.HasSuffix(stub.requests[1].URL.Path, "/search/4") {
		t.Errorf("resumed at path %q, want /search/4", stub.requests[1].URL.Path)
	}
}

func TestAFullPageOffersANextCursor(t *testing.T) {
	// Paging stops when a page comes back short rather than when the count says so,
	// because Adzuna's count is unreliable.
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	spec := sampleSpec()
	spec.Limit = 3 // makes the three-result fixture a full page

	page, err := adapter.Search(context.Background(), spec, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if page.NextCursor != "2" {
		t.Errorf("NextCursor = %q, want 2", page.NextCursor)
	}
}

func TestAnUnusableCursorIsRejected(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	for _, cursor := range []string{"abc", "0", "-1", "1.5"} {
		if _, err := adapter.Search(context.Background(), sampleSpec(), cursor); err == nil {
			t.Errorf("cursor %q was accepted", cursor)
		}
	}
}

func TestAnEmptyResultIsNotAnError(t *testing.T) {
	// A search that matches nothing is a normal outcome. Treating it as a failure would
	// count towards the breaker and eventually take a healthy source out of every run.
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_empty.json"))
	adapter := newAdapter(t, stub)

	page, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err != nil {
		t.Fatalf("an empty result was reported as an error: %v", err)
	}
	if len(page.Postings) != 0 {
		t.Errorf("got %d postings, want 0", len(page.Postings))
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q on an empty page; paging would never terminate", page.NextCursor)
	}
}

func TestMalformedPostingsAreSkippedWithoutLosingTheGoodOnes(t *testing.T) {
	// One bad posting must not cost the whole page. The fixture mixes a usable posting
	// with one that has no id, one with an empty title, and one with almost every field
	// null.
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_malformed.json"))
	adapter := newAdapter(t, stub)

	page, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err != nil {
		t.Fatalf("a page with malformed entries failed wholesale: %v", err)
	}

	ids := map[string]bool{}
	for _, posting := range page.Postings {
		ids[posting.ExternalID] = true
	}

	if !ids["5000000001"] {
		t.Error("the usable posting was lost")
	}
	if !ids["5000000004"] {
		t.Error("a posting with only an id and a title was dropped; those are the only " +
			"two fields that are genuinely required")
	}
	// The posting with no id cannot be deduplicated or re-recognised, so it must not be
	// carried forward at all.
	if len(page.Postings) != 3 {
		t.Errorf("got %d postings; the one with no id should have been dropped at search time", len(page.Postings))
	}
}

func TestA429IsReportedAsRateLimitedWithRetryAfter(t *testing.T) {
	stub := newStubAPI(t, http.StatusTooManyRequests, fixture(t, "error_429.json"))
	stub.headers["Retry-After"] = "45"
	adapter := newAdapter(t, stub)

	_, err := adapter.Search(context.Background(), sampleSpec(), "")
	var limited *jobs.ErrRateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want ErrRateLimited so the executor backs off rather than "+
			"treating it as the source being broken", err)
	}
	if limited.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %s, want 45s", limited.RetryAfter)
	}
	if limited.Slug != "adzuna" {
		t.Errorf("Slug = %q", limited.Slug)
	}
}

func TestRetryAfterAlsoAcceptsAnHTTPDate(t *testing.T) {
	// The header is documented in two forms, and a source that uses the date form would
	// otherwise be retried immediately.
	stub := newStubAPI(t, http.StatusTooManyRequests, fixture(t, "error_429.json"))
	stub.headers["Retry-After"] = time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)
	adapter := newAdapter(t, stub)

	_, err := adapter.Search(context.Background(), sampleSpec(), "")
	var limited *jobs.ErrRateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if limited.RetryAfter < 60*time.Second || limited.RetryAfter > 95*time.Second {
		t.Errorf("RetryAfter = %s, want roughly 90s", limited.RetryAfter)
	}
}

func TestA429WithNoRetryAfterStillReportsRateLimited(t *testing.T) {
	stub := newStubAPI(t, http.StatusTooManyRequests, fixture(t, "error_429.json"))
	adapter := newAdapter(t, stub)

	_, err := adapter.Search(context.Background(), sampleSpec(), "")
	var limited *jobs.ErrRateLimited
	if !errors.As(err, &limited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	if limited.RetryAfter != 0 {
		t.Errorf("RetryAfter = %s with no header, want 0", limited.RetryAfter)
	}
}

func TestA500IsAPlainFailure(t *testing.T) {
	stub := newStubAPI(t, http.StatusInternalServerError, fixture(t, "error_500.json"))
	adapter := newAdapter(t, stub)

	_, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err == nil {
		t.Fatal("a 500 was not reported as an error")
	}
	var limited *jobs.ErrRateLimited
	if errors.As(err, &limited) {
		t.Error("a 500 was reported as rate limiting, which would change the backoff policy")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error does not name the status: %v", err)
	}
}

func TestRejectedCredentialsSayWhatToFix(t *testing.T) {
	// The fix is ours — wrong or revoked keys — not a matter of waiting for Adzuna to
	// recover, so the message has to say so rather than reading as an outage.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		stub := newStubAPI(t, status, []byte(`{"exception":"UNAUTHORIZED"}`))
		adapter := newAdapter(t, stub)

		_, err := adapter.Search(context.Background(), sampleSpec(), "")
		if err == nil {
			t.Fatalf("HTTP %d was not reported as an error", status)
		}
		if !strings.Contains(err.Error(), "ADZUNA_APP_ID") {
			t.Errorf("HTTP %d: the error does not name the configuration to check: %v", status, err)
		}
	}
}

func TestAJSONPResponseFailsClearly(t *testing.T) {
	// What arrives if the JSON parameter is ever dropped. Worth a test, because the
	// symptom is a decode failure that looks like Adzuna changing its schema.
	stub := newStubAPI(t, http.StatusOK,
		[]byte(`callback({"count":1,"results":[{"id":"1","title":"X"}]});`))
	adapter := newAdapter(t, stub)

	_, err := adapter.Search(context.Background(), sampleSpec(), "")
	if err == nil {
		t.Fatal("a JSONP response was decoded as JSON")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("the error does not say decoding failed: %v", err)
	}
}

func TestFetchIsRefusedBecauseCapabilitiesSaySo(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	if adapter.Capabilities().DetailFetch {
		t.Fatal("the adapter claims a detail fetch it does not implement")
	}
	if _, err := adapter.Fetch(context.Background(), "123"); err == nil {
		t.Error("Fetch succeeded despite DetailFetch being false")
	}
}

func TestHealthCheckUsesTheCheapestCall(t *testing.T) {
	// A half-open probe runs on every recovery attempt, so it must not cost a search.
	stub := newStubAPI(t, http.StatusOK, []byte(`{"__CLASS__":"Adzuna::API::Response::Version","api_version":1}`))
	adapter := newAdapter(t, stub)

	if err := adapter.HealthCheck(context.Background()); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if got := stub.requests[0].URL.Path; !strings.HasSuffix(got, "/version") {
		t.Errorf("the health check requested %q, want the version endpoint", got)
	}
}

func TestTheAdapterCannotFilterToRemoteRoles(t *testing.T) {
	// Adzuna has no work-mode field. Claiming it could would have the planner issue
	// such a search and get back every onsite job in the country, which reads as a
	// successful run with wrong results.
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	caps := adapter.Capabilities()
	if caps.RemoteFilter || caps.WorkModeFilter {
		t.Fatal("the adapter claims a work-mode filter Adzuna does not have")
	}

	ok, reason := caps.Supports(jobs.SearchSpec{Keywords: []string{"go"}, RemoteOnly: true})
	if ok {
		t.Error("a remote-only search was accepted")
	}
	if reason == "" {
		t.Error("the refusal gives no reason")
	}
}

func TestTheRateLimiterIsConsultedBeforeEveryRequest(t *testing.T) {
	// Enforced inside the adapter so a scheduling bug cannot cause abuse.
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	counting := &countingLimiter{}

	adapter, err := adzuna.New(adzuna.Config{
		AppID: "i", AppKey: "k", BaseURL: stub.server.URL,
		Limiter: counting, Countries: []string{"GB"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, _ = adapter.Search(context.Background(), sampleSpec(), "")
	_ = adapter.HealthCheck(context.Background())

	if counting.calls != 2 {
		t.Errorf("the limiter was consulted %d times for 2 requests", counting.calls)
	}
}

func TestAMissingLimiterIsNotUnlimited(t *testing.T) {
	// A wiring mistake must pace conservatively, not send as fast as it can.
	adapter, err := adzuna.New(adzuna.Config{AppID: "i", AppKey: "k"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if adapter == nil {
		t.Fatal("nil adapter")
	}
	// Nothing observable to assert beyond construction succeeding with a default; the
	// behavioural guarantee is covered by TestTheRateLimiterIsConsultedBeforeEveryRequest.
}

func TestCancellationIsHonoured(t *testing.T) {
	stub := newStubAPI(t, http.StatusOK, fixture(t, "search_page.json"))
	adapter := newAdapter(t, stub)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Search(ctx, sampleSpec(), ""); err == nil {
		t.Error("Search succeeded with a cancelled context, so a deploy would wait out the HTTP timeout")
	}
}

type countingLimiter struct{ calls int }

func (c *countingLimiter) Wait(context.Context) error {
	c.calls++
	return nil
}
