// Package worker assembles the event consumers each domain contributes.
//
// Domains own their handlers; this package only collects them. That keeps the consumer
// configuration — idempotency key, AckWait, MaxDeliver, in-flight policy — next to the
// handler whose semantics determine it, rather than in a central file that drifts out of
// step with the code it configures.
package worker

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Joakim-animate90/myjobfyt/backend/internal/platform/events"
)

// Provider is a domain that contributes consumers.
type Provider interface {
	Registrations() []events.Registration
}

// Collect gathers registrations from every provider, rejecting duplicates.
//
// A duplicate durable name would have two handlers competing for the same consumer, so
// each message would reach only one of them at random — a bug that presents as
// "sometimes the pipeline just skips a stage" and is very hard to find. Better to
// refuse to start.
func Collect(providers ...Provider) ([]events.Registration, error) {
	var all []events.Registration
	seen := make(map[string]struct{})

	for _, provider := range providers {
		for _, reg := range provider.Registrations() {
			if reg.Name == "" {
				return nil, fmt.Errorf("a consumer was registered with no durable name")
			}
			if _, duplicate := seen[reg.Name]; duplicate {
				return nil, fmt.Errorf("duplicate consumer name %q", reg.Name)
			}
			if reg.Handler == nil {
				return nil, fmt.Errorf("consumer %q has no handler", reg.Name)
			}
			if reg.Group == "" {
				return nil, fmt.Errorf("consumer %q has no group, so no worker would ever run it", reg.Name)
			}
			seen[reg.Name] = struct{}{}
			all = append(all, reg)
		}
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	return all, nil
}

// Groups returns the distinct consumer groups present in a set of registrations, for
// logging and for validating the configured group list.
func Groups(regs []events.Registration) []string {
	seen := make(map[string]struct{}, len(regs))
	for _, reg := range regs {
		seen[reg.Group] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// ValidateGroups checks the configured groups against the available consumers.
//
// unmatched names a group that is configured but has no consumers. That is expected
// while a domain is still being built — the default WORKER_GROUPS lists every planned
// group — so it is reported for logging rather than treated as fatal.
//
// An error is returned only when NO configured group matches anything, because that is
// the failure the check actually exists to catch: a worker that starts, reports healthy,
// and processes nothing. That is a confusing way to discover a typo in WORKER_GROUPS.
func ValidateGroups(regs []events.Registration, configured []string) (unmatched []string, err error) {
	available := make(map[string]struct{})
	for _, g := range Groups(regs) {
		available[g] = struct{}{}
	}

	matched := 0
	for _, g := range configured {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		if _, ok := available[g]; ok {
			matched++
			continue
		}
		unmatched = append(unmatched, g)
	}

	if matched == 0 {
		return unmatched, fmt.Errorf(
			"none of the configured groups %v match a consumer; available groups are %v",
			configured, Groups(regs))
	}
	return unmatched, nil
}
