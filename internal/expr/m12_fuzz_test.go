package expr_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// Fuzzing the M12 operator nodes.
//
// Infix and Prefix carry their operator spelling rather than an enumeration the
// compiler knows, which is what makes them the two worth fuzzing: a JSON key, a
// network value or a search string reaches them as an argument, and the
// property that matters is that the argument never reaches the text.

// A key, however hostile, is an argument — so the statement's shape does not
// depend on it.
func FuzzJSONKeyIsData(f *testing.F) {
	for _, seed := range []string{
		"", "x", "a'b", `c"d`, `e\f`, "ü", "x,y", "{z}", "$1",
		"'; DROP TABLE users; --", strings.Repeat("k", 300),
	} {
		f.Add(seed)
	}
	src := expr.NewSource("public", "t")
	shape := ""

	f.Fuzz(func(t *testing.T, key string) {
		if strings.ContainsRune(key, 0) {
			return
		}
		sel := &expr.Select{
			From: src,
			Items: []expr.SelectItem{{
				Node: expr.Infix{
					Op:    "->>",
					Left:  expr.Column{Source: src, Name: "doc"},
					Right: expr.Arg{Value: key},
				},
				Alias: "v", Nullable: true,
			}},
		}
		sql, args, err := sel.Compile()
		if err != nil {
			t.Fatalf("compiling with key %q: %v", key, err)
		}
		if len(args) != 1 || args[0] != key {
			t.Fatalf("key %q did not travel as the argument: %v", key, args)
		}
		// Every statement is the same statement: the key is nowhere in it.
		if shape == "" {
			shape = sql
		} else if sql != shape {
			t.Fatalf("key %q changed the statement:\n%s\n%s", key, shape, sql)
		}
		// A substring check would be wrong rather than strict: a key of "$1"
		// appears in every statement because the placeholder does. Shape
		// invariance is the property that actually distinguishes a bound value
		// from a spliced one, and it is checked above.
	})
}

// The two new nodes keep every child's source dependency, whatever they are
// nested inside. A compiler that dropped one would let a column of an
// unattached source through.
func FuzzOperatorNodesKeepDependencies(f *testing.F) {
	for _, seed := range []uint8{0, 1, 2, 3, 4, 5, 6, 7} {
		f.Add(seed)
	}
	inScope := expr.NewSource("public", "a")
	stranger := expr.NewSource("public", "b")

	f.Fuzz(func(t *testing.T, shape uint8) {
		bad := expr.Column{Source: stranger, Name: "x"}
		good := expr.Column{Source: inScope, Name: "x"}

		var node expr.Node
		switch shape % 8 {
		case 0:
			node = expr.Infix{Op: "@>", Left: good, Right: bad}
		case 1:
			node = expr.Infix{Op: "@>", Left: bad, Right: good}
		case 2:
			node = expr.Prefix{Op: "!!", X: bad}
		case 3:
			node = expr.Infix{Op: "->", Left: expr.Prefix{Op: "!!", X: bad}, Right: good}
		case 4:
			node = expr.Call{Func: "coalesce", Args: []expr.Node{
				expr.Infix{Op: "&&", Left: good, Right: bad}, good,
			}}
		case 5:
			node = expr.Case{
				When: []expr.CaseBranch{{Cond: expr.Bool{Value: true}, Then: expr.Prefix{Op: "!!", X: bad}}},
				Else: good,
			}
		case 6:
			node = expr.Cast{X: expr.Infix{Op: "->>", Left: bad, Right: expr.Arg{Value: "k"}}, Type: "text"}
		default:
			node = expr.Infix{Op: "<<", Left: expr.Cast{X: bad, Type: "inet"}, Right: good}
		}

		sel := &expr.Select{
			From:  inScope,
			Items: []expr.SelectItem{{Node: node, Alias: "v", Nullable: true}},
		}
		if _, _, err := sel.Compile(); err == nil {
			t.Fatalf("shape %d hid a dependency on a source the statement does not select from", shape%8)
		}
	})
}
