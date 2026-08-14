package orm_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
)

// A set operation used as a row source.
//
// A compound is a Subquery like any other statement, so a derived table and a
// WITH item hold one without learning what it is. What these tests are about is
// the typed layer above that: which builders may be a source, what names the
// source provides, and whether one statement comes out with one parameter list.
//
// The SQL is asserted in full. A compound nested inside another statement is the
// place where a missing parenthesis or a restarted parameter produces something
// that still looks like SQL.

// The two branches, and the declarations that address the source they make.
//
// The compound's output names are the first branch's — PostgreSQL's rule — so
// the declarations name that branch's aliases, and the second branch is free to
// call its own columns whatever it likes.
type sourced struct {
	ID    int64
	Label string
}

var (
	srcFromUsers = func() *orm.ComposedQuery[sourced] {
		return orm.Compose(nil, orm.Project2(
			orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Email).As("label"),
			func(id int64, s string) sourced { return sourced{id, s} },
		)).From(usersSrc)
	}
	srcFromPosts = func() *orm.ComposedQuery[sourced] {
		return orm.Compose(nil, orm.Project2(
			orm.Of(Posts.ID).As("post_id"), orm.Cast(orm.Val("post"), orm.Text).As("kind"),
			func(id int64, s string) sourced { return sourced{id, s} },
		)).From(postsSrc)
	}

	outThingID = orm.Named("thing_id", orm.Of(Users.ID))
	outLabel   = orm.Named("label", orm.Of(Users.Email))
)

func unionSource() *orm.UnionQuery[sourced] {
	return orm.UnionAll[sourced](srcFromUsers(), srcFromPosts())
}

func TestUnionSource_isADerivedTable(t *testing.T) {
	u := orm.Sub("u", unionSource())
	shape := orm.Project2(orm.Ref(u, outThingID), orm.Ref(u, outLabel),
		func(id int64, s string) sourced { return sourced{id, s} })

	sql, args, err := orm.Compose(nil, shape).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "u"."thing_id", "u"."label" FROM (` +
		`SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" AS "post_id", CAST($1 AS "text") AS "kind" FROM "public"."posts"` +
		`) AS "u"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 || args[0] != "post" {
		t.Errorf("args = %v, want [post]", args)
	}
}

// The compound is parenthesised because it is a derived table's body, which is
// where the grammar puts the parentheses — the union does not carry its own.
func TestUnionSource_isParenthesisedByTheSourceItBecomes(t *testing.T) {
	u := orm.Sub("u", unionSource())
	sql, _, err := orm.Compose(nil,
		orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id }),
	).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `FROM (SELECT `) || !strings.Contains(sql, `) AS "u"`) {
		t.Errorf("the compound is not wrapped as a derived table:\n%s", sql)
	}
	// One pair of parentheses, not two: the union is not wrapped again inside
	// the source's own.
	if strings.Contains(sql, `FROM ((`) {
		t.Errorf("the compound was parenthesised twice:\n%s", sql)
	}
}

// The alias belongs to the source, not to the compound: the same union becomes
// two independent derived tables with two names.
func TestUnionSource_aliasesIndependently(t *testing.T) {
	a := orm.Sub("a", unionSource())
	b := orm.Sub("b", unionSource())
	shape := orm.Project2(orm.Ref(a, outThingID), orm.Ref(b, outThingID),
		func(x, y int64) sourced { return sourced{x + y, ""} })

	sql, _, err := orm.Compose(nil, shape).From(a).
		Join(b, orm.Eq(orm.Ref(a, outThingID), orm.Ref(b, outThingID))).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `) AS "a"`) || !strings.Contains(sql, `) AS "b"`) {
		t.Errorf("the two derived tables are not named separately:\n%s", sql)
	}
	if !strings.Contains(sql, `SELECT "a"."thing_id", "b"."thing_id"`) {
		t.Errorf("the columns are not qualified by the two aliases:\n%s", sql)
	}
	if n := strings.Count(sql, "UNION ALL"); n != 2 {
		t.Errorf("the union was rendered %d times for two sources", n)
	}
}

// Parameters are numbered across the whole statement in the order the SQL reads:
// the outer select list, then the branches, then the outer WHERE.
func TestUnionSource_numbersEveryParameterAcrossOneStatement(t *testing.T) {
	inner := orm.UnionAll[sourced](
		orm.Compose(nil, orm.Project2(
			orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Email).As("label"),
			func(id int64, s string) sourced { return sourced{id, s} },
		)).From(usersSrc).Where(orm.Of(Users.Age).Gt(18)),
		orm.Compose(nil, orm.Project2(
			orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Email).As("label"),
			func(id int64, s string) sourced { return sourced{id, s} },
		)).From(usersSrc).Where(orm.Of(Users.Age).Lt(65)),
	)
	u := orm.Sub("u", inner)
	shape := orm.Project2(
		orm.Cast(orm.Val("tag"), orm.Text), orm.Ref(u, outLabel),
		func(tag, label string) sourced { return sourced{0, tag + label} },
	)

	sql, args, err := orm.Compose(nil, shape).From(u).
		Where(orm.Ref(u, outThingID).Gt(int64(100))).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT CAST($1 AS "text"), "u"."label" FROM (` +
		`SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users" WHERE "users"."age" > $2 ` +
		`UNION ALL ` +
		`SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users" WHERE "users"."age" < $3` +
		`) AS "u" WHERE "u"."thing_id" > $4`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	wantArgs := []any{"tag", int32(18), int32(65), int64(100)}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("$%d = %#v, want %#v", i+1, args[i], wantArgs[i])
		}
	}
}

