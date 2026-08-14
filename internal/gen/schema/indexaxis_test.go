package schema

import (
	"fmt"
	"slices"
	"testing"
)

// One axis at a time.
//
// A drift test that changes two properties at once proves nothing about either:
// delete the comparison for one and the other still reports the difference, so
// the test passes and the mutation survives while looking covered. That is how
// index-method comparison went untested — its fixture changed the method and
// dropped the predicate in the same statement.
//
// So each case here differs from the baseline in exactly one field, and the
// baseline is written once. A mutation that removes any single comparison makes
// exactly one of these fail, which is what makes each one a valid killer.

// axisBaseline is the one index every case differs from in exactly one field.
//
// It is shared with the fixture precondition so that what is asserted to carry
// every axis is the same value the cases are built from.
func axisBaseline() (Index, IndexColumn) {
	key := IndexColumn{Name: "owner_id", Direction: Asc, Nulls: NullsLast, OpClass: "int8_ops"}
	return Index{
		Name:    "ix",
		Unique:  true,
		Method:  "btree",
		Where:   "score > 0",
		Include: []string{"title"},
		Columns: []IndexColumn{key},
	}, key
}

// axisCase is one difference from the baseline, named.
type axisCase struct {
	axis   string
	mutate func(Index) Index
}

// axisCases is the list, shared with the fixture precondition so that what is
// checked to be one axis apart is what the killers run.
func axisCases() []axisCase {
	return []axisCase{
		{"uniqueness", func(i Index) Index { i.Unique = false; return i }},
		{"method", func(i Index) Index { i.Method = "hash"; return i }},
		// The access methods are listed one at a time rather than left to the
		// hash case above. A comparison can be made to ignore one method
		// without ignoring the rest — a name special-cased, a default applied
		// too widely — and a single hash case would not notice. Each of these
		// is a method the project claims to support on a materialized view.
		{"gist method", func(i Index) Index { i.Method = "gist"; return i }},
		{"brin method", func(i Index) Index { i.Method = "brin"; return i }},
		{"gin method", func(i Index) Index { i.Method = "gin"; return i }},
		{"predicate", func(i Index) Index { i.Where = "score > 1"; return i }},
		{"INCLUDE", func(i Index) Index { i.Include = []string{"title", "body"}; return i }},
		{"key name", func(i Index) Index {
			i.Columns = []IndexColumn{{Name: "other", Direction: Asc, Nulls: NullsLast, OpClass: "int8_ops"}}
			return i
		}},
		{"direction", func(i Index) Index {
			i.Columns = []IndexColumn{{Name: "owner_id", Direction: Desc, Nulls: NullsLast, OpClass: "int8_ops"}}
			return i
		}},
		{"nulls ordering", func(i Index) Index {
			i.Columns = []IndexColumn{{Name: "owner_id", Direction: Asc, Nulls: NullsFirst, OpClass: "int8_ops"}}
			return i
		}},
		{"operator class", func(i Index) Index {
			i.Columns = []IndexColumn{{Name: "owner_id", Direction: Asc, Nulls: NullsLast, OpClass: "int8_pattern_ops"}}
			return i
		}},
		{"expression", func(i Index) Index {
			i.Columns = []IndexColumn{{Expression: "lower(title)", Direction: Asc, Nulls: NullsLast}}
			return i
		}},
		{"key count", func(i Index) Index {
			i.Columns = append(append([]IndexColumn{}, i.Columns...),
				IndexColumn{Name: "score", Direction: Asc, Nulls: NullsLast})
			return i
		}},
	}
}

func TestSameIndex_eachAxisAlone(t *testing.T) {
	base, _ := axisBaseline()
	if !sameIndex(base, base) {
		t.Fatal("an index does not compare equal to itself")
	}

	for _, c := range axisCases() {
		t.Run(c.axis, func(t *testing.T) {
			other := c.mutate(base)
			if sameIndex(base, other) {
				t.Errorf("two indexes differing only in %s compared equal, so that difference "+
					"would never be reported as drift", c.axis)
			}
		})
	}
}

