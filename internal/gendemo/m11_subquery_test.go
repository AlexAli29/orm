package gendemo_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// M11.1: subqueries and derived tables.
//
// The claim under test is that a typed query becomes a typed source without
// losing result types, column names, source identity or NULL semantics — and
// that nesting one statement inside another produces one statement, with one
// parameter list, numbered in the order the SQL reads.

// The declarations a derived source is built from. They are values, built once,
// and they are the select list and the output column names at the same time:
// nothing has to be kept in step by hand.
var (
	statsUserID = orm.Named("user_id", orm.Of(gendemo.Posts.AuthorID))
	statsCount  = orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))
)

// postStats is the aggregate subquery every derived-table test reads from.
func postStats() *orm.ComposedQuery[orm.NoResult] {
	return orm.Rows(statsUserID, statsCount).
		From(gendemo.Posts.Source()).
		GroupBy(orm.Of(gendemo.Posts.AuthorID))
}

func TestDerived_compilesAsAFromItemWithTypedColumns(t *testing.T) {
	stats := orm.Sub("post_stats", postStats())

	shape := orm.Project2(
		orm.Ref(stats, statsUserID), orm.Ref(stats, statsCount),
		func(id *int64, n int64) [2]int64 {
			var out [2]int64
			if id != nil {
				out[0] = *id
			}
			out[1] = n
			return out
		},
	)
	sql, args, err := orm.Compose(nil, shape).From(stats).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "post_stats"."user_id", "post_stats"."post_count" FROM (` +
		`SELECT "posts"."author_id" AS "user_id", count(*) AS "post_count" ` +
		`FROM "public"."posts" GROUP BY "posts"."author_id") AS "post_stats"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// The inner statement's parameters and the outer one's are one list, numbered
// in the order the SQL is written. Compiling the inner query separately and
// splicing it in would restart at $1 and bind the wrong values.
func TestNesting_hasOneParameterNamespace(t *testing.T) {
	inner := orm.Rows(statsUserID, statsCount).
		From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Posts.Published.Eq(true))).
		GroupBy(orm.Of(gendemo.Posts.AuthorID))
	stats := orm.Sub("s", inner)

	shape := orm.Project1(orm.Ref(stats, statsCount), func(n int64) int64 { return n })
	sql, args, err := orm.Compose(nil, shape).
		From(stats).
		Where(orm.Ref(stats, statsCount).Gt(int64(1))).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `"posts"."published" = $1`) {
		t.Errorf("the inner parameter is not $1: %s", sql)
	}
	if !strings.Contains(sql, `"s"."post_count" > $2`) {
		t.Errorf("the outer parameter is not $2: %s", sql)
	}
	if len(args) != 2 || args[0] != true || args[1] != int64(1) {
		t.Errorf("args = %v, want [true 1] in that order", args)
	}
	if strings.Count(sql, "$1") != 1 || strings.Count(sql, "$2") != 1 {
		t.Errorf("a placeholder is repeated: %s", sql)
	}
}

// A derived source is a value. Aliasing one, or building two from one query,
// leaves the query and the first source untouched.
func TestDerived_aliasesAreIndependent(t *testing.T) {
	q := postStats()
	a := orm.Sub("a", q)
	b := orm.Sub("b", q)
	if a == b {
		t.Fatal("two derived tables from one query are the same source")
	}
	if a.Ref() != "a" || b.Ref() != "b" {
		t.Errorf("refs = %q, %q", a.Ref(), b.Ref())
	}
	c := a.As("c")
	if a.Ref() != "a" {
		t.Errorf("As mutated the source it was called on: %q", a.Ref())
	}
	if c.Ref() != "c" {
		t.Errorf("the aliased source refs %q", c.Ref())
	}

	// The same query definition compiles independently in two statements, so
	// nothing about a compilation is retained in it.
	shape := orm.Project1(orm.Ref(a, statsCount), func(n int64) int64 { return n })
	first, _, err := orm.Compose(nil, shape).From(a).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	second, _, err := orm.Compose(nil, shape).From(a).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if first != second {
		t.Errorf("compiling one query twice produced two statements:\n%s\n%s", first, second)
	}
}

func TestDerived_refusesTwoColumnsOfOneName(t *testing.T) {
	a := orm.Named("n", orm.Of(gendemo.Posts.ID))
	b := orm.Named("n", orm.Of(gendemo.Posts.Score))
	_, _, err := orm.Rows(a, b).From(gendemo.Posts.Source()).SQL()
	if err == nil {
		t.Fatal("a row source with two columns named n compiled")
	}
	if !strings.Contains(err.Error(), `two outputs are named "n"`) {
		t.Errorf("error = %v", err)
	}
}

