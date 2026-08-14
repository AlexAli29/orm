package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Planning.
//
// A plan is computed and validated in full before anything runs. Everything
// that can be known without touching the schema — that an applied migration was
// edited, that a target names nothing, that a reverse path contains something
// irreversible, that an atomic migration cannot be atomic — is known here, so
// that execution either does the whole thing or does not start.

// Direction is which way a plan moves.
type Direction uint8

const (
	// Forward applies migrations.
	Forward Direction = iota
	// Reverse undoes them, in the opposite order.
	Reverse
)

func (d Direction) String() string {
	if d == Reverse {
		return "reverse"
	}
	return "forward"
}

// Step is one migration's worth of a plan.
type Step struct {
	Migration *Migration
	Direction Direction
	// Operations are what will actually run: the migration's own for a forward
	// step, and their reverses in the opposite order for a reverse one.
	Operations []Operation
	// Atomic reports whether the step runs in a transaction.
	Atomic bool
	// Checksum is the artifact's fingerprint, recorded with the migration when
	// it is applied.
	Checksum string
}

// Describe renders the step for a plan listing.
func (s Step) Describe() string {
	var b strings.Builder
	b.WriteString(s.Migration.ID)
	if s.Direction == Reverse {
		b.WriteString(" (reverse)")
	}
	if !s.Atomic {
		b.WriteString(" (non-atomic)")
	}
	b.WriteString("\n")
	for _, op := range s.Operations {
		fmt.Fprintf(&b, "    %-28s %s\n", op.Safety(), op.Describe())
	}
	return b.String()
}

// Plan is what a migration run will do.
type Plan struct {
	Direction Direction
	Steps     []Step
}

// Empty reports whether there is nothing to do.
func (p Plan) Empty() bool { return len(p.Steps) == 0 }

// Describe renders the whole plan, deterministically.
func (p Plan) Describe() string {
	if p.Empty() {
		return "no migrations to apply\n"
	}
	var b strings.Builder
	for _, s := range p.Steps {
		b.WriteString(s.Describe())
	}
	// The warnings a reader most needs are the ones about how the migration
	// runs rather than what it does, so they are collected at the end where
	// they cannot be lost among the operations.
	var warnings []string
	for _, s := range p.Steps {
		if !s.Atomic {
			warnings = append(warnings,
				fmt.Sprintf("%s runs outside a transaction: an operation that fails part way through leaves the ones before it applied", s.Migration.ID))
		}
		for _, op := range s.Operations {
			switch op.Safety() {
			case Destructive:
				warnings = append(warnings, fmt.Sprintf("%s: %s discards data or a constraint", s.Migration.ID, op.Describe()))
			case RequiresData:
				warnings = append(warnings, fmt.Sprintf("%s: %s cannot succeed on a table that already has rows without a backfill first", s.Migration.ID, op.Describe()))
			}
		}
	}
	if len(warnings) > 0 {
		b.WriteString("\nwarnings:\n")
		for _, w := range warnings {
			b.WriteString("    " + w + "\n")
		}
	}
	return b.String()
}

// Applied is one row of migration history.
type Applied struct {
	ID       string
	Checksum string
}

// PlanTarget computes what it would take to move from the applied history to a
// target.
//
// An empty target means the latest migration in the set. A target naming a
// migration behind the current state produces a reverse plan; one ahead
// produces a forward plan; the current state produces nothing.
func PlanTarget(set *Set, applied []Applied, target string) (Plan, error) {
	if err := validateHistory(set, applied); err != nil {
		return Plan{}, err
	}

	appliedIDs := make(map[string]bool, len(applied))
	for _, a := range applied {
		appliedIDs[a.ID] = true
	}

	wanted, err := set.Upto(target)
	if err != nil {
		return Plan{}, err
	}
	wantedIDs := make(map[string]bool, len(wanted))
	for _, m := range wanted {
		wantedIDs[m.ID] = true
	}

	// Anything applied that the target does not include has to come off, newest
	// first, before anything is added.
	var toReverse []*Migration
	for _, m := range set.Migrations() {
		if appliedIDs[m.ID] && !wantedIDs[m.ID] {
			toReverse = append(toReverse, m)
		}
	}
	if len(toReverse) > 0 {
		slices.Reverse(toReverse)
		return reversePlan(set, toReverse)
	}

	var steps []Step
	for _, m := range wanted {
		if appliedIDs[m.ID] {
			continue
		}
		sum, _ := set.Checksum(m.ID)
		steps = append(steps, Step{
			Migration:  m,
			Direction:  Forward,
			Operations: slices.Clone(m.Operations),
			Atomic:     m.Atomic,
			Checksum:   sum,
		})
	}
	return Plan{Direction: Forward, Steps: steps}, nil
}

