package expr_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// Fuzzing the nested compiler.
//
// Placeholder scanning reads text; everything else in this package reads a tree
// this package built, so the fuzzing worth doing is over tree *shapes* rather
// than over strings. A random byte string is turned into a bounded composition
// of subqueries, joins, WITH items and expressions, and four properties are
// asserted of whatever comes out:
//
//	it does not panic
//	compiling twice produces the same SQL, arguments and error
//	the placeholders run 1..n with no gaps and no repeats
//	a scope mistake is refused rather than compiled
//
// The last is the one worth stating plainly. The validator must not accept a
// source because its alias matches another source's: identity is the pointer,
// and a generator that hands out two sources with one name has to be refused.

func FuzzCompose(f *testing.F) {
	for _, seed := range [][]byte{
		{},
		{0},
		{1, 2, 3},
		{7, 3, 9, 1, 4},
		{255, 254, 253, 252},
		{5, 5, 5, 5, 5, 5, 5, 5},
		{1, 0, 2, 0, 3, 0, 4, 0, 5, 0},
		{9, 9, 9, 1, 1, 1, 7, 7, 7, 3, 3, 3},
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, script []byte) {
		g := &gen{script: script}
		sel := g.selectStmt(0)

		sql, args, err := sel.Compile()
		again, argsAgain, errAgain := sel.Compile()

		// Determinism. Two compilations of one tree agree about everything,
		// including about failing.
		switch {
		case (err == nil) != (errAgain == nil):
			t.Fatalf("compiling twice disagreed about failing: %v then %v", err, errAgain)
		case err != nil && err.Error() != errAgain.Error():
			t.Fatalf("two failures differ:\n%v\n%v", err, errAgain)
		case err != nil:
			// A refusal is a legitimate outcome. What matters is that it is a
			// refusal rather than a crash, and that it is the same one twice.
			return
		case sql != again:
			t.Fatalf("two compilations differ:\n%s\n%s", sql, again)
		case len(args) != len(argsAgain):
			t.Fatalf("two compilations bound %d and %d arguments", len(args), len(argsAgain))
		}

		// The placeholders are 1..n, each written exactly once. A nested
		// statement compiled separately and spliced in would break this.
		for i := 1; i <= len(args); i++ {
			if n := countPlaceholder(sql, i); n != 1 {
				t.Fatalf("$%d appears %d times in\n%s", i, n, sql)
			}
		}
		if n := countPlaceholder(sql, len(args)+1); n != 0 {
			t.Fatalf("the statement writes $%d and binds %d arguments:\n%s", len(args)+1, len(args), sql)
		}
	})
}

// A source that merely shares a name with one in scope is not that source, and
// the validator has to say so. Identity is the pointer.
func FuzzScopeIdentity(f *testing.F) {
	f.Add("users", "users")
	f.Add("users", "posts")
	f.Add("a", "a")

	f.Fuzz(func(t *testing.T, inName, outName string) {
		if inName == "" || outName == "" ||
			strings.ContainsRune(inName, 0) || strings.ContainsRune(outName, 0) {
			return
		}
		inScope := expr.NewSource("public", inName)
		stranger := expr.NewSource("public", outName)

		sel := &expr.Select{
			From:    inScope,
			Columns: []expr.Column{{Source: inScope, Name: "id"}},
			Where: expr.Binary{
				Op:    expr.OpEq,
				Left:  expr.Column{Source: stranger, Name: "id"},
				Right: expr.Arg{Value: 1},
			},
		}
		if _, _, err := sel.Compile(); err == nil {
			t.Fatalf("a column of a source the statement does not select from compiled; the two are named %q and %q",
				inName, outName)
		}
	})
}

// gen turns a byte script into a bounded tree. It never fails and never
// allocates without bound: every choice is taken modulo a small number, and the
// depth is capped well inside the compiler's own limit.
type gen struct {
	script []byte
	pos    int
	tables int
}

// next returns the next choice, cycling the script rather than running out so
// that a short input still produces a whole tree.
func (g *gen) next(mod int) int {
	if len(g.script) == 0 || mod <= 0 {
		return 0
	}
	b := g.script[g.pos%len(g.script)]
	g.pos++
	return int(b) % mod
}

func (g *gen) source() *expr.Source {
	g.tables++
	return expr.NewSource("public", "t").Reserved("_t" + string(rune('a'+g.tables%26)) + string(rune('a'+g.tables/26%26)))
}

