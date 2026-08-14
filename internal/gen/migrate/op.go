// Package migrate is the migration engine: operations, state, diffing and
// execution.
//
// The design decision that everything else follows from is that a migration is
// computed against the state the migrations themselves describe, never against
// a live database. Diffing against a database would mean the migration you get
// depends on which database you happened to point at, that two developers on
// one branch produce different migrations, and that a rename is indistinguishable
// from a drop and an add because there is no history to compare with.
//
// So migrations own a state. Each operation says how it changes an in-memory
// schema, applying them in order reconstructs what the schema should be, and a
// diff against the desired schema produces the next migration. Nothing in that
// path needs a network.
package migrate

import (
	"fmt"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Safety is what an operation risks, as far as PostgreSQL's own semantics say.
//
// It is advisory. It describes what the operation does to a table, not what
// that means for a particular deployment: whether a lock matters depends on the
// traffic, and whether dropping a column loses anything depends on what is in
// it. Presenting it as a guarantee would be worse than not classifying at all.
type Safety uint8

const (
	// Safe is metadata-only or an operation PostgreSQL performs without
	// blocking readers or writers for a meaningful time.
	Safe Safety = iota
	// Locking takes a lock that blocks concurrent access to the table for the
	// duration of the operation.
	Locking
	// Destructive discards data or a constraint that cannot be recovered by
	// reversing the operation.
	Destructive
	// RequiresData means the operation cannot succeed on a non-empty table
	// without something first putting values in place.
	RequiresData
)

func (s Safety) String() string {
	switch s {
	case Locking:
		return "LOCKING"
	case Destructive:
		return "DESTRUCTIVE"
	case RequiresData:
		return "REQUIRES DATA MIGRATION"
	default:
		return "SAFE"
	}
}

// Operation is one step of a migration.
//
// Every operation answers three questions: what it does to the schema state,
// what SQL it runs, and whether it can be undone. The three are separate
// because they genuinely differ — a raw SQL operation has SQL and no state
// change, and a state-only operation has the reverse.
type Operation interface {
	// Describe renders the operation for a plan, in one line.
	Describe() string
	// Apply changes the in-memory schema state. An operation whose SQL the
	// engine cannot model returns an error rather than silently leaving the
	// state wrong.
	Apply(s *schema.Schema) error
	// SQL renders the statements to run, in order.
	SQL() ([]string, error)
	// Safety classifies what the operation risks.
	Safety() Safety
	// Reverse returns the operation that undoes this one, or an error naming
	// why it cannot be undone. Nothing invents a reverse that would lose data
	// quietly.
	Reverse(before *schema.Schema) (Operation, error)
	// Transactional reports whether the operation may run inside a transaction.
	// CREATE INDEX CONCURRENTLY may not, and that decides how the migration
	// containing it executes.
	Transactional() bool
}

// ErrIrreversible reports an operation that cannot be undone.
type ErrIrreversible struct {
	Op     string
	Reason string
}

func (e *ErrIrreversible) Error() string {
	return fmt.Sprintf("%s cannot be reversed: %s", e.Op, e.Reason)
}

// irreversible is the helper every operation with no safe inverse returns.
func irreversible(op, reason string) (Operation, error) {
	return nil, &ErrIrreversible{Op: op, Reason: reason}
}

// tableOf finds a table in a state, for an operation that has to change it.
func tableOf(s *schema.Schema, schemaName, name string) (int, error) {
	for i := range s.Tables {
		if s.Tables[i].Schema == schemaName && s.Tables[i].Name == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("table %s.%s is not in the migration state", schemaName, name)
}

// enumOf finds an enum in a state.
func enumOf(s *schema.Schema, schemaName, name string) (int, error) {
	for i := range s.Enums {
		if s.Enums[i].Schema == schemaName && s.Enums[i].Name == name {
			return i, nil
		}
	}
	return 0, fmt.Errorf("enum %s.%s is not in the migration state", schemaName, name)
}
