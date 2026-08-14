package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// The M11 scope and source-identity audit.
//
// The premise of the whole milestone is that a source is identified by what it
// is rather than by what it is called. Every test here tries to defeat that:
// two sources with one name, one source used twice, a name that matches
// something in an enclosing query, a reference that should be out of scope but
// looks like one that is in it.

// Two aliases of one table are two sources, and an expression built from one
// is not accepted because the other happens to be visible.
func TestAudit_sourceIdentityIsNotTheAliasString(t *testing.T) {
	manager := gendemo.Users.As("manager")
	reviewer := gendemo.Users.As("reviewer")
	shape := orm.Project1(orm.Of(manager.ID), func(id int64) int64 { return id })

	// reviewer is never introduced, so its column is out of scope even though
	// a different occurrence of the same table is not.
	_, _, err := orm.Compose(nil, shape).
		From(manager.Source()).
		Where(orm.Cond(reviewer.Email.Eq("x"))).
		SQL()
	if err == nil {
		t.Fatal("a column of an occurrence the query never introduced compiled")
	}
	if !strings.Contains(err.Error(), "scope error") {
		t.Errorf("error = %v", err)
	}
}

// The same trap with identical alias text: two occurrences built separately
// under one name are still two sources, and only the one in the FROM clause is
// in scope.
func TestAudit_identicalAliasTextIsNotIdentity(t *testing.T) {
	a := gendemo.Users.As("u")
	b := gendemo.Users.As("u")
	if a.Source() == b.Source() {
		t.Fatal("two calls to As returned one source")
	}
	shape := orm.Project1(orm.Of(a.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(a.Source()).
		Where(orm.Cond(b.Email.Eq("x"))).
		SQL()
	if err == nil {
		t.Fatal("a column of a different occurrence with the same alias text compiled; " +
			"identity is being compared by name")
	}
}

// A derived source aliased twice, and a CTE referenced twice, are distinct
// sources — and a column of one is not admitted through the other.
func TestAudit_derivedAndCTEAliasesAreDistinctIdentities(t *testing.T) {
	out := orm.Named("id", orm.Of(gendemo.Users.ID))
	def := orm.Rows(out).From(gendemo.Users.Source())

	a := orm.Sub("a", def)
	b := orm.Sub("b", def)
	shape := orm.Project1(orm.Ref(a, out), func(v int64) int64 { return v })
	if _, _, err := orm.Compose(nil, shape).From(b).SQL(); err == nil {
		t.Fatal("a column of derived table a compiled in a query selecting from b")
	}

	cte := orm.CTE("c", def)
	x, y := cte.As("x"), cte.As("y")
	if x == y || x == cte {
		t.Fatal("aliasing a CTE reused the same source")
	}
	shape2 := orm.Project1(orm.Ref(x, out), func(v int64) int64 { return v })
	if _, _, err := orm.Compose(nil, shape2).With(cte).From(y).SQL(); err == nil {
		t.Fatal("a column of CTE reference x compiled in a query selecting only through y")
	}
}

// One source introduced twice in one FROM clause is ambiguous SQL. PostgreSQL
// refuses it; so should the builder, before the round trip.
func TestAudit_oneSourceCannotBeIntroducedTwice(t *testing.T) {
	src := gendemo.Users.Source()
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(src).
		Join(src, orm.Cond(orm.Expr[orm.Composed]("TRUE"))).
		SQL()
	if err == nil {
		t.Fatal("one source was introduced twice in one FROM clause")
	}
}

// A correlated subquery may name the queries above it and nothing else. A
// sibling subquery's source is not an ancestor.
func TestAudit_correlationDoesNotLeakBetweenSiblings(t *testing.T) {
	comments := gendemo.Comments.As("c")
	// A subquery over posts, correlated to a source that only exists inside a
	// *different* subquery. The comparison is typed, so it carries the
	// dependency the scope check reads.
	sibling := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(comments.ID, gendemo.Posts.ID))

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		// comments is introduced only inside this EXISTS...
		Where(orm.Exists[orm.Composed](
			orm.Rows(orm.Named("x", orm.Val(1))).From(comments.Source()),
		)).
		// ...so the second EXISTS cannot see it.
		Where(orm.Exists[orm.Composed](sibling)).
		SQL()
	if err == nil {
		t.Fatal("a subquery named a source introduced only inside a sibling subquery")
	}
}

// Correlation reaches two levels out, and the SQL qualifies each reference
// against the occurrence it was built from rather than against the nearest
// alias of that name.
func TestAudit_correlationShadowing(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `UPDATE users SET manager_id = 1 WHERE id = 2`)

	// Two occurrences of users, both aliased "u" would collide; use distinct
	// names and confirm each reference resolves to its own occurrence.
	outer := gendemo.Users.As("outer_u")
	inner := gendemo.Users.As("inner_u")

	// The inner query selects from inner_u and correlates to outer_u.
	nested := orm.Rows(orm.Named("x", orm.Val(1))).
		From(inner.Source()).
		Where(orm.Eq(inner.ManagerID, outer.ID))

	got := userIDs2(t, orm.Compose(db.Executor(),
		orm.Project1(orm.Of(outer.ID), func(id int64) int64 { return id })).
		From(outer.Source()).
		Where(orm.Exists[orm.Composed](nested)).
		OrderBy(orm.Of(outer.ID).Asc()))

	assertIDs(t, conn, `
		SELECT o.id FROM users o
		WHERE EXISTS (SELECT 1 FROM users i WHERE i.manager_id = o.id)
		ORDER BY o.id`, got)

	sql, _, err := orm.Compose(nil,
		orm.Project1(orm.Of(outer.ID), func(id int64) int64 { return id })).
		From(outer.Source()).
		Where(orm.Exists[orm.Composed](nested)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `"inner_u"."manager_id" = "outer_u"."id"`) {
		t.Errorf("the correlation did not qualify each side against its own occurrence:\n%s", sql)
	}
}

