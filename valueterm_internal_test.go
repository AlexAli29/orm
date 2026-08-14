package orm

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// What a query contributes when it becomes a value subquery.
//
// One thing beyond the statement: how many columns its result has. These tests
// are about where that number comes from — construction-time metadata each
// builder already held — and about it agreeing with the statement it describes.

func oneColumn() *ComposedQuery[int64] {
	return Compose(nil, Project1(Of(shapeID), func(id int64) int64 { return id })).From(shapeSrc)
}

func twoColumns() *ComposedQuery[[2]any] {
	return Compose(nil, Project2(Of(shapeID), Of(shapeName),
		func(id int64, s string) [2]any { return [2]any{id, s} })).From(shapeSrc)
}

// Every builder reports an arity, and it is the arity of the statement it hands
// over. The two are computed differently — a select list here, a result shape
// there, a generated descriptor in the third — so a test that they agree is a
// test that none of them is describing a different statement.
func TestValue_everyBuilderReportsTheArityOfTheStatementItRenders(t *testing.T) {
	entity := NewRepo(nil, &shapeMeta).Query()
	projection := Select(NewRepo(nil, &shapeMeta), Project2(shapeID, shapeName,
		func(id int64, s string) [2]any { return [2]any{id, s} }))
	union := UnionAll[int64](oneColumn(), oneColumn())

	cases := []struct {
		name string
		term ValueTerm
		want int
	}{
		{"an entity query", entity, len(shapeMeta.Columns)},
		{"a projection query", projection, 2},
		{"a composed query", oneColumn(), 1},
		{"a composed query of two columns", twoColumns(), 2},
		{"a set operation", union, 1},
		{"a nested set operation", UnionAll[int64](union, oneColumn()), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub, arity, err := c.term.valueTerm()
			if err != nil {
				t.Fatalf("valueTerm: %v", err)
			}
			if arity != c.want {
				t.Errorf("%s reports %d columns, want %d", c.name, arity, c.want)
			}
			// The AST counts the same statement independently. A builder whose
			// number came from something other than what it renders would differ here.
			if got := expr.ResultArity(sub); got != arity {
				t.Errorf("%s reports %d columns and the statement it renders has %d",
					c.name, arity, got)
			}
		})
	}
}

// A set operation's arity is the result shape's, not one branch's select list.
// The shape is what every branch was checked against, so it is the only thing
// that speaks for the whole operation.
func TestValue_compoundArityComesFromTheResultShape(t *testing.T) {
	u := UnionAll[int64](oneColumn(), oneColumn())
	_, arity, err := u.valueTerm()
	if err != nil {
		t.Fatalf("valueTerm: %v", err)
	}
	if arity != u.shape.columns() {
		t.Errorf("the set operation reports %d columns and its shape has %d", arity, u.shape.columns())
	}

	wide := UnionAll[[2]any](twoColumns(), twoColumns())
	_, arity, err = wide.valueTerm()
	if err != nil {
		t.Fatalf("valueTerm: %v", err)
	}
	if arity != 2 || arity != wide.shape.columns() {
		t.Errorf("the two-column set operation reports %d columns and its shape has %d",
			arity, wide.shape.columns())
	}
}

// A composed query's arity is its select list, and a projection guarantees the
// shape agrees with that: it is refused unless it has one result slot per
// expression. So there is one number, not two that could drift.
func TestValue_composedArityIsTheSelectListAndTheShapeAgrees(t *testing.T) {
	q := twoColumns()
	_, arity, err := q.valueTerm()
	if err != nil {
		t.Fatalf("valueTerm: %v", err)
	}
	if arity != len(q.items) {
		t.Errorf("the query reports %d columns for a select list of %d", arity, len(q.items))
	}
	if !q.shape.known() {
		t.Fatal("a composed query built with Compose has no shape to agree with")
	}
	if q.shape.columns() != arity {
		t.Errorf("the shape describes %d columns and the select list has %d", q.shape.columns(), arity)
	}

	// A query built to be a source has no shape, and still has an arity: a
	// value subquery does not need one, which is why the two capabilities are
	// separate interfaces.
	rows := Rows(Named("n", Of(shapeID))).From(shapeSrc)
	if rows.shape.known() {
		t.Fatal("a Rows query now carries a shape")
	}
	if _, arity, err := rows.valueTerm(); err != nil || arity != 1 {
		t.Errorf("a source-only query reports %d columns and %v, want 1 and no error", arity, err)
	}
}

