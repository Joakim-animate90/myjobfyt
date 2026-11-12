package remotefeeds_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/jobstest"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/jobs/remotefeeds"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// serve stands in for a feed, returning a recorded body.
//
// Paginated requests get distinct ids per page. Serving the same body for every page
// would make the conformance suite's resumability check compare page one with itself,
// which it correctly reported as a cursor being ignored.
func serve(t *testing.T, status int, body []byte) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var requests []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(bodyForPage(body, r.URL.Query().Get("page")))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

// bodyForPage rewrites a recorded body so each page carries different postings.
func bodyForPage(body []byte, page string) []byte {
	if page == "" || page == "1" {
		return body
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return body
	}
	data, ok := envelope["data"].([]any)
	if !ok {
		return body
	}
	for _, item := range data {
		if job, ok := item.(map[string]any); ok {
			job["slug"] = fmt.Sprintf("%v-p%s", job["slug"], page)
		}
	}
	rewritten, err := json.Marshal(envelope)
	if err != nil {
		return body
	}
	return rewritten
}

func feedConfig(server *httptest.Server) remotefeeds.Config {
	return remotefeeds.Config{BaseURL: server.URL, Limiter: jobs.Unlimited()}
}

// --- conformance ----------------------------------------------------------------

func TestRemotiveConformance(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "remotive_page.json"))
	jobstest.Run(t, jobstest.Subject{
		Adapter:    remotefeeds.NewRemotive(feedConfig(server)),
		SampleSpec: jobs.SearchSpec{Keywords: []string{"engineer"}},
	})
}

func TestRemoteOKConformance(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "remoteok_page.json"))
	jobstest.Run(t, jobstest.Subject{
		Adapter:    remotefeeds.NewRemoteOK(feedConfig(server)),
		SampleSpec: jobs.SearchSpec{Keywords: []string{"engineer"}},
	})
}

func TestArbeitnowConformance(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "arbeitnow_page.json"))
	jobstest.Run(t, jobstest.Subject{
		Adapter: remotefeeds.NewArbeitnow(feedConfig(server)),
		SampleSpec: jobs.SearchSpec{
			Keywords: []string{"personal"}, Location: "Leipzig", Country: "DE",
		},
	})
}

// --- capabilities ---------------------------------------------------------------

func TestTheRemoteOnlyFeedsRefuseAnOnsiteSearch(t *testing.T) {
	// Everything these two carry is remote. Answering an onsite search with remote jobs
	// would look like a successful run while every result was wrong for the candidate.
	server, _ := serve(t, http.StatusOK, fixture(t, "remotive_page.json"))
	for name, adapter := range map[string]jobs.Adapter{
		"remotive": remotefeeds.NewRemotive(feedConfig(server)),
		"remoteok": remotefeeds.NewRemoteOK(feedConfig(server)),
	} {
		t.Run(name, func(t *testing.T) {
			caps := adapter.Capabilities()
			if !caps.RemoteOnly {
				t.Fatal("the adapter does not declare itself remote-only")
			}
			for _, mode := range []string{"onsite", "hybrid"} {
				ok, reason := caps.Supports(jobs.SearchSpec{
					Keywords: []string{"engineer"}, WorkModes: []string{mode},
				})
				if ok {
					t.Errorf("a %s search was accepted", mode)
				}
				if reason == "" {
					t.Errorf("the %s refusal gives no reason", mode)
				}
			}
			// And a remote search is served.
			if ok, reason := caps.Supports(jobs.SearchSpec{
				Keywords: []string{"engineer"}, RemoteOnly: true,
			}); !ok {
				t.Errorf("a remote search was refused: %s", reason)
			}
		})
	}
}

