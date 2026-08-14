package orm

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TableID names the table an entity maps to.
type TableID struct {
	// Schema is the PostgreSQL schema the table lives in, always explicit:
	// resolving it through the search path would make the identity of a table
	// depend on a session setting.
	Schema string
	// Name is the table's name within that schema.
	Name string
}

// String renders the table as schema.name.
func (t TableID) String() string { return t.Schema + "." + t.Name }

// ColumnMeta describes one mapped column.
//
// The flags come from the catalog by way of reconciliation, so the write path
// knows what PostgreSQL will accept without asking it. A generated column
// cannot be written and an identity column supplies itself; discovering either
// from an error message at execution time would be discovering it too late.
type ColumnMeta struct {
	// Name is the PostgreSQL column name.
	Name string
	// Field is the Go field it maps to, carried for error messages rather than
	// for dispatch — nothing in the read path looks a field up by name.
	Field string
	// NotNull reports a column that rejects NULL.
	NotNull bool
	// HasDefault reports a column with a DEFAULT expression.
	HasDefault bool
	// Identity reports GENERATED ... AS IDENTITY.
	Identity bool
	// Generated reports a stored generated column, which PostgreSQL computes
	// and refuses to be told.
	Generated bool
}

// Writable reports whether an INSERT or UPDATE may supply a value.
//
// Identity columns are excluded along with generated ones. PostgreSQL would
// accept a value for a BY DEFAULT identity, but accepting it here would mean
// deciding from a Go field whether the author meant to override the sequence —
// which is exactly the kind of inference this project refuses to make. An
// explicit override is a separate API, not a heuristic.
func (c ColumnMeta) Writable() bool { return !c.Generated && !c.Identity }

// Defaultable reports whether PostgreSQL can supply a value for a column left
// out of an INSERT. A nullable column can: its default is NULL.
func (c ColumnMeta) Defaultable() bool {
	return c.HasDefault || c.Identity || c.Generated || !c.NotNull
}

// EntityMeta is the generated description of an entity.
//
// It is written by the generator from a mapping the reconciler proved, so its
// column list is not a guess: every entry corresponds to a column that exists,
// with a type the entity can hold.
type EntityMeta[E any] struct {
	// Table is the relation this entity maps to, schema and name.
	Table TableID
	// Source is the table occurrence the entity's column descriptors were
	// built from.
	//
	// It matters that this is the same value and not merely the same name. A
	// source's identity is its pointer, because two aliases of one table have
	// to be distinguishable; a repository that made its own would hold an
	// occurrence none of the descriptors belong to, and every predicate would
	// be reported as out of scope. Generated code passes the one the
	// descriptors share. When it is nil a matching occurrence is derived from
	// Table, which is enough for queries that name no columns.
	Source *Source
	// Columns is the mapped column list in the order the entity declares its
	// fields. That order is the contract between the SELECT list and Dest —
	// both index into it, so they cannot drift apart.
	Columns []ColumnMeta
	// Dest returns a pointer to the field holding column idx, so that a row can
	// be scanned straight into the struct. It is generated as a switch over a
	// constant index, which is why the read path uses no reflection at all.
	Dest func(e *E, idx int) any
	// Value returns the value of the field holding column idx, which is what
	// an INSERT sends. It is Dest's counterpart and is generated the same way,
	// so writing a row costs no reflection either.
	Value func(e *E, idx int) any
}

// validate reports the ways generated metadata could be unusable. It runs once
// per query rather than per row, and it exists because a nil Dest would
// otherwise surface as a nil-pointer panic several frames away from its cause.
func (m *EntityMeta[E]) validate() error {
	switch {
	case m == nil:
		return fmt.Errorf("entity metadata is nil")
	case m.Table.Name == "":
		return fmt.Errorf("entity metadata names no table")
	case len(m.Columns) == 0:
		return fmt.Errorf("entity metadata for %s has no columns", m.Table)
	case m.Dest == nil:
		return fmt.Errorf("entity metadata for %s has no scanner", m.Table)
	}
	for i, c := range m.Columns {
		if c.Name == "" {
			return fmt.Errorf("entity metadata for %s has an unnamed column at index %d", m.Table, i)
		}
	}
	return nil
}

// Executor is the part of pgx a query needs.
//
// It is exactly one method so that *pgxpool.Pool, pgx.Tx and *pgx.Conn all
// satisfy it as they are, with no adapter to write and nothing to wrap. Later
// milestones need more of pgx than this; they will need a wider interface, and
// widening one is a breaking change, so the name is chosen to be the thing that
// grows rather than a Queryer that would have to be replaced.
type Executor interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// pgxRows names the part of pgx.Rows the read path uses, so that the scanning
// code reads as what it does rather than as everything a result set can do.
type pgxRows = pgx.Rows
