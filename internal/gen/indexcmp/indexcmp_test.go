package indexcmp_test

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/indexcmp"
)

// One axis at a time, against the rule itself.
//
// A case that changes two properties proves nothing about either: delete the
// comparison for one and the other still reports the difference, so the test
// passes and the defect survives while looking covered. That is how index
// method comparison went untested once already.
//
// So each case here differs from one baseline in exactly one field, and the
// baseline is written once. Removing any single comparison from Equal makes
// exactly one of these fail, which is what makes each one a valid killer rather
// than a test that happens to be red.

// baseline carries a non-default value on every axis, so that a case moving one
// of them really moves it. A baseline holding the value a case sets would give
// a pair that does not differ at all.
func baseline() indexcmp.Index {
	return indexcmp.Index{
		Unique:  true,
		Method:  "btree",
		Where:   "score > 0",
		Include: []string{"title", "body"},
		Keys: []indexcmp.Key{
			{Name: "owner_id", Direction: 1, Nulls: 2, OpClass: "int8_ops"},
			{Name: "score", Direction: 0, Nulls: 1, OpClass: "int8_ops"},
		},
	}
}

// with returns the baseline with one change applied.
func with(f func(*indexcmp.Index)) indexcmp.Index {
	out := baseline()
	out.Include = append([]string{}, out.Include...)
	out.Keys = append([]indexcmp.Key{}, out.Keys...)
	f(&out)
	return out
}

func TestEqual_anIndexIsItself(t *testing.T) {
	if !indexcmp.Equal(baseline(), baseline()) {
		t.Fatal("an index does not compare equal to itself, so every case below would " +
			"pass for the wrong reason")
	}
	// Rebuilt each time, so nothing about slice identity is contributing.
	a, b := baseline(), baseline()
	a.Include, b.Include = append([]string{}, a.Include...), append([]string{}, b.Include...)
	if !indexcmp.Equal(a, b) {
		t.Error("two separately built copies of one index are not equal")
	}
	// And the zero index equals itself, which is the shape a caller building an
	// index with no keys and no options hands over.
	if !indexcmp.Equal(indexcmp.Index{}, indexcmp.Index{}) {
		t.Error("the zero index does not equal itself")
	}
}