// The column list of a derived source is known, so naming a column it does not
// have is a mistake this package can report — with the columns it does have.
func TestDerived_refusesAColumnItDoesNotProvide(t *testing.T) {
	stats := orm.Sub("post_stats", postStats())
	other := orm.Named("elsewhere", orm.Of(gendemo.Posts.Title))

	shape := orm.Project1(orm.Ref(stats, other), func(s string) string { return s })
	_, _, err := orm.Compose(nil, shape).From(stats).SQL()
	if err == nil {
		t.Fatal("a reference to a column the derived table does not provide compiled")
	}
	if !strings.Contains(err.Error(), `has no column "elsewhere"`) ||
		!strings.Contains(err.Error(), "post_count") {
		t.Errorf("error = %v, want it to name the column and list what the source provides", err)
	}
}

// A derived table is evaluated once, independently of the sources beside it, so
// it cannot name one of them. PostgreSQL says so too; saying it here names the
// derived table rather than a column that does not exist.
func TestDerived_refusesCorrelationWithoutLateral(t *testing.T) {
	latest := orm.Sub("latest", orm.Rows(orm.Named("id", orm.Of(gendemo.Posts.ID))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)))

	shape := orm.Project1(orm.Ref(latest, orm.Named("id", orm.Of(gendemo.Posts.ID))),
		func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).From(latest).SQL()
	if err == nil {
		t.Fatal("a derived table correlated to a sibling compiled without LATERAL")
	}
}

// Scope is not a formality. A projection naming a source the statement never
// introduces is refused before PostgreSQL is asked about it.
func TestComposed_refusesASourceTheQueryDoesNotSelectFrom(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Posts.Title), func(s string) string { return s })
	_, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL()
	if err == nil {
		t.Fatal("a projection over posts compiled in a query over users")
	}
	if !strings.Contains(err.Error(), "scope error") {
		t.Errorf("error = %v", err)
	}
}

// A scalar subquery is nullable whatever it selects, because a statement that
// matches no row yields NULL rather than no value.
func TestScalar_isNullWhenTheSubqueryMatchesNothing(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	latest := orm.Scalar[gendemo.User, time.Time](
		orm.Rows(orm.NamedNull("m", orm.Max(gendemo.Posts.CreatedAt))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)),
	)
	type row struct {
		ID     int64
		Latest *time.Time
	}
	shape := orm.Project2(gendemo.Users.ID, latest,
		func(id int64, at *time.Time) row { return row{ID: id, Latest: at} })

	got, err := orm.Select(db.Users, shape).OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	// Users 1 and 3 have posts; user 2 has none, and its scalar subquery
	// returns no row at all.
	if got[0].Latest == nil {
		t.Errorf("user 1 has posts and read NULL")
	}
	if got[1].Latest != nil {
		t.Errorf("user 2 has no posts and read %v", *got[1].Latest)
	}
}

// Even a count, which cannot itself be NULL, is NULL through a scalar subquery
// that returns no row. This is why the wrapper is nullable by construction.
func TestScalar_countOfNoRowsIsStillNull(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	n := orm.Scalar[gendemo.User, int64](
		orm.Rows(orm.Named("n", orm.Of(orm.Count[orm.Composed]()))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
			// A grouped statement produces no row for a user with no posts,
			// where an ungrouped count would produce one row holding zero.
			GroupBy(orm.Of(gendemo.Posts.AuthorID)),
	)
	type row struct {
		ID int64
		N  *int64
	}
	shape := orm.Project2(gendemo.Users.ID, n,
		func(id int64, n *int64) row { return row{ID: id, N: n} })

	got, err := orm.Select(db.Users, shape).OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got[1].N != nil {
		t.Errorf("a grouped count over no rows read %d, want NULL", *got[1].N)
	}
	if got[0].N == nil || *got[0].N != 2 {
		t.Errorf("user 1 counted %v, want 2", got[0].N)
	}
}

// More than one row is PostgreSQL's cardinality violation, and it arrives as
// PostgreSQL's own error rather than as something this package invented.
func TestScalar_moreThanOneRowIsTheServersError(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	titles := orm.Scalar[gendemo.User, string](
		orm.Rows(orm.Named("t", orm.Of(gendemo.Posts.Title))).From(gendemo.Posts.Source()),
	)
	shape := orm.Project1(titles, func(s *string) *string { return s })
	_, err := orm.Select(db.Users, shape).All(t.Context())
	if err == nil {
		t.Fatal("a scalar subquery returning three rows succeeded")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("error = %v, want a *pgconn.PgError", err)
	}
	if pg.Code != "21000" {
		t.Errorf("SQLSTATE = %s, want 21000 (cardinality violation)", pg.Code)
	}
}

// EXISTS and NOT EXISTS against a handwritten statement, on the same data.
func TestExists_matchesHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	hasPost := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID))

	for _, tt := range []struct {
		name string
		orm  orm.Predicate[gendemo.User]
		sql  string
	}{
		{
			"exists",
			orm.Exists[gendemo.User](hasPost),
			`SELECT id FROM users u WHERE EXISTS (SELECT 1 FROM posts p WHERE p.author_id = u.id) ORDER BY id`,
		},
		{
			"not exists",
			orm.NotExists[gendemo.User](hasPost),
			`SELECT id FROM users u WHERE NOT EXISTS (SELECT 1 FROM posts p WHERE p.author_id = u.id) ORDER BY id`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.Users.Query().Where(tt.orm).OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			ids := make([]int64, 0, len(got))
			for _, u := range got {
				ids = append(ids, u.ID)
			}
			assertIDs(t, conn, tt.sql, ids)
		})
	}
}

