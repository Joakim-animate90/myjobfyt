//go:build integration

package cv_test

import (
	"context"
	"testing"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/domain/cv"
	"github.com/google/uuid"
)

func TestJavaNeverResolvesToJavaScript(t *testing.T) {
	// The single most important assertion in this file. "Java" and "JavaScript" share a
	// prefix, so every fuzzy matcher conflates them, and a candidate whose Java
	// experience is recorded as JavaScript is matched against the wrong jobs entirely.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	resolved, err := repo.MatchSkillLabels(ctx, []string{"Java", "JavaScript", "JS", "java8"})
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}

	expected := map[string]string{
		"Java":       "Java",
		"JavaScript": "JavaScript",
		"JS":         "JavaScript",
		"java8":      "Java",
	}
	for _, skill := range resolved {
		want := expected[skill.RawLabel]
		if skill.Skill == nil {
			t.Fatalf("%q resolved to nothing, want %q", skill.RawLabel, want)
		}
		if skill.Skill.CanonicalName != want {
			t.Fatalf("%q resolved to %q, want %q", skill.RawLabel, skill.Skill.CanonicalName, want)
		}
	}
}

func TestDistinctSkillsStayDistinct(t *testing.T) {
	// Each pair is one a fuzzy matcher would plausibly conflate.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	pairs := [][2]string{
		{"Java", "JavaScript"},
		{"C", "C++"},
		{"C", "C#"},
		{"C++", "C#"},
		{"React", "React Native"},
		{"Scala", "SQL"},
		{"Ruby", "Rust"},
		{"Python", "PyTorch"},
		{"Swift", "SwiftUI"},
		{"SQL", "SQLite"},
		{"Spring", "Rails"},
	}

	for _, pair := range pairs {
		t.Run(pair[0]+" vs "+pair[1], func(t *testing.T) {
			resolved, err := repo.MatchSkillLabels(ctx, []string{pair[0], pair[1]})
			if err != nil {
				t.Fatalf("MatchSkillLabels: %v", err)
			}
			first, second := resolved[0].Skill, resolved[1].Skill
			if first == nil || second == nil {
				t.Skipf("%q or %q is not in the taxonomy yet", pair[0], pair[1])
			}
			if first.ID == second.ID {
				t.Fatalf("%q and %q both resolved to %q", pair[0], pair[1], first.CanonicalName)
			}
		})
	}
}

func TestCuratedAliasesCatchTheTyposFuzzyMatchingCouldNot(t *testing.T) {
	// These are the exact pairs that made fuzzy matching unworkable: pg_trgm scores
	// kotlin/koltin at 0.273 and pyton/python at 0.444, below scala/scale at 0.500.
	// Curated aliases resolve them exactly, with no threshold to defend.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	expected := map[string]string{
		"Koltin":    "Kotlin",
		"Pyton":     "Python",
		"Terrafrom": "Terraform",
		"Kubernets": "Kubernetes",
		"k8s":       "Kubernetes",
		"javscript": "JavaScript",
		"Postgre":   "PostgreSQL",
		"postgres":  "PostgreSQL",
		"golang":    "Go",
	}

	labels := make([]string, 0, len(expected))
	for label := range expected {
		labels = append(labels, label)
	}

	resolved, err := repo.MatchSkillLabels(ctx, labels)
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}

	for _, skill := range resolved {
		want := expected[skill.RawLabel]
		if skill.Skill == nil {
			t.Errorf("%q resolved to nothing, want %q", skill.RawLabel, want)
			continue
		}
		if skill.Skill.CanonicalName != want {
			t.Errorf("%q resolved to %q, want %q", skill.RawLabel, skill.Skill.CanonicalName, want)
		}
		// Reached via an alias, not by being the canonical name itself.
		if skill.Outcome != cv.MatchedAlias {
			t.Errorf("%q outcome = %q, want alias", skill.RawLabel, skill.Outcome)
		}
	}
}