func TestArbeitnowIsNotRemoteOnly(t *testing.T) {
	// The issue's framing grouped Arbeitnow with the remote feeds. Reading the live
	// feed settled it: 286 of 300 postings were not remote. It is a general German job
	// board carrying a remote flag, and declaring it remote-only would make it refuse
	// location-bound searches — which is most of what it actually holds.
	server, _ := serve(t, http.StatusOK, fixture(t, "arbeitnow_page.json"))
	caps := remotefeeds.NewArbeitnow(feedConfig(server)).Capabilities()

	if caps.RemoteOnly {
		t.Error("Arbeitnow is declared remote-only, which would refuse the location searches " +
			"that match most of its postings")
	}
	if !caps.RemoteFilter {
		t.Error("Arbeitnow does not declare the remote filter it applies")
	}
	// A plain location search is served, which is the point: declaring Arbeitnow
	// remote-only would have refused this and excluded most of its postings.
	if ok, reason := caps.Supports(jobs.SearchSpec{
		Keywords: []string{"entwickler"}, Location: "Berlin", Country: "DE",
	}); !ok {
		t.Errorf("a German location search was refused: %s", reason)
	}

	// An onsite-SPECIFIC search is still refused, and that is correct rather than a
	// gap. Arbeitnow's remote flag being false means the posting did not say it was
	// remote, which is not the same as the employer stating the role is office-based —
	// so the source cannot confirm onsite work and must not pretend to.
	if ok, _ := caps.Supports(jobs.SearchSpec{
		Keywords: []string{"entwickler"}, WorkModes: []string{"onsite"},
	}); ok {
		t.Error("an onsite-specific search was accepted; a false remote flag is not a " +
			"statement that the role is onsite")
	}

	// But a remote-only search is served, because that flag is meaningful when set.
	if ok, reason := caps.Supports(jobs.SearchSpec{
		Keywords: []string{"entwickler"}, RemoteOnly: true,
	}); !ok {
		t.Errorf("a remote search was refused despite the feed carrying a remote flag: %s", reason)
	}
}

func TestArbeitnowDeclaresItsRegions(t *testing.T) {
	// So the planner does not spend a request asking a German board about Nairobi.
	server, _ := serve(t, http.StatusOK, fixture(t, "arbeitnow_page.json"))
	caps := remotefeeds.NewArbeitnow(feedConfig(server)).Capabilities()

	if ok, _ := caps.Supports(jobs.SearchSpec{Keywords: []string{"engineer"}, Country: "DE"}); !ok {
		t.Error("a German search was refused by a German board")
	}
	ok, reason := caps.Supports(jobs.SearchSpec{Keywords: []string{"engineer"}, Country: "KE"})
	if ok {
		t.Error("a Kenyan search was accepted by a German board")
	}
	if reason == "" {
		t.Error("the region refusal gives no reason")
	}
}

// --- the Remote OK legal-notice element -----------------------------------------

func TestRemoteOKSkipsTheLegalNoticeElement(t *testing.T) {
	// The feed is a JSON array whose FIRST element is a legal notice, not a posting.
	// Treating it as a job would create an untitled, id-less posting on every run, and
	// it is the single most likely mistake in this adapter.
	server, _ := serve(t, http.StatusOK, fixture(t, "remoteok_page.json"))
	adapter := remotefeeds.NewRemoteOK(feedConfig(server))

	page, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// The fixture has the notice plus three postings.
	if len(page.Postings) != 3 {
		t.Fatalf("got %d postings, want 3; the legal notice is probably being counted", len(page.Postings))
	}
	for _, posting := range page.Postings {
		if posting.ExternalID == "" {
			t.Error("a posting has no external id, which is what the notice element looks like")
		}
		if strings.Contains(string(posting.Payload), `"legal"`) {
			t.Error("the legal-notice element was stored as a posting")
		}
	}
}

func TestDecodingTheLegalNoticeIsRefusedDistinctly(t *testing.T) {
	// If it ever reaches storage, the message has to say what it is rather than
	// reporting a posting with a missing field.
	notice := []byte(`{"last_updated":1794499238,"legal":"API Terms of Service: Please link back…"}`)
	_, err := remotefeeds.DecodeRemoteOK(notice)
	if err == nil {
		t.Fatal("the legal notice decoded as a posting")
	}
	if !strings.Contains(err.Error(), "legal notice") {
		t.Errorf("the error does not identify what the payload is: %v", err)
	}
}

// --- salary integrity -----------------------------------------------------------