// IN and NOT IN over a subquery, and the NULL case that makes them different
// questions from EXISTS.
func TestInSubquery_keepsPostgreSQLsThreeValuedLogic(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	// posts.author_id is nullable and one post has none, so the subquery
	// yields a NULL and NOT IN is UNKNOWN for every row.
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, created_at) VALUES (99, NULL, 'orphan', now())`)

	authors := orm.Rows(orm.Named("author_id", orm.Of(gendemo.Posts.AuthorID))).
		From(gendemo.Posts.Source())

	t.Run("in", func(t *testing.T) {
		got := userIDs(t, db.Users.Query().
			Where(orm.InSub(gendemo.Users.ID, authors)).
			OrderBy(gendemo.Users.ID.Asc()))
		assertIDs(t, conn,
			`SELECT id FROM users WHERE id IN (SELECT author_id FROM posts) ORDER BY id`, got)
	})

	t.Run("not in with a null", func(t *testing.T) {
		got := userIDs(t, db.Users.Query().
			Where(orm.NotInSub(gendemo.Users.ID, authors)).
			OrderBy(gendemo.Users.ID.Asc()))
		assertIDs(t, conn,
			`SELECT id FROM users WHERE id NOT IN (SELECT author_id FROM posts) ORDER BY id`, got)
		if len(got) != 0 {
			t.Errorf("NOT IN over a subquery yielding NULL returned %v; PostgreSQL returns nothing", got)
		}
	})

	t.Run("not exists is a different question", func(t *testing.T) {
		hasPost := orm.Rows(orm.Named("x", orm.Val(1))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID))
		got := userIDs(t, db.Users.Query().
			Where(orm.NotExists[gendemo.User](hasPost)).
			OrderBy(gendemo.Users.ID.Asc()))
		if len(got) == 0 {
			t.Error("NOT EXISTS returned nothing; it is not NOT IN and should not behave like it")
		}
	})
}

// A subquery may correlate to the query above it, and that one to the query
// above it: the scope is a stack of frames rather than one parent.
func TestCorrelation_nestsTwoLevels(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	m11exec(t, conn, `INSERT INTO comments (id, post_id, author_id, body) VALUES (1, 1, 3, 'hi'), (2, 2, 2, 'yo')`)

	// users -> posts -> comments, where the innermost statement names a source
	// two levels out.
	comments := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Comments.Source()).
		Where(orm.Cond(orm.Expr[orm.Composed](`"comments"."post_id" = "posts"."id"`)),
			orm.Cond(orm.Expr[orm.Composed](`"comments"."author_id" <> "users"."id"`)))
	posts := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID),
			orm.Exists[orm.Composed](comments))

	got := userIDs(t, db.Users.Query().
		Where(orm.Exists[gendemo.User](posts)).
		OrderBy(gendemo.Users.ID.Asc()))
	assertIDs(t, conn, `
		SELECT id FROM users u
		WHERE EXISTS (
		    SELECT 1 FROM posts p
		    WHERE p.author_id = u.id
		      AND EXISTS (
		          SELECT 1 FROM comments c
		          WHERE c.post_id = p.id AND c.author_id <> u.id
		      )
		)
		ORDER BY id`, got)
}

// A derived table read end to end, against the handwritten statement.
func TestDerived_matchesHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	stats := orm.Sub("post_stats", postStats())
	type row struct {
		UserID int64
		Count  int64
	}
	shape := orm.Project2(
		orm.Ref(stats, statsUserID), orm.Ref(stats, statsCount),
		func(id *int64, n int64) row {
			var out row
			if id != nil {
				out.UserID = *id
			}
			out.Count = n
			return out
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(stats).
		OrderBy(orm.Ref(stats, statsUserID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	rows, err := conn.Query(t.Context(), `
		SELECT user_id, post_count FROM (
		    SELECT author_id AS user_id, count(*) AS post_count
		    FROM posts GROUP BY author_id
		) s ORDER BY user_id`)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	var want []row
	for rows.Next() {
		var id *int64
		var n int64
		if err := rows.Scan(&id, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var r row
		if id != nil {
			r.UserID = *id
		}
		r.Count = n
		want = append(want, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, handwritten SQL returned %+v", i, got[i], want[i])
		}
	}
}