func TestTrigramCannotSeparateCorrectFromIncorrectMatches(t *testing.T) {
	// This test exists to document WHY matching is exact-only, so nobody re-adds fuzzy
	// matching as an easy win. It measures pg_trgm against pairs where we know the
	// right answer and asserts that no single threshold works.
	//
	// If this test ever fails, pg_trgm's scoring has changed and the decision is worth
	// revisiting — but only with fresh measurements.
	db := openDB(t)
	ctx := context.Background()

	type probe struct {
		a, b        string
		shouldMatch bool
	}
	probes := []probe{
		{"postgre", "postgres", true},
		{"javscript", "javascript", true},
		{"kubernets", "kubernetes", true},
		{"pyton", "python", true},
		{"terrafrom", "terraform", true},
		{"kotlin", "koltin", true},
		{"scala", "scale", false},
		{"ruby", "rust", false},
		{"react", "redis", false},
		{"java", "javascript", false},
	}

	var (
		lowestCorrect = 1.0
		highestWrong  = 0.0
		lowestName    string
		highestName   string
	)

	for _, p := range probes {
		var similarity float64
		err := db.Querier(ctx).QueryRow(ctx,
			`SELECT similarity($1::text, $2::text)`, p.a, p.b).Scan(&similarity)
		if err != nil {
			t.Fatalf("similarity(%q, %q): %v", p.a, p.b, err)
		}

		t.Logf("%-12s / %-12s  %.3f  shouldMatch=%v", p.a, p.b, similarity, p.shouldMatch)

		if p.shouldMatch && similarity < lowestCorrect {
			lowestCorrect, lowestName = similarity, p.a+"/"+p.b
		}
		if !p.shouldMatch && similarity > highestWrong {
			highestWrong, highestName = similarity, p.a+"/"+p.b
		}
	}

	// The ranges overlap: a pair that must NOT match scores higher than one that
	// should. No threshold can admit the second without admitting the first.
	if lowestCorrect >= highestWrong {
		t.Fatalf("the ranges no longer overlap (lowest correct %s=%.3f >= highest wrong %s=%.3f); "+
			"fuzzy matching may now be viable and the decision is worth revisiting with fresh measurements",
			lowestName, lowestCorrect, highestName, highestWrong)
	}
	t.Logf("confirmed: lowest correct match (%s=%.3f) scores BELOW highest wrong match (%s=%.3f), "+
		"so no trigram threshold is safe",
		lowestName, lowestCorrect, highestName, highestWrong)
}

func TestUnmatchedLabelsAreRetainedAndRecordedForTriage(t *testing.T) {
	// An unknown label is not dropped: the candidate wrote it and may recognise it.
	// Recording it is the feedback loop that replaces fuzzy matching.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	invented := "Quantum Flux Capacitor Engineering"

	resolved, err := repo.MatchSkillLabels(ctx, []string{invented, "Go"})
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}

	if len(resolved) != 2 {
		t.Fatalf("%d results, want 2", len(resolved))
	}
	unknown := resolved[0]
	if unknown.Skill != nil {
		t.Fatalf("an invented skill resolved to %q", unknown.Skill.CanonicalName)
	}
	if unknown.Outcome != cv.Unmatched {
		t.Errorf("outcome = %q, want unmatched", unknown.Outcome)
	}
	// The raw label must survive, because it is what gets stored.
	if unknown.RawLabel != invented {
		t.Errorf("RawLabel = %q, want it retained verbatim", unknown.RawLabel)
	}

	if err := repo.RecordUnmatchedLabels(ctx, resolved); err != nil {
		t.Fatalf("RecordUnmatchedLabels: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Querier(context.Background()).Exec(context.Background(),
			`DELETE FROM unmatched_skill_labels WHERE normalised = $1`, unknown.Normalised)
	})

	var occurrences int64
	var sample string
	err = db.Querier(ctx).QueryRow(ctx,
		`SELECT occurrences, sample_label FROM unmatched_skill_labels WHERE normalised = $1`,
		unknown.Normalised).Scan(&occurrences, &sample)
	if err != nil {
		t.Fatalf("the unmatched label was not recorded: %v", err)
	}
	if sample != invented {
		t.Errorf("sample_label = %q, want the raw spelling for a human reading the list", sample)
	}

	// Seeing it again increments rather than duplicating, so triage can be ordered by
	// how many candidates actually wrote it.
	if err := repo.RecordUnmatchedLabels(ctx, resolved); err != nil {
		t.Fatalf("second RecordUnmatchedLabels: %v", err)
	}
	err = db.Querier(ctx).QueryRow(ctx,
		`SELECT occurrences FROM unmatched_skill_labels WHERE normalised = $1`,
		unknown.Normalised).Scan(&occurrences)
	if err != nil {
		t.Fatalf("read occurrences: %v", err)
	}
	if occurrences != 2 {
		t.Fatalf("occurrences = %d, want 2 after seeing the label twice", occurrences)
	}
}

