package migrate

import (
	"cmp"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Discovery, the dependency graph and the order migrations run in.
//
// Nothing here reads a directory or a database. A Set is built from artifacts a
// caller collected, validated as a whole, and ordered deterministically — so
// two machines given the same migrations produce the same order, the same
// state and the same checksums.

// Set is a validated collection of migrations.
//
// It is ordered: Migrations returns them in the order they must be applied, and
// that order is a property of the dependency graph rather than of how the
// artifacts arrived.
type Set struct {
	ordered []*Migration
	byID    map[string]*Migration
	sums    map[string]string
}

// NewSet validates a collection of migrations and orders it.
//
// Everything that can be wrong with a set as a whole is reported here, before
// anything is planned: a duplicate ID, a dependency naming nothing, a cycle, an
// artifact from a future format, an atomic migration containing an operation
// that cannot be atomic.
func NewSet(migrations []*Migration) (*Set, error) {
	s := &Set{
		byID: make(map[string]*Migration, len(migrations)),
		sums: make(map[string]string, len(migrations)),
	}

	// Duplicates first: everything below assumes an ID names one migration.
	var dupes []string
	for _, m := range migrations {
		if err := m.Validate(); err != nil {
			return nil, err
		}
		if _, ok := s.byID[m.ID]; ok {
			dupes = append(dupes, m.ID)
			continue
		}
		s.byID[m.ID] = m
	}
	if len(dupes) > 0 {
		slices.Sort(dupes)
		return nil, &ErrGraph{Reason: "these IDs are used by more than one migration", IDs: slices.Compact(dupes)}
	}

	var missing []string
	for _, m := range migrations {
		for _, dep := range m.DependsOn {
			if _, ok := s.byID[dep]; !ok {
				missing = append(missing, m.ID+" -> "+dep)
			}
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return nil, &ErrGraph{Reason: "these dependencies name no migration", IDs: missing}
	}

	ordered, err := topological(s.byID)
	if err != nil {
		return nil, err
	}
	s.ordered = ordered

	for _, m := range ordered {
		sum, err := m.Checksum()
		if err != nil {
			return nil, err
		}
		s.sums[m.ID] = sum
	}
	return s, nil
}

// topological orders migrations so that every dependency precedes its dependents.
//
// Ties are broken by ID. Without that, a set with two independent roots would
// order differently between runs — and the order decides the state a migration
// is computed against, so a nondeterministic order is a nondeterministic
// migration.
func topological(byID map[string]*Migration) ([]*Migration, error) {
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	indegree := make(map[string]int, len(ids))
	dependents := make(map[string][]string, len(ids))
	for _, id := range ids {
		m := byID[id]
		deps := slices.Compact(slices.Sorted(slices.Values(m.DependsOn)))
		indegree[id] = len(deps)
		for _, dep := range deps {
			dependents[dep] = append(dependents[dep], id)
		}
	}
	for dep := range dependents {
		slices.Sort(dependents[dep])
	}

	// Kahn's algorithm over a ready set kept sorted, which is what makes the
	// result the same every time rather than merely valid every time.
	ready := make([]string, 0, len(ids))
	for _, id := range ids {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	out := make([]*Migration, 0, len(ids))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		out = append(out, byID[id])
		for _, next := range dependents[id] {
			indegree[next]--
			if indegree[next] == 0 {
				ready = append(ready, next)
				slices.Sort(ready)
			}
		}
	}

	if len(out) != len(ids) {
		// Whatever is left is in a cycle or downstream of one.
		var stuck []string
		for _, id := range ids {
			if !slices.ContainsFunc(out, func(m *Migration) bool { return m.ID == id }) {
				stuck = append(stuck, id)
			}
		}
		return nil, &ErrGraph{Reason: "these migrations depend on each other in a cycle", IDs: stuck}
	}
	return out, nil
}

// Migrations returns the set in application order.
func (s *Set) Migrations() []*Migration { return slices.Clone(s.ordered) }

// Get returns a migration by ID.
func (s *Set) Get(id string) (*Migration, bool) {
	m, ok := s.byID[id]
	return m, ok
}

// Checksum returns a migration's fingerprint, computed once when the set was
// built.
func (s *Set) Checksum(id string) (string, bool) {
	sum, ok := s.sums[id]
	return sum, ok
}

// Len reports how many migrations the set holds.
func (s *Set) Len() int { return len(s.ordered) }

// State reconstructs the schema the migrations describe, in memory.
//
// This is the state a future diff is computed against, and computing it needs
// no database: the migrations are the history, and applying them to an empty
// schema is what that history means. Reading it from a live database instead
// would make the next migration depend on which database somebody pointed at.
func (s *Set) State() (*schema.Schema, error) {
	return s.StateAt("")
}

// StateAt reconstructs the schema as of a migration, inclusive. An empty target
// means every migration in the set.
func (s *Set) StateAt(target string) (*schema.Schema, error) {
	if target != "" {
		if _, ok := s.byID[target]; !ok {
			return nil, &ErrUnknownTarget{Target: target}
		}
	}
	state := &schema.Schema{}
	for _, m := range s.ordered {
		for _, op := range m.Operations {
			if err := op.Apply(state); err != nil {
				return nil, &ErrHistory{
					Reason: "migration " + m.ID + " cannot be applied to the state the migrations before it produced: " + err.Error(),
					IDs:    []string{m.ID},
				}
			}
		}
		if m.ID == target {
			break
		}
	}
	state.Normalize()
	return state, nil
}

// Upto returns the migrations at and before target, in order.
func (s *Set) Upto(target string) ([]*Migration, error) {
	if target == "" {
		return s.Migrations(), nil
	}
	if _, ok := s.byID[target]; !ok {
		return nil, &ErrUnknownTarget{Target: target}
	}
	var out []*Migration
	for _, m := range s.ordered {
		out = append(out, m)
		if m.ID == target {
			break
		}
	}
	return out, nil
}

// Describe renders the set as a stable, human-readable listing. It exists so
// that determinism can be asserted on something a person can also read.
func (s *Set) Describe() string {
	var b strings.Builder
	for _, m := range s.ordered {
		sum := s.sums[m.ID]
		b.WriteString(m.ID)
		b.WriteString(" [" + short(sum) + "]")
		if !m.Atomic {
			b.WriteString(" non-atomic")
		}
		if len(m.DependsOn) > 0 {
			deps := slices.Clone(m.DependsOn)
			slices.Sort(deps)
			b.WriteString(" after " + strings.Join(deps, ","))
		}
		b.WriteString("\n")
		for _, op := range m.Operations {
			b.WriteString("    " + op.Describe() + "\n")
		}
	}
	return b.String()
}

// sortIDs is the deterministic ordering used wherever IDs are reported.
func sortIDs(ids []string) []string {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(a, b) })
	return out
}
