package embed

import (
	"math"
	"strings"
	"unicode"
)

// Lexical similarity: the fallback used when no embedding provider is available.
//
// The bar it has to clear is real. "Go Developer" and "Backend Engineer (Golang)" are
// the same job, and a naive token overlap scores them at zero — they share no words.
// Three things close most of that gap without any model:
//
//   - Separator-insensitive normalisation, so "Node.js" and "NodeJS" are one token.
//   - A role-synonym table, so developer, engineer and programmer are interchangeable
//     where they genuinely are.
//   - Informativeness weighting, so "Senior" and "of" count for little and "Kubernetes"
//     counts for a lot. Without this, "Senior Manager" and "Senior Engineer" score
//     highly on the word they share and nothing else.
//
// The caller supplies a term expander, which in practice is backed by the skill
// taxonomy — that is what turns "Golang" into "Go" and makes the example above work.
// Keeping it an interface means this package does not depend on the cv domain.

// TermExpander maps a token to its canonical form.
//
// Returns the token unchanged when it knows nothing about it, so an unexpanded token is
// still compared on its own terms rather than being dropped.
type TermExpander interface {
	Expand(token string) string
}

// NoExpansion is a TermExpander that changes nothing. Used where no taxonomy is
// available; similarity still works, just less well on synonyms.
type NoExpansion struct{}

// Expand returns the token unchanged.
func (NoExpansion) Expand(token string) string { return token }

// MapExpander expands tokens from a map, for tests and for a small static table.
type MapExpander map[string]string

// Expand looks the token up, returning it unchanged when absent.
func (m MapExpander) Expand(token string) string {
	if canonical, found := m[token]; found {
		return canonical
	}
	return token
}

// roleSynonyms collapses job-title words that are genuinely interchangeable.
//
// Deliberately conservative. "Developer" and "engineer" really are used
// interchangeably across the whole market; "manager" and "engineer" are not, and
// collapsing them would match a candidate into roles they cannot do. Every entry here
// is a word pair I would defend to a candidate who asked why they were matched.
var roleSynonyms = map[string]string{
	"developer":   "engineer",
	"dev":         "engineer",
	"programmer":  "engineer",
	"engineer":    "engineer",
	"engineering": "engineer",
	"swe":         "engineer",

	"architect": "architect",

	// Seniority words are normalised to one token each so they can be weighted down
	// together rather than individually.
	"snr":      "senior",
	"sr":       "senior",
	"senior":   "senior",
	"jnr":      "junior",
	"jr":       "junior",
	"junior":   "junior",
	"grad":     "junior",
	"graduate": "junior",
	"intern":   "junior",

	"lead":      "lead",
	"principal": "principal",
	"staff":     "staff",

	"manager":    "manager",
	"management": "manager",
	"mgr":        "manager",
	"head":       "head",
	"director":   "director",

	"frontend":  "frontend",
	"front":     "frontend",
	"fe":        "frontend",
	"backend":   "backend",
	"back":      "backend",
	"be":        "backend",
	"fullstack": "fullstack",
	"full":      "fullstack",

	"devops":         "devops",
	"sre":            "sre",
	"platform":       "platform",
	"infra":          "infrastructure",
	"infrastructure": "infrastructure",

	"qa":     "qa",
	"tester": "qa",
	"test":   "qa",

	"analyst":   "analyst",
	"analytics": "analyst",
	"scientist": "scientist",
	"science":   "scientist",
	"designer":  "designer",
	"design":    "designer",
}

// lowInformationTokens carry almost no matching signal.
//
// Seniority words are here rather than removed entirely: they matter for the seniority
// dimension, which is scored separately, and double-counting them in title similarity
// would make "Senior Manager" look like "Senior Engineer".
var lowInformationTokens = map[string]float64{
	"senior": 0.15, "junior": 0.15, "mid": 0.15, "level": 0.1,
	"the": 0.0, "of": 0.0, "and": 0.0, "a": 0.0, "an": 0.0, "in": 0.0,
	"at": 0.0, "for": 0.0, "to": 0.0, "with": 0.0, "or": 0.0,
	"i": 0.05, "ii": 0.05, "iii": 0.05, "iv": 0.05,
	"1": 0.05, "2": 0.05, "3": 0.05, "4": 0.05,
	"remote": 0.1, "hybrid": 0.1, "contract": 0.1, "permanent": 0.1,
	"fulltime": 0.1, "parttime": 0.1, "position": 0.05, "role": 0.05,
	"job": 0.05, "opportunity": 0.05, "team": 0.1, "new": 0.05,
	"experienced": 0.1, "talented": 0.0, "passionate": 0.0, "rockstar": 0.0,
	"ninja": 0.0, "guru": 0.0, "wizard": 0.0,
}

