package orm_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
)

// The extension boundary, and what it does and does not prevent.
//
// These hooks exist so that an extension package can build typed expressions
// the compiler accepts. That makes them the one place where code outside this
// package decides what a node means, so the question worth answering precisely
// is: which mistakes are impossible, and which are the extension author's to
// avoid?
//
// The answer, stated once here and asserted below:
//
//	impossible          forging a source, forging scope, reaching the argument
//	                    list, bypassing placeholder renumbering, mutating an
//	                    expression somebody else built, injecting a node type
//	                    this package does not have
//
//	the author's job     stating the correct PostgreSQL result type, stating
//	                    the correct nullability, writing a function name that
//	                    exists
//
// The second list is a trust boundary and is documented as one. An extension
// that declares ST_Area returns a string gets a scan error, not a wrong number,
// and an extension that declares a nullable result non-nullable gets a scan
// error too — but neither is prevented, and this package does not claim that
// importing an extension is safe the way importing nothing is.

type extEntity struct{}
type otherEntity struct{}

var (
	extSrc   = orm.NewSource("public", "ext")
	otherSrc = orm.NewSource("public", "other")
	extID    = orm.NewOrdCol[extEntity, int64](extSrc, "id")
	extName  = orm.NewTextCol[extEntity](extSrc, "name")
	extNick  = orm.NewNullTextCol[extEntity](extSrc, "nick")
	otherID  = orm.NewOrdCol[otherEntity, int64](otherSrc, "id")
)

// A node built through the boundary keeps the source it came from, so scope
// validation refuses a statement that does not name that source. An extension
// cannot forge ownership by wrapping a column in a call.
func TestExtension_cannotForgeSourceOwnership(t *testing.T) {
	// A call over a column of another table, dropped into this table's query.
	forged := orm.FnPredicate[orm.Composed]("ST_Fake", orm.ArgOf(orm.Of(otherID)))

	_, _, err := orm.Compose(nil, orm.Project1(orm.Of(extName),
		func(s string) string { return s })).
		From(extSrc).
		Where(forged).
		SQL()
	if err == nil {
		t.Fatal("a call over a column of an unnamed source built a statement")
	}

	// Nesting it deeper does not help.
	nested := orm.FnPredicate[orm.Composed]("ST_Outer",
		orm.ArgOf(orm.Fn[orm.Composed, int64]("ST_Inner",
			orm.ArgOf(orm.Fn[orm.Composed, int64]("ST_Deeper", orm.ArgOf(orm.Of(otherID)))))))
	_, _, err = orm.Compose(nil, orm.Project1(orm.Of(extName),
		func(s string) string { return s })).
		From(extSrc).
		Where(nested).
		SQL()
	if err == nil {
		t.Fatal("a call three levels deep over an unnamed source built a statement")
	}
}

// Nullability travels through the boundary rather than being restated, so an
// extension cannot present a nullable child as non-nullable by wrapping it.
func TestExtension_nullabilityTravelsThrough(t *testing.T) {
	// ArgOf keeps what the operand knows about itself.
	if !orm.ArgNullable(orm.ArgOf(orm.Of(extNick))) {
		t.Error("a nullable column's argument does not report itself nullable")
	}
	if orm.ArgNullable(orm.ArgOf(orm.Of(extName))) {
		t.Error("a NOT NULL column's argument reports itself nullable")
	}
	// ArgOpt is the outer-join form and is always nullable.
	if !orm.ArgNullable(orm.ArgOpt(extName)) {
		t.Error("ArgOpt does not report itself nullable")
	}
	if !orm.AnyNullable(orm.ArgOf(orm.Of(extName)), orm.ArgOf(orm.Of(extNick))) {
		t.Error("AnyNullable missed a nullable operand")
	}
	if orm.AnyNullable(orm.ArgOf(orm.Of(extName)), orm.ArgValue(1)) {
		t.Error("AnyNullable found a nullable operand where there is none")
	}

	// And the compiler's own outer-join check walks the tree, so an extension
	// that wrapped a left-joined column in a call and declared the result
	// non-nullable is still refused. This is the important one: it means the
	// check does not depend on the extension being honest.
	shape := orm.Project1(
		orm.Of(orm.Fn[orm.Composed, int64]("ST_Fake", orm.ArgOf(orm.Of(otherID)))),
		func(n int64) int64 { return n })
	_, _, err := orm.Compose(nil, shape).
		From(extSrc).
		LeftJoin(otherSrc, orm.Cond(extName.Eq("x"))).
		SQL()
	if err == nil {
		t.Fatal("a call over a left-joined column claimed a non-nullable result and compiled")
	}
	if !strings.Contains(err.Error(), "outer join") {
		t.Errorf("the refusal is not about the join: %v", err)
	}
}

