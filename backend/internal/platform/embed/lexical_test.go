package embed_test

import (
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
)

// taxonomyExpander stands in for the skill taxonomy, which is what makes "Golang"
// resolve to "go" in production.
var taxonomyExpander = embed.MapExpander{
	"golang":   "go",
	"js":       "javascript",
	"ts":       "typescript",
	"k8s":      "kubernetes",
	"py":       "python",
	"postgres": "postgresql",
	"node":     "nodejs",
	"ml":       "machinelearning",
}

func TestTheMotivatingCase(t *testing.T) {
	// "Go Developer" and "Backend Engineer (Golang)" are the same job and share no
	// words. A naive token overlap scores them at zero. This is the case the whole
	// lexical design exists to handle.
	score := embed.TitleSimilarity("Go Developer", "Backend Engineer (Golang)", taxonomyExpander)

	if score < 0.6 {
		t.Fatalf("similarity = %.3f, want at least 0.6; the fallback cannot match the job it was built for", score)
	}
	t.Logf("Go Developer / Backend Engineer (Golang) = %.3f", score)

	// Without the taxonomy the Golang/Go link is lost and the score should be
	// noticeably worse — which is the evidence that the expander is doing the work
	// rather than something else coincidentally matching.
	without := embed.TitleSimilarity("Go Developer", "Backend Engineer (Golang)", embed.NoExpansion{})
	if without >= score {
		t.Errorf("score without the taxonomy (%.3f) is not worse than with it (%.3f); "+
			"the expander is not contributing", without, score)
	}
}

func TestTitleSimilarityOnRealVariation(t *testing.T) {
	// Pairs taken from how postings and CVs actually differ. The thresholds are loose
	// on purpose: this is a fallback, and over-fitting it to exact numbers would make
	// the test brittle without making the matching better.
	cases := []struct {
		candidate, posting string
		minScore           float64
	}{
		{"Backend Engineer", "Senior Backend Engineer", 0.9},
		{"Software Engineer", "Software Developer", 0.9},
		{"Backend Developer", "Backend Engineer", 0.9},
		{"Senior Backend Engineer", "Backend Engineer (Remote)", 0.8},
		{"Full Stack Developer", "Fullstack Engineer", 0.8},
		{"DevOps Engineer", "Senior DevOps Engineer — Kubernetes", 0.6},
		{"Data Scientist", "Senior Data Scientist", 0.9},
		{"Frontend Engineer", "Front End Developer", 0.8},
		{"SRE", "Site Reliability Engineer", 0.4},
		{"Python Developer", "Senior Python Engineer", 0.8},
		{"Platform Engineer", "Platform Engineering Lead", 0.6},
	}

	for _, tc := range cases {
		t.Run(tc.candidate+" / "+tc.posting, func(t *testing.T) {
			score := embed.TitleSimilarity(tc.candidate, tc.posting, taxonomyExpander)
			if score < tc.minScore {
				t.Fatalf("similarity = %.3f, want at least %.2f", score, tc.minScore)
			}
			t.Logf("%.3f", score)
		})
	}
}

func TestTitleSimilarityKeepsDifferentRolesApart(t *testing.T) {
	// The failure that matters: matching a candidate into a role they cannot do. A
	// "Senior Engineer" must not match "Senior Manager" strongly just because they
	// share the word "Senior" — which is exactly what an unweighted overlap would do.
	cases := []struct {
		candidate, posting string
		maxScore           float64
	}{
		{"Senior Backend Engineer", "Senior Engineering Manager", 0.7},
		{"Backend Engineer", "Sales Manager", 0.2},
		{"Software Engineer", "Marketing Manager", 0.2},
		{"Data Scientist", "Data Entry Clerk", 0.5},
		{"Frontend Engineer", "Backend Engineer", 0.7},
		{"Senior Engineer", "Junior Engineer", 0.9},
		{"QA Engineer", "Security Engineer", 0.7},
		{"Product Manager", "Project Manager", 0.7},
		{"Accountant", "Software Engineer", 0.1},
	}

	for _, tc := range cases {
		t.Run(tc.candidate+" / "+tc.posting, func(t *testing.T) {
			score := embed.TitleSimilarity(tc.candidate, tc.posting, taxonomyExpander)
			if score > tc.maxScore {
				t.Fatalf("similarity = %.3f, want at most %.2f; these are different roles",
					score, tc.maxScore)
			}
			t.Logf("%.3f", score)
		})
	}
}