// titleAcronyms expand before tokenisation, because the expansion is multi-word and a
// per-token expander cannot produce it.
//
// "SRE" and "Site Reliability Engineer" are the same title and share no tokens, so
// without this they score zero — and SRE is one of the most common titles in the
// market. Expanding the acronym to its phrase makes the comparison work in both
// directions at once.
//
// Only unambiguous acronyms are listed. "PM" is deliberately absent: it means product
// manager and project manager about equally often, and guessing would match candidates
// into the wrong discipline.
var titleAcronyms = map[string]string{
	"sre":      "site reliability engineer",
	"swe":      "software engineer",
	"sde":      "software development engineer",
	"qa":       "quality assurance",
	"ba":       "business analyst",
	"ds":       "data scientist",
	"de":       "data engineer",
	"ml":       "machine learning",
	"ai":       "artificial intelligence",
	"ux":       "user experience",
	"ui":       "user interface",
	"cto":      "chief technology officer",
	"ceo":      "chief executive officer",
	"cfo":      "chief financial officer",
	"coo":      "chief operating officer",
	"vp":       "vice president",
	"evp":      "executive vice president",
	"svp":      "senior vice president",
	"hr":       "human resources",
	"seo":      "search engine optimisation",
	"sdr":      "sales development representative",
	"ae":       "account executive",
	"csm":      "customer success manager",
	"it":       "information technology",
	"dba":      "database administrator",
	"sysadmin": "systems administrator",
}

// expandAcronyms rewrites known acronyms in text before tokenisation.
//
// Operates on whole words only, so "AI" in "AI Engineer" expands while the "ai" inside
// "Email" does not.
func expandAcronyms(text string) string {
	words := splitWords(text)
	if len(words) == 0 {
		return text
	}

	out := make([]string, 0, len(words)*3)
	for _, word := range words {
		if phrase, known := titleAcronyms[word]; known {
			out = append(out, phrase)
			continue
		}
		out = append(out, word)
	}
	return strings.Join(out, " ")
}

// minInformativeWeight is the least informative content a title must carry before a
// similarity score means anything.
//
// Without this floor, a ratio over tiny weights is meaningless: "Senior Remote Role"
// against "Senior Remote Position" scored 0.833, because the shared weight was small
// but so was the denominator. Two content-free titles matching at 0.833 would match a
// candidate to everything. Half a full-weight token is the floor — enough that at least
// one real word of content is present.
const minInformativeWeight = 0.5

// defaultTokenWeight is what an unrecognised token is worth.
//
// Full weight, deliberately: an unknown token is usually the informative one — a
// technology, a domain, a specialism. Weighting unknowns down would mean the more
// specific the title, the less it counted.
const defaultTokenWeight = 1.0

// Tokenise splits text into normalised, expanded comparison tokens.
func Tokenise(text string, expander TermExpander) []string {
	if expander == nil {
		expander = NoExpansion{}
	}

	raw := splitWords(expandAcronyms(text))
	out := make([]string, 0, len(raw))

	for _, word := range raw {
		token := normaliseToken(word)
		if token == "" {
			continue
		}
		// Role synonyms first, then the caller's expander. Order matters: the expander
		// is backed by the skill taxonomy, and "design" should become "designer" as a
		// role word rather than being looked up as a skill.
		if canonical, found := roleSynonyms[token]; found {
			token = canonical
		} else {
			token = normaliseToken(expander.Expand(token))
		}
		if token != "" {
			out = append(out, token)
		}
	}
	return out
}

