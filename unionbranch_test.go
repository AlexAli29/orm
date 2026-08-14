package orm_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
)

// What a branch may carry.
//
// A branch is a statement, and not every statement is one a set operation can
// hold. Two things go wrong and they go wrong differently: a clause the grammar
// attaches to the compound rather than to the branch has to be parenthesised, and
// a locking clause cannot be there at all. Both were found by asking PostgreSQL
// what it accepts rather than by reading the tests.

func branchPlain() *orm.ComposedQuery[int64] {
	return orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"),
		func(v int64) int64 { return v })).From(usersSrc)
}

// PostgreSQL refuses a locking clause in a set operation, and parenthesising the
// branch does not help:
//
//	(SELECT ... FOR UPDATE) UNION ALL SELECT ...
//	ERROR: FOR UPDATE is not allowed with UNION/INTERSECT/EXCEPT
//
// So it is refused when the branch is handed over, not when the statement is
// written and not by the server.
func TestUnionBranch_refusesALockingClause(t *testing.T) {
	cases := []struct {
		name  string
		build func() (string, []any, error)
		want  string
	}{
		{
			name: "a composed branch with FOR UPDATE",
			build: func() (string, []any, error) {
				locked := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"),
					func(v int64) int64 { return v })).From(usersSrc).ForUpdate()
				return orm.UnionAll[int64](locked, branchPlain()).SQL()
			},
			want: "FOR UPDATE",
		},
		{
			name: "a locked branch in second position",
			build: func() (string, []any, error) {
				locked := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"),
					func(v int64) int64 { return v })).From(usersSrc).ForUpdate()
				return orm.UnionAll[int64](branchPlain(), locked).SQL()
			},
			want: "branch 2",
		},
		{
			name: "an entity branch with FOR UPDATE",
			build: func() (string, []any, error) {
				repo := orm.NewRepo(nil, &userMeta)
				return orm.UnionAll[User](repo.Query().ForUpdate(), repo.Query()).SQL()
			},
			want: "FOR UPDATE",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := c.build()
			if err == nil {
				t.Fatalf("a locked branch was accepted:\n%s", sql)
			}
			if sql != "" || args != nil {
				t.Errorf("a refused union still produced SQL %q and args %v", sql, args)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the diagnostic %q does not mention %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "parenthesising the branch does not help") {
				t.Errorf("the diagnostic %q does not say why this is not a parenthesisation problem", err)
			}
			// The writer refuses this too, with the same sentence, so the
			// sentence alone does not say which check spoke. The builder names
			// the operation — "UNION ALL branch 1" — and the writer names only
			// the position, so this is what distinguishes a refusal when the
			// branch was handed over from one when the statement was written.
			if !strings.Contains(err.Error(), "UNION ALL branch") {
				t.Errorf("the diagnostic %q came from the writer's floor rather than from the builder", err)
			}
		})
	}

	// And the clause is fine on a query that is not a branch.
	if _, _, err := branchPlain().ForUpdate().SQL(); err != nil {
		t.Errorf("FOR UPDATE was refused on a query that is not a branch: %v", err)
	}
}

// A branch carrying its own WITH is parenthesised, because the bare form means
// something else. In the first branch PostgreSQL accepts it and hoists the CTE
// to the whole compound; in any later branch it is a syntax error.
func TestUnionBranch_parenthesisesAWithClause(t *testing.T) {
	withCTE := func(name string) *orm.ComposedQuery[int64] {
		c := orm.CTE(name, branchPlain())
		return orm.Compose(nil, orm.Project1(
			orm.Ref(c, orm.Named("id", orm.Of(Users.ID))).As("id"),
			func(v int64) int64 { return v })).With(c).From(c)
	}

	t.Run("in the first branch", func(t *testing.T) {
		sql, _, err := orm.UnionAll[int64](withCTE("x"), branchPlain()).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.HasPrefix(sql, `(WITH "x" AS `) {
			t.Errorf("the branch's WITH was not kept inside it:\n%s", sql)
		}
		// Hoisted, it would declare x for the operation and every branch would
		// see it. Inside the parentheses it declares x for this branch.
		if strings.HasPrefix(sql, `WITH "x"`) {
			t.Errorf("the branch's WITH became the compound's:\n%s", sql)
		}
	})

	t.Run("in a later branch", func(t *testing.T) {
		sql, _, err := orm.UnionAll[int64](branchPlain(), withCTE("y")).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, `UNION ALL (WITH "y" AS `) {
			t.Errorf("the later branch's WITH was written bare, which is a syntax error:\n%s", sql)
		}
	})

	t.Run("in both, under one name", func(t *testing.T) {
		sql, _, err := orm.UnionAll[int64](withCTE("z"), withCTE("z")).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if n := strings.Count(sql, `(WITH "z" AS `); n != 2 {
			t.Errorf("two branches declaring one name produced %d parenthesised clauses:\n%s", n, sql)
		}
	})
}

