package orm

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// What an ordering term is, and what decides whether it is allowed.
//
// A compound may be ordered by an output column name and by nothing else, so a
// term is a name and a direction. Which names are allowed comes from the result
// shape, which is where the compound's output names have lived since they were
// needed for a source.

func orderedBranch() *ComposedQuery[[2]any] {
	return Compose(nil, Project2(
		Of(shapeID).As("thing_id"), Of(shapeName).As("label"),
		func(id int64, s string) [2]any { return [2]any{id, s} },
	)).From(shapeSrc)
}

var (
	outID    = Named("thing_id", Of(shapeID))
	outLabel = Named("label", Of(shapeName))
)

// A term carries a name and a direction. There is nowhere in it for the
// expression PostgreSQL refuses, which is what makes the invalid term
// unwritable rather than merely rejected.
func TestOrder_aTermIsANameAndADirection(t *testing.T) {
	if got := outID.Asc(); got.Name() != "thing_id" || got.desc {
		t.Errorf("Asc = %+v, want thing_id ascending", got)
	}
	if got := outLabel.Desc(); got.Name() != "label" || !got.desc {
		t.Errorf("Desc = %+v, want label descending", got)
	}
	// The declaration is untouched, so one value orders several operations.
	if outID.Name() != "thing_id" {
		t.Errorf("the declaration was changed by ordering with it: %q", outID.Name())
	}
}

// The allowed names are the result shape's aliases, position for position. That
// is the same list a source's columns come from, so a union that is ordered and
// selected from cannot be ordered by a name it does not provide.
func TestOrder_theAllowedNamesAreTheShapes(t *testing.T) {
	u := UnionAll[[2]any](orderedBranch(), orderedBranch())
	for _, slot := range u.shape.slots {
		if !u.provides(slot.alias) {
			t.Errorf("the shape has a column %q the union does not admit to", slot.alias)
		}
	}
	if u.provides("nope") {
		t.Error("the union admits to a column its shape does not have")
	}

	_, outs, err := u.sourceTerm()
	if err != nil {
		t.Fatalf("sourceTerm: %v", err)
	}
	for _, name := range outs {
		if !u.provides(name) {
			t.Errorf("the union provides %q as a source and refuses to order by it", name)
		}
	}
}

// OrderBy checks the term against the result shape and refuses a name the
// operation does not have. Nothing downstream can: the writer emits whatever
// name it is given, because it has no idea what a compound's columns are called.
func TestOrder_orderByRefusesAStranger(t *testing.T) {
	stranger := Named("nope", Of(shapeID))

	q := UnionAll[[2]any](orderedBranch(), orderedBranch()).OrderBy(stranger.Asc())
	if len(q.errs) == 0 {
		t.Fatal("an ordering term naming a column the result does not have was accepted")
	}
	if _, err := q.build(); err == nil {
		t.Fatal("the union built anyway")
	} else if !strings.Contains(err.Error(), `no result column "nope"`) {
		t.Errorf("the diagnostic %q does not name the column that was asked for", err)
	}
	// And the term did not reach the tree.
	accepted := UnionAll[[2]any](orderedBranch(), orderedBranch()).OrderBy(stranger.Asc())
	if len(accepted.orderBy) != 0 {
		t.Errorf("a refused term was recorded anyway: %+v", accepted.orderBy)
	}
}

// A descriptor declaring two columns of one name, which is the one route by
// which a duplicate output name can reach a union: every projection refuses one
// already, and EntityMeta is public.
var ambiguousShapeMeta = EntityMeta[shapeEntity]{
	Table:  TableID{Schema: "public", Name: "shape_entity"},
	Source: shapeSrc,
	Columns: []ColumnMeta{
		{Name: "k", Field: "ID", NotNull: true},
		{Name: "k", Field: "Name", NotNull: true},
	},
	Dest: func(e *shapeEntity, i int) any {
		if i == 0 {
			return &e.ID
		}
		return &e.Name
	},
}