// splitWords breaks text on anything that is not a letter or digit.
//
// Note this splits "Node.js" into "node" and "js" — which is why the expander runs
// afterwards, mapping "js" to "javascript" through the taxonomy. Trying to keep
// dotted names whole here would require knowing which dots are separators and which
// are sentence punctuation, and the expander handles it without that guesswork.
func splitWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		// '+' and '#' are kept inside tokens so c++ and c# survive.
		if r == '+' || r == '#' {
			return false
		}
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// normaliseToken trims a token to its comparable form.
func normaliseToken(token string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(token)) {
		switch {
		case r == '+' || r == '#':
			b.WriteRune(r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// tokenWeight returns how much a token contributes.
func tokenWeight(token string) float64 {
	if weight, known := lowInformationTokens[token]; known {
		return weight
	}
	// A single character carries almost no signal unless it is a known language name,
	// which the expander will already have canonicalised to something longer.
	if len(token) == 1 && token != "r" && token != "c" {
		return 0.2
	}
	return defaultTokenWeight
}

// TitleSimilarity scores two job titles in [0, 1].
//
// Weighted overlap divided by the weight of the smaller title, not the union. A
// containment measure rather than a Jaccard one, because job postings pad their titles
// — "Senior Backend Engineer (Go) — Remote, Fintech" against a CV's "Backend Engineer"
// is a strong match, and dividing by the union would punish the candidate for the
// employer's verbosity.
func TitleSimilarity(candidateTitle, postingTitle string, expander TermExpander) float64 {
	candidate := Tokenise(candidateTitle, expander)
	posting := Tokenise(postingTitle, expander)

	if len(candidate) == 0 || len(posting) == 0 {
		return 0
	}

	candidateWeights := weighTokens(candidate)
	postingWeights := weighTokens(posting)

	var shared float64
	for token, weight := range candidateWeights {
		if _, inPosting := postingWeights[token]; inPosting {
			// The lower of the two weights, so a token cannot contribute more than it
			// is worth on either side.
			shared += math.Min(weight, postingWeights[token])
		}
	}

	candidateTotal := sumWeights(candidateWeights)
	postingTotal := sumWeights(postingWeights)
	smaller := math.Min(candidateTotal, postingTotal)

	// A ratio over tiny weights is meaningless. If the smaller title carries less than
	// half a full-weight token of content, there is nothing substantive to compare and
	// a high ratio would be an artefact of the small denominator rather than a match.
	if smaller < minInformativeWeight {
		return 0
	}

	return math.Min(shared/smaller, 1)
}

// weighTokens collapses duplicates and assigns each token its weight.
//
// Duplicates collapse because a posting that says "engineer" three times is not three
// times more of an engineering role.
func weighTokens(tokens []string) map[string]float64 {
	out := make(map[string]float64, len(tokens))
	for _, token := range tokens {
		weight := tokenWeight(token)
		if existing, seen := out[token]; !seen || weight > existing {
			out[token] = weight
		}
	}
	return out
}

func sumWeights(weights map[string]float64) float64 {
	var total float64
	for _, weight := range weights {
		total += weight
	}
	return total
}

// SetSimilarity scores the overlap of two sets of terms, such as skills.
//
// Containment against the smaller set again, and for the same reason: a posting listing
// 30 skills against a candidate with 12 of them is a strong match, and a Jaccard
// measure would score it at 0.4 and reject it.
func SetSimilarity(candidateTerms, postingTerms []string, expander TermExpander) float64 {
	candidate := expandSet(candidateTerms, expander)
	posting := expandSet(postingTerms, expander)

	if len(candidate) == 0 || len(posting) == 0 {
		return 0
	}

	shared := 0
	for term := range candidate {
		if _, inPosting := posting[term]; inPosting {
			shared++
		}
	}

	smaller := len(candidate)
	if len(posting) < smaller {
		smaller = len(posting)
	}
	return float64(shared) / float64(smaller)
}

// expandSet normalises and expands a list of terms into a set.
func expandSet(terms []string, expander TermExpander) map[string]struct{} {
	if expander == nil {
		expander = NoExpansion{}
	}
	out := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		// A multi-word term such as "machine learning" is kept whole, because splitting
		// it would match "learning" against unrelated postings.
		normalised := normaliseToken(strings.Join(splitWords(term), ""))
		if normalised == "" {
			continue
		}
		normalised = normaliseToken(expander.Expand(normalised))
		if normalised != "" {
			out[normalised] = struct{}{}
		}
	}
	return out
}