// Being a value subquery does not touch the shape. It is asked twice and the
// answer does not move.
func TestValue_becomingAValueSubqueryLeavesTheShapeAlone(t *testing.T) {
	u := UnionAll[int64](oneColumn(), oneColumn())
	before := u.shape

	for range 3 {
		if _, _, err := u.valueTerm(); err != nil {
			t.Fatalf("valueTerm: %v", err)
		}
	}
	after := u.shape
	if after.columns() != before.columns() {
		t.Fatalf("the shape had %d columns and now has %d", before.columns(), after.columns())
	}
	for i := range before.slots {
		if before.slots[i] != after.slots[i] {
			t.Errorf("column %d changed when the union became a value subquery", i+1)
		}
	}
	if err := compareResultShapes(before, after, 1, 2); err != nil {
		t.Errorf("the shape stopped comparing equal to itself: %v", err)
	}
}

// The two arity rules are refused at construction, and each is probed on its
// own.
//
// They were one test, and it asserted both. That made it fail under a mutation
// of either rule, so as evidence about one of them it said only that some arity
// check somewhere still worked — which is what the cross-function control
// caught: the probe for a defect in Scalar was equally disturbed by a defect in
// inSub. A probe that cannot tell two functions apart cannot be evidence about
// either.

// The membership test's rule: as many columns as the expression on its left.
func TestValue_membershipArityIsRefusedAtConstruction(t *testing.T) {
	wide := UnionAll[[2]any](twoColumns(), twoColumns())

	p := InSub(Of(shapeID), wide)
	if p.Err() == nil {
		t.Fatal("a two-column set operation was accepted in a membership test")
	}
	if !strings.Contains(p.Err().Error(), "compares 1 column") {
		t.Errorf("the diagnostic %q does not state the left-hand side's arity", p.Err())
	}
	if !strings.Contains(p.Err().Error(), "returns 2") {
		t.Errorf("the diagnostic %q does not state what the subquery returns", p.Err())
	}

	// What is deliberately not asserted here: that a right-hand side of the
	// left's width is accepted. That depends on the arity being reported
	// correctly, which is another function's job — and asserting it made this
	// probe fail under a mutation of that function, which is the thing that
	// stops a probe being evidence about its own. The accepted case is the
	// precondition's, and correct reporting is P4-8's.
}

// The scalar's rule: exactly one column. The mistake travels in the tree,
// because a value has no builder to record it in, and surfaces when something
// with an error return touches it.
func TestValue_scalarArityIsRefusedAtConstruction(t *testing.T) {
	wide := UnionAll[[2]any](twoColumns(), twoColumns())

	shape := Project1(Scalar[Composed, int64](wide), func(*int64) int64 { return 0 })
	_, _, err := Compose(nil, shape).From(shapeSrc).SQL()
	if err == nil {
		t.Fatal("a two-column set operation was accepted as a scalar value")
	}
	if !strings.Contains(err.Error(), "reads one column") {
		t.Errorf("the diagnostic %q is not the scalar arity rule's", err)
	}
	if !strings.Contains(err.Error(), "returns 2") {
		t.Errorf("the diagnostic %q does not state how many columns it has", err)
	}

	// The accepted case is not asserted here, for the reason given on the
	// membership probe above: it belongs to the function that reports the
	// arity, not to the one that checks it.
}