func TestSeniorityWordsAreWeightedDown(t *testing.T) {
	// Seniority is scored on its own dimension. Letting it carry full weight in title
	// similarity would double-count it and make "Senior Manager" look like "Senior
	// Engineer".
	sharedSeniorityOnly := embed.TitleSimilarity("Senior Accountant", "Senior Welder", taxonomyExpander)
	if sharedSeniorityOnly > 0.3 {
		t.Fatalf("two unrelated roles sharing only 'Senior' scored %.3f, want at most 0.3",
			sharedSeniorityOnly)
	}

	// And a seniority difference must not destroy an otherwise strong match: the
	// seniority dimension handles that, not this one.
	acrossSeniority := embed.TitleSimilarity("Senior Backend Engineer", "Junior Backend Engineer", taxonomyExpander)
	if acrossSeniority < 0.8 {
		t.Fatalf("the same role at different seniority scored %.3f, want at least 0.8", acrossSeniority)
	}
}

func TestPostingPaddingDoesNotPunishTheCandidate(t *testing.T) {
	// Postings pad their titles. Dividing by the union would punish a candidate for the
	// employer's verbosity, so containment against the smaller side is used instead.
	score := embed.TitleSimilarity(
		"Backend Engineer",
		"Senior Backend Engineer (Go) — Remote, Fintech, Permanent, Immediate Start",
		taxonomyExpander)

	if score < 0.8 {
		t.Fatalf("similarity = %.3f, want at least 0.8 despite the padded posting title", score)
	}
}

func TestTitleSimilarityHandlesDegenerateInput(t *testing.T) {
	cases := []struct{ candidate, posting string }{
		{"", "Backend Engineer"},
		{"Backend Engineer", ""},
		{"", ""},
		{"   ", "Backend Engineer"},
		{"---", "Backend Engineer"},
		{"!!!", "???"},
	}
	for _, tc := range cases {
		score := embed.TitleSimilarity(tc.candidate, tc.posting, taxonomyExpander)
		if score != 0 {
			t.Errorf("TitleSimilarity(%q, %q) = %.3f, want 0", tc.candidate, tc.posting, score)
		}
	}
}

func TestTitlesOfOnlyLowInformationWordsScoreZero(t *testing.T) {
	// "Senior Remote Role" matching "Senior Remote Position" at 1.0 would tell a
	// candidate nothing, and would match them to everything.
	score := embed.TitleSimilarity("Senior Remote Role", "Senior Remote Position", taxonomyExpander)
	if score > 0.5 {
		t.Fatalf("two content-free titles scored %.3f, want at most 0.5", score)
	}
}

func TestTitleSimilarityIsBounded(t *testing.T) {
	pairs := [][2]string{
		{"Backend Engineer", "Backend Engineer"},
		{"engineer engineer engineer", "engineer"},
		{"Go Go Go Developer", "Go Developer"},
	}
	for _, pair := range pairs {
		score := embed.TitleSimilarity(pair[0], pair[1], taxonomyExpander)
		if score < 0 || score > 1 {
			t.Fatalf("TitleSimilarity(%q, %q) = %.3f, outside [0, 1]", pair[0], pair[1], score)
		}
	}
}

func TestRepeatedTokensDoNotInflateTheScore(t *testing.T) {
	// A posting that says "engineer" three times is not three times more of an
	// engineering role.
	plain := embed.TitleSimilarity("Backend Engineer", "Backend Engineer", taxonomyExpander)
	repeated := embed.TitleSimilarity("Backend Engineer", "Engineer Engineer Backend Engineer", taxonomyExpander)

	if repeated > plain {
		t.Fatalf("repetition inflated the score: %.3f vs %.3f", repeated, plain)
	}
}

func TestTitleSimilarityIsSymmetric(t *testing.T) {
	// Not strictly required by the containment measure, but it holds because the
	// denominator is the smaller side either way — and a caller would reasonably
	// assume it.
	pairs := [][2]string{
		{"Backend Engineer", "Senior Backend Engineer (Go)"},
		{"Go Developer", "Backend Engineer (Golang)"},
		{"Data Scientist", "Machine Learning Engineer"},
	}
	for _, pair := range pairs {
		forward := embed.TitleSimilarity(pair[0], pair[1], taxonomyExpander)
		reverse := embed.TitleSimilarity(pair[1], pair[0], taxonomyExpander)
		if forward != reverse {
			t.Errorf("asymmetric for %q / %q: %.3f vs %.3f", pair[0], pair[1], forward, reverse)
		}
	}
}