func TestUnionSource_isAnOrdinaryCTEBody(t *testing.T) {
	c := orm.CTE("recent", unionSource())
	shape := orm.Project2(orm.Ref(c, outThingID), orm.Ref(c, outLabel),
		func(id int64, s string) sourced { return sourced{id, s} })

	sql, args, err := orm.Compose(nil, shape).With(c).From(c).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `WITH "recent" AS (` +
		`SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" AS "post_id", CAST($1 AS "text") AS "kind" FROM "public"."posts"` +
		`) SELECT "recent"."thing_id", "recent"."label" FROM "recent"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want one", args)
	}
	// WITH is not RECURSIVE: an ordinary item that happens to be a compound is
	// not a recursive one, and writing the keyword would change what PostgreSQL
	// permits inside every other item of the clause.
	if strings.Contains(sql, "RECURSIVE") {
		t.Errorf("an ordinary CTE was written as recursive:\n%s", sql)
	}
}

// The materialization hints apply to a compound body like any other, because
// they are properties of the WITH item rather than of the statement in it.
func TestUnionSource_cteBodyTakesMaterializationHints(t *testing.T) {
	for _, tt := range []struct {
		name string
		opt  orm.CTEOption
		want string
	}{
		{"materialized", orm.Materialized, `AS MATERIALIZED (`},
		{"not materialized", orm.NotMaterialized, `AS NOT MATERIALIZED (`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := orm.CTE("recent", unionSource(), tt.opt)
			shape := orm.Project1(orm.Ref(c, outThingID), func(id int64) int64 { return id })
			sql, _, err := orm.Compose(nil, shape).With(c).From(c).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if !strings.Contains(sql, tt.want) {
				t.Errorf("SQL =\n%s\nwant it to contain %q", sql, tt.want)
			}
		})
	}
}

// A union of a union is a source too, and the inner one keeps its parentheses
// inside the outer one's.
func TestUnionSource_nestedCompoundIsASource(t *testing.T) {
	nested := orm.UnionAll[sourced](unionSource(), srcFromUsers())
	u := orm.Sub("u", nested)
	shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })

	sql, args, err := orm.Compose(nil, shape).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "u"."thing_id" FROM ((` +
		`SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" AS "post_id", CAST($1 AS "text") AS "kind" FROM "public"."posts") ` +
		`UNION ALL ` +
		`SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users"` +
		`) AS "u"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want one", args)
	}
}

// The other two builders are sources as well, and were before this. The
// assertion is that widening Sub and CTE to take a set operation did not change
// what they do with a plain one.
func TestUnionSource_theOtherBuildersAreStillSources(t *testing.T) {
	t.Run("ComposedQuery", func(t *testing.T) {
		u := orm.Sub("u", srcFromUsers())
		shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })
		sql, _, err := orm.Compose(nil, shape).From(u).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		want := `SELECT "u"."thing_id" FROM (SELECT "users"."id" AS "thing_id", ` +
			`"users"."email" AS "label" FROM "public"."users") AS "u"`
		if sql != want {
			t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
		}
	})

	t.Run("SelectQuery", func(t *testing.T) {
		repo := orm.NewRepo(nil, &userMeta)
		q := orm.Select(repo, orm.Project2(
			Users.ID.As("thing_id"), Users.Email.As("label"),
			func(id int64, s string) sourced { return sourced{id, s} },
		))
		u := orm.Sub("u", q)
		shape := orm.Project1(orm.Ref(u, outLabel), func(s string) string { return s })
		sql, _, err := orm.Compose(nil, shape).From(u).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		want := `SELECT "u"."label" FROM (SELECT "users"."id" AS "thing_id", ` +
			`"users"."email" AS "label" FROM "public"."users") AS "u"`
		if sql != want {
			t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
		}
	})

	t.Run("a Rows query, which has no result type", func(t *testing.T) {
		// This is the original derived-table path: a query built to be a source
		// rather than read. It cannot be a union branch, and it is still a
		// source.
		rows := orm.Rows(outThingID, outLabel).From(usersSrc)
		u := orm.Sub("u", rows)
		shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })
		sql, _, err := orm.Compose(nil, shape).From(u).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		want := `SELECT "u"."thing_id" FROM (SELECT "users"."id" AS "thing_id", ` +
			`"users"."email" AS "label" FROM "public"."users") AS "u"`
		if sql != want {
			t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
		}
	})
}

// A union that never validated is not a source. The mistake travels into the
// source and out of the statement, rather than producing SQL from whichever
// branches happened to survive.
func TestUnionSource_refusesAUnionThatDidNotValidate(t *testing.T) {
	wrongWidth := orm.Compose(nil, orm.Project3(
		orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Email).As("label"), orm.Of(Users.Active).As("ok"),
		func(id int64, s string, ok bool) sourced { return sourced{id, s} },
	)).From(usersSrc)
	broken := orm.UnionAll[sourced](srcFromUsers(), wrongWidth)

	t.Run("as a derived table", func(t *testing.T) {
		u := orm.Sub("u", broken)
		shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })
		sql, _, err := orm.Compose(nil, shape).From(u).SQL()
		if err == nil {
			t.Fatalf("a union that did not validate became a derived table:\n%s", sql)
		}
		if !strings.Contains(err.Error(), "2 columns") {
			t.Errorf("the diagnostic %q is not the union's own", err)
		}
	})

	t.Run("as a CTE body", func(t *testing.T) {
		c := orm.CTE("broken", broken)
		shape := orm.Project1(orm.Ref(c, outThingID), func(id int64) int64 { return id })
		if _, _, err := orm.Compose(nil, shape).With(c).From(c).SQL(); err == nil {
			t.Fatal("a union that did not validate became a CTE body")
		}
	})
}

// A compound's output names are its first branch's. A union whose first branch
// does not name its columns has nothing for a source's columns to be addressed
// by, and the diagnostic says which branch to fix — naming the second one would
// not change what PostgreSQL calls them.
func TestUnionSource_refusesAUnionWhoseFirstBranchNamesNothing(t *testing.T) {
	unnamed := orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Email),
		func(id int64, s string) sourced { return sourced{id, s} },
	)).From(usersSrc)
	named := srcFromUsers()

	u := orm.Sub("u", orm.UnionAll[sourced](unnamed, named))
	shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).From(u).SQL()
	if err == nil {
		t.Fatal("a union whose first branch names no column became a derived table")
	}
	if !strings.Contains(err.Error(), "column 1") || !strings.Contains(err.Error(), "first branch") {
		t.Errorf("the diagnostic %q does not say which column and which branch", err)
	}
}

// A column the source does not provide is refused when the statement is built,
// naming the ones it has. The names come from the first branch, so a caller
// reaching for the second branch's alias is told so here rather than by
// PostgreSQL.
func TestUnionSource_refusesAColumnTheFirstBranchDoesNotName(t *testing.T) {
	u := orm.Sub("u", unionSource())
	outKind := orm.Named("kind", orm.Of(Users.Email)) // the second branch's name
	shape := orm.Project1(orm.Ref(u, outKind), func(s string) string { return s })

	_, _, err := orm.Compose(nil, shape).From(u).SQL()
	if err == nil {
		t.Fatal("a column named only by the second branch was accepted")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("the diagnostic %q does not name the column asked for", err)
	}
}

// Becoming a source does not consume the union or disturb its result shape: the
// same value is still a branch, and still reads its own rows.
func TestUnionSource_beingASourceDoesNotEraseTheShape(t *testing.T) {
	u := unionSource()

	// Used as a source once.
	src := orm.Sub("u", u)
	if src.Err() != nil {
		t.Fatalf("Sub: %v", src.Err())
	}

	// Still a branch of another union, with the shape it validated.
	if _, _, err := orm.UnionAll[sourced](u, srcFromUsers()).SQL(); err != nil {
		t.Errorf("after being a source, the union is no longer a branch: %v", err)
	}
	// And a branch of the wrong shape is still refused, so the shape was not
	// merely retained but is still the thing being compared.
	wrong := orm.Compose(nil, orm.Project2(
		orm.Of(Users.Age).As("thing_id"), orm.Of(Users.Email).As("label"),
		func(age int32, s string) sourced { return sourced{int64(age), s} },
	)).From(usersSrc)
	if _, _, err := orm.UnionAll[sourced](u, wrong).SQL(); err == nil {
		t.Error("after being a source, the union accepted a branch of another shape")
	}

	// And it still reads its own rows.
	got, err := u.Using(stubExecutor{rows: [][]any{{int64(1), "a@example.com"}}}).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != 1 {
		t.Errorf("All returned %v", got)
	}

	// Two sources from one union are independent, as two derived tables from one
	// query already were.
	if orm.Sub("a", u) == orm.Sub("b", u) {
		t.Error("two sources built from one union are the same source")
	}
}

// The Phase 2 compatibility rule still decides what may be a source, because a
// source is built from a union and a union is built from branches. These are the
// three cases stated as source outcomes rather than as builder outcomes.
func TestUnionSource_shapeCompatibilityDecidesWhatCanBeASource(t *testing.T) {
	same := orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID).As("other_id"), orm.Of(Users.Email).As("other_label"),
		func(id int64, s string) sourced { return sourced{id, s} },
	)).From(usersSrc)
	otherType := orm.Compose(nil, orm.Project2(
		orm.Of(Users.Age).As("thing_id"), orm.Of(Users.Email).As("label"),
		func(age int32, s string) sourced { return sourced{int64(age), s} },
	)).From(usersSrc)
	otherNullability := orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Nickname).As("label"),
		func(id int64, s *string) sourced { return sourced{id, ""} },
	)).From(usersSrc)

	build := func(second orm.Branch[sourced]) error {
		u := orm.Sub("u", orm.UnionAll[sourced](srcFromUsers(), second))
		shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })
		_, _, err := orm.Compose(nil, shape).From(u).SQL()
		return err
	}

	if err := build(same); err != nil {
		t.Errorf("two branches of one shape were refused as a source: %v", err)
	}
	if err := build(otherType); err == nil {
		t.Error("a branch of another Go type became a source")
	} else if !strings.Contains(err.Error(), "int32") {
		t.Errorf("the diagnostic %q does not name the types", err)
	}
	if err := build(otherNullability); err == nil {
		t.Error("a branch of another nullability became a source")
	}
}

// A source's columns are addressed by name, so every one of them has to have
// one. The diagnostic says which column, because a select list of eight
// expressions is not a useful place to be told "somewhere".
func TestUnionSource_refusesASelectListThatNamesNothing(t *testing.T) {
	unnamed := orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Email).As("label"),
		func(id int64, s string) sourced { return sourced{id, s} },
	)).From(usersSrc)

	t.Run("as a derived table", func(t *testing.T) {
		u := orm.Sub("u", unnamed)
		shape := orm.Project1(orm.Ref(u, outLabel), func(s string) string { return s })
		_, _, err := orm.Compose(nil, shape).From(u).SQL()
		if err == nil {
			t.Fatal("a derived table whose first column has no name was accepted")
		}
		if !strings.Contains(err.Error(), "output column 1") || !strings.Contains(err.Error(), "no name") {
			t.Errorf("the diagnostic %q does not say which column has no name", err)
		}
	})

	t.Run("as a CTE body", func(t *testing.T) {
		c := orm.CTE("c", unnamed)
		shape := orm.Project1(orm.Ref(c, outLabel), func(s string) string { return s })
		_, _, err := orm.Compose(nil, shape).With(c).From(c).SQL()
		if err == nil {
			t.Fatal("a CTE whose first column has no name was accepted")
		}
		if !strings.Contains(err.Error(), "output column 1") || !strings.Contains(err.Error(), "no name") {
			t.Errorf("the diagnostic %q does not say which column has no name", err)
		}
	})
}

// Every join that takes a source takes one built from a set operation, because
// Sub returns a *Source and nothing downstream asks what is inside it.
//
// The outer joins read their nullable side with OptRef, which is the existing
// rule about outer joins rather than anything to do with compounds: a LEFT JOIN
// can leave every column of its right-hand source NULL whatever the inner query
// proved.
func TestUnionSource_everyJoinTakesACompoundSource(t *testing.T) {
	a := orm.Sub("a", unionSource())
	b := orm.Sub("b", unionSource())
	on := orm.Eq(orm.Ref(a, outThingID), orm.Ref(b, outThingID))
	inner := orm.Project2(orm.Ref(a, outThingID), orm.Ref(b, outThingID),
		func(x, y int64) sourced { return sourced{x + y, ""} })
	outer := orm.Project2(orm.Ref(a, outThingID), orm.OptRef(b, outThingID),
		func(x int64, y *int64) sourced { return sourced{x, ""} })

	cases := []struct {
		name  string
		build func() (string, []any, error)
	}{
		{"From", func() (string, []any, error) {
			return orm.Compose(nil, inner).From(a).Join(b, on).SQL()
		}},
		{"Join", func() (string, []any, error) {
			return orm.Compose(nil, inner).From(a).Join(b, on).SQL()
		}},
		{"LeftJoin", func() (string, []any, error) {
			return orm.Compose(nil, outer).From(a).LeftJoin(b, on).SQL()
		}},
		{"RightJoin", func() (string, []any, error) {
			return orm.Compose(nil, outer).From(b).RightJoin(a, on).SQL()
		}},
		{"CrossJoin", func() (string, []any, error) {
			return orm.Compose(nil, inner).From(a).CrossJoin(b).SQL()
		}},
		{"JoinLateral", func() (string, []any, error) {
			return orm.Compose(nil, inner).From(a).JoinLateral(b, on).SQL()
		}},
		{"LeftJoinLateral", func() (string, []any, error) {
			return orm.Compose(nil, outer).From(a).LeftJoinLateral(b, on).SQL()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, _, err := c.build()
			if err != nil {
				t.Fatalf("%s over a compound source: %v", c.name, err)
			}
			if n := strings.Count(sql, "UNION ALL"); n != 2 {
				t.Errorf("%s rendered %d set operations for two compound sources:\n%s", c.name, n, sql)
			}
			if !strings.Contains(sql, `) AS "a"`) || !strings.Contains(sql, `) AS "b"`) {
				t.Errorf("%s lost one of the two aliases:\n%s", c.name, sql)
			}
		})
	}
}

// An entity query still needs a real table occurrence, compound or not. An
// entity's columns are the descriptor's, qualified by the source it names, and a
// derived table is not that source however its columns happen to be spelled.
// This is the rule QueryFrom already had, restated here because a compound
// source is a new way to reach it.
func TestUnionSource_anEntityQueryStillNeedsItsOwnTable(t *testing.T) {
	u := orm.Sub("u", unionSource())
	_, _, err := orm.NewRepo(nil, &userMeta).QueryFrom(u).SQL()
	if err == nil {
		t.Fatal("an entity query read a compound derived table as its own table")
	}
	if !strings.Contains(err.Error(), "not an occurrence of public.users") {
		t.Errorf("the diagnostic %q is not the occurrence rule's", err)
	}
}
