package expr

import (
	"strings"
	"testing"
)

// The compound writer, at the level it is written.
//
// These are the properties that cannot be checked anywhere else: what the SQL
// looks like, which branch a clause attaches to, and whether the second branch's
// parameters continue the first's. Everything above this package can only see
// the result of getting them right.

var orders = NewSource("public", "orders")

// branch builds a single-column SELECT over a source, with an optional WHERE.
func branch(src *Source, name string, where Node) *Select {
	return &Select{From: src, Columns: []Column{{Source: src, Name: name}}, Where: where}
}

func compoundOf(branches ...Subquery) *Compound {
	return &Compound{Op: SetUnionAll, Branches: branches}
}

func compile(t *testing.T, c *Compound) (string, []any) {
	t.Helper()
	sql, args, err := c.Compile()
	if err != nil {
		t.Fatalf("compiling: %v", err)
	}
	return sql, args
}

// The operator is UNION ALL and the branches are written in order.
func TestCompound_writesUnionAllInBranchOrder(t *testing.T) {
	sql, args := compile(t, compoundOf(
		branch(users, "id", nil),
		branch(orders, "id", nil),
	))

	want := `SELECT "users"."id" FROM "public"."users" UNION ALL ` +
		`SELECT "orders"."id" FROM "public"."orders"`
	if sql != want {
		t.Errorf("compiled\n\t%s\nwant\n\t%s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	// UNION, not UNION ALL, would be a different query: it would remove
	// duplicate rows, which is the one thing this operation promises not to do.
	if strings.Contains(sql, "UNION ") && !strings.Contains(sql, "UNION ALL ") {
		t.Error("the operator is UNION rather than UNION ALL")
	}
}

// Parameters are numbered across the whole compound, not per branch.
//
// Restarting them in the second branch produces SQL PostgreSQL accepts and
// binds the wrong values into, which is why this is the property the node's
// documentation leads with.
func TestCompound_placeholdersAreGlobal(t *testing.T) {
	left := branch(users, "id", Binary{
		Op: OpEq, Left: Column{Source: users, Name: "email"}, Right: Arg{Value: "a@example.com"},
	})
	right := branch(orders, "id", Binary{
		Op: OpEq, Left: Column{Source: orders, Name: "label"}, Right: Arg{Value: "b"},
	})

	sql, args := compile(t, compoundOf(left, right))

	if !strings.Contains(sql, "$1") || !strings.Contains(sql, "$2") {
		t.Errorf("the compound does not number both parameters:\n%s", sql)
	}
	if strings.Count(sql, "$1") != 1 {
		t.Errorf("$1 appears %d times; the two branches share a placeholder for two "+
			"different values:\n%s", strings.Count(sql, "$1"), sql)
	}
	if len(args) != 2 || args[0] != "a@example.com" || args[1] != "b" {
		t.Errorf("args = %v, want the left branch's value then the right's", args)
	}
	// The left branch's placeholder precedes the right's in the text, which is
	// what makes the argument order the caller's rather than the writer's.
	if strings.Index(sql, "$1") > strings.Index(sql, "$2") {
		t.Errorf("$2 is written before $1:\n%s", sql)
	}
}

// A branch carrying ORDER BY, LIMIT or OFFSET is parenthesised.
func TestCompound_branchLocalClausesAreParenthesised(t *testing.T) {
	two := 2
	left := branch(users, "id", nil)
	left.OrderBy = []Order{{Column: Column{Source: users, Name: "id"}}}
	left.Limit = &two

	sql, _ := compile(t, compoundOf(left, branch(orders, "id", nil)))

	if !strings.HasPrefix(sql, "(SELECT ") {
		t.Errorf("the branch with its own ORDER BY and LIMIT is not parenthesised, so "+
			"the clauses bind to the compound instead of to the branch:\n%s", sql)
	}
	if !strings.Contains(sql, `LIMIT 2) UNION ALL `) {
		t.Errorf("the parentheses do not close before the operator:\n%s", sql)
	}
}

// A branch with none of them is written bare.
func TestCompound_plainBranchesAreNotParenthesised(t *testing.T) {
	sql, _ := compile(t, compoundOf(branch(users, "id", nil), branch(orders, "id", nil)))
	if strings.Contains(sql, "(") {
		t.Errorf("a plain branch was parenthesised; the parentheses are for the "+
			"branches that need them:\n%s", sql)
	}
}

// The compound's own ORDER BY, LIMIT and OFFSET are written after every branch.
func TestCompound_compoundClausesFollowTheLastBranch(t *testing.T) {
	three, one := 3, 1
	c := compoundOf(branch(users, "id", nil), branch(orders, "id", nil))
	c.OrderBy = []OutputOrder{{Name: "id"}}
	c.Limit, c.Offset = &three, &one

	sql, _ := compile(t, c)

	want := ` ORDER BY "id" ASC LIMIT 3 OFFSET 1`
	if !strings.HasSuffix(sql, want) {
		t.Errorf("compiled\n\t%s\nwant it to end with\n\t%s", sql, want)
	}
	// The second branch must not have absorbed them.
	if strings.Contains(sql, `"public"."orders" ORDER BY`) &&
		!strings.Contains(sql, "UNION ALL SELECT") {
		t.Errorf("the compound's ORDER BY attached to the right branch:\n%s", sql)
	}
}

// A branch carrying its own WITH is parenthesised, because the bare form does
// not mean what it says.
//
// In the first branch PostgreSQL accepts it and declares the item for the whole
// compound — evaluated once for the operation and visible to every branch. In
// any later branch it is a syntax error. So the same omission is silent in one
// position and loud in the other, which is how it stood unnoticed.
func TestCompound_aBranchesWithClauseIsParenthesised(t *testing.T) {
	withItem := func(name string) *Select {
		item := NewCTE(name, branch(orders, "id", nil), []string{"id"})
		b := branch(users, "id", nil)
		b.With = []*Source{item}
		return b
	}

	sql, _ := compile(t, compoundOf(withItem("a"), branch(users, "id", nil)))
	if !strings.HasPrefix(sql, `(WITH "a" AS `) {
		t.Errorf("the first branch's WITH was not kept inside it:\n%s", sql)
	}

	sql, _ = compile(t, compoundOf(branch(users, "id", nil), withItem("b")))
	if !strings.Contains(sql, `UNION ALL (WITH "b" AS `) {
		t.Errorf("a later branch's WITH was written bare, which is a syntax error:\n%s", sql)
	}
}

// An ordering term is an output name and a direction, and every term is
// written. A compound may be ordered by nothing else, so there is no expression
// here to render and no source to qualify against.
func TestCompound_orderingTermsAreOutputNamesWithDirections(t *testing.T) {
	c := compoundOf(branch(users, "id", nil), branch(orders, "id", nil))
	c.OrderBy = []OutputOrder{{Name: "label", Desc: true}, {Name: "id"}}

	sql, _ := compile(t, c)

	want := ` ORDER BY "label" DESC, "id" ASC`
	if !strings.HasSuffix(sql, want) {
		t.Errorf("compiled\n\t%s\nwant it to end with\n\t%s", sql, want)
	}
	// Bare identifiers: the names belong to the result, not to a source.
	if strings.Contains(sql, `ORDER BY "public"`) || strings.Contains(sql, `."label"`) {
		t.Errorf("an ordering term was qualified:\n%s", sql)
	}
}

// A term that names nothing is refused rather than written as an empty
// identifier, which would be a syntax error the caller could not read.
func TestCompound_orderingTermMustNameAColumn(t *testing.T) {
	c := compoundOf(branch(users, "id", nil), branch(orders, "id", nil))
	c.OrderBy = []OutputOrder{{Name: "id"}, {}}

	if _, _, err := c.Compile(); err == nil {
		t.Fatal("an ordering term naming no column was written")
	} else if !strings.Contains(err.Error(), "ordering term 2") {
		t.Errorf("the diagnostic %q does not say which term", err)
	}
}

// A nested compound is parenthesised, so its own clauses stay its own.
func TestCompound_nestedCompoundIsParenthesised(t *testing.T) {
	inner := compoundOf(branch(orders, "id", nil), branch(orders, "user_id", nil))
	sql, _ := compile(t, compoundOf(branch(users, "id", nil), inner))

	if !strings.Contains(sql, "UNION ALL (SELECT") {
		t.Errorf("the nested compound is not parenthesised:\n%s", sql)
	}
}

// Three branches are one operation over three inputs.
func TestCompound_threeBranches(t *testing.T) {
	sql, _ := compile(t, compoundOf(
		branch(users, "id", nil), branch(orders, "id", nil), branch(orders, "user_id", nil),
	))
	if n := strings.Count(sql, "UNION ALL"); n != 2 {
		t.Errorf("three branches produced %d operators, want 2:\n%s", n, sql)
	}
	if strings.Contains(sql, "(") {
		t.Errorf("flat construction was parenthesised:\n%s", sql)
	}
}

// The refusals.
func TestCompound_refusals(t *testing.T) {
	tests := []struct {
		name string
		c    *Compound
		want string
	}{
		{
			name: "no operation",
			c:    &Compound{Branches: []Subquery{branch(users, "id", nil), branch(orders, "id", nil)}},
			want: "UNION ALL and nothing else",
		},
		{
			name: "one branch",
			c:    compoundOf(branch(users, "id", nil)),
			want: "at least two branches",
		},
		{
			name: "an empty branch",
			c:    compoundOf(branch(users, "id", nil), nil),
			want: "branch 2 of the compound statement is empty",
		},
		{
			// PostgreSQL refuses a locking clause anywhere in a set operation,
			// parenthesised or not, so the writer refuses it too. The layer
			// that assembles a compound refuses it earlier; this is the floor
			// under a tree built inside this package.
			name: "a branch that locks the rows it reads",
			c: func() *Compound {
				b := branch(users, "id", nil)
				b.ForUpdate = true
				return compoundOf(b, branch(orders, "id", nil))
			}(),
			want: "does not allow a locking clause in a set operation",
		},
		{
			name: "a branch locked with an explicit strength",
			c: func() *Compound {
				b := branch(users, "id", nil)
				b.Lock = Lock{Strength: LockShare}
				return compoundOf(b, branch(orders, "id", nil))
			}(),
			want: "does not allow a locking clause in a set operation",
		},
		{
			name: "a negative limit",
			c: func() *Compound {
				c := compoundOf(branch(users, "id", nil), branch(orders, "id", nil))
				n := -1
				c.Limit = &n
				return c
			}(),
			want: "negative limit",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.c.Compile()
			if err == nil {
				t.Fatal("compiled, and it must not")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error is %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A branch does not see the other branch's sources.
//
// Each branch pushes its own scope frame, so a column of the left branch's
// source written into the right branch names a relation the right branch's FROM
// clause does not introduce.
func TestCompound_branchesDoNotShareScope(t *testing.T) {
	right := branch(orders, "id", Binary{
		Op:    OpEq,
		Left:  Column{Source: users, Name: "id"}, // the left branch's source
		Right: Arg{Value: 1},
	})
	_, _, err := compoundOf(branch(users, "id", nil), right).Compile()
	if err == nil {
		t.Fatal("the right branch referenced the left branch's source and compiled; " +
			"a branch is not a scope-sharing mechanism")
	}
}

// The arity of a compound is its branches' shared arity, and -1 when they
// disagree — which is what stops a scalar or IN subquery accepting one.
func TestCompound_resultArity(t *testing.T) {
	same := compoundOf(branch(users, "id", nil), branch(orders, "id", nil))
	if got := same.resultArity(); got != 1 {
		t.Errorf("arity = %d, want 1", got)
	}

	wide := &Select{From: orders, Columns: []Column{
		{Source: orders, Name: "id"}, {Source: orders, Name: "user_id"},
	}}
	mixed := compoundOf(branch(users, "id", nil), wide)
	if got := mixed.resultArity(); got != -1 {
		t.Errorf("arity = %d for branches of 1 and 2 columns, want -1", got)
	}
}

// A statement with no WITH clause allocates nothing for one.
//
// The writer tracks which named queries are in force, so that a reference to one
// nothing declared is refused rather than rendered as a relation that does not
// exist. That tracking is on the path every compile takes, and the first version
// of it allocated a map, a frame and a closure per statement whether or not
// there was a clause to track — three allocations on every query this package
// compiles, measured as an eleven percent cost on the small-statement benchmark.
//
// This asserts the property rather than a total, so it cannot be satisfied by a
// generous ceiling and does not move when a Go release changes what something
// else allocates.
func TestWriter_anAbsentWithClauseAllocatesNothing(t *testing.T) {
	w := &writer{}
	if n := testing.AllocsPerRun(100, func() {
		pop, err := w.writeWith(nil)
		if err != nil {
			t.Fatal(err)
		}
		pop()
	}); n != 0 {
		t.Errorf("a statement with no WITH clause allocates %v times for one; the tracking "+
			"belongs on the path that has a clause, not on the path every compile takes", n)
	}

	// And the frame is balanced: what is pushed for a clause comes off again, so
	// a name does not stay in force past the statement that declared it.
	if len(w.withNames) != 0 {
		t.Errorf("%d frames were left in force", len(w.withNames))
	}
}
