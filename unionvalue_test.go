package orm_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
)

// A set operation used where a value is expected.
//
// Two positions, two rules, and they are not the same rule: a membership test
// compares as many columns as the expression on its left, and a scalar subquery
// reads exactly one. Both numbers are known when the query is built — for a
// compound, from the result shape its branches were validated against — so both
// are decided before any SQL exists.
//
// What is not decided here is whether the values are comparable in SQL, or how
// many rows come back. Those are PostgreSQL's, and the tests that prove the
// division of labour run against a server.

// One-column branches, which is what both positions want.
func oneColUsers() *orm.ComposedQuery[int64] {
	return orm.Compose(nil, orm.Project1(
		orm.Of(Users.ID), func(id int64) int64 { return id },
	)).From(usersSrc)
}

func oneColPosts() *orm.ComposedQuery[int64] {
	return orm.Compose(nil, orm.Project1(
		orm.Of(Posts.ID), func(id int64) int64 { return id },
	)).From(postsSrc)
}

// Two-column branches, which neither position accepts.
func twoColUsers() *orm.ComposedQuery[[2]any] {
	return orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Email),
		func(id int64, s string) [2]any { return [2]any{id, s} },
	)).From(usersSrc)
}

func TestUnionValue_isAMembershipTest(t *testing.T) {
	u := orm.UnionAll[int64](oneColUsers(), oneColPosts())
	sql, args, err := orm.NewRepo(nil, &userMeta).Query().
		Where(orm.InSub(Users.ID, u)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."id" IN (` +
		`SELECT "users"."id" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" FROM "public"."posts")`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	// Exactly the parentheses the grammar needs: IN's own pair, and none around
	// the compound inside it.
	if strings.Contains(sql, "IN ((") {
		t.Errorf("the compound was parenthesised twice:\n%s", sql)
	}
}

func TestUnionValue_isANegatedMembershipTest(t *testing.T) {
	u := orm.UnionAll[int64](oneColUsers(), oneColPosts())
	sql, _, err := orm.NewRepo(nil, &userMeta).Query().
		Where(orm.NotInSub(Users.ID, u)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `"users"."id" NOT IN (SELECT `) {
		t.Errorf("NOT IN was not rendered as one test:\n%s", sql)
	}
	if n := strings.Count(sql, "UNION ALL"); n != 1 {
		t.Errorf("the statement has %d set operations, want 1:\n%s", n, sql)
	}
}

func TestUnionValue_isAScalarValue(t *testing.T) {
	u := orm.UnionAll[int64](oneColUsers(), oneColPosts())
	shape := orm.Project2(
		orm.Of(Users.Email), orm.Scalar[orm.Composed, int64](u),
		func(email string, n *int64) [2]any { return [2]any{email, n} },
	)
	sql, args, err := orm.Compose(nil, shape).From(usersSrc).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."email", (` +
		`SELECT "users"."id" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" FROM "public"."posts") ` +
		`FROM "public"."users"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// A scalar subquery is always nullable, whatever it selects, because a statement
// matching no row still yields one value and that value is NULL. A compound does
// not change that: reading one into a destination that cannot hold NULL is
// refused, which is the rule the projection already had.
func TestUnionValue_aScalarCompoundIsStillNullable(t *testing.T) {
	u := orm.UnionAll[int64](oneColUsers(), oneColPosts())
	v := orm.Scalar[orm.Composed, int64](u)
	shape := orm.Project1(v, func(n *int64) int64 {
		if n == nil {
			return 0
		}
		return *n
	})
	if _, _, err := orm.Compose(nil, shape).From(usersSrc).SQL(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// The arity rules are two rules, not one applied twice, so they are asserted
// separately: a membership test compares as many columns as the expression on
// its left, and a scalar value reads one.
func TestUnionValue_refusesTheWrongNumberOfColumnsInAMembershipTest(t *testing.T) {
	wide := orm.UnionAll[[2]any](twoColUsers(), twoColUsers())

	t.Run("a two-column union in a membership test", func(t *testing.T) {
		_, _, err := orm.NewRepo(nil, &userMeta).Query().
			Where(orm.InSub(Users.ID, wide)).SQL()
		if err == nil {
			t.Fatal("a two-column set operation was compared against one expression")
		}
		if !strings.Contains(err.Error(), "compares 1 column") || !strings.Contains(err.Error(), "returns 2") {
			t.Errorf("the diagnostic %q does not say what was compared against what", err)
		}
	})

	t.Run("a two-column union negated", func(t *testing.T) {
		_, _, err := orm.NewRepo(nil, &userMeta).Query().
			Where(orm.NotInSub(Users.ID, wide)).SQL()
		if err == nil {
			t.Fatal("a two-column set operation was compared against one expression")
		}
	})

	t.Run("an entity query, which selects its whole descriptor", func(t *testing.T) {
		_, _, err := orm.NewRepo(nil, &userMeta).Query().
			Where(orm.InSub(Users.ID, orm.NewRepo(nil, &userMeta).Query())).SQL()
		if err == nil {
			t.Fatal("an eight-column entity query was compared against one expression")
		}
		if !strings.Contains(err.Error(), "compares 1 column") || !strings.Contains(err.Error(), "returns 8") {
			t.Errorf("the diagnostic %q does not report the entity's column count", err)
		}
	})
}

func TestUnionValue_refusesTheWrongNumberOfColumnsInAScalar(t *testing.T) {
	wide := orm.UnionAll[[2]any](twoColUsers(), twoColUsers())

	t.Run("a two-column union as a scalar value", func(t *testing.T) {
		shape := orm.Project1(orm.Scalar[orm.Composed, int64](wide),
			func(n *int64) int64 { return 0 })
		_, _, err := orm.Compose(nil, shape).From(usersSrc).SQL()
		if err == nil {
			t.Fatal("a two-column set operation was read as a scalar value")
		}
		if !strings.Contains(err.Error(), "reads one column") || !strings.Contains(err.Error(), "returns 2") {
			t.Errorf("the diagnostic %q does not say how many columns it has", err)
		}
	})

	t.Run("a plain two-column query as a scalar value", func(t *testing.T) {
		// Not a set operation: the same rule, and the same place it is applied.
		shape := orm.Project1(orm.Scalar[orm.Composed, int64](twoColUsers()),
			func(n *int64) int64 { return 0 })
		_, _, err := orm.Compose(nil, shape).From(usersSrc).SQL()
		if err == nil {
			t.Fatal("a two-column query was read as a scalar value")
		}
		if !strings.Contains(err.Error(), "reads one column") {
			t.Errorf("the diagnostic %q is not the arity rule's", err)
		}
	})
}

// A union that never validated is not a value subquery either: the mistake
// travels out of the statement rather than producing SQL from whichever branches
// happened to survive.
func TestUnionValue_refusesAUnionThatDidNotValidate(t *testing.T) {
	broken := orm.UnionAll[int64](oneColUsers(), orm.Compose(nil, orm.Project1(
		orm.Of(Users.Age), func(age int32) int64 { return int64(age) },
	)).From(usersSrc))

	t.Run("in a membership test", func(t *testing.T) {
		_, _, err := orm.NewRepo(nil, &userMeta).Query().
			Where(orm.InSub(Users.ID, broken)).SQL()
		if err == nil {
			t.Fatal("a union that did not validate became a membership test")
		}
		if !strings.Contains(err.Error(), "int32") {
			t.Errorf("the diagnostic %q is not the union's own", err)
		}
	})

	t.Run("as a scalar value", func(t *testing.T) {
		shape := orm.Project1(orm.Scalar[orm.Composed, int64](broken), func(n *int64) int64 { return 0 })
		_, _, err := orm.Compose(nil, shape).From(usersSrc).SQL()
		if err == nil {
			t.Fatal("a union that did not validate became a scalar value")
		}
		if !strings.Contains(err.Error(), "int32") {
			t.Errorf("the diagnostic %q is not the union's own", err)
		}
	})
}

// Parameters are numbered across one statement in the order the SQL reads: the
// predicate before the test, then each branch of the compound inside it, then
// the predicate after.
func TestUnionValue_numbersEveryParameterAcrossOneStatement(t *testing.T) {
	branch := func(v int32) *orm.ComposedQuery[int64] {
		return orm.Compose(nil, orm.Project1(
			orm.Of(Users.ID), func(id int64) int64 { return id },
		)).From(usersSrc).Where(orm.Of(Users.Age).Gt(v))
	}
	u := orm.UnionAll[int64](branch(18), branch(30), branch(65))

	sql, args, err := orm.NewRepo(nil, &userMeta).Query().Where(
		Users.Email.Eq("a@example.com"),
		orm.InSub(Users.ID, u),
		Users.Active.Eq(true),
	).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."email" = $1 AND "users"."id" IN (` +
		`SELECT "users"."id" FROM "public"."users" WHERE "users"."age" > $2 ` +
		`UNION ALL ` +
		`SELECT "users"."id" FROM "public"."users" WHERE "users"."age" > $3 ` +
		`UNION ALL ` +
		`SELECT "users"."id" FROM "public"."users" WHERE "users"."age" > $4` +
		`) AND "users"."active" = $5`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	wantArgs := []any{"a@example.com", int32(18), int32(30), int32(65), true}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("$%d = %#v, want %#v", i+1, args[i], wantArgs[i])
		}
	}
}

// A union of a union in a value position: the inner one keeps its parentheses
// inside the outer one's, and the outer one gets IN's.
func TestUnionValue_nestedCompound(t *testing.T) {
	inner := orm.UnionAll[int64](oneColUsers(), oneColPosts())
	nested := orm.UnionAll[int64](inner, oneColUsers())

	sql, _, err := orm.NewRepo(nil, &userMeta).Query().
		Where(orm.InSub(Users.ID, nested)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."id" IN ((` +
		`SELECT "users"."id" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" FROM "public"."posts") ` +
		`UNION ALL ` +
		`SELECT "users"."id" FROM "public"."users")`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
}

// A query whose own source is a set operation is a value subquery like any
// other: the compound is inside the FROM of the statement being nested, and
// nothing about the value position looks at it.
func TestUnionValue_aQueryOverACompoundSource(t *testing.T) {
	u := orm.Sub("u", orm.UnionAll[sourced](srcFromUsers(), srcFromPosts()))
	over := orm.Compose(nil, orm.Project1(
		orm.Ref(u, outThingID), func(id int64) int64 { return id },
	)).From(u)

	sql, args, err := orm.NewRepo(nil, &userMeta).Query().
		Where(orm.InSub(Users.ID, over)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `IN (SELECT "u"."thing_id" FROM (`) {
		t.Errorf("the compound source is not inside the nested statement:\n%s", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want the compound source's one", args)
	}
}

// And the reverse nesting: a branch whose own source is a set operation.
func TestUnionValue_aBranchOverACompoundSource(t *testing.T) {
	u := orm.Sub("u", orm.UnionAll[sourced](srcFromUsers(), srcFromPosts()))
	over := orm.Compose(nil, orm.Project1(
		orm.Ref(u, outThingID), func(id int64) int64 { return id },
	)).From(u)

	sql, _, err := orm.NewRepo(nil, &userMeta).Query().
		Where(orm.InSub(Users.ID, orm.UnionAll[int64](oneColUsers(), over))).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if n := strings.Count(sql, "UNION ALL"); n != 2 {
		t.Errorf("the statement has %d set operations, want 2:\n%s", n, sql)
	}
}

// The builders that were value subqueries before this still are, unchanged.
func TestUnionValue_theOtherBuildersAreStillValueSubqueries(t *testing.T) {
	t.Run("ComposedQuery", func(t *testing.T) {
		sql, _, err := orm.NewRepo(nil, &userMeta).Query().
			Where(orm.InSub(Users.ID, oneColUsers())).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		want := selectAll + ` WHERE "users"."id" IN (SELECT "users"."id" FROM "public"."users")`
		if sql != want {
			t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
		}
	})

	t.Run("SelectQuery", func(t *testing.T) {
		q := orm.Select(orm.NewRepo(nil, &userMeta), orm.Project1(
			Users.ID, func(id int64) int64 { return id },
		))
		if _, _, err := orm.NewRepo(nil, &userMeta).Query().Where(orm.InSub(Users.ID, q)).SQL(); err != nil {
			t.Fatalf("SQL: %v", err)
		}
	})

	t.Run("a Rows query, which names its column and has no result type", func(t *testing.T) {
		// A value subquery does not need named columns, but a query that has
		// them is still one. This is the form the Scalar documentation shows.
		rows := orm.Rows(orm.Named("n", orm.Of(Users.ID))).From(usersSrc)
		shape := orm.Project1(orm.Scalar[orm.Composed, int64](rows), func(n *int64) int64 { return 0 })
		if _, _, err := orm.Compose(nil, shape).From(usersSrc).SQL(); err != nil {
			t.Fatalf("SQL: %v", err)
		}
	})

	t.Run("a query whose column has no name", func(t *testing.T) {
		// Unlike a source, a value subquery does not require its columns to be
		// named — nothing addresses them.
		if _, _, err := orm.NewRepo(nil, &userMeta).Query().
			Where(orm.InSub(Users.ID, oneColUsers())).SQL(); err != nil {
			t.Fatalf("an unnamed column was refused in a value position: %v", err)
		}
	})
}

// Being a value subquery is read-only with respect to the union. The shape is
// still the one the branches were validated against, so the same value is still
// a branch, still a source, and still reads its own rows.
func TestUnionValue_beingAValueSubqueryDoesNotEraseTheShape(t *testing.T) {
	u := orm.UnionAll[int64](oneColUsers(), oneColPosts())

	for range 2 {
		if _, _, err := orm.NewRepo(nil, &userMeta).Query().Where(orm.InSub(Users.ID, u)).SQL(); err != nil {
			t.Fatalf("as a membership test: %v", err)
		}
		shape := orm.Project1(orm.Scalar[orm.Composed, int64](u), func(n *int64) int64 { return 0 })
		if _, _, err := orm.Compose(nil, shape).From(usersSrc).SQL(); err != nil {
			t.Fatalf("as a scalar value: %v", err)
		}
	}

	// Still a branch of another union, and still refusing one of another shape.
	if _, _, err := orm.UnionAll[int64](u, oneColUsers()).SQL(); err != nil {
		t.Errorf("after being a value subquery, the union is no longer a branch: %v", err)
	}
	narrower := orm.Compose(nil, orm.Project1(
		orm.Of(Users.Age), func(age int32) int64 { return int64(age) },
	)).From(usersSrc)
	if _, _, err := orm.UnionAll[int64](u, narrower).SQL(); err == nil {
		t.Error("after being a value subquery, the union accepted a branch of another shape")
	}

	// Still reads its own rows.
	got, err := u.Using(stubExecutor{rows: [][]any{{int64(1)}, {int64(2)}}}).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("All returned %v", got)
	}
}

// Tuple comparisons exist against a list of values and not against a subquery.
// This is the regression check for the form that does exist; a tuple left-hand
// side against a set operation would need infrastructure this package does not
// have, and inventing it is not what this phase is for.
func TestUnionValue_tupleComparisonsAreStillAgainstValueLists(t *testing.T) {
	sql, args, err := orm.Compose(nil, orm.Project1(
		orm.Of(Users.ID), func(id int64) int64 { return id },
	)).From(usersSrc).Where(
		orm.Row2In(Users.Email, Users.Age, orm.Both("a@example.com", int32(30))),
	).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `("users"."email", "users"."age") IN (($1, $2))`) {
		t.Errorf("the tuple test did not render as a row value against a list:\n%s", sql)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want two", args)
	}
}

// The two read capabilities ask for different things, and this is the query that
// shows it: one union that is a valid value subquery and an invalid row source.
//
// A source's columns are addressed by name, so a source term declares them. A
// value subquery's columns are not addressed at all, so requiring names there
// would refuse membership tests PostgreSQL is perfectly happy with. Collapsing
// the two into one interface would have to pick one rule, and either choice is
// wrong for the other position.
func TestUnionValue_aValueSubqueryNeedsNoColumnNames(t *testing.T) {
	unnamed := orm.UnionAll[int64](oneColUsers(), oneColPosts())

	if _, _, err := orm.NewRepo(nil, &userMeta).Query().
		Where(orm.InSub(Users.ID, unnamed)).SQL(); err != nil {
		t.Errorf("a union whose columns have no name was refused in a membership test: %v", err)
	}
	shape := orm.Project1(orm.Scalar[orm.Composed, int64](unnamed), func(*int64) int64 { return 0 })
	if _, _, err := orm.Compose(nil, shape).From(usersSrc).SQL(); err != nil {
		t.Errorf("a union whose columns have no name was refused as a scalar value: %v", err)
	}

	// The same union is not a source, and says why.
	src := orm.Sub("u", unnamed)
	if src.Err() == nil {
		t.Fatal("a union whose columns have no name became a derived table")
	}
	if !strings.Contains(src.Err().Error(), "no name") {
		t.Errorf("the diagnostic %q is not the source rule's", src.Err())
	}
	if item := orm.CTE("c", unnamed); item.Err() == nil {
		t.Error("a union whose columns have no name became a CTE body")
	}
}