// The fixture contains what each value-subquery mutation class attacks,
// asserted on clean code by construction.
func TestValueMutationPrecondition(t *testing.T) {
	// P4-1, P4-2: a set operation of two columns exists and is not one column,
	// so a scalar arity rule that stopped firing would be observable.
	t.Run("P4-1", func(t *testing.T) {
		wide := UnionAll[[2]any](twoColumns(), twoColumns())
		_, arity, err := wide.valueTerm()
		if err != nil {
			t.Fatalf("the fixture's union is not a value subquery: %v", err)
		}
		if arity == 1 {
			t.Fatal("the fixture's wide union reports one column, so a scalar arity rule " +
				"that stopped firing would change nothing")
		}
	})
	t.Run("P4-2", func(t *testing.T) {
		narrow := UnionAll[int64](oneColumn(), oneColumn())
		_, arity, err := narrow.valueTerm()
		if err != nil {
			t.Fatalf("the fixture's union is not a value subquery: %v", err)
		}
		if arity != 1 {
			t.Fatalf("the fixture's narrow union reports %d columns, so the accepted case "+
				"is not the one the rule is about", arity)
		}
	})

	// P4-3, P4-4: the same pair reaches a membership test, whose rule is the
	// left-hand side's arity rather than one.
	t.Run("P4-3", func(t *testing.T) {
		if p := InSub(Of(shapeID), UnionAll[int64](oneColumn(), oneColumn())); p.Err() != nil {
			t.Fatalf("the fixture's one-column union is refused in a membership test: %v", p.Err())
		}
		if p := InSub(Of(shapeID), UnionAll[[2]any](twoColumns(), twoColumns())); p.Err() == nil {
			t.Fatal("the fixture's two-column union is already accepted in a membership test")
		}
	})
	t.Run("P4-4", func(t *testing.T) {
		// The left-hand side is one expression. If it were a tuple the required
		// arity would be its width, and the rule would still be "as many as the
		// left-hand side".
		p := InSub(Of(shapeName), UnionAll[[2]any](twoColumns(), twoColumns()))
		if p.Err() == nil {
			t.Fatal("a scalar left-hand side already accepts a two-column right-hand side")
		}
	})

	// P4-5: a compound nested inside another, in a value position.
	t.Run("P4-5", func(t *testing.T) {
		inner := UnionAll[int64](oneColumn(), oneColumn())
		p := InSub(Of(shapeID), UnionAll[int64](inner, oneColumn()))
		if p.Err() != nil {
			t.Fatalf("the fixture cannot nest a compound in a value position: %v", p.Err())
		}
	})

	// P4-6, P4-7: branches that bind parameters, inside a statement that binds
	// its own before and after.
	t.Run("P4-6", func(t *testing.T) {
		sql, args := valueFixtureSQL(t)
		if len(args) != 4 || !strings.Contains(sql, "$4") {
			t.Fatalf("the fixture binds %d values: %s / %v", len(args), sql, args)
		}
	})
	t.Run("P4-7", func(t *testing.T) {
		sql, _ := valueFixtureSQL(t)
		if !strings.Contains(sql, "$2") || !strings.Contains(sql, "$3") {
			t.Fatalf("the fixture's two branches do not bind distinct placeholders: %s", sql)
		}
	})

	// P4-8: the union carries a shape, which is what its arity is read from.
	t.Run("P4-8", func(t *testing.T) {
		u := UnionAll[int64](oneColumn(), oneColumn())
		if !u.shape.known() || u.shape.columns() != 1 {
			t.Fatal("the fixture's union has no shape to lose")
		}
	})

	// P4-10: two branches that differ only in Go type, which is what phase two's
	// comparison refuses and what a value position must not start accepting.
	t.Run("P4-10", func(t *testing.T) {
		narrower := Compose(nil, Project1(Of(shapeAge), func(int32) int64 { return 0 })).From(shapeSrc)
		if _, _, err := UnionAll[int64](oneColumn(), narrower).SQL(); err == nil {
			t.Fatal("the fixture's mismatched branches are already accepted")
		}
	})
}

// valueFixtureSQL is the statement P4-6 and P4-7 are about: a predicate before a
// membership test, two branches inside it that each bind a value, and a
// predicate after.
func valueFixtureSQL(t *testing.T) (string, []any) {
	t.Helper()
	branch := func(v int64) *ComposedQuery[int64] {
		return Compose(nil, Project1(Of(shapeID), func(id int64) int64 { return id })).
			From(shapeSrc).Where(Of(shapeID).Gt(v))
	}
	sql, args, err := Compose(nil, Project1(Of(shapeID), func(id int64) int64 { return id })).
		From(shapeSrc).
		Where(
			Of(shapeName).Eq("a"),
			InSub(Of(shapeID), UnionAll[int64](branch(1), branch(2))),
			Of(shapeName).Eq("b"),
		).SQL()
	if err != nil {
		t.Fatalf("the fixture does not compile: %v", err)
	}
	return sql, args
}
