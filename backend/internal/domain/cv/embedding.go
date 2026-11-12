package cv

import (
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/embed"
	"github.com/google/uuid"
)

// EmbeddingInput is the text a version's vectors are built from.
//
// Focused extracts rather than the whole CV. An embedding model truncates at a few
// hundred tokens, so embedding a full CV would capture only its opening — and the
// title vector would end up describing the candidate's address.
type EmbeddingInput struct {
	CVVersionID     uuid.UUID
	MostRecentTitle string
	Headline        string
	Summary         string
	// Skills is the canonical skill list, comma-joined. Canonical names rather than raw
	// labels, so a candidate who wrote "JS" embeds the same text as one who wrote
	// "JavaScript" and the two land in the same region of the space.
	Skills string
}

// TitleText returns the text for the title vector.
//
// The most recent job title, falling back to the headline. Both carry the same
// information when present, and a candidate whose CV has only a headline should still
// get a title vector rather than none.
func (in EmbeddingInput) TitleText() string {
	if title := strings.TrimSpace(in.MostRecentTitle); title != "" {
		return title
	}
	return strings.TrimSpace(in.Headline)
}

// Texts returns the kinds that can be embedded and their text, skipping any that are
// empty.
//
// Empty inputs are skipped rather than embedded: an empty string produces a zero
// vector, which has no direction and cannot be compared, so storing one would create a
// row that fails every later comparison.
func (in EmbeddingInput) Texts() ([]embed.Kind, []string) {
	candidates := []struct {
		kind embed.Kind
		text string
	}{
		{embed.KindTitle, in.TitleText()},
		{embed.KindSkills, strings.TrimSpace(in.Skills)},
		{embed.KindSummary, strings.TrimSpace(in.Summary)},
	}

	kinds := make([]embed.Kind, 0, len(candidates))
	texts := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.text == "" {
			continue
		}
		kinds = append(kinds, c.kind)
		texts = append(texts, c.text)
	}
	return kinds, texts
}

// HasAnything reports whether there is anything worth embedding.
func (in EmbeddingInput) HasAnything() bool {
	kinds, _ := in.Texts()
	return len(kinds) > 0
}