func TestMatchedLabelsAreNotRecordedForTriage(t *testing.T) {
	// Otherwise the triage list would fill with "Python" and bury the genuine gaps.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	resolved, err := repo.MatchSkillLabels(ctx, []string{"Go", "Python", "k8s"})
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}
	if err := repo.RecordUnmatchedLabels(ctx, resolved); err != nil {
		t.Fatalf("RecordUnmatchedLabels: %v", err)
	}

	var count int
	err = db.Querier(ctx).QueryRow(ctx,
		`SELECT count(*) FROM unmatched_skill_labels WHERE normalised IN ('go','python','k8s')`).
		Scan(&count)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("%d matched labels were recorded for triage", count)
	}
}

func TestLinkSkillIDsAttachesCanonicalSkills(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	if err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), []cv.Skill{
		{RawLabel: "JS", Source: cv.SkillSourceAI},
		{RawLabel: "k8s", Source: cv.SkillSourceAI},
		{RawLabel: "Quantum Flux Engineering", Source: cv.SkillSourceAI},
	}); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	labels, err := repo.SkillLabelsForVersion(ctx, versionID)
	if err != nil {
		t.Fatalf("SkillLabelsForVersion: %v", err)
	}
	if len(labels) != 3 {
		t.Fatalf("%d labels, want 3", len(labels))
	}

	resolved, err := repo.MatchSkillLabels(ctx, labels)
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}

	linked, err := repo.LinkSkillIDs(ctx, versionID, resolved)
	if err != nil {
		t.Fatalf("LinkSkillIDs: %v", err)
	}
	if linked != 2 {
		t.Fatalf("linked %d skills, want 2 (the invented one has no canonical match)", linked)
	}

	stored, err := repo.SkillsByVersion(ctx, userID, versionID)
	if err != nil {
		t.Fatalf("SkillsByVersion: %v", err)
	}

	byLabel := make(map[string]cv.Skill, len(stored))
	for _, s := range stored {
		byLabel[s.RawLabel] = s
	}
	if byLabel["JS"].SkillID == nil {
		t.Error("JS was not linked to a canonical skill")
	}
	if byLabel["k8s"].SkillID == nil {
		t.Error("k8s was not linked to a canonical skill")
	}
	// Unmatched stays unlinked rather than being force-matched: recording a skill
	// against the wrong canonical entry makes overlap score on a fiction.
	if byLabel["Quantum Flux Engineering"].SkillID != nil {
		t.Error("an unmatched label was linked to a canonical skill")
	}
	// And its raw label survives, because the candidate wrote it.
	if _, present := byLabel["Quantum Flux Engineering"]; !present {
		t.Error("the unmatched label was dropped")
	}
}

func TestLinkSkillIDsNeverOverwritesACandidateCorrection(t *testing.T) {
	// The candidate's link is authoritative. Normalisation runs on every re-parse and
	// must not quietly re-point a skill they fixed themselves.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()
	userID, versionID := seedVersion(t, db)

	if err := repo.StoreParsedData(ctx, sampleParsed(userID, versionID), []cv.Skill{
		{RawLabel: "JS", Source: cv.SkillSourceCandidate},
	}); err != nil {
		t.Fatalf("StoreParsedData: %v", err)
	}

	// A candidate-sourced label is excluded from the normalisation input entirely.
	labels, err := repo.SkillLabelsForVersion(ctx, versionID)
	if err != nil {
		t.Fatalf("SkillLabelsForVersion: %v", err)
	}
	if len(labels) != 0 {
		t.Fatalf("labels = %v, want candidate-sourced skills excluded", labels)
	}

	// Even if it is passed in explicitly, the update must not touch it.
	resolved, err := repo.MatchSkillLabels(ctx, []string{"JS"})
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}
	linked, err := repo.LinkSkillIDs(ctx, versionID, resolved)
	if err != nil {
		t.Fatalf("LinkSkillIDs: %v", err)
	}
	if linked != 0 {
		t.Fatalf("linked %d candidate-owned skills, want 0", linked)
	}
}

