package eligible_test

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/eligible"
)

// The rule, stated once and tested once.
//
// Both index models pass through this, so a case proved here is proved for
// generation, for the fingerprint and for the runtime descriptor at the same
// time — which is the property the two independent copies did not have.
func TestChoose(t *testing.T) {
	plain := func(name string) eligible.Candidate {
		return eligible.Candidate{Name: name, Unique: true, Columns: 1}
	}
	for _, c := range []struct {
		what string
		in   []eligible.Candidate
		want string
	}{
		{"a plain unique index", []eligible.Candidate{plain("k")}, "k"},
		{"nothing at all", nil, ""},
		{"a non-unique index",
			[]eligible.Candidate{{Name: "k", Columns: 1}}, ""},
		{"a partial unique index, which covers only some rows",
			[]eligible.Candidate{{Name: "k", Unique: true, Partial: true, Columns: 1}}, ""},
		{"an expression unique index, which has no columns to match rows by",
			[]eligible.Candidate{{Name: "k", Unique: true, Expression: true, Columns: 1}}, ""},
		{"a unique index over no columns",
			[]eligible.Candidate{{Name: "k", Unique: true}}, ""},
		{"the lowest name among several",
			[]eligible.Candidate{plain("zzz"), plain("aaa"), plain("mmm")}, "aaa"},
		{"the same set in another order",
			[]eligible.Candidate{plain("mmm"), plain("aaa"), plain("zzz")}, "aaa"},
		{"a unique index with covering columns, which stay out of the key",
			[]eligible.Candidate{{Name: "k", Unique: true, Columns: 1}}, "k"},
		{"one qualifying among disqualified ones",
			[]eligible.Candidate{
				{Name: "aaa", Unique: true, Partial: true, Columns: 1},
				{Name: "bbb", Columns: 1},
				plain("zzz"),
			}, "zzz"},
	} {
		t.Run(c.what, func(t *testing.T) {
			if got := eligible.Choose(c.in); got != c.want {
				t.Errorf("Choose = %q, want %q", got, c.want)
			}
		})
	}
}

// The Unique field is defensive validation, not dead weight.
//
// The production adapter sources candidates from PGTable.Uniques, which is
// populated by a query carrying AND i.indisunique, so a non-unique index cannot
// reach it. The check here is the second layer: the canonical schema's adapter
// does pass every index through, and both layers together are what a mutation
// campaign has to defeat before a non-unique index can qualify.
func TestChoose_uniqueIsCheckedNotAssumed(t *testing.T) {
	if got := eligible.Choose([]eligible.Candidate{
		{Name: "k", Unique: false, Columns: 1},
	}); got != "" {
		t.Errorf("a non-unique candidate qualified as %q", got)
	}
}

// Choose does not rely on its caller for ordering.
//
// The introspection query already orders unique indexes by name, so in practice
// the input arrives sorted. That is input hygiene rather than the guarantee:
// this picks the lexical minimum whatever order it is given, so neither layer
// alone decides the answer.
func TestChoose_doesNotDependOnInputOrder(t *testing.T) {
	forward := []eligible.Candidate{
		{Name: "zzz", Unique: true, Columns: 1},
		{Name: "aaa", Unique: true, Columns: 1},
	}
	reversed := []eligible.Candidate{forward[1], forward[0]}
	if a, b := eligible.Choose(forward), eligible.Choose(reversed); a != b || a != "aaa" {
		t.Errorf("Choose = %q forward and %q reversed, want aaa both ways", a, b)
	}
}