func TestTokeniseExpandsThroughTheTaxonomy(t *testing.T) {
	// splitWords breaks "Node.js" into "node" and "js", and the expander is what puts
	// them back together into canonical skills.
	tokens := embed.Tokenise("Node.js and Golang with k8s", taxonomyExpander)

	found := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		found[token] = true
	}

	for _, want := range []string{"nodejs", "go", "kubernetes"} {
		if !found[want] {
			t.Errorf("expected token %q, got %v", want, tokens)
		}
	}
	// "and" and "with" are weighted to zero rather than removed, so they may appear as
	// tokens; what matters is that they contribute nothing to a score.
}

func TestTokeniseKeepsPlusAndHash(t *testing.T) {
	tokens := embed.Tokenise("C++ and C# developer", taxonomyExpander)

	found := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		found[token] = true
	}
	if !found["c++"] {
		t.Errorf("c++ was lost: %v", tokens)
	}
	if !found["c#"] {
		t.Errorf("c# was lost: %v", tokens)
	}
}

func TestTokeniseToleratesANilExpander(t *testing.T) {
	// A caller with no taxonomy available must still get tokens rather than a panic.
	tokens := embed.Tokenise("Backend Engineer", nil)
	if len(tokens) == 0 {
		t.Fatal("Tokenise returned nothing with a nil expander")
	}
}

func TestSetSimilarityUsesContainment(t *testing.T) {
	// A posting listing 30 skills against a candidate with 12 of them is a strong
	// match. A Jaccard measure would score it at 0.4 and reject it.
	candidate := []string{"Go", "PostgreSQL", "Kubernetes", "Docker", "Terraform"}
	posting := []string{
		"Go", "PostgreSQL", "Kubernetes", "Docker", "Terraform",
		"AWS", "Kafka", "Redis", "gRPC", "Prometheus", "Grafana", "Helm",
		"ArgoCD", "Linux", "Bash", "Python", "CI/CD", "Microservices",
	}

	score := embed.SetSimilarity(candidate, posting, taxonomyExpander)
	if score != 1.0 {
		t.Fatalf("SetSimilarity = %.3f, want 1.0; every candidate skill is in the posting", score)
	}

	jaccard := float64(5) / float64(18)
	if score <= jaccard {
		t.Fatalf("the measure behaves like Jaccard (%.3f), which would reject a strong match", jaccard)
	}
}

func TestSetSimilarityExpandsAliases(t *testing.T) {
	candidate := []string{"Golang", "JS", "k8s"}
	posting := []string{"Go", "JavaScript", "Kubernetes"}

	score := embed.SetSimilarity(candidate, posting, taxonomyExpander)
	if score != 1.0 {
		t.Fatalf("SetSimilarity = %.3f, want 1.0; these are the same three skills", score)
	}
}

func TestSetSimilarityKeepsMultiWordTermsWhole(t *testing.T) {
	// Splitting "machine learning" would match "learning" against unrelated postings.
	candidate := []string{"Machine Learning"}
	unrelated := []string{"Learning and Development", "Organisational Learning"}

	score := embed.SetSimilarity(candidate, unrelated, taxonomyExpander)
	if score > 0 {
		t.Fatalf("SetSimilarity = %.3f, want 0; 'machine learning' is not 'learning'", score)
	}
}

func TestSetSimilarityHandlesEmptySets(t *testing.T) {
	if got := embed.SetSimilarity(nil, []string{"Go"}, taxonomyExpander); got != 0 {
		t.Errorf("empty candidate set = %.3f, want 0", got)
	}
	if got := embed.SetSimilarity([]string{"Go"}, nil, taxonomyExpander); got != 0 {
		t.Errorf("empty posting set = %.3f, want 0", got)
	}
	if got := embed.SetSimilarity([]string{"", "  "}, []string{"Go"}, taxonomyExpander); got != 0 {
		t.Errorf("blank terms = %.3f, want 0", got)
	}
}

func TestSetSimilarityIsBounded(t *testing.T) {
	// Duplicates on either side must not push the score past 1.
	candidate := []string{"Go", "go", "GO", "Golang"}
	posting := []string{"Go"}

	score := embed.SetSimilarity(candidate, posting, taxonomyExpander)
	if score < 0 || score > 1 {
		t.Fatalf("SetSimilarity = %.3f, outside [0, 1]", score)
	}
}