// A join condition sees the sources written before it and the one it attaches.
// Three joins, so the middle one is genuinely in the middle.
func TestAudit_sequentialScopeAtEveryPosition(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	base := func() *orm.ComposedQuery[int64] {
		return orm.Compose(nil, shape).From(gendemo.Users.Source())
	}
	ok := orm.Cond(orm.Expr[orm.Composed]("TRUE"))

	t.Run("first join cannot see the second", func(t *testing.T) {
		_, _, err := base().
			Join(gendemo.Posts.Source(), orm.Eq(gendemo.Comments.ID, gendemo.Posts.ID)).
			Join(gendemo.Comments.Source(), ok).
			SQL()
		if err == nil {
			t.Fatal("a join condition named a source attached after it")
		}
	})
	t.Run("second join cannot see the third", func(t *testing.T) {
		_, _, err := base().
			Join(gendemo.Posts.Source(), ok).
			Join(gendemo.Comments.Source(), orm.Eq(gendemo.Avatars.ID, gendemo.Comments.ID)).
			Join(gendemo.Avatars.Source(), ok).
			SQL()
		if err == nil {
			t.Fatal("a join condition named a source attached after it")
		}
	})
	t.Run("each join can see everything before it", func(t *testing.T) {
		_, _, err := base().
			Join(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
			Join(gendemo.Comments.Source(),
				orm.Eq(gendemo.Comments.ID, gendemo.Posts.ID),
				orm.Cond(gendemo.Users.Active.Eq(true))).
			SQL()
		if err != nil {
			t.Fatalf("a join condition naming every preceding source was refused: %v", err)
		}
	})
}

// A lateral source sees what precedes it and nothing that follows.
func TestAudit_lateralVisibilityIsLeftwardOnly(t *testing.T) {
	postID := orm.Named("id", orm.Of(gendemo.Posts.ID))
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })

	t.Run("sees a preceding source", func(t *testing.T) {
		lat := orm.Sub("p", orm.Rows(postID).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)))
		if _, _, err := orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			LeftJoinLateral(lat).
			SQL(); err != nil {
			t.Fatalf("a lateral source naming a preceding one was refused: %v", err)
		}
	})
	t.Run("cannot see a following source", func(t *testing.T) {
		lat := orm.Sub("p", orm.Rows(postID).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Comments.ID, gendemo.Posts.ID)))
		_, _, err := orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			LeftJoinLateral(lat).
			Join(gendemo.Comments.Source(), orm.Cond(orm.Expr[orm.Composed]("TRUE"))).
			SQL()
		if err == nil {
			t.Fatal("a lateral source named a source attached after it")
		}
	})
}