// A descriptor naming one column twice does not produce a shape at all, so no
// consumer of a shape has to ask about it. This is the probe for that: the
// refusal is at construction, and the union that would have carried the shape
// never forms.
func TestOrder_aShapeCannotNameOneColumnTwice(t *testing.T) {
	_, err := entityShape(&ambiguousShapeMeta)
	if err == nil {
		t.Fatal("a descriptor naming one column twice produced a result shape")
	}
	if !strings.Contains(err.Error(), `both named "k"`) {
		t.Errorf("the diagnostic %q does not say which name is carried twice", err)
	}
	if !strings.Contains(err.Error(), "columns 1 and 2") {
		t.Errorf("the diagnostic %q does not say which two columns collide", err)
	}

	// So the union does not form, and the ordering path is never reached.
	repo := func() *Repo[shapeEntity] { return NewRepo(nil, &ambiguousShapeMeta) }
	u := UnionAll[shapeEntity](repo().Query(), repo().Query())
	if len(u.errs) == 0 {
		t.Fatal("a union formed over a descriptor that has no result shape")
	}
	if u.shape.known() {
		t.Error("the union carries a shape built from a descriptor that has none")
	}
	if _, _, err := u.OrderBy(Named("k", Of(shapeID)).Asc()).SQL(); err == nil {
		t.Fatal("the union rendered")
	}
}

func TestOrder_describesWhatItDoesProvide(t *testing.T) {
	u := UnionAll[[2]any](orderedBranch(), orderedBranch())
	got := u.describeOutputs()
	if !strings.Contains(got, `"thing_id"`) || !strings.Contains(got, `"label"`) {
		t.Errorf("the description %q does not list the columns", got)
	}

	// A union whose first branch names nothing describes the situation rather
	// than an empty list.
	bare := Compose(nil, Project1(Of(shapeID), func(int64) int64 { return 0 })).From(shapeSrc)
	none := UnionAll[int64](bare, bare)
	if got := none.describeOutputs(); !strings.Contains(got, "names none of its columns") {
		t.Errorf("the description %q does not say why there is nothing to order by", got)
	}
}

// The term reaches the tree as an output order, not as an expression. A compound
// carrying an Order would be one that could render SQL PostgreSQL refuses.
func TestOrder_reachesTheTreeAsAnOutputOrder(t *testing.T) {
	u := UnionAll[[2]any](orderedBranch(), orderedBranch()).
		OrderBy(outID.Desc(), outLabel.Asc())
	c, err := u.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	want := []expr.OutputOrder{{Name: "thing_id", Desc: true}, {Name: "label"}}
	if len(c.OrderBy) != len(want) {
		t.Fatalf("the compound carries %d ordering terms, want %d", len(c.OrderBy), len(want))
	}
	for i := range want {
		if c.OrderBy[i] != want[i] {
			t.Errorf("term %d = %+v, want %+v", i+1, c.OrderBy[i], want[i])
		}
	}
}

// Building the compound twice does not accumulate the terms, because the slice
// the tree gets is a copy rather than the builder's own.
func TestOrder_buildingTwiceDoesNotDuplicateTheTerms(t *testing.T) {
	u := UnionAll[[2]any](orderedBranch(), orderedBranch()).OrderBy(outID.Asc())
	first, err := u.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	second, err := u.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(first.OrderBy) != 1 || len(second.OrderBy) != 1 {
		t.Fatalf("two builds produced %d and %d terms, want 1 each", len(first.OrderBy), len(second.OrderBy))
	}
	// And the two trees do not share the slice, so appending to one is not felt
	// by the other.
	first.OrderBy = append(first.OrderBy, expr.OutputOrder{Name: "label"})
	if len(second.OrderBy) != 1 {
		t.Error("the two compounds share their ordering slice")
	}
}