func (g *gen) selectStmt(depth int) *expr.Select {
	src := g.source()
	sel := &expr.Select{
		From:  src,
		Items: []expr.SelectItem{{Node: expr.Column{Source: src, Name: "id"}, Alias: "id"}},
	}
	if depth < 3 {
		for range g.next(3) {
			sel.Joins = append(sel.Joins, g.join(src, depth))
		}
		for range g.next(2) {
			sel.With = append(sel.With, g.cte(depth))
		}
	}
	sel.Where = g.node(src, depth)
	if g.next(2) == 0 {
		sel.GroupBy = []expr.Node{expr.Column{Source: src, Name: "id"}}
	}
	if g.next(3) == 0 {
		sel.OrderBy = []expr.Order{{Column: expr.Column{Source: src, Name: "id"}}}
	}
	return sel
}

func (g *gen) join(left *expr.Source, depth int) expr.Join {
	kinds := []expr.JoinKind{expr.JoinInner, expr.JoinLeft, expr.JoinRight, expr.JoinFull, expr.JoinCross}
	kind := kinds[g.next(len(kinds))]

	var src *expr.Source
	lateral := false
	if depth < 2 && g.next(3) == 0 {
		inner := g.selectStmt(depth + 1)
		lateral = g.next(2) == 0
		if lateral {
			// A lateral item may name what came before it, which is the case
			// worth generating: without it the correlation is a mistake the
			// compiler has to catch.
			inner.Where = expr.Binary{
				Op:    expr.OpEq,
				Left:  expr.Column{Source: inner.From, Name: "id"},
				Right: expr.Column{Source: left, Name: "id"},
			}
		}
		src = expr.NewDerived("_d"+string(rune('a'+g.tables%26)), inner, []string{"id"})
		g.tables++
	} else {
		src = g.source()
	}

	j := expr.Join{Kind: kind, Source: src, Lateral: lateral}
	if kind != expr.JoinCross {
		j.On = expr.Binary{
			Op:    expr.OpEq,
			Left:  expr.Column{Source: src, Name: "id"},
			Right: expr.Column{Source: left, Name: "id"},
		}
	}
	return j
}

func (g *gen) cte(depth int) *expr.Source {
	body := g.selectStmt(depth + 1)
	g.tables++
	return expr.NewCTE("_c"+string(rune('a'+g.tables%26)), body, []string{"id"})
}

// node builds a bounded expression over the source, mixing in the node kinds
// M11 added so that the walkers and the writer see all of them.
func (g *gen) node(src *expr.Source, depth int) expr.Node {
	col := expr.Column{Source: src, Name: "id"}
	// The script cycles, so a value that recurses would recurse forever
	// without a cap. The cap is the generator's, not the compiler's: what is
	// being fuzzed is bounded compositions, and an unbounded one says nothing
	// about them.
	if depth > 4 {
		return expr.Binary{Op: expr.OpEq, Left: col, Right: expr.Arg{Value: 1}}
	}
	switch g.next(8) {
	case 0:
		return expr.Binary{Op: expr.OpEq, Left: col, Right: expr.Arg{Value: g.next(100)}}
	case 1:
		return expr.Group{Op: expr.OpAnd, Items: []expr.Node{
			g.node(src, depth+1), expr.Binary{Op: expr.OpGt, Left: col, Right: expr.Arg{Value: 1}},
		}}
	case 2:
		return expr.Case{
			When: []expr.CaseBranch{{Cond: expr.Bool{Value: true}, Then: expr.Arg{Value: 1}}},
			Else: expr.Arg{Value: 2},
		}
	case 3:
		return expr.Call{Func: "coalesce", Args: []expr.Node{col, expr.Arg{Value: 0}}}
	case 4:
		return expr.Cast{X: col, Type: "text"}
	case 5:
		if depth > 2 {
			return expr.Bool{Value: true}
		}
		return expr.Exists{Sub: g.selectStmt(depth + 1)}
	case 6:
		return expr.Aggregate{Func: "count", Star: true, Over: &expr.WindowSpec{
			PartitionBy: []expr.Node{col},
			OrderBy:     []expr.Order{{Column: col}},
			Frame: &expr.Frame{
				Mode:  expr.FrameRows,
				Start: expr.Bound{Kind: expr.UnboundedPreceding},
				End:   expr.Bound{Kind: expr.CurrentRow},
			},
		}}
	default:
		return expr.Infix{Op: "@>", Left: col, Right: expr.Arg{Value: 1}}
	}
}

// countPlaceholder counts occurrences of $n that are not the prefix of a longer
// number, so that $1 is not found inside $10.
func countPlaceholder(sql string, n int) int {
	want := "$" + itoa(n)
	count := 0
	for i := 0; i+len(want) <= len(sql); i++ {
		if sql[i:i+len(want)] != want {
			continue
		}
		if i+len(want) < len(sql) {
			if c := sql[i+len(want)]; c >= '0' && c <= '9' {
				continue
			}
		}
		count++
	}
	return count
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
