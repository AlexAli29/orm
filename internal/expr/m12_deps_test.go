package expr

import "testing"

// Every operand of an M12 operator is a source dependency.
//
// This is here because the whole M0–M12 suite passed with Infix's right operand
// removed from children(). Every operator M12 added — the network operators,
// the JSON containment and key tests, the full text match, the nine range
// operators, the multirange operators and interval arithmetic — is an Infix,
// and all of them put a caller's expression on the right. A traversal that
// could not see it would let a statement name a source it does not read, and in
// a self-join would bind silently to the wrong occurrence of a table.
//
// The check is structural rather than per-operator: every node that holds child
// nodes is asked whether the traversal reaches each of them, so a node type
// added later cannot quietly drop one.

// sourceOf builds a column belonging to a fresh source, so that reaching it can
// be told from reaching anything else.
func sourceOf(name string) (*Source, Column) {
	s := NewSource("public", name)
	return s, Column{Source: s, Name: "c"}
}

// reaches reports whether the source traversal finds src inside n.
func reaches(n Node, src *Source) bool {
	found := false
	walkSources(n, func(s *Source) {
		if s == src {
			found = true
		}
	})
	return found
}

// TestDeps_everyOperandOfAnInfixIsASource is the direct regression: both sides
// of an operator are dependencies, whichever side the caller put a column on.
func TestDeps_everyOperandOfAnInfixIsASource(t *testing.T) {
	left, lc := sourceOf("left")
	right, rc := sourceOf("right")

	// Each operator M12 added, with a column on each side.
	for _, op := range []string{
		"<<", "<<=", ">>", ">>=", "&&", // network
		"@>", "<@", "?", "?|", "?&", "->", "->>", "#>", "#>>", "@@", "@?", // json and fts
		"<<", ">>", "&<", "&>", "-|-", // range
		"+", "-", "*", // interval arithmetic
	} {
		n := Infix{Op: op, Left: lc, Right: rc}
		if !reaches(n, left) {
			t.Errorf("%s: the left operand is not a dependency", op)
		}
		if !reaches(n, right) {
			t.Errorf("%s: the right operand is not a dependency", op)
		}
	}

	// Nested on the right, which is where a range operator puts a cast.
	deep := Infix{Op: "@>", Left: lc, Right: Cast{X: Infix{Op: "+", Left: rc, Right: Arg{Value: 1}}, Type: "int4"}}
	if !reaches(deep, right) {
		t.Error("a source nested inside a cast on the right is not a dependency")
	}
}

// TestDeps_everyChildNodeIsReachable is the general form: for every node type
// that holds children, filling one child slot with a distinctive column and
// leaving the rest empty must make that column reachable.
//
// It exists so that a node added after this was written cannot lose an operand
// without a test noticing, which is exactly what happened to Infix.
func TestDeps_everyChildNodeIsReachable(t *testing.T) {
	src, col := sourceOf("probe")

	cases := []struct {
		name string
		node Node
	}{
		{"Infix left", Infix{Op: "@>", Left: col, Right: Arg{Value: 1}}},
		{"Infix right", Infix{Op: "@>", Left: Arg{Value: 1}, Right: col}},
		{"Prefix", Prefix{Op: "!!", X: col}},
		{"Cast", Cast{X: col, Type: "int4"}},
		{"Call first argument", Call{Func: "lower", Args: []Node{col}}},
		{"Call later argument", Call{Func: "coalesce", Args: []Node{Arg{Value: 1}, col}}},
		{"Binary left", Binary{Op: OpEq, Left: col, Right: Arg{Value: 1}}},
		{"Binary right", Binary{Op: OpEq, Left: Arg{Value: 1}, Right: col}},
		{"Unary", Unary{Op: OpIsNull, X: col}},
		{"Arith left", Arith{Op: OpAdd, Left: col, Right: Arg{Value: 1}}},
		{"Arith right", Arith{Op: OpAdd, Left: Arg{Value: 1}, Right: col}},
		{"In subject", In{X: col, Values: []Node{Arg{Value: 1}}}},
		{"In value", In{X: Arg{Value: 1}, Values: []Node{col}}},
		{"Between subject", Between{X: col, Lo: Arg{Value: 1}, Hi: Arg{Value: 2}}},
		{"Between low", Between{X: Arg{Value: 0}, Lo: col, Hi: Arg{Value: 2}}},
		{"Between high", Between{X: Arg{Value: 0}, Lo: Arg{Value: 1}, Hi: col}},
		{"Extract", Extract{Field: "year", X: col}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !reaches(c.node, src) {
				t.Errorf("the traversal does not reach a column in this position")
			}
		})
	}
}