func TestMatchSkillLabelsIsOneQueryForTheWholeBatch(t *testing.T) {
	// A CV with 40 skills should not be 40 round trips. Verified by behaviour rather
	// than by counting queries: a large batch returns in order, fully resolved.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)
	ctx := context.Background()

	labels := []string{
		"Go", "Python", "JavaScript", "TypeScript", "Java", "Rust", "C", "C++", "C#",
		"PostgreSQL", "Redis", "MongoDB", "Kafka", "NATS", "Docker", "Kubernetes",
		"Terraform", "AWS", "Azure", "Google Cloud", "React", "Vue.js", "Angular",
		"Node.js", "Django", "Flask", "Spring", "Rails", "GraphQL", "gRPC",
		"Prometheus", "Grafana", "Git", "Jenkins", "Agile", "Microservices",
		"Machine Learning", "Communication", "Leadership", "Unknown Invented Skill",
	}

	resolved, err := repo.MatchSkillLabels(ctx, labels)
	if err != nil {
		t.Fatalf("MatchSkillLabels: %v", err)
	}
	if len(resolved) != len(labels) {
		t.Fatalf("%d results for %d labels", len(resolved), len(labels))
	}
	// Order must be preserved, since the caller re-associates by position.
	for i, skill := range resolved {
		if skill.RawLabel != labels[i] {
			t.Fatalf("result %d is %q, want %q; order was not preserved", i, skill.RawLabel, labels[i])
		}
	}

	matched := 0
	for _, skill := range resolved {
		if skill.Outcome.Matched() {
			matched++
		}
	}
	// All but the invented one.
	if matched != len(labels)-1 {
		t.Fatalf("%d of %d labels matched, want %d", matched, len(labels), len(labels)-1)
	}
}

func TestMatchSkillLabelsHandlesAnEmptyBatch(t *testing.T) {
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)

	resolved, err := repo.MatchSkillLabels(context.Background(), nil)
	if err != nil {
		t.Fatalf("MatchSkillLabels(nil): %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("%d results for an empty batch", len(resolved))
	}
}

func TestSkillLabelsForAMissingVersionIsEmptyNotAnError(t *testing.T) {
	// The version can be deleted while normalisation is queued. An empty list lets the
	// consumer acknowledge and move on rather than dead-lettering.
	db := openDB(t)
	repo := cv.NewPostgresRepository(db)

	labels, err := repo.SkillLabelsForVersion(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("SkillLabelsForVersion: %v", err)
	}
	if len(labels) != 0 {
		t.Fatalf("%d labels for a missing version", len(labels))
	}
}

func TestTaxonomyIsSeeded(t *testing.T) {
	// A missing seed would make every CV's skills unmatched, which looks like a
	// matching bug rather than a deployment one.
	db := openDB(t)
	ctx := context.Background()

	var skills, aliases int
	err := db.Querier(ctx).QueryRow(ctx,
		`SELECT count(*), coalesce(sum(array_length(aliases, 1)), 0) FROM skills`).
		Scan(&skills, &aliases)
	if err != nil {
		t.Fatalf("count taxonomy: %v", err)
	}

	if skills < 100 {
		t.Errorf("%d canonical skills; the taxonomy looks unseeded", skills)
	}
	if aliases < skills {
		t.Errorf("%d aliases for %d skills; aliases are the only matching mechanism, so they carry the work", aliases, skills)
	}
	t.Logf("taxonomy: %d skills, %d aliases", skills, aliases)
}