// A branch is not a scope-sharing mechanism, which the compiler's comment has
// claimed since the node was written. It became true when the WITH stopped being
// hoisted: a branch that borrows another branch's named query is refused.
func TestUnionBranch_isNotAScopeSharingMechanism(t *testing.T) {
	c := orm.CTE("shared", branchPlain())
	declaring := orm.Compose(nil, orm.Project1(
		orm.Ref(c, orm.Named("id", orm.Of(Users.ID))).As("id"),
		func(v int64) int64 { return v })).With(c).From(c)
	borrowing := orm.Compose(nil, orm.Project1(
		orm.Ref(c, orm.Named("id", orm.Of(Users.ID))).As("id"),
		func(v int64) int64 { return v })).From(c)

	_, _, err := orm.UnionAll[int64](declaring, borrowing).SQL()
	if err == nil {
		t.Fatal("a branch selected from a named query only another branch declared")
	}
	if !strings.Contains(err.Error(), `"shared"`) || !strings.Contains(err.Error(), "without declaring it") {
		t.Errorf("the diagnostic %q does not say which query is undeclared", err)
	}
}

// The same rule outside a set operation, which is where it was missing first: a
// statement that selects from a named query it never declared renders SQL naming
// a relation that does not exist.
func TestUnionBranch_aNamedQueryHasToBeDeclaredWhereItIsUsed(t *testing.T) {
	c := orm.CTE("orphan", branchPlain())
	shape := orm.Project1(orm.Ref(c, orm.Named("id", orm.Of(Users.ID))),
		func(v int64) int64 { return v })

	sql, _, err := orm.Compose(nil, shape).From(c).SQL()
	if err == nil {
		t.Fatalf("a statement selected from an undeclared named query:\n%s", sql)
	}
	if !strings.Contains(err.Error(), `"orphan"`) {
		t.Errorf("the diagnostic %q does not name the query that is missing", err)
	}
	if !strings.Contains(err.Error(), "pass it to With") {
		t.Errorf("the diagnostic %q does not say what to do about it", err)
	}

	// Declared, it compiles.
	if _, _, err := orm.Compose(nil, shape).With(c).From(c).SQL(); err != nil {
		t.Errorf("the same statement with the declaration was refused: %v", err)
	}
}

// A named query declared by an enclosing statement is in force inside the
// statements nested in it, so the rule does not refuse what PostgreSQL allows.
func TestUnionBranch_anEnclosingDeclarationReachesNestedStatements(t *testing.T) {
	c := orm.CTE("outer", branchPlain())
	ref := orm.Named("id", orm.Of(Users.ID))

	// A derived table over the CTE, inside a statement that declares it.
	inner := orm.Compose(nil, orm.Project1(orm.Ref(c, ref).As("id"),
		func(v int64) int64 { return v })).From(c)
	d := orm.Sub("d", inner)
	shape := orm.Project1(orm.Ref(d, ref), func(v int64) int64 { return v })

	sql, _, err := orm.Compose(nil, shape).With(c).From(d).SQL()
	if err != nil {
		t.Fatalf("a nested statement was refused a name its enclosing statement declares: %v", err)
	}
	if !strings.Contains(sql, `FROM "outer"`) {
		t.Errorf("the nested statement does not read the CTE:\n%s", sql)
	}

	// And a membership test over the same CTE, inside the declaring statement.
	if _, _, err := orm.Compose(nil, shape).With(c).From(d).
		Where(orm.InSub(orm.Ref(d, ref), inner)).SQL(); err != nil {
		t.Errorf("a value subquery was refused a name its enclosing statement declares: %v", err)
	}
}

// A deeply nested compound reports what went wrong once.
//
// Each level used to add "branch 1: " in front of the message, so a statement
// nested past the depth limit produced the one useful sentence behind thirty-two
// repetitions of the same prefix.
func TestUnionBranch_nestingDoesNotRepeatItselfInTheDiagnostic(t *testing.T) {
	deep := orm.UnionAll[int64](branchPlain(), branchPlain())
	for range 60 {
		deep = orm.UnionAll[int64](deep, branchPlain())
	}
	_, _, err := deep.SQL()
	if err == nil {
		t.Fatal("a 60-deep nest compiled; the depth limit did not fire")
	}
	if !strings.Contains(err.Error(), "nested more than") {
		t.Fatalf("the diagnostic %q is not the depth limit's", err)
	}
	if n := strings.Count(err.Error(), "branch 1: "); n > 1 {
		t.Errorf("the diagnostic repeats its prefix %d times:\n%s", n, err)
	}
}