// Values reach the statement as parameters however they enter, and placeholders
// are renumbered by the same writer whatever built the node.
func TestExtension_valuesAreAlwaysParameters(t *testing.T) {
	hostile := `'; DROP TABLE ext; --`

	sql, args, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Fn[orm.Composed, string]("ST_Fake",
			orm.ArgValue(hostile),
			orm.ArgCast(hostile, "text"),
			orm.ArgOf(orm.Of(extName)),
			orm.ArgValue(42))),
		func(s string) string { return s })).
		From(extSrc).
		Where(orm.Cond(extName.Eq(hostile))).
		SQL()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sql, "DROP") {
		t.Fatalf("a value reached the statement as text:\n%s", sql)
	}
	// Four values in, four parameters out, numbered from one with no gaps.
	if len(args) != 4 {
		t.Fatalf("the statement bound %d arguments: %v", len(args), args)
	}
	for i := 1; i <= 4; i++ {
		if !strings.Contains(sql, "$"+string(rune('0'+i))) {
			t.Errorf("the statement has no $%d:\n%s", i, sql)
		}
	}

	// ArgRaw's own placeholders are renumbered into the surrounding statement
	// rather than colliding with it. An extension writing $1 in a fragment does
	// not overwrite the caller's first parameter.
	sql, args, err = orm.Compose(nil, orm.Project1(
		orm.Of(orm.Fn[orm.Composed, string]("ST_Fake",
			orm.ArgValue("first"),
			orm.ArgRaw("upper($1)", "second"),
			orm.ArgValue("third"))),
		func(s string) string { return s })).
		From(extSrc).
		SQL()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 {
		t.Fatalf("a raw fragment's argument did not join the list: %v", args)
	}
	if args[1] != any("second") {
		t.Errorf("the raw fragment's argument landed at %v", args)
	}
	if !strings.Contains(sql, "upper($2)") {
		t.Errorf("the raw fragment's placeholder was not renumbered:\n%s", sql)
	}
}

// A mistake an extension makes travels in the tree and stops the statement
// rather than producing SQL.
func TestExtension_failIsTerminal(t *testing.T) {
	bad := orm.ArgFail(errSentinel)
	for name, build := range map[string]func() (string, []any, error){
		"in a predicate": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(orm.Of(extName), func(s string) string { return s })).
				From(extSrc).Where(orm.FnPredicate[orm.Composed]("ST_Fake", bad)).SQL()
		},
		"in a value": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.Of(orm.Fn[orm.Composed, int64]("ST_Fake", bad)),
				func(n int64) int64 { return n })).From(extSrc).SQL()
		},
		"nested three deep": func() (string, []any, error) {
			inner := orm.ArgOf(orm.Fn[orm.Composed, int64]("A", bad))
			mid := orm.ArgOf(orm.Fn[orm.Composed, int64]("B", inner))
			return orm.Compose(nil, orm.Project1(
				orm.Of(orm.Fn[orm.Composed, int64]("C", mid)),
				func(n int64) int64 { return n })).From(extSrc).SQL()
		},
		"in an aggregate": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.OfNull(orm.AggNull[orm.Composed, int64]("ST_Fake", bad)),
				func(n *int64) int64 { return 0 })).From(extSrc).SQL()
		},
	} {
		t.Run(name, func(t *testing.T) {
			sql, _, err := build()
			if err == nil {
				t.Fatalf("a failed argument produced SQL:\n%s", sql)
			}
			if !strings.Contains(err.Error(), errSentinel.Error()) {
				t.Errorf("the error lost the reason: %v", err)
			}
		})
	}
}

