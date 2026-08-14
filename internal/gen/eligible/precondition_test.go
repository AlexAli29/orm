package eligible_test

import (
	"slices"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/eligible"
)

// M16.5 G2 #40: the fixture precondition for order-independent selection.
//
// A chooser that returned whichever qualifying candidate it met first would be
// caught only by a fixture where the first-met and the lowest-named are
// different candidates. Given one qualifying candidate, or two supplied in an
// order that happens to agree with their names, encounter order and the stable
// rule give the same answer and the mutation is invisible.

// orderCandidates is the fixture #40 attacks: two qualifying candidates
// declared so that the first met is not the lowest named, plus non-qualifying
// ones so that filtering is exercised alongside selection.
func orderCandidates() []eligible.Candidate {
	return []eligible.Candidate{
		{Name: "zzz_key", Unique: true, Columns: 1},
		{Name: "plain_idx", Columns: 1},
		{Name: "partial_key", Unique: true, Partial: true, Columns: 1},
		{Name: "aaa_key", Unique: true, Columns: 1},
		{Name: "expr_key", Unique: true, Expression: true, Columns: 1},
	}
}

func TestMutationFixturePrecondition(t *testing.T) {
	t.Run("c40", func(t *testing.T) {
		forward := orderCandidates()
		reversed := slices.Clone(forward)
		slices.Reverse(reversed)

		var qualifying []string
		for _, c := range forward {
			if c.Qualifies() {
				qualifying = append(qualifying, c.Name)
			}
		}
		if len(qualifying) < 2 {
			t.Fatalf("the fixture has %d qualifying candidate(s): %v. With fewer than two "+
				"there is nothing for an order-dependent chooser to choose between",
				len(qualifying), qualifying)
		}
		if slices.Equal(namesOf(forward), namesOf(reversed)) {
			t.Fatal("reversing the candidates left the order unchanged")
		}
		// The first qualifying candidate met must not be the lowest named, or
		// picking the first and picking the lowest are the same answer.
		first := qualifying[0]
		lowest := slices.Min(qualifying)
		if first == lowest {
			t.Fatalf("the first qualifying candidate met (%s) is also the lowest named, so "+
				"a chooser following encounter order would return the correct answer "+
				"and the mutation could not be observed", first)
		}
		t.Logf("first met %s, lowest named %s, qualifying %v", first, lowest, qualifying)
	})
}

func namesOf(cs []eligible.Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// Selection does not depend on the order the candidates arrive in.
func TestChoose_isOrderIndependent(t *testing.T) {
	forward := orderCandidates()
	reversed := slices.Clone(forward)
	slices.Reverse(reversed)

	a, b := eligible.Choose(forward), eligible.Choose(reversed)
	if a != b {
		t.Errorf("Choose returned %q forward and %q reversed: selection follows the order "+
			"the candidates were supplied in", a, b)
	}
	if a != "aaa_key" {
		t.Errorf("Choose returned %q; the rule is the lowest qualifying name, and the "+
			"fixture supplies zzz_key first to prove the rule is being applied", a)
	}
}
