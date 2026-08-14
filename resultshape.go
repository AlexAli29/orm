package orm

import (
	"fmt"
	"reflect"
)

// What a query's result looks like, positionally.
//
// A set operation has to decide whether two branches produce the same thing,
// and the Go result type is not enough to decide it. These two projections
// build the same struct out of different columns:
//
//	orm.Project2(Users.Email, Users.Name,   func(a, b string) Pair { ... })
//	orm.Project2(Users.Name,  Users.Nick,   func(a, b string) Pair { ... })
//
// so a compatibility rule reading only R would accept a union whose second
// branch feeds a nullable nickname into a destination the first branch proved
// non-null. What decides it is the ordered list of result slots, which is what
// this is.
//
// It is deliberately small. Every field here is something the typed layer
// already knows at construction time — the Go type from the generic parameter,
// the nullability bit the select list already carries, the output alias — and
// nothing here is inferred from a rendered statement or from a row that came
// back.

// resultSlot is one column of a result, described by what the ORM can know.
type resultSlot struct {
	// goType is the Go type the value is read into. It comes from the type
	// parameter of the Selectable that produced the column, or from the
	// generated entity descriptor, so it is decided when the shape is built
	// rather than looked up per row.
	goType reflect.Type
	// nullable is the bit the select list already carries: whether the
	// destination can hold a NULL. It is not implied by goType. A *string
	// destination may be non-nullable because the expression cannot produce
	// NULL, and the two facts are compared separately for that reason.
	nullable bool
	// alias is the output name this column is given, empty when the caller
	// named none. See compareResultShapes for why it is carried and not
	// compared.
	alias string
}

// resultShape is a result's slots in select-list order.
type resultShape struct {
	slots []resultSlot
}

func (s resultShape) columns() int { return len(s.slots) }

// known reports whether a shape was built at all.
//
// A query that cannot describe its own result cannot be a branch of a set
// operation: accepting one would mean comparing something against nothing and
// calling it agreement.
func (s resultShape) known() bool { return s.slots != nil }

// shapeOf collects slots into a shape.
func shapeOf(slots ...resultSlot) resultShape {
	if slots == nil {
		slots = []resultSlot{}
	}
	return resultShape{slots: slots}
}

// slotOf describes one projected expression.
//
// The Go type comes from T rather than from the value, so a nil interface or an
// untyped nil never reaches it: reflect.TypeOf((*T)(nil)).Elem() is the type the
// scanner will read into whatever the expression turns out to be.
func slotOf[E, T any](s Selectable[E, T]) resultSlot {
	it := s.selectItem()
	return resultSlot{
		goType:   reflect.TypeOf((*T)(nil)).Elem(),
		nullable: it.Nullable,
		alias:    it.Alias,
	}
}

// entityShape describes the rows an entity query selects.
//
// It is derived from the generated descriptor rather than from E. The column
// order is meta.Columns, which the comment on that field calls the contract
// between the SELECT list and Dest — both index into it — so it is the same
// order the statement selects and the scanner reads. Nullability is the
// catalog's NotNull, and the Go type is the type of the destination Dest hands
// out for that column.
//
// Reading the field types through Dest is construction-time metadata, not
// scanner reflection: it happens once when a branch is described, the row path
// is untouched, and the alternative — reflecting over E's fields — would be a
// second, guessing description of a schema the generated code already states.
func entityShape[E any](meta *EntityMeta[E]) (resultShape, error) {
	if meta == nil {
		return resultShape{}, fmt.Errorf("the entity query has no metadata, so its result shape is unknown")
	}
	if len(meta.Columns) == 0 {
		return resultShape{}, fmt.Errorf("the entity's generated descriptor lists no columns")
	}
	if meta.Dest == nil {
		return resultShape{}, fmt.Errorf("the entity's generated descriptor has no destination function, so the Go type of each column is unknown")
	}
	var zero E
	slots := make([]resultSlot, 0, len(meta.Columns))
	for i, c := range meta.Columns {
		dest := meta.Dest(&zero, i)
		if dest == nil {
			return resultShape{}, fmt.Errorf("the generated descriptor has no destination for column %d (%s)", i+1, c.Name)
		}
		t := reflect.TypeOf(dest)
		if t == nil || t.Kind() != reflect.Pointer {
			return resultShape{}, fmt.Errorf("the generated destination for column %d (%s) is not a pointer", i+1, c.Name)
		}
		slots = append(slots, resultSlot{
			goType:   t.Elem(),
			nullable: !c.NotNull,
			alias:    c.Name,
		})
	}
	shape := shapeOf(slots...)
	if err := uniqueOutputNames(shape.slots); err != nil {
		return resultShape{}, fmt.Errorf("the entity's generated descriptor is unusable as a result shape: %w", err)
	}
	return shape, nil
}

