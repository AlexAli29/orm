package orm

import (
	"strings"
	"testing"
	"time"
)

// The fixture contains what each UNION ALL mutation class attacks.
//
// A mutation whose fixture lacks the attacked structure cannot be caught, and
// recording it as a survivor would be recording that nothing noticed something
// nothing could have noticed. So each precondition is asserted here, on clean
// code, by construction rather than by reading source text — a grep would pass
// on a fixture that no longer builds what the string describes.

func TestUnionMutationPrecondition(t *testing.T) {
	type box struct{}

	// P2-1: two shapes that differ only in the Go type of one slot. Without
	// this pair, removing the type comparison changes nothing observable.
	t.Run("P2-1", func(t *testing.T) {
		text := shapeOf(slotOf(shapeName))
		other := shapeOf(slotOf(shapeAge))
		if text.slots[0].goType == other.slots[0].goType {
			t.Fatal("the fixture has no two columns of different Go types")
		}
		if text.slots[0].nullable != other.slots[0].nullable {
			t.Fatal("the two columns differ in nullability as well, so a type " +
				"comparison removed would still be caught by the nullability one")
		}
	})

	// P2-2: two entity descriptors of one Go type that differ only in
	// nullability. Without this pair, removing the nullability comparison is
	// invisible.
	t.Run("P2-2", func(t *testing.T) {
		relaxed := shapeMeta
		relaxed.Columns = append([]ColumnMeta(nil), shapeMeta.Columns...)
		relaxed.Columns[1].NotNull = false

		strict, err := entityShape(&shapeMeta)
		if err != nil {
			t.Fatalf("entityShape: %v", err)
		}
		loose, err := entityShape(&relaxed)
		if err != nil {
			t.Fatalf("entityShape: %v", err)
		}
		if strict.slots[1].goType != loose.slots[1].goType {
			t.Fatal("the two descriptors differ in Go type too, so a nullability " +
				"comparison removed would still be caught by the type one")
		}
		if strict.slots[1].nullable == loose.slots[1].nullable {
			t.Fatal("the two descriptors do not differ in nullability")
		}
	})

	// P2-3: a narrow shape and a wider one that agrees on every column the
	// narrow one has. That is the pair a count comparison weakened to "wider is
	// fine" would accept, and a fixture whose shapes disagree early would be
	// caught by the type comparison instead.
	t.Run("P2-3", func(t *testing.T) {
		narrow := shapeOf(slotOf(shapeID), slotOf(shapeName))
		wide := shapeOf(slotOf(shapeID), slotOf(shapeName), slotOf(shapeAct))
		if narrow.columns() >= wide.columns() {
			t.Fatal("the fixture has no shape wider than another")
		}
		for i := range narrow.slots {
			if narrow.slots[i] != wide.slots[i] {
				t.Fatalf("the two shapes disagree at column %d, so a count "+
					"comparison removed would still be caught per column", i+1)
			}
		}
	})

	// P2-4: a composed query carries the shape of the projection it was built
	// from. If Compose never had one to carry, dropping it would change nothing.
	t.Run("P2-4", func(t *testing.T) {
		p := Project2(Of(shapeID), Of(shapeName), func(int64, string) box { return box{} })
		if !p.shape.known() || p.shape.columns() != 2 {
			t.Fatal("the projection the fixture composes has no shape to carry")
		}
		q := Compose(nil, p).From(shapeSrc)
		if !q.shape.known() {
			t.Fatal("a composed query does not carry a shape, so dropping it is not observable")
		}
	})

	// P2-5: a union of a union is expressible and validates, which is what makes
	// a nested union's shape something that can be lost.
	t.Run("P2-5", func(t *testing.T) {
		p := Project2(Of(shapeID), Of(shapeName), func(int64, string) box { return box{} })
		branch := func() *ComposedQuery[box] { return Compose(nil, p).From(shapeSrc) }
		inner := UnionAll[box](branch(), branch())
		if _, _, err := UnionAll[box](inner, branch()).SQL(); err != nil {
			t.Fatalf("the fixture cannot nest a union: %v", err)
		}
		term, err := inner.unionBranch()
		if err != nil {
			t.Fatalf("the nested union is not a branch: %v", err)
		}
		if !term.shape.known() {
			t.Fatal("the nested union has no shape to lose")
		}
	})

	// P2-6: a query with no result shape exists and is reachable as an argument.
	// Two independent guards refuse it, which is why the class has two sites:
	// bypassing one alone proves nothing about the other.
	t.Run("P2-6", func(t *testing.T) {
		rows := Rows(Named("id", Of(shapeID))).From(shapeSrc)
		if rows.shape.known() {
			t.Fatal("a source-only query now describes a result, so there is nothing shapeless to refuse")
		}
		if _, err := rows.unionBranch(); err == nil {
			t.Fatal("the builder guard is already gone")
		}
		if err := compareResultShapes(shapeOf(slotOf(shapeID)), resultShape{}, 1, 2); err == nil {
			t.Fatal("the comparison guard is already gone")
		}
	})
}

// The semantic probe for P2-4, at the level of the code it mutates: a composed
// query holds the shape of the projection it was built from, and holds it as a
// copy of that projection's rather than as something rebuilt.
func TestShape_composeCarriesTheProjectionsShape(t *testing.T) {
	type box struct{}
	p := Project3(Of(shapeID), Of(shapeNick), Of(shapeSeen),
		func(int64, *string, time.Time) box { return box{} })

	q := Compose(nil, p).From(shapeSrc)
	if !q.shape.known() {
		t.Fatal("a composed query does not carry a result shape")
	}
	if q.shape.columns() != p.shape.columns() {
		t.Fatalf("the projection describes %d columns and the query %d",
			p.shape.columns(), q.shape.columns())
	}
	for i := range p.shape.slots {
		if q.shape.slots[i] != p.shape.slots[i] {
			t.Errorf("column %d: the query says {%s %s %q} and the projection {%s %s %q}", i+1,
				typeName(q.shape.slots[i].goType), nullability(q.shape.slots[i].nullable), q.shape.slots[i].alias,
				typeName(p.shape.slots[i].goType), nullability(p.shape.slots[i].nullable), p.shape.slots[i].alias)
		}
	}

	// And the query can then be a branch, which is the thing the shape is for.
	term, err := q.unionBranch()
	if err != nil {
		t.Fatalf("a composed query is not a branch: %v", err)
	}
	if !term.shape.known() {
		t.Fatal("the branch a composed query contributes has no shape")
	}
	if term.newScan == nil {
		t.Fatal("the branch a composed query contributes has no scanner")
	}
	if strings.Contains(typeName(term.shape.slots[0].goType), "0x") {
		t.Error("a slot's type renders as an address")
	}
}
