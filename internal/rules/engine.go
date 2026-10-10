package rules

import (
	"sort"
	"time"
)

// Timing is how long a problem must last before it alerts, so short blips
// (a pod restarting once, a rollout in progress) don't wake anyone, and how
// long it must be gone before it resolves, so a crash loop's brief healthy
// moments don't resolve and re-fire it over and over.
type Timing struct {
	Critical time.Duration
	Warning  time.Duration
	Resolve  time.Duration
}

var DefaultTiming = Timing{Critical: 30 * time.Second, Warning: 5 * time.Minute, Resolve: 3 * time.Minute}

func (t Timing) of(s Severity) time.Duration {
	if s == Critical {
		return t.Critical
	}
	return t.Warning
}

// Engine turns snapshots of a cluster into alerts that fire once a problem
// has lasted long enough, and resolve when it's gone. Not safe for
// concurrent use.
type Engine struct {
	Timing     Timing
	candidates map[string]time.Time // seen, not yet old enough: first seen
	active     map[string]Alert
	missing    map[string]time.Time // active but not seen: since when
}

func NewEngine(t Timing) *Engine {
	return &Engine{Timing: t, candidates: map[string]time.Time{}, active: map[string]Alert{}, missing: map[string]time.Time{}}
}

// Restore picks up where a previous run left off, so a restart doesn't re-alert.
func (e *Engine) Restore(alerts []Alert) {
	for _, a := range alerts {
		e.active[a.ID] = a
	}
}

// Active lists current alerts, sorted by id.
func (e *Engine) Active() []Alert {
	out := make([]Alert, 0, len(e.active))
	for _, a := range e.active {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Update evaluates rows. Only alerts of the given kinds can resolve, so a
// kind that failed to load doesn't look like it recovered. With baseline,
// problems already present become active without being reported as fired.
func (e *Engine) Update(rows []Row, now time.Time, kinds map[Kind]bool, baseline bool) Changes {
	current := map[string]Alert{}
	problems := Problems(rows, now)
	for _, a := range problems {
		current[a.ID] = a
	}
	var ch Changes
	for _, a := range problems {
		if existing, ok := e.active[a.ID]; ok {
			delete(e.missing, a.ID)
			if existing.Title != a.Title || existing.Detail != a.Detail || existing.Severity != a.Severity {
				existing.Title, existing.Detail, existing.Severity, existing.Affected = a.Title, a.Detail, a.Severity, a.Affected
				e.active[a.ID] = existing
				ch.Updated = append(ch.Updated, existing)
			}
			continue
		}
		first, seen := e.candidates[a.ID]
		if !seen {
			first = now
			e.candidates[a.ID] = now
		}
		if baseline || now.Sub(first) >= e.Timing.of(a.Severity) {
			delete(e.candidates, a.ID)
			a.Since = first
			e.active[a.ID] = a
			if !baseline {
				ch.Fired = append(ch.Fired, a)
			}
		}
	}
	for id := range e.candidates {
		if _, ok := current[id]; !ok {
			delete(e.candidates, id)
		}
	}
	for id, a := range e.active {
		if a.Source != FromKubctl {
			continue
		}
		if _, ok := current[id]; ok {
			continue
		}
		if a.Kind != "" && !kinds[a.Kind] {
			continue
		}
		gone, seen := e.missing[id]
		if !seen {
			gone = now
			e.missing[id] = now
		}
		if now.Sub(gone) < e.Timing.Resolve {
			continue
		}
		delete(e.active, id)
		delete(e.missing, id)
		ch.Resolved = append(ch.Resolved, a)
	}
	sort.Slice(ch.Resolved, func(i, j int) bool { return ch.Resolved[i].ID < ch.Resolved[j].ID })
	return ch
}