// reversePlan builds the steps that undo migrations, newest first.
//
// Every reverse is computed before any of them runs. Discovering halfway
// through a rollback that the next step cannot be undone would leave the
// database in a state nobody planned and no migration describes.
func reversePlan(set *Set, migrations []*Migration) (Plan, error) {
	var steps []Step
	for _, m := range migrations {
		// The state each operation is reversed against is the state as of the
		// migration before it, which is what holds the definitions a drop threw
		// away.
		before, err := stateBefore(set, m.ID)
		if err != nil {
			return Plan{}, err
		}
		ops := make([]Operation, 0, len(m.Operations))
		for i := len(m.Operations) - 1; i >= 0; i-- {
			rev, err := m.Operations[i].Reverse(before)
			if err != nil {
				return Plan{}, fmt.Errorf("migration %s cannot be reversed: %w", m.ID, err)
			}
			ops = append(ops, rev)
		}
		atomic := m.Atomic
		for _, op := range ops {
			if !op.Transactional() {
				atomic = false
			}
		}
		sum, _ := set.Checksum(m.ID)
		steps = append(steps, Step{
			Migration:  m,
			Direction:  Reverse,
			Operations: ops,
			Atomic:     atomic,
			Checksum:   sum,
		})
	}
	return Plan{Direction: Reverse, Steps: steps}, nil
}

// Reverse returns the operations that undo a migration, in the order they run.
//
// It is what `sqlmigrate --reverse` shows and what a rollback would execute.
// The reverse of an operation is computed against the state as of the migration
// before it — the state that still holds the definitions a drop threw away —
// so a migration whose reverse cannot be computed says so here rather than
// halfway through a rollback.
func (s *Set) Reverse(id string) ([]Operation, error) {
	m, ok := s.Get(id)
	if !ok {
		return nil, &ErrUnknownTarget{Target: id}
	}
	plan, err := reversePlan(s, []*Migration{m})
	if err != nil {
		return nil, err
	}
	return plan.Steps[0].Operations, nil
}

// stateBefore reconstructs the schema as it was just before a migration ran.
func stateBefore(set *Set, id string) (*schema.Schema, error) {
	ordered := set.Migrations()
	at := slices.IndexFunc(ordered, func(m *Migration) bool { return m.ID == id })
	if at < 0 {
		return nil, &ErrUnknownTarget{Target: id}
	}
	if at == 0 {
		return &schema.Schema{}, nil
	}
	return set.StateAt(ordered[at-1].ID)
}

// validateHistory refuses a history that does not match the migrations present.
func validateHistory(set *Set, applied []Applied) error {
	var unknown, modified []string
	for _, a := range applied {
		sum, ok := set.Checksum(a.ID)
		if !ok {
			unknown = append(unknown, a.ID)
			continue
		}
		if a.Checksum != "" && a.Checksum != sum {
			modified = append(modified, a.ID)
		}
	}
	if len(unknown) > 0 {
		return &ErrHistory{
			Reason: "these migrations are recorded as applied but are not among the migrations present",
			IDs:    sortIDs(unknown),
		}
	}
	if len(modified) > 0 {
		id := sortIDs(modified)[0]
		sum, _ := set.Checksum(id)
		var stored string
		for _, a := range applied {
			if a.ID == id {
				stored = a.Checksum
			}
		}
		return &ErrMigrationModified{ID: id, Applied: stored, Current: sum}
	}

	// History has to be a prefix of the order the migrations run in: a
	// migration applied while one it depends on is not means the two databases
	// this history came from are not the same database.
	appliedIDs := make(map[string]bool, len(applied))
	for _, a := range applied {
		appliedIDs[a.ID] = true
	}
	for _, m := range set.Migrations() {
		if !appliedIDs[m.ID] {
			continue
		}
		for _, dep := range m.DependsOn {
			if !appliedIDs[dep] {
				return &ErrHistory{
					Reason: fmt.Sprintf("migration %s is applied but %s, which it depends on, is not", m.ID, dep),
					IDs:    []string{m.ID, dep},
				}
			}
		}
	}
	return nil
}
