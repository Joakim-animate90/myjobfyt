package cv_test

import (
	"strings"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
)

func TestNormaliseLabelRemovesSeparatorsButNotContent(t *testing.T) {
	// Separator removal is what lets a curated alias list cover real variation without
	// fuzzy matching: four spellings of Node.js collapse to one key.
	cases := map[string]string{
		"Node.js":      "nodejs",
		"node js":      "nodejs",
		"node-js":      "nodejs",
		"NODEJS":       "nodejs",
		"Node_JS":      "nodejs",
		"  Node.js ":   "nodejs",
		"React Native": "reactnative",
		"scikit-learn": "scikitlearn",
		"CI/CD":        "cicd",
		"Objective-C":  "objectivec",
		"Power BI":     "powerbi",
		"UI/UX Design": "uiuxdesign",
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			if got := cv.NormaliseLabel(input); got != want {
				t.Fatalf("NormaliseLabel(%q) = %q, want %q", input, got, want)
			}
		})
	}
}

func TestNormaliseLabelKeepsPlusAndHash(t *testing.T) {
	// Stripping these would collapse C, C++ and C# into one skill — three different
	// languages matching three different sets of jobs.
	if cv.NormaliseLabel("C") == cv.NormaliseLabel("C++") {
		t.Fatal("C and C++ normalise to the same key")
	}
	if cv.NormaliseLabel("C") == cv.NormaliseLabel("C#") {
		t.Fatal("C and C# normalise to the same key")
	}
	if cv.NormaliseLabel("C++") == cv.NormaliseLabel("C#") {
		t.Fatal("C++ and C# normalise to the same key")
	}
	if got := cv.NormaliseLabel("C++"); got != "c++" {
		t.Fatalf("NormaliseLabel(\"C++\") = %q, want c++", got)
	}
	if got := cv.NormaliseLabel("C#"); got != "c#" {
		t.Fatalf("NormaliseLabel(\"C#\") = %q, want c#", got)
	}
}

func TestNormaliseLabelNeverConflatesDistinctSkills(t *testing.T) {
	// The pairs a fuzzy matcher would get wrong. Normalisation must keep them distinct,
	// because it is the only matching step there is.
	pairs := [][2]string{
		{"Java", "JavaScript"},
		{"Scala", "Scale"},
		{"Ruby", "Rust"},
		{"React", "React Native"},
		{"Go", "Golang"}, // same skill, but via alias — not via normalisation
		{"Python", "PyTorch"},
		{"Swift", "SwiftUI"},
		{"Spring", "Spring Boot"},
		{"SQL", "SQLite"},
		{"Angular", "AngularJS"},
	}
	for _, pair := range pairs {
		t.Run(pair[0]+" vs "+pair[1], func(t *testing.T) {
			if cv.NormaliseLabel(pair[0]) == cv.NormaliseLabel(pair[1]) {
				t.Fatalf("%q and %q normalise to the same key %q",
					pair[0], pair[1], cv.NormaliseLabel(pair[0]))
			}
		})
	}
}

func TestNormaliseLabelStripsPunctuationNoise(t *testing.T) {
	// CVs pick up punctuation from their own bullet formatting.
	for _, input := range []string{"Python,", "(Python)", "Python;", "“Python”", "Python•"} {
		if got := cv.NormaliseLabel(input); got != "python" {
			t.Errorf("NormaliseLabel(%q) = %q, want python", input, got)
		}
	}
}

func TestUsableSkillLabelRejectsWhatIsNotASkill(t *testing.T) {
	// A model occasionally puts a sentence in the skills array. Storing it would
	// pollute both the candidate's profile and the taxonomy triage list.
	unusable := []string{
		"", "   ", "-", "•", "123", "2019", "...", "()",
		strings.Repeat("a", cv.MaxSkillLabelLength+1),
		"Extensive experience building distributed systems at scale across multiple teams and organisations",
	}
	for _, label := range unusable {
		if cv.UsableSkillLabel(label) {
			t.Errorf("UsableSkillLabel(%q) = true, want false", label)
		}
	}

	for _, label := range []string{"Go", "C", "C++", "Node.js", "Machine Learning", "k8s"} {
		if !cv.UsableSkillLabel(label) {
			t.Errorf("UsableSkillLabel(%q) = false, want true", label)
		}
	}
}

func TestDedupeLabelsKeepsTheFirstSpelling(t *testing.T) {
	// The first spelling is the one highest in the CV, which is usually the
	// candidate's preferred rendering.
	labels := []string{"Node.js", "nodejs", "NODE JS", "Go", "golang", "", "  ", "Go"}

	out := cv.DedupeLabels(labels)

	if len(out) != 3 {
		t.Fatalf("DedupeLabels returned %d labels, want 3: %v", len(out), out)
	}
	if out[0] != "Node.js" {
		t.Errorf("first label = %q, want the first spelling Node.js", out[0])
	}
	// "Go" and "golang" normalise differently, so both survive here — the alias list
	// resolves them to one canonical skill later.
	if out[1] != "Go" || out[2] != "golang" {
		t.Errorf("labels = %v, want Go and golang both retained for alias resolution", out)
	}
}

func TestMatchOutcomeMatched(t *testing.T) {
	if !cv.MatchedCanonical.Matched() || !cv.MatchedAlias.Matched() {
		t.Error("a canonical or alias match should report Matched")
	}
	if cv.Unmatched.Matched() {
		t.Error("Unmatched reported Matched")
	}
}