func TestRemoteOKZeroSalaryBecomesUnknownNotZero(t *testing.T) {
	// Sixty-eight of ninety-eight postings in a live sample had salary_min of 0. It
	// means "not stated", and carrying it through would show a candidate a zero-salary
	// job and match it against the lower bound of any salary filter.
	payload := []byte(`{"id":"1","position":"Engineer","company":"X","salary_min":0,"salary_max":0}`)
	posting, err := remotefeeds.DecodeRemoteOK(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.SalaryMin != nil || posting.SalaryMax != nil {
		t.Errorf("a zero salary became %v–%v rather than unknown", posting.SalaryMin, posting.SalaryMax)
	}
	if posting.SalaryCurrency != "" || posting.SalaryPeriod != "" {
		t.Error("a currency and period were set for a posting with no salary")
	}
}

func TestRemoteOKARealSalaryIsKept(t *testing.T) {
	payload := []byte(`{"id":"1","position":"Engineer","salary_min":90000,"salary_max":140000}`)
	posting, err := remotefeeds.DecodeRemoteOK(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.SalaryMin == nil || *posting.SalaryMin != 90000 {
		t.Errorf("SalaryMin = %v, want 90000", posting.SalaryMin)
	}
	if posting.SalaryCurrency != "USD" || posting.SalaryPeriod != "year" {
		t.Errorf("currency/period = %q/%q, want USD/year", posting.SalaryCurrency, posting.SalaryPeriod)
	}
}

func TestRemotiveSalaryTextIsNeverParsedIntoANumber(t *testing.T) {
	// Remotive sends free text: "$14 - $20/hour", "OTE $25k - $35k", or empty. Guessing
	// a figure from that would produce a salary the employer never stated, filterable as
	// though they had. The text is kept where a candidate can read it and judge.
	payload := []byte(`{"id":"1","title":"Engineer","salary":"OTE $25k - $35k","url":"https://remotive.com/x"}`)
	posting, err := remotefeeds.DecodeRemotive(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.SalaryMin != nil || posting.SalaryMax != nil {
		t.Errorf("free-text salary was parsed into %v–%v", posting.SalaryMin, posting.SalaryMax)
	}
	if !strings.Contains(posting.Description, "OTE $25k - $35k") {
		t.Error("the salary text was dropped, so the candidate cannot see what was offered")
	}
}

// --- attribution ----------------------------------------------------------------

func TestEveryFeedCarriesItsRequiredAttribution(t *testing.T) {
	// Remotive and Remote OK both state they will suspend API access without a visible
	// link back and a named credit, so the credit travels with the posting rather than
	// being a fact about the source a UI has to remember to look up.
	cases := map[string]struct {
		decode     func([]byte) (jobs.CanonicalPosting, error)
		payload    string
		wantName   string
		wantFollow bool
	}{
		"remotive": {
			decode:   remotefeeds.DecodeRemotive,
			payload:  `{"id":"1","title":"Engineer","url":"https://remotive.com/remote-jobs/x"}`,
			wantName: "Remotive",
		},
		"remoteok": {
			decode:     remotefeeds.DecodeRemoteOK,
			payload:    `{"id":"1","position":"Engineer","url":"https://remoteOK.com/remote-jobs/x"}`,
			wantName:   "Remote OK",
			wantFollow: true,
		},
		"arbeitnow": {
			decode:   remotefeeds.DecodeArbeitnow,
			payload:  `{"slug":"x","title":"Entwickler","url":"https://www.arbeitnow.com/jobs/x"}`,
			wantName: "Arbeitnow",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			posting, err := tc.decode([]byte(tc.payload))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !posting.Attribution.Required() {
				t.Fatal("no attribution was recorded; these feeds suspend access without a credit")
			}
			if posting.Attribution.SourceName != tc.wantName {
				t.Errorf("SourceName = %q, want %q", posting.Attribution.SourceName, tc.wantName)
			}
			if posting.Attribution.SourceURL == "" {
				t.Error("no link back was recorded")
			}
			if posting.Attribution.NoFollowProhibited != tc.wantFollow {
				t.Errorf("NoFollowProhibited = %v, want %v; Remote OK's terms specifically "+
					"require a followable link", posting.Attribution.NoFollowProhibited, tc.wantFollow)
			}
		})
	}
}

// --- mapping against the real captured data -------------------------------------

func TestRemotiveMapsTheRealFeed(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "remotive_page.json"))
	adapter := remotefeeds.NewRemotive(feedConfig(server))

	page, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Postings) == 0 {
		t.Fatal("no postings from the recorded feed")
	}

	for _, posting := range page.Postings {
		decoded, err := remotefeeds.DecodeRemotive(posting.Payload)
		if err != nil {
			t.Errorf("posting %s does not decode: %v", posting.ExternalID, err)
			continue
		}
		if decoded.Title == "" || decoded.CompanyName == "" {
			t.Errorf("posting %s lost its title or company", posting.ExternalID)
		}
		if decoded.WorkMode != "remote" {
			t.Errorf("posting %s has work mode %q; every Remotive posting is remote",
				posting.ExternalID, decoded.WorkMode)
		}
		if decoded.PostedAt == nil {
			t.Errorf("posting %s lost its publication date", posting.ExternalID)
		}
	}
}