// uniqueOutputNames refuses a shape that names one column twice.
//
// A shape describes a result by position and names the positions, and those
// names are what every consumer of a shape addresses columns by: a derived
// table's, a CTE's, an ordering term's. A name carried twice identifies nothing,
// and PostgreSQL agrees — it refuses ORDER BY "k" as ambiguous, and refuses a
// derived table that provides two columns of one name the moment either is
// referenced.
//
// It is checked where the shape is built rather than where the shape is used.
// Four consumers read this name list, and when the rule lived at the consumers
// three of them asked and the fourth did not; a fifth would inherit nothing.
// Built-once is the only version of this rule that a new consumer cannot forget.
//
// # What this forecloses
//
// PostgreSQL does permit duplicate output names in a select list as long as
// nothing refers to them: SELECT id AS k, name AS k FROM t is a legal statement.
// A result described by a shape can no longer be that statement. That is a real
// restriction and it is deliberate — every path this package offers for a shape
// addresses its columns by name, so a shape whose names do not identify columns
// is one no consumer could use. Reading such a result positionally is still
// available: an entity query selects its descriptor's columns and needs no shape
// to do it.
//
// Unnamed columns are not duplicates of each other. A value subquery needs no
// names at all, so several empty ones are not a collision.
func uniqueOutputNames(slots []resultSlot) error {
	seen := make(map[string]int, len(slots))
	for i, slot := range slots {
		if slot.alias == "" {
			continue
		}
		if first, ok := seen[slot.alias]; ok {
			return fmt.Errorf("result columns %d and %d are both named %q; a name identifies one column, so a result carrying it twice cannot be addressed, ordered by, or selected from",
				first+1, i+1, slot.alias)
		}
		seen[slot.alias] = i
	}
	return nil
}

// compareResultShapes is the one authority on whether two branches produce the
// same result.
//
// Every route that builds a set operation asks this and nothing else. A second
// validator — in the public API, in the compiler, in a derived source — would be
// a second opinion, and the one that disagreed would be found by a user rather
// than by a test.
//
// # What is compared
//
// The column count, then each slot's Go type and nullability, positionally.
// Those are the three things the ORM can state without guessing.
//
// # What is not compared, and why
//
// Aliases are not compared. In this ORM an alias is an SQL output name, not part
// of result identity: results are scanned positionally — every ProjectN builds a
// dest slice in select-list order and calls rows.Scan on it — so a union whose
// branches name their columns differently still reads correctly. PostgreSQL
// takes a compound's output names from its first branch, which is the same
// answer from the other direction. Where names do matter is a derived table or a
// CTE, and that is a property of the enclosing source rather than of the union:
// those call sites already require every output to be named and take the names
// from the term they were built from.
//
// Requiring alias equality would therefore reject unions PostgreSQL executes
// correctly and this scanner reads correctly, for no property anything depends
// on.
//
// # What is left to PostgreSQL
//
// SQL type identity is not compared, because the AST does not have it. An
// expr.SelectItem carries a node, an alias and the nullability bit the typed
// layer computed; there is no PostgreSQL type on it, and inventing a shadow type
// system to get one would mean modelling every function's result type and every
// implicit cast. So what is statically enforced is: same number of columns, same
// Go destination type, same nullability. What PostgreSQL remains responsible for
// is whether the two SQL expressions in a slot have compatible SQL types — it
// checks that when the statement runs and reports it precisely.
//
// # No widening
//
// A mismatch is refused rather than reconciled. int32 against int64, string
// against uuid.UUID, T against *T, nullable against non-nullable: PostgreSQL
// could coerce some of those pairs, and coercing them here would mean choosing a
// destination type the caller did not write.
func compareResultShapes(left, right resultShape, leftBranch, rightBranch int) error {
	if !left.known() {
		return fmt.Errorf("UNION ALL branch %d does not describe its result shape", leftBranch)
	}
	if !right.known() {
		return fmt.Errorf("UNION ALL branch %d does not describe its result shape", rightBranch)
	}
	if len(left.slots) != len(right.slots) {
		return fmt.Errorf("UNION ALL branch %d selects %d columns and branch %d selects %d; every branch of a set operation returns the same number of columns",
			leftBranch, len(left.slots), rightBranch, len(right.slots))
	}
	for i := range left.slots {
		l, r := left.slots[i], right.slots[i]
		if l.goType != r.goType {
			return fmt.Errorf("UNION ALL branch %d result column %d has Go type %s; branch %d has %s",
				rightBranch, i+1, typeName(r.goType), leftBranch, typeName(l.goType))
		}
		if l.nullable != r.nullable {
			return fmt.Errorf("UNION ALL branch %d result column %d is %s; branch %d is %s. A set operation does not widen: declare both sides the same way",
				rightBranch, i+1, nullability(r.nullable), leftBranch, nullability(l.nullable))
		}
	}
	return nil
}

// typeName renders a Go type for a diagnostic.
//
// It is the type's own string, which is stable and readable — never a pointer
// address or a reflect dump, because a diagnostic that changes between runs
// cannot be asserted on and will not be trusted.
func typeName(t reflect.Type) string {
	if t == nil {
		return "unknown"
	}
	return t.String()
}

func nullability(n bool) string {
	if n {
		return "nullable"
	}
	return "not nullable"
}
