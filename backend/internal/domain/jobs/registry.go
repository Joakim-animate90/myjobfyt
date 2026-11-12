package jobs

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/apperr"
)

// SourceRecord is a job source as the database holds it.
//
// Plain data, with no adapter attached. Separate from Source because the database says
// which sources are configured while the registry says which ones this binary can talk
// to, and conflating them hides the case that matters: a source enabled in the database
// with no adapter compiled in.
type SourceRecord struct {
	Slug       string
	Name       string
	Kind       SourceKind
	State      SourceState
	Regions    []string
	RateLimit  int
	LegalBasis string
	TermsURL   string
	Breaker    Breaker
}

// Bind attaches an adapter, producing a usable Source.
func (r SourceRecord) Bind(adapter Adapter) Source {
	return Source{SourceRecord: r, adapter: adapter}
}

// Source is a configured job source bound to the adapter that can talk to it.
type Source struct {
	SourceRecord
	adapter Adapter
}

// Adapter returns the source's adapter.
func (s Source) Adapter() Adapter { return s.adapter }

// Usable reports whether this source may be used in a run, and why not when it cannot.
func (s Source) Usable() (bool, string) {
	switch {
	case s.adapter == nil:
		return false, "no adapter is registered for this source"
	case s.State == StatePartnershipRequired:
		return false, "access requires a partnership agreement that is not in place"
	case s.State == StateDisabled:
		return false, "the source is disabled"
	case strings.TrimSpace(s.LegalBasis) == "":
		// The database enforces this too. Repeating it here means a source constructed
		// in memory — in a test, or by a future code path — cannot skip the check.
		return false, "no legal basis is recorded for this source"
	}
	return true, ""
}

// Registry holds the adapters this process knows how to use.
//
// Separate from the database records on purpose: the database says which sources are
// configured and enabled, the registry says which ones this binary can actually talk to.
// A source enabled in the database with no adapter compiled in is a configuration error
// worth reporting, not a crash.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{adapters: map[string]Adapter{}}
}

// Register adds an adapter.
//
// Rejects a duplicate slug rather than overwriting. Two adapters claiming one slug means
// one of them is silently never used, and discovering that from missing postings is far
// harder than from a startup error.
func (r *Registry) Register(adapter Adapter) error {
	if adapter == nil {
		return fmt.Errorf("cannot register a nil adapter")
	}
	slug := strings.TrimSpace(adapter.Slug())
	if slug == "" {
		return fmt.Errorf("cannot register an adapter with no slug")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[slug]; exists {
		return fmt.Errorf("an adapter for %q is already registered", slug)
	}
	r.adapters[slug] = adapter
	return nil
}

// MustRegister panics on a duplicate. For wiring at startup, where a duplicate is a
// programming error that should stop the process rather than degrade it.
func (r *Registry) MustRegister(adapter Adapter) {
	if err := r.Register(adapter); err != nil {
		panic(err)
	}
}

// Get returns an adapter by slug.
func (r *Registry) Get(slug string) (Adapter, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	adapter, ok := r.adapters[slug]
	if !ok {
		return nil, apperr.NotFound("job source adapter")
	}
	return adapter, nil
}

// Slugs lists the registered slugs, sorted.
//
// Sorted because ranging a map is randomised, and this list appears in startup logs and
// admin output where a changing order is noise that hides a real change.
func (r *Registry) Slugs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for slug := range r.adapters {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// Bind attaches adapters to database records, reporting records with no adapter.
//
// Returns the bound sources and the slugs that could not be bound. An unbound enabled
// source is reported rather than ignored: it means this deployment is configured to use
// a source it cannot talk to, which otherwise shows up only as a source that silently
// returns nothing.
func (r *Registry) Bind(records []SourceRecord) (bound []Source, unbound []string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, record := range records {
		adapter, ok := r.adapters[record.Slug]
		if !ok {
			if record.State == StateEnabled {
				unbound = append(unbound, record.Slug)
			}
			continue
		}
		bound = append(bound, record.Bind(adapter))
	}

	sort.Slice(bound, func(i, j int) bool { return bound[i].Slug < bound[j].Slug })
	sort.Strings(unbound)
	return bound, unbound
}