// The fixture contains what each ordering mutation class attacks.
func TestOrderMutationPrecondition(t *testing.T) {
	u := func() *UnionQuery[[2]any] { return UnionAll[[2]any](orderedBranch(), orderedBranch()) }

	// P5-1, P5-2: the union provides two named columns and does not provide a
	// third, so both accepting a stranger and refusing a real one are visible.
	t.Run("P5-1", func(t *testing.T) {
		q := u()
		if !q.provides("thing_id") || q.provides("nope") {
			t.Fatal("the fixture's union does not distinguish a column it has from one it does not")
		}
	})
	t.Run("P5-2", func(t *testing.T) {
		if _, _, err := u().OrderBy(outID.Asc()).SQL(); err != nil {
			t.Fatalf("the fixture's union cannot be ordered by a column it has: %v", err)
		}
	})

	// P5-3: a descending term, so a direction that is always ascending is
	// observable.
	t.Run("P5-3", func(t *testing.T) {
		sql, _, err := u().OrderBy(outID.Desc()).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if !strings.Contains(sql, "DESC") {
			t.Fatalf("the fixture orders nothing descending: %s", sql)
		}
	})

	// P5-4: two terms, so a clause that writes one is observable.
	t.Run("P5-4", func(t *testing.T) {
		sql, _, err := u().OrderBy(outID.Asc(), outLabel.Desc()).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if strings.Count(sql, "ASC")+strings.Count(sql, "DESC") != 2 {
			t.Fatalf("the fixture does not order by two columns: %s", sql)
		}
	})

	// P5-5: an ordering and a limit together, so their order in the statement
	// is observable.
	t.Run("P5-5", func(t *testing.T) {
		sql, _, err := u().OrderBy(outID.Asc()).Limit(2).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if !strings.HasSuffix(sql, `ORDER BY "thing_id" ASC LIMIT 2`) {
			t.Fatalf("the fixture does not end with an ordering and a limit: %s", sql)
		}
	})

	// P5-8 was a class about an ambiguity check in the ordering path. It is now
	// not expressible: a shape refuses to name one column twice, so no consumer
	// receives one and there is no check to remove. This asserts the state the
	// class needed cannot be reached.
	t.Run("P5-8", func(t *testing.T) {
		repo := func() *Repo[shapeEntity] { return NewRepo(nil, &ambiguousShapeMeta) }
		u := UnionAll[shapeEntity](repo().Query(), repo().Query())
		if u.shape.known() {
			t.Fatal("a union carries a shape naming one column twice, so the ambiguity " +
				"the class was about is reachable again and the class should be reinstated")
		}
	})

	// P5-6: a branch carrying its own ordering, which has to stay inside it.
	t.Run("P5-6", func(t *testing.T) {
		inner := UnionAll[[2]any](orderedBranch(), orderedBranch()).OrderBy(outID.Asc()).Limit(3)
		sql, _, err := UnionAll[[2]any](inner, orderedBranch()).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if !strings.HasPrefix(sql, "(SELECT ") {
			t.Fatalf("the fixture's ordered branch is not parenthesised: %s", sql)
		}
	})
}

// A locked branch is refused when the branch is handed over, which is a
// different moment from when the statement is written.
//
// The writer refuses it too, so a probe that only asked whether the union
// rendered would pass with the construction-time refusal gone — which is what
// the campaign said when this probe was first pointed at the whole statement.
// What distinguishes the authority from the floor is that the mistake is
// recorded on the builder before anything is rendered.
func TestAudit_aLockedBranchIsRefusedWhenHandedOver(t *testing.T) {
	branch := func() *ComposedQuery[int64] {
		return Compose(nil, Project1(Of(shapeID).As("id"), func(v int64) int64 { return v })).From(shapeSrc)
	}
	locked := Compose(nil, Project1(Of(shapeID).As("id"), func(v int64) int64 { return v })).
		From(shapeSrc).ForUpdate()

	u := UnionAll[int64](locked, branch())
	if len(u.errs) == 0 {
		t.Fatal("a locked branch was taken without complaint; the refusal happens later, if at all")
	}
	if !strings.Contains(errors.Join(u.errs...).Error(), "locking clause in a set operation") {
		t.Errorf("the recorded mistake %q is not the locking rule's", errors.Join(u.errs...))
	}
	// And the refused branch did not reach the operation. Put it second, so
	// that the first one is taken and the count says which of them was dropped.
	second := UnionAll[int64](branch(), locked)
	if len(second.errs) == 0 {
		t.Fatal("a locked branch in second position was taken without complaint")
	}
	if len(second.branches) != 1 {
		t.Errorf("the union holds %d branches, want only the one that was accepted", len(second.branches))
	}
}