// Sharing a named query between branches.
//
// A branch's own WITH belongs to that branch and is parenthesised to keep it
// there, so one branch cannot read what another declared. What is left for the
// case where two branches genuinely want the same rows is the operation's own
// WITH, which PostgreSQL puts in front of the compound and evaluates once.
//
// That is not a convenience. Before a branch's WITH was parenthesised, sharing
// worked by accident — the declaration was hoisted, and the accident was the
// only route. Closing the accident without opening this would have taken the
// capability away.
func TestUnionBranch_theOperationCanDeclareItsOwnNamedQueries(t *testing.T) {
	c := orm.CTE("shared", branchPlain())
	ref := orm.Named("id", orm.Of(Users.ID))
	reading := func() *orm.ComposedQuery[int64] {
		return orm.Compose(nil, orm.Project1(orm.Ref(c, ref).As("id"),
			func(v int64) int64 { return v })).From(c)
	}

	sql, _, err := orm.UnionAll[int64](reading(), reading()).With(c).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `WITH "shared" AS (SELECT "users"."id" AS "id" FROM "public"."users") ` +
		`SELECT "shared"."id" AS "id" FROM "shared" ` +
		`UNION ALL ` +
		`SELECT "shared"."id" AS "id" FROM "shared"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	// In front of the operation, and declared once for it — not once per branch
	// and not inside either.
	if n := strings.Count(sql, `AS (SELECT`); n != 1 {
		t.Errorf("the named query was declared %d times:\n%s", n, sql)
	}
	if strings.Contains(sql, `(WITH `) {
		t.Errorf("the operation's WITH was written as a branch's:\n%s", sql)
	}

	// Undeclared, the same branches are refused — so the declaration is what
	// makes it legal rather than the writing being lax.
	if _, _, err := orm.UnionAll[int64](reading(), reading()).SQL(); err == nil {
		t.Error("two branches read a named query the operation never declared")
	}
}

// The operation's WITH takes the same items a query's does, and refuses the same
// things — one rule, asked by both.
func TestUnionBranch_theOperationsWithRefusesWhatAQuerysDoes(t *testing.T) {
	// A reference without a declaration is what a recursive term is handed to
	// name the CTE being defined. It is a source, and it is a named query, and
	// it declares nothing — which is the case worth refusing separately.
	var selfRef *orm.Source
	anchor := orm.Rows(orm.Named("id", orm.Of(Users.ID))).From(usersSrc)
	_ = orm.RecursiveCTE("walk", anchor, func(self *orm.Source) orm.Term {
		selfRef = self
		return anchor
	})
	if selfRef == nil {
		t.Fatal("the recursive builder did not hand its term a self-reference")
	}

	cases := []struct {
		name string
		item *orm.Source
		want string
	}{
		{"a missing item", nil, "WITH item 1 is missing"},
		{"a table", usersSrc, "is not one"},
		{"a reference rather than a declaration", selfRef, "has no statement"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := orm.UnionAll[int64](branchPlain(), branchPlain()).With(c.item).SQL()
			if err == nil {
				t.Fatalf("%s was declared", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the diagnostic %q does not mention %q", err, c.want)
			}
		})
	}
}

// The operation's items are written before the branches, so their parameters are
// numbered first — one statement, one parameter list, in the order the SQL reads.
func TestUnionBranch_theOperationsWithIsNumberedFirst(t *testing.T) {
	filtered := orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"),
		func(v int64) int64 { return v })).From(usersSrc).
		Where(orm.Of(Users.Email).Eq("a@example.com"))
	c := orm.CTE("filtered", filtered)
	ref := orm.Named("id", orm.Of(Users.ID))
	branch := func(min int32) *orm.ComposedQuery[int64] {
		return orm.Compose(nil, orm.Project1(orm.Ref(c, ref).As("id"),
			func(v int64) int64 { return v })).From(c).
			Where(orm.Ref(c, ref).Gt(int64(min)))
	}

	sql, args, err := orm.UnionAll[int64](branch(1), branch(2)).With(c).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, `WITH "filtered" AS (SELECT "users"."id" AS "id" FROM "public"."users" WHERE "users"."email" = $1)`) {
		t.Errorf("the declaration's parameter is not the first:\n%s", sql)
	}
	wantArgs := []any{"a@example.com", int64(1), int64(2)}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("$%d = %#v, want %#v", i+1, args[i], wantArgs[i])
		}
	}
}
