package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Assign is one column set to one value, produced by a column descriptor:
//
//	Users.Name.Set("Alexander")
//	Users.DeletedAt.SetNull()
//
// Like a predicate it carries its entity, so an assignment to one entity's
// column cannot reach another entity's update.
type Assign[E any] struct {
	assignment expr.Assignment
	err        error
}

// IsZero reports whether the assignment names no column.
func (a Assign[E]) IsZero() bool { return a.assignment.Column.Name == "" }

// Column returns the name of the column being written.
func (a Assign[E]) Column() string { return a.assignment.Column.Name }

// Err returns the mistake made building the assignment, if any.
func (a Assign[E]) Err() error { return a.err }

// Set assigns a value to the column.
//
// The value is bound as a parameter, and a Go zero value is a value: Set(false)
// writes FALSE and Set("") writes the empty string. Nothing here reads a zero
// value as an absence.
func (c Col[E, V]) Set(v V) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c.col, Value: expr.Arg{Value: v}}}
}

// SetNull assigns SQL NULL to the column.
//
// It exists only on nullable columns, so writing NULL to a NOT NULL column does
// not compile. Note that NULL and DEFAULT are different operations: this writes
// a value, while [Default] omits the column so PostgreSQL supplies one.
func (c NullCol[E, V]) SetNull() Assign[E] { return setNull[E](c.col) }

// SetNull assigns SQL NULL to the column.
func (c NullOrdCol[E, V]) SetNull() Assign[E] { return setNull[E](c.col) }

// SetNull assigns SQL NULL to the column.
func (c NullTextCol[E]) SetNull() Assign[E] { return setNull[E](c.col) }

func setNull[E any](c expr.Column) Assign[E] {
	return Assign[E]{assignment: expr.Assignment{Column: c, Value: expr.Null{}}}
}

// InsertColumn is any column descriptor of entity E, whatever type it holds.
//
// It exists for the places that name a column without comparing it to
// anything — [Default] and [OnConflict] — where the value type is irrelevant
// but the entity is not. E appears in a method signature rather than only in
// the type name, which is what both keeps ColumnOf[User] distinct from
// ColumnOf[Post] and lets Go infer E from the arguments, so callers write
// Default(Users.CreatedAt) rather than Default[User](Users.CreatedAt).
type InsertColumn[E any] interface {
	insertColumn() expr.Column
	insertEntity(E)
}

func (c Col[E, V]) insertColumn() expr.Column { return c.col }
func (c Col[E, V]) insertEntity(E)            {}