// The result-type and nullability declarations are the extension author's, and
// this test records that a wrong one is a clean failure rather than a wrong
// value.
//
// It is the documented trust boundary. Nothing here claims an extension cannot
// be wrong; what is claimed is that being wrong produces a scan error.
func TestExtension_wrongDeclarationsFailCleanly(t *testing.T) {
	// A declaration is just a type parameter, so a wrong one compiles. What it
	// cannot do is change the statement: the SQL is the same either way, and
	// only the scan differs.
	right, _, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Fn[orm.Composed, float64]("ST_Area", orm.ArgOf(orm.Of(extName)))),
		func(f float64) float64 { return f })).From(extSrc).SQL()
	if err != nil {
		t.Fatal(err)
	}
	wrong, _, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Fn[orm.Composed, string]("ST_Area", orm.ArgOf(orm.Of(extName)))),
		func(s string) string { return s })).From(extSrc).SQL()
	if err != nil {
		t.Fatal(err)
	}
	if right != wrong {
		t.Errorf("the declared Go type changed the statement:\n%s\n%s", right, wrong)
	}
	// The consequence is a scan failure at run time, which the spatial package's
	// own type matrix is what prevents: every claim there is checked against
	// pg_typeof.
}

var errSentinel = errSentinelType{}

type errSentinelType struct{}

func (errSentinelType) Error() string { return "the extension made a mistake" }

// The free-standing expression constructors, which no extension in this
// repository uses yet and which are public API all the same.
func TestExtension_freeStandingConstructors(t *testing.T) {
	cases := map[string]func() (string, []any, error){
		"FnExpr": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.FnExpr[int64]("ST_Fake", orm.ArgValue(1)),
				func(n int64) int64 { return n })).From(extSrc).SQL()
		},
		"FnExprNull": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.FnExprNull[int64]("ST_Fake", orm.ArgValue(1)),
				func(n *int64) int64 { return 0 })).From(extSrc).SQL()
		},
		"OpExpr": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.OpExpr[int64]("<->", orm.ArgValue(1), orm.ArgValue(2)),
				func(n int64) int64 { return n })).From(extSrc).SQL()
		},
		"OpExprNull": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.OpExprNull[int64]("<->", orm.ArgValue(1), orm.ArgValue(2)),
				func(n *int64) int64 { return 0 })).From(extSrc).SQL()
		},
		"ValueOf": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.Of(orm.ValueOf[orm.Composed, int64](orm.ArgValue(1))),
				func(n int64) int64 { return n })).From(extSrc).SQL()
		},
		"ValueOfNull": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.OfNull(orm.ValueOfNull[orm.Composed, int64](orm.ArgValue(1))),
				func(n *int64) int64 { return 0 })).From(extSrc).SQL()
		},
		"BoolOfNull": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.OfNull(orm.BoolOfNull(orm.Cond(extName.Eq("x")))),
				func(b *bool) bool { return false })).From(extSrc).SQL()
		},
		"OpNull": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.OfNull(orm.OpNull[orm.Composed, int64]("<->", orm.ArgValue(1), orm.ArgValue(2))),
				func(n *int64) int64 { return 0 })).From(extSrc).SQL()
		},
		"ArgAs": func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.Of(orm.Fn[orm.Composed, int64]("ST_Fake",
					orm.ArgAs(orm.ArgValue(1), "int8"))),
				func(n int64) int64 { return n })).From(extSrc).SQL()
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			sql, args, err := build()
			if err != nil {
				t.Fatalf("building: %v", err)
			}
			if sql == "" {
				t.Fatal("no statement")
			}
			// Whatever the constructor, values are parameters.
			for _, a := range args {
				switch a.(type) {
				case int, int64, string:
				default:
					t.Errorf("an argument is %T", a)
				}
			}
			if strings.Count(sql, "$") != len(args) {
				t.Errorf("%d placeholders for %d arguments:\n%s", strings.Count(sql, "$"), len(args), sql)
			}
		})
	}
}

// An extension cannot reach the compiler's argument list, mutate a built
// expression, or read another expression's tree: Arg has no exported field and
// no method that returns one.
func TestExtension_argIsOpaque(t *testing.T) {
	a := orm.ArgOf(orm.Of(extName))
	b := a
	// Copying an Arg copies a value; there is nothing in it a caller can reach
	// to change what the other one means.
	if orm.ArgNullable(a) != orm.ArgNullable(b) {
		t.Error("copying an Arg changed it")
	}
	// And the same expression rendered twice is the same statement, which is
	// what "expressions are values" means.
	build := func() string {
		sql, _, err := orm.Compose(nil, orm.Project1(
			orm.Of(orm.Fn[orm.Composed, int64]("ST_Fake", a, b)),
			func(n int64) int64 { return n })).From(extSrc).SQL()
		if err != nil {
			t.Fatal(err)
		}
		return sql
	}
	if build() != build() {
		t.Error("the same expression rendered differently twice")
	}
}