// A CTE body cannot name the FROM items of the statement that declares it,
// because it is evaluated before them.
func TestAudit_cteBodyCannotNameTheOuterFrom(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Posts.ID))
	bad := orm.CTE("bad", orm.Rows(id).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)))

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
	_, _, err := orm.Compose(nil, shape).
		With(bad).
		From(gendemo.Users.Source()).
		Join(bad, orm.Cond(orm.Expr[orm.Composed]("TRUE"))).
		SQL()
	if err == nil {
		t.Fatal("a WITH item correlated to the FROM clause of the statement declaring it")
	}
}

// Every part of a composite expression contributes its dependencies. A CASE
// whose ELSE alone is out of scope is out of scope.
func TestAudit_compositeExpressionsDependOnEveryBranch(t *testing.T) {
	stranger := gendemo.Posts.As("stranger")
	inScope := orm.Of(gendemo.Users.Age)

	for _, tt := range []struct {
		name string
		expr orm.Selectable[orm.Composed, int32]
	}{
		{"case condition", orm.Case(orm.Cond(stranger.Score.Gt(int32(1))), inScope).Else(inScope)},
		{"case then", orm.Case(orm.Cond(gendemo.Users.Age.Gt(int32(1))), orm.Of(stranger.Score)).Else(inScope)},
		{"case else", orm.Case(orm.Cond(gendemo.Users.Age.Gt(int32(1))), inScope).Else(orm.Of(stranger.Score))},
		{"coalesce argument", orm.Coalesce(inScope, orm.Opt(stranger.Score))},
		{"arithmetic operand", inScope.AddOf(orm.Of(stranger.Score))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shape := orm.Project1(tt.expr, func(v int32) int32 { return v })
			_, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL()
			if err == nil {
				t.Fatal("an expression with an out-of-scope operand compiled")
			}
			if !strings.Contains(err.Error(), "scope error") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// The same for the parts of a window specification and an aggregate filter.
func TestAudit_windowAndFilterDependenciesAreChecked(t *testing.T) {
	stranger := gendemo.Posts.As("stranger")

	t.Run("partition by", func(t *testing.T) {
		w := orm.Window().PartitionBy(orm.Of(stranger.Score))
		shape := orm.Project1(orm.RowNumber().Over(w), func(n int64) int64 { return n })
		if _, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL(); err == nil {
			t.Fatal("a PARTITION BY over an unattached source compiled")
		}
	})
	t.Run("window order by", func(t *testing.T) {
		w := orm.Window().OrderBy(orm.Of(stranger.Score).Asc())
		shape := orm.Project1(orm.RowNumber().Over(w), func(n int64) int64 { return n })
		if _, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL(); err == nil {
			t.Fatal("a window ORDER BY over an unattached source compiled")
		}
	})
	t.Run("aggregate filter", func(t *testing.T) {
		agg := orm.Count[orm.Composed]().Filter(orm.Cond(stranger.Score.Gt(int32(1))))
		shape := orm.Project1(orm.Of(agg), func(n int64) int64 { return n })
		if _, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL(); err == nil {
			t.Fatal("a FILTER over an unattached source compiled")
		}
	})
}

// userIDs2 runs a composed query returning bigints.
func userIDs2(t *testing.T, q *orm.ComposedQuery[int64]) []int64 {
	t.Helper()
	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got == nil {
		return []int64{}
	}
	return got
}
