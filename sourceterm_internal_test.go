package orm

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// What a query contributes when it becomes a row source.
//
// A source needs two things: a statement to nest, and the names of the columns
// it provides. These tests are about the second — where the names come from, and
// that they come from metadata the query already held rather than from anything
// recovered afterwards.

// sourceProjection is a two-column shape whose columns are named, which is what
// a derived table or a CTE requires.
func sourceProjection() Projection[Composed, [2]any] {
	return Project2(
		Of(shapeID).As("thing_id"), Of(shapeName).As("label"),
		func(id int64, s string) [2]any { return [2]any{id, s} },
	)
}

func sourceBranch() *ComposedQuery[[2]any] {
	return Compose(nil, sourceProjection()).From(shapeSrc)
}

func TestSource_composedQueryReportsItsSelectListNames(t *testing.T) {
	sub, outs, err := sourceBranch().sourceTerm()
	if err != nil {
		t.Fatalf("sourceTerm: %v", err)
	}
	if _, ok := sub.(*expr.Select); !ok {
		t.Errorf("a composed query contributed %T, want a *expr.Select", sub)
	}
	if got := strings.Join(outs, ","); got != "thing_id,label" {
		t.Errorf("the source provides %q, want thing_id,label", got)
	}
}

// A set operation's output names are its first branch's, read from the result
// shape it was validated with. Not from the statement it renders, which carries
// no Go types and would be a second description of something already known.
func TestSource_compoundNamesItsColumnsAfterTheFirstBranch(t *testing.T) {
	second := Compose(nil, Project2(
		Of(shapeID).As("other_id"), Of(shapeName).As("other_label"),
		func(id int64, s string) [2]any { return [2]any{id, s} },
	)).From(shapeSrc)
	u := UnionAll[[2]any](sourceBranch(), second)

	sub, outs, err := u.sourceTerm()
	if err != nil {
		t.Fatalf("sourceTerm: %v", err)
	}
	// The whole compound, not one of its branches: a source built from the
	// first branch alone would render a statement that is a SELECT and produces
	// half the rows.
	c, ok := sub.(*expr.Compound)
	if !ok {
		t.Fatalf("a set operation contributed %T, want a *expr.Compound", sub)
	}
	if len(c.Branches) != 2 {
		t.Errorf("the compound contributed %d branches, want 2", len(c.Branches))
	}
	if got := strings.Join(outs, ","); got != "thing_id,label" {
		t.Errorf("the source provides %q, want the first branch's names thing_id,label", got)
	}

	// And the names are the shape's, position for position.
	if len(outs) != u.shape.columns() {
		t.Fatalf("the source provides %d names for a shape of %d columns", len(outs), u.shape.columns())
	}
	for i, name := range outs {
		if name != u.shape.slots[i].alias {
			t.Errorf("column %d is provided as %q and the shape calls it %q", i+1, name, u.shape.slots[i].alias)
		}
	}
}

// A first branch that names nothing is refused with a diagnostic saying so.
// Naming the second branch would not help, because PostgreSQL does not read it.
func TestSource_compoundRefusesAFirstBranchThatNamesNothing(t *testing.T) {
	unnamed := Compose(nil, Project2(
		Of(shapeID), Of(shapeName),
		func(id int64, s string) [2]any { return [2]any{id, s} },
	)).From(shapeSrc)
	u := UnionAll[[2]any](unnamed, sourceBranch())

	_, _, err := u.sourceTerm()
	if err == nil {
		t.Fatal("a set operation whose first branch names no column became a source")
	}
	if !strings.Contains(err.Error(), "column 1") || !strings.Contains(err.Error(), "first branch") {
		t.Errorf("the diagnostic %q does not say which column and which branch", err)
	}
}