func TestArbeitnowMapsTheRealFeedIncludingGermanJobTypes(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "arbeitnow_page.json"))
	adapter := remotefeeds.NewArbeitnow(feedConfig(server))

	page, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Postings) == 0 {
		t.Fatal("no postings from the recorded feed")
	}

	for _, posting := range page.Postings {
		decoded, err := remotefeeds.DecodeArbeitnow(posting.Payload)
		if err != nil {
			t.Errorf("posting %s does not decode: %v", posting.ExternalID, err)
			continue
		}
		if decoded.Title == "" {
			t.Errorf("posting %s lost its title", posting.ExternalID)
		}
		if decoded.PostedAt == nil {
			t.Errorf("posting %s lost its created_at; it arrives as a unix timestamp in a "+
				"JSON string, which a plain RFC3339 parse would miss", posting.ExternalID)
		}
	}
}

func TestUnbefristetIsPermanentNotTemporary(t *testing.T) {
	// "unbefristet" means permanent and contains "befristet", so a substring match
	// without an explicit exclusion reports every permanent German role as temporary.
	payload := []byte(`{"slug":"x","title":"Entwickler","job_types":["Unbefristeter arbeitsvertrag"]}`)
	posting, err := remotefeeds.DecodeArbeitnow(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.EmploymentType == "Temporary" {
		t.Error("a permanent contract (unbefristet) was reported as temporary")
	}
	if posting.EmploymentType != "Full-time" {
		t.Errorf("EmploymentType = %q, want Full-time", posting.EmploymentType)
	}

	// And a genuinely fixed-term role still maps to Temporary.
	fixed := []byte(`{"slug":"y","title":"Entwickler","job_types":["Befristeter Vertrag"]}`)
	temp, err := remotefeeds.DecodeArbeitnow(fixed)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if temp.EmploymentType != "Temporary" {
		t.Errorf("a fixed-term role mapped to %q, want Temporary", temp.EmploymentType)
	}
}

func TestArbeitnowAbsentRemoteFlagIsNotReportedAsOnsite(t *testing.T) {
	// The flag's absence means the posting did not say it was remote, which is not the
	// same as the employer stating it is office-based.
	payload := []byte(`{"slug":"x","title":"Entwickler","remote":false,"location":"Berlin"}`)
	posting, err := remotefeeds.DecodeArbeitnow(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if posting.WorkMode != "" {
		t.Errorf("WorkMode = %q; a false remote flag is not a statement that the role is onsite",
			posting.WorkMode)
	}
}

// --- failure modes --------------------------------------------------------------

func TestAFeedOutageDegradesTheRunOnly(t *testing.T) {
	// The acceptance criterion. Routed through the real executor, because that is what
	// actually has to keep going.
	server, _ := serve(t, http.StatusInternalServerError, []byte(`{"error":"down"}`))
	adapter := remotefeeds.NewRemotive(feedConfig(server))

	source := jobs.SourceRecord{
		Slug: "remotive", Name: "Remotive", Kind: jobs.KindAuthorisedFeed,
		State: jobs.StateEnabled, RateLimit: 1,
		LegalBasis: "Public JSON feed offered for third-party use",
	}.Bind(adapter)

	exec := jobs.NewExecutor(jobs.ExecutorConfig{}, discardLogger())
	result := exec.Search(context.Background(), source, jobs.SearchSpec{Keywords: []string{"engineer"}}, "")

	if result.Outcome != jobs.OutcomeFailed {
		t.Errorf("outcome = %s, want failed", result.Outcome)
	}
	if result.Reason == "" {
		t.Error("the failure carries no reason for the run record")
	}
	if result.Breaker.ConsecutiveFailures != 1 {
		t.Errorf("consecutive failures = %d, want 1", result.Breaker.ConsecutiveFailures)
	}
}

func TestA403SaysItMayBeATermsProblem(t *testing.T) {
	// These feeds suspend access for excessive requests or missing attribution rather
	// than returning 401, so a 403 is more likely a policy problem than a transient one.
	server, _ := serve(t, http.StatusForbidden, []byte(`Forbidden`))
	adapter := remotefeeds.NewRemoteOK(feedConfig(server))

	_, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "")
	if err == nil {
		t.Fatal("a 403 was not reported as an error")
	}
	if !strings.Contains(err.Error(), "attribution") {
		t.Errorf("the error does not mention the likely cause: %v", err)
	}
}

func TestANonPaginatedFeedRejectsACursor(t *testing.T) {
	// A cursor means a caller believes the feed pages. Failing is better than silently
	// refetching page one for ever.
	server, _ := serve(t, http.StatusOK, fixture(t, "remotive_page.json"))
	for name, adapter := range map[string]jobs.Adapter{
		"remotive": remotefeeds.NewRemotive(feedConfig(server)),
		"remoteok": remotefeeds.NewRemoteOK(feedConfig(server)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "2"); err == nil {
				t.Error("a cursor was accepted by a non-paginated feed")
			}
		})
	}
}