// The fixture contains what each audit-fix mutation class attacks.
func TestAuditMutationPrecondition(t *testing.T) {
	branch := func() *ComposedQuery[int64] {
		return Compose(nil, Project1(Of(shapeID).As("id"), func(v int64) int64 { return v })).From(shapeSrc)
	}

	// A0, A1: a branch carrying a locking clause exists and is refused.
	t.Run("A0", func(t *testing.T) {
		locked := Compose(nil, Project1(Of(shapeID).As("id"), func(v int64) int64 { return v })).
			From(shapeSrc).ForUpdate()
		sel, err := locked.build()
		if err != nil {
			t.Fatalf("the fixture's locked branch does not build: %v", err)
		}
		if !sel.ForUpdate {
			t.Fatal("the fixture's branch carries no locking clause, so refusing one is not observable")
		}
		if _, _, err := UnionAll[int64](locked, branch()).SQL(); err == nil {
			t.Fatal("the fixture's locked branch is already accepted")
		}
	})

	// A2: a branch carrying its own WITH, which has to stay inside it.
	t.Run("A2", func(t *testing.T) {
		c := CTE("f", branch())
		withCTE := Compose(nil, Project1(
			Ref(c, Named("id", Of(shapeID))).As("id"), func(v int64) int64 { return v },
		)).With(c).From(c)
		sql, _, err := UnionAll[int64](withCTE, branch()).SQL()
		if err != nil {
			t.Fatalf("the fixture does not compile: %v", err)
		}
		if !strings.HasPrefix(sql, `(WITH `) {
			t.Fatalf("the fixture's branch does not carry a parenthesised WITH: %s", sql)
		}
	})

	// A3: a statement selecting from a named query it does not declare.
	t.Run("A3", func(t *testing.T) {
		c := CTE("g", branch())
		shape := Project1(Ref(c, Named("id", Of(shapeID))), func(v int64) int64 { return v })
		if _, _, err := Compose(nil, shape).From(c).SQL(); err == nil {
			t.Fatal("the fixture's undeclared reference is already accepted")
		}
		if _, _, err := Compose(nil, shape).With(c).From(c).SQL(); err != nil {
			t.Fatalf("the fixture's declared reference is refused: %v", err)
		}
	})
}

// A named query declared on the operation reaches the statement.
//
// It is the only route left for two branches to read one declaration: a branch's
// own WITH is parenthesised to keep it that branch's, so sharing is either
// declared on the operation or it does not happen.
func TestAudit_theOperationsWithReachesTheStatement(t *testing.T) {
	base := Compose(nil, Project1(Of(shapeID).As("id"), func(v int64) int64 { return v })).From(shapeSrc)
	c := CTE("shared", base)
	ref := Named("id", Of(shapeID))
	reading := func() *ComposedQuery[int64] {
		return Compose(nil, Project1(Ref(c, ref).As("id"), func(v int64) int64 { return v })).From(c)
	}

	u := UnionAll[int64](reading(), reading()).With(c)
	built, err := u.build()
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(built.With) != 1 {
		t.Fatalf("the compound carries %d WITH items, want 1", len(built.With))
	}
	if built.With[0] != c {
		t.Error("the compound carries a different item from the one declared")
	}

	// What is deliberately not asserted here: that the branches would be refused
	// without the declaration. That is the rule about references to undeclared
	// names, which lives in the writer — and asserting it made this probe fail
	// under a mutation of that function, which is what stops a probe being
	// evidence about its own. It is asserted by the tests that own it.
}