func TestEqual_eachAxisAlone(t *testing.T) {
	for _, c := range []struct {
		axis  string
		other indexcmp.Index
		why   string
	}{
		{
			"unique", with(func(i *indexcmp.Index) { i.Unique = false }),
			"a unique index and a plain one over the same keys enforce different things",
		},
		{
			"method", with(func(i *indexcmp.Index) { i.Method = "hash" }),
			"a btree and a hash index answer different queries and are rebuilt to change",
		},
		{
			// Every method the project supports on a materialized view, listed
			// separately: a comparison can be made to ignore one and not the
			// rest, and a single hash case would not notice.
			"method gist", with(func(i *indexcmp.Index) { i.Method = "gist" }), "",
		},
		{"method gin", with(func(i *indexcmp.Index) { i.Method = "gin" }), ""},
		{"method brin", with(func(i *indexcmp.Index) { i.Method = "brin" }), ""},
		{"method spgist", with(func(i *indexcmp.Index) { i.Method = "spgist" }), ""},
		{
			"predicate", with(func(i *indexcmp.Index) { i.Where = "score > 1" }),
			"a partial index covers only the rows its predicate selects",
		},
		{
			"predicate removed", with(func(i *indexcmp.Index) { i.Where = "" }),
			"an index over every row is not the same as one over some of them",
		},
		{
			"INCLUDE contents", with(func(i *indexcmp.Index) { i.Include = []string{"title", "note"} }),
			"a covering column decides which queries the index answers without a heap fetch",
		},
		{
			"INCLUDE count", with(func(i *indexcmp.Index) { i.Include = []string{"title"} }), "",
		},
		{
			"INCLUDE removed", with(func(i *indexcmp.Index) { i.Include = nil }), "",
		},
		{
			"INCLUDE order", with(func(i *indexcmp.Index) { i.Include = []string{"body", "title"} }),
			"the covering columns are stored in the order given",
		},
		{
			"key name", with(func(i *indexcmp.Index) { i.Keys[0].Name = "other" }), "",
		},
		{
			"key count", with(func(i *indexcmp.Index) {
				i.Keys = append(i.Keys, indexcmp.Key{Name: "extra"})
			}), "",
		},
		{
			"key order", with(func(i *indexcmp.Index) {
				i.Keys[0], i.Keys[1] = i.Keys[1], i.Keys[0]
			}),
			"(owner_id, score) serves a query filtering on owner_id alone and (score, owner_id) does not",
		},
		{
			"direction", with(func(i *indexcmp.Index) { i.Keys[0].Direction = 0 }), "",
		},
		{
			"nulls ordering", with(func(i *indexcmp.Index) { i.Keys[0].Nulls = 1 }), "",
		},
		{
			"operator class", with(func(i *indexcmp.Index) { i.Keys[0].OpClass = "int8_pattern_ops" }),
			"the operator class decides which operators the index can serve",
		},
		{
			"operator class removed", with(func(i *indexcmp.Index) { i.Keys[0].OpClass = "" }), "",
		},
		{
			"expression added", with(func(i *indexcmp.Index) {
				i.Keys[0] = indexcmp.Key{Expression: "lower(title)"}
			}), "",
		},
	} {
		t.Run(c.axis, func(t *testing.T) {
			if indexcmp.Equal(baseline(), c.other) {
				why := c.why
				if why == "" {
					why = "that difference would never be reported"
				}
				t.Errorf("two indexes differing only in %s compared equal: %s", c.axis, why)
			}
			// Symmetric, or which side a caller passed first would decide the
			// answer.
			if indexcmp.Equal(c.other, baseline()) {
				t.Errorf("%s is a difference in one direction only", c.axis)
			}
		})
	}
}

// A different expression is a difference, not merely a present-versus-absent
// one. The axis case above replaces a plain key with an expression, which also
// removes a name and an operator class; this moves only the expression.
func TestEqual_expressionsAreCompared(t *testing.T) {
	lower := indexcmp.Index{Keys: []indexcmp.Key{{Expression: "lower(title)"}}}
	upper := indexcmp.Index{Keys: []indexcmp.Key{{Expression: "upper(title)"}}}
	if indexcmp.Equal(lower, upper) {
		t.Error("lower(title) and upper(title) compared equal")
	}
	if !indexcmp.Equal(lower, lower) {
		t.Error("an expression index does not equal itself")
	}
}

// What the rule deliberately does not cover, recorded as the contract it is.
//
// These are not oversights. A caller pairs indexes by name before asking, so a
// name on one side only is a create or a drop rather than a change; and
// CONCURRENTLY is how an index was built, not what it is, so rebuilding one to
// change it would achieve nothing. Neither field exists on the canonical shape,
// and this test says so where a reader looking for them will be.
func TestEqual_whatIsNotPartOfIdentity(t *testing.T) {
	// The shape carries no name and no concurrently flag, so two indexes that
	// differ only in those are the same value here by construction. The
	// assertion is that the callers' fields did not sneak in: an index built
	// from the same identity fields is equal whatever its caller called it.
	a := baseline()
	b := baseline()
	b.Include = append([]string{}, a.Include...)
	b.Keys = append([]indexcmp.Key{}, a.Keys...)
	if !indexcmp.Equal(a, b) {
		t.Fatal("two indexes with identical identity fields are not equal")
	}
	// And an empty method is not resolved here: callers resolve the default
	// before comparing, so an unresolved one is genuinely a different value.
	// Asserting it keeps the responsibility where the adapter's test expects.
	unset := baseline()
	unset.Method = ""
	if indexcmp.Equal(baseline(), unset) {
		t.Error("Equal resolved a default access method. The default is the adapter's to " +
			"resolve, so that one place decides it; resolving it here as well would put " +
			"half the rule back in two places")
	}
}