func TestArbeitnowPagesFromItsOwnNextLink(t *testing.T) {
	// Not from the page size: the client-side filtering means a page can come back
	// nearly empty while more pages remain, and stopping on a short page would cut the
	// run off after the first one.
	server, requests := serve(t, http.StatusOK, fixture(t, "arbeitnow_page.json"))
	adapter := remotefeeds.NewArbeitnow(feedConfig(server))

	// The fixture carries three postings and a next link.
	page, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if page.NextCursor != "2" {
		t.Errorf("NextCursor = %q, want 2 from the feed's own next link", page.NextCursor)
	}

	if _, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "2"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(*requests) < 2 || !strings.Contains((*requests)[1].URL.RawQuery, "page=2") {
		t.Error("resuming did not request page 2")
	}
}

func TestAFilteredOutPageStillOffersTheNextCursor(t *testing.T) {
	// The reason paging follows the next link. A keyword that matches nothing on page
	// one must not end the run.
	server, _ := serve(t, http.StatusOK, fixture(t, "arbeitnow_page.json"))
	adapter := remotefeeds.NewArbeitnow(feedConfig(server))

	page, err := adapter.Search(context.Background(),
		jobs.SearchSpec{Keywords: []string{"zzzzznomatch"}}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Postings) != 0 {
		t.Fatalf("got %d postings for a keyword that matches nothing", len(page.Postings))
	}
	if page.NextCursor == "" {
		t.Error("a fully filtered page ended the run, so later pages are never reached")
	}
}

func TestKeywordFilteringIsAppliedClientSide(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "remoteok_page.json"))
	adapter := remotefeeds.NewRemoteOK(feedConfig(server))

	all, err := adapter.Search(context.Background(), jobs.SearchSpec{}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	filtered, err := adapter.Search(context.Background(),
		jobs.SearchSpec{Keywords: []string{"zzzzznomatch"}}, "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(all.Postings) == 0 {
		t.Fatal("the unfiltered search returned nothing")
	}
	if len(filtered.Postings) != 0 {
		t.Errorf("a keyword matching nothing returned %d postings", len(filtered.Postings))
	}
}

func TestCancellationIsHonouredByEveryFeed(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "remotive_page.json"))
	adapters := map[string]jobs.Adapter{
		"remotive":  remotefeeds.NewRemotive(feedConfig(server)),
		"remoteok":  remotefeeds.NewRemoteOK(feedConfig(server)),
		"arbeitnow": remotefeeds.NewArbeitnow(feedConfig(server)),
	}
	for name, adapter := range adapters {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := adapter.Search(ctx, jobs.SearchSpec{}, ""); err == nil {
				t.Error("Search succeeded with a cancelled context")
			}
		})
	}
}

func TestEachAdapterRefusesFetchBecauseCapabilitiesSaySo(t *testing.T) {
	server, _ := serve(t, http.StatusOK, fixture(t, "remotive_page.json"))
	adapters := map[string]jobs.Adapter{
		"remotive":  remotefeeds.NewRemotive(feedConfig(server)),
		"remoteok":  remotefeeds.NewRemoteOK(feedConfig(server)),
		"arbeitnow": remotefeeds.NewArbeitnow(feedConfig(server)),
	}
	for name, adapter := range adapters {
		t.Run(name, func(t *testing.T) {
			if adapter.Capabilities().DetailFetch {
				t.Fatal("the adapter claims a detail fetch it does not implement")
			}
			if _, err := adapter.Fetch(context.Background(), "1"); err == nil {
				t.Error("Fetch succeeded despite DetailFetch being false")
			}
		})
	}
}

func TestADecodedPostingIsStable(t *testing.T) {
	// A stored payload is re-decoded whenever a mapping changes, so the same bytes must
	// always produce the same posting.
	payload := []byte(`{"id":"1","position":"Engineer","company":"X","salary_min":90000,"date":"2026-11-11T08:00:08+00:00","url":"https://remoteOK.com/x"}`)

	first, err := remotefeeds.DecodeRemoteOK(payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	a, _ := json.Marshal(first)
	for i := 0; i < 10; i++ {
		next, err := remotefeeds.DecodeRemoteOK(payload)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		b, _ := json.Marshal(next)
		if string(a) != string(b) {
			t.Fatalf("decode %d differed", i)
		}
	}
}

// discardLogger keeps the executor quiet in tests.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