// Becoming a source changes nothing about the query. The shape is still the one
// the branches were validated against, so it can be a branch again.
func TestSource_becomingASourceLeavesTheShapeAlone(t *testing.T) {
	u := UnionAll[[2]any](sourceBranch(), sourceBranch())
	before := u.shape

	if _, _, err := u.sourceTerm(); err != nil {
		t.Fatalf("sourceTerm: %v", err)
	}
	after := u.shape
	if after.columns() != before.columns() {
		t.Fatalf("the shape had %d columns and now has %d", before.columns(), after.columns())
	}
	for i := range before.slots {
		if before.slots[i] != after.slots[i] {
			t.Errorf("column %d changed when the union became a source", i+1)
		}
	}
	if err := compareResultShapes(before, after, 1, 2); err != nil {
		t.Errorf("the shape a union carries stopped comparing equal to itself: %v", err)
	}
}

// The alias is the caller's, and it belongs to the source rather than to the
// statement inside it. PostgreSQL puts it after the parenthesised body, which is
// why two derived tables over one query are two sources with two names.
func TestSource_theAliasIsTheCallers(t *testing.T) {
	a := Sub("stats", sourceBranch())
	if a.Err() != nil {
		t.Fatalf("Sub: %v", a.Err())
	}
	if a.AliasName() != "stats" {
		t.Errorf("the derived table is named %q, want stats", a.AliasName())
	}
	b := Sub("other", sourceBranch())
	if b.AliasName() == a.AliasName() {
		t.Errorf("two derived tables named differently came out as %q", a.AliasName())
	}

	// And a compound body is aliased the same way: the name is outside it,
	// because the body is not a SELECT and cannot carry one.
	u := Sub("u", UnionAll[[2]any](sourceBranch(), sourceBranch()))
	if u.Err() != nil {
		t.Fatalf("Sub over a set operation: %v", u.Err())
	}
	if u.AliasName() != "u" {
		t.Errorf("the compound derived table is named %q, want u", u.AliasName())
	}
}

// One rule about output names, spelled once, and it refuses a column that has
// none. A derived table and a CTE address their columns the same way, so a
// second copy of this would be a second answer.
func TestSource_namedOutputsRefusesAnUnnamedColumn(t *testing.T) {
	if err := namedOutputs([]string{"a", "b"}); err != nil {
		t.Errorf("two named columns were refused: %v", err)
	}
	err := namedOutputs([]string{"a", "", "c"})
	if err == nil {
		t.Fatal("a source with an unnamed second column was accepted")
	}
	if !strings.Contains(err.Error(), "output column 2") {
		t.Errorf("the diagnostic %q does not name the column that has none", err)
	}
	if err := namedOutputs(nil); err == nil {
		t.Error("a source declaring no columns at all was accepted")
	}
}