// Key order is its own axis, and the single-key baseline above cannot reach it.
//
// An index over (a, b) is a different index from one over (b, a): the first can
// answer a query filtering on a alone and the second cannot. Comparing the key
// list as a set — sorting it, or counting names present — would call them equal,
// and no case above would notice, because with one key there is no order to get
// wrong. Changing a key's name and changing two keys' order are different
// defects, so they get different tests.
func TestSameIndex_keyOrderIsCompared(t *testing.T) {
	key := func(name string) IndexColumn {
		return IndexColumn{Name: name, Direction: Asc, Nulls: NullsLast}
	}
	forward := Index{Name: "ix", Columns: []IndexColumn{key("owner_id"), key("score")}}
	swapped := Index{Name: "ix", Columns: []IndexColumn{key("score"), key("owner_id")}}

	if !sameIndex(forward, forward) {
		t.Fatal("a two-key index does not compare equal to itself")
	}
	if sameIndex(forward, swapped) {
		t.Error("(owner_id, score) and (score, owner_id) compared equal. They are different " +
			"indexes — only the first can serve a query filtering on owner_id alone — so " +
			"reordering keys would never be reported as drift")
	}
}

// And a different expression is a difference, not merely a present-vs-absent one.
func TestSameIndex_expressionsAreCompared(t *testing.T) {
	lower := Index{Name: "ix", Columns: []IndexColumn{{Expression: "lower(title)"}}}
	upper := Index{Name: "ix", Columns: []IndexColumn{{Expression: "upper(title)"}}}
	if sameIndex(lower, upper) {
		t.Error("lower(title) and upper(title) compared equal")
	}
}

// TestMutationFixturePrecondition asserts that each case really is one axis
// away from the baseline.
//
// The cases are built by hand, so a case can drift into changing nothing — a
// field set to the value the baseline already held — or into changing two
// things, which is the defect this whole file exists to prevent. Neither is
// visible by reading, because the baseline is elsewhere.
//
// So the difference is counted rather than trusted. The slots are the fields
// sameIndex actually compares, with the method normalised the way it normalises
// it, so a case setting btree against an unset baseline would count as no
// difference at all rather than as an axis.
func TestMutationFixturePrecondition(t *testing.T) {
	// The one case that legitimately moves more than one slot, and why.
	//
	// An expression key has no name and no operator class, so a plain column
	// becoming an expression moves three at once. The one-axis version of that
	// question is TestSameIndex_expressionsAreCompared, which compares two
	// different expressions and so moves only the expression.
	expected := map[string]int{"expression": 3}

	t.Run("axes", func(t *testing.T) {
		base, _ := axisBaseline()
		for _, c := range axisCases() {
			other := c.mutate(base)
			got := axisSlotsDiffering(base, other)
			want := expected[c.axis]
			if want == 0 {
				want = 1
			}
			if len(got) != want {
				t.Errorf("the %s case differs from the baseline in %d slot(s) %v, want %d. "+
					"A case differing in none would pass whether or not the comparison "+
					"existed; one differing in more would still fail after the axis it "+
					"names had been removed", c.axis, len(got), got, want)
			}
		}
	})
}

// axisSlotsDiffering names the fields sameIndex compares that differ between two
// indexes.
func axisSlotsDiffering(a, b Index) []string {
	var out []string
	note := func(cond bool, name string) {
		if cond {
			out = append(out, name)
		}
	}
	note(a.Unique != b.Unique, "unique")
	note(method(a.Method) != method(b.Method), "method")
	note(!sameExpr(a.Where, b.Where), "where")
	note(!slices.Equal(a.Include, b.Include), "include")
	note(len(a.Columns) != len(b.Columns), "key count")
	for i := range a.Columns {
		if i >= len(b.Columns) {
			break
		}
		x, y := a.Columns[i], b.Columns[i]
		note(x.Name != y.Name, fmt.Sprintf("key[%d].name", i))
		note(x.Direction != y.Direction, fmt.Sprintf("key[%d].direction", i))
		note(x.Nulls != y.Nulls, fmt.Sprintf("key[%d].nulls", i))
		note(x.OpClass != y.OpClass, fmt.Sprintf("key[%d].opclass", i))
		note(!sameExpr(x.Expression, y.Expression), fmt.Sprintf("key[%d].expression", i))
	}
	return out
}