// The fixture contains what each source-integration mutation class attacks,
// asserted on clean code by construction.
func TestSourceMutationPrecondition(t *testing.T) {
	// P3-1, P3-2: a set operation that is a valid source, whose first branch
	// names its columns and whose second names them differently.
	t.Run("P3-1", func(t *testing.T) {
		differently := Compose(nil, Project2(
			Of(shapeID).As("other_id"), Of(shapeName).As("other_label"),
			func(id int64, s string) [2]any { return [2]any{id, s} },
		)).From(shapeSrc)
		u := UnionAll[[2]any](sourceBranch(), differently)
		_, outs, err := u.sourceTerm()
		if err != nil {
			t.Fatalf("the fixture's union is not a source: %v", err)
		}
		if outs[0] == "other_id" {
			t.Fatal("the two branches name their columns the same way, so taking " +
				"the names from the wrong one would not be observable")
		}
	})
	t.Run("P3-2", func(t *testing.T) {
		u := UnionAll[[2]any](sourceBranch(), sourceBranch())
		sub, _, err := u.sourceTerm()
		if err != nil {
			t.Fatalf("the fixture's union is not a source: %v", err)
		}
		c, ok := sub.(*expr.Compound)
		if !ok || len(c.Branches) < 2 {
			t.Fatal("the fixture's source is not a compound of several branches, so " +
				"building it from one branch would change nothing")
		}
	})

	// P3-3: a composed query whose select list names nothing, which is what the
	// every-column-named rule refuses.
	t.Run("P3-3", func(t *testing.T) {
		unnamed := Compose(nil, Project1(Of(shapeID), func(int64) [2]any { return [2]any{} })).From(shapeSrc)
		_, outs, err := unnamed.sourceTerm()
		if err != nil {
			t.Fatalf("the fixture's query does not build: %v", err)
		}
		if len(outs) != 1 || outs[0] != "" {
			t.Fatalf("the fixture's query names its columns %v, so there is nothing unnamed to refuse", outs)
		}
	})

	// P3-4: a composed query that does name its columns, so dropping the names
	// on the way to a source is observable.
	t.Run("P3-4", func(t *testing.T) {
		_, outs, err := sourceBranch().sourceTerm()
		if err != nil {
			t.Fatalf("the fixture's query is not a source: %v", err)
		}
		if len(outs) != 2 || outs[0] == "" {
			t.Fatalf("the fixture's query provides %v", outs)
		}
	})

	// P3-5: an ordinary CTE is not written as recursive, so writing it as one is
	// a change.
	t.Run("P3-5", func(t *testing.T) {
		c := CTE("c", sourceBranch())
		if c.Err() != nil {
			t.Fatalf("the fixture's CTE does not build: %v", c.Err())
		}
		shape := Project1(Ref(c, Named("thing_id", Of(shapeID))), func(id int64) int64 { return id })
		sql, _, err := Compose(nil, shape).With(c).From(c).SQL()
		if err != nil {
			t.Fatalf("the fixture's CTE does not compile: %v", err)
		}
		if strings.Contains(sql, "RECURSIVE") {
			t.Fatal("the fixture's ordinary CTE is already written as recursive")
		}
	})

	// P3-6, P3-7, P3-8: a compound nested in another compound, inside a source,
	// with parameters in the branches and in the query around it.
	t.Run("P3-6", func(t *testing.T) {
		inner := UnionAll[[2]any](sourceBranch(), sourceBranch())
		u := Sub("u", UnionAll[[2]any](inner, sourceBranch()))
		if u.Err() != nil {
			t.Fatalf("the fixture cannot nest a compound in a source: %v", u.Err())
		}
	})
	t.Run("P3-7", func(t *testing.T) {
		withArgs := func(v int64) *ComposedQuery[[2]any] {
			return Compose(nil, sourceProjection()).From(shapeSrc).Where(Of(shapeID).Gt(v))
		}
		u := Sub("u", UnionAll[[2]any](withArgs(1), withArgs(2)))
		shape := Project1(Ref(u, Named("thing_id", Of(shapeID))), func(id int64) int64 { return id })
		sql, args, err := Compose(nil, shape).From(u).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if len(args) != 2 || !strings.Contains(sql, "$2") {
			t.Fatalf("the fixture's branches do not both bind a parameter: %s / %v", sql, args)
		}
	})
	t.Run("P3-8", func(t *testing.T) {
		u := Sub("u", UnionAll[[2]any](sourceBranch(), sourceBranch()))
		shape := Project1(Cast(Val("tag"), Text), func(s string) string { return s })
		sql, args, err := Compose(nil, shape).From(u).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if len(args) != 1 || !strings.HasPrefix(sql, "SELECT CAST($1") {
			t.Fatalf("the fixture binds no parameter before the compound: %s / %v", sql, args)
		}
	})
	t.Run("P3-9", func(t *testing.T) {
		a := Sub("a", sourceBranch())
		b := Sub("b", sourceBranch())
		if a.AliasName() == b.AliasName() {
			t.Fatal("the fixture builds two sources of one name, so losing the caller's alias would not be observable")
		}
	})
}
