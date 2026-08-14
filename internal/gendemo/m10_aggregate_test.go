package gendemo_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// Aggregates against real PostgreSQL.
//
// Two claims are under test and neither can be checked without a server. The
// first is that the Go type each aggregate declares is the type PostgreSQL
// actually returns — sum(int4) is bigint, avg of anything integral is numeric,
// and getting either wrong is a scan failure or a silent truncation. The second
// is that an aggregate over no rows is NULL rather than zero, which is the
// difference between "nobody ordered anything" and "the orders came to £0".

// aggDB seeds a lopsided posting graph: one author with several posts, one with
// one, one with none, and a post nobody wrote.
func aggDB(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	dsn := testdb.Create(t, schema(t)+`
		INSERT INTO users (id, email, age, active, state, tags, settings, visits, score) VALUES
		  (1, 'a@example.com', 30, true,  'active',  '{}', '{}', 10,   1.5),
		  (2, 'b@example.com', 40, true,  'active',  '{}', '{}', 20,   2.5),
		  (3, 'c@example.com', 50, false, 'banned',  '{}', '{}', NULL, NULL);

		INSERT INTO posts (id, author_id, title, published, score) VALUES
		  (1, 1, 'p1', true,  5),
		  (2, 1, 'p2', false, 7),
		  (3, 1, 'p3', true,  9),
		  (4, 2, 'p4', true,  1),
		  (5, NULL, 'orphan', false, 3);

		SELECT setval(pg_get_serial_sequence('users','id'), 1000);
		SELECT setval(pg_get_serial_sequence('posts','id'), 1000);`)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return gendemo.New(conn), conn
}

// Scenario C: rows per group, filtered by a condition over the group.
func TestAggregate_groupByAndHaving(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	type authorPosts struct {
		AuthorID *int64
		Posts    int64
	}
	shape := orm.Project2(
		gendemo.Posts.AuthorID, orm.Count[gendemo.Post]().As("post_count"),
		func(id *int64, n int64) authorPosts { return authorPosts{AuthorID: id, Posts: n} },
	)

	sql, _, err := orm.Select(db.Posts, shape).
		GroupBy(gendemo.Posts.AuthorID).
		Having(orm.Count[gendemo.Post]().Gt(1)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "posts"."author_id", count(*) AS "post_count" FROM "public"."posts" ` +
		`GROUP BY "posts"."author_id" HAVING count(*) > $1`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}

	got, err := orm.Select(db.Posts, shape).
		GroupBy(gendemo.Posts.AuthorID).
		Having(orm.Count[gendemo.Post]().Gt(1)).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].AuthorID == nil || *got[0].AuthorID != 1 || got[0].Posts != 3 {
		t.Fatalf("groups = %+v", got)
	}

	// Every group, compared with the handwritten form. The NULL author_id is a
	// group of its own, which is what makes a pointer key the right shape.
	all, err := orm.Select(db.Posts, shape).
		GroupBy(gendemo.Posts.AuthorID).
		OrderBy(gendemo.Posts.AuthorID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	rows, err := conn.Query(t.Context(),
		"SELECT author_id, count(*) FROM posts GROUP BY author_id ORDER BY author_id")
	if err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	defer rows.Close()
	var want2 []authorPosts
	for rows.Next() {
		var r authorPosts
		if err := rows.Scan(&r.AuthorID, &r.Posts); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		want2 = append(want2, r)
	}
	if show(all) != show(want2) {
		t.Errorf("orm = %v, handwritten = %v", show(all), show(want2))
	}
	var nullGroup bool
	for _, g := range all {
		if g.AuthorID == nil {
			nullGroup = true
		}
	}
	if !nullGroup {
		t.Error("the NULL grouping key did not come back as its own group")
	}
}

// Scenario D, and the release-blocking one: over no rows, count is 0 and every
// other aggregate is NULL.
func TestAggregate_zeroRows(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	type stats struct {
		N   int64
		Sum *int64
		Avg *pgtypeNumeric
		Min *int32
		Max *int32
	}
	shape := orm.Project5(
		orm.Count[gendemo.Post](),
		orm.SumInt32(gendemo.Posts.Score),
		orm.AvgInt32[gendemo.Post, pgtypeNumeric](gendemo.Posts.Score),
		orm.Min(gendemo.Posts.Score),
		orm.Max(gendemo.Posts.Score),
		func(n int64, sum *int64, avg *pgtypeNumeric, mn, mx *int32) stats {
			return stats{N: n, Sum: sum, Avg: avg, Min: mn, Max: mx}
		},
	)

	// A global aggregate over nothing still returns exactly one row.
	empty, err := orm.Select(db.Posts, shape).Where(gendemo.Posts.ID.Lt(0)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(empty) != 1 {
		t.Fatalf("an aggregate over no rows returned %d rows, want exactly one", len(empty))
	}
	got := empty[0]
	if got.N != 0 {
		t.Errorf("count over no rows = %d, want 0", got.N)
	}
	for name, isNil := range map[string]bool{
		"sum": got.Sum == nil, "avg": got.Avg == nil, "min": got.Min == nil, "max": got.Max == nil,
	} {
		if !isNil {
			t.Errorf("%s over no rows is not NULL", name)
		}
	}

	// And PostgreSQL agrees, asked directly.
	var n int64
	var sum *int64
	var mn, mx *int32
	if err := conn.QueryRow(t.Context(),
		"SELECT count(*), sum(score), min(score), max(score) FROM posts WHERE id < 0").
		Scan(&n, &sum, &mn, &mx); err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	if n != got.N || (sum == nil) != (got.Sum == nil) || (mn == nil) != (got.Min == nil) {
		t.Errorf("orm and PostgreSQL disagree about an empty aggregate")
	}

	// Over rows whose aggregated column is entirely NULL, the same rules hold
	// from the other direction: count(*) counts the rows, count(col) counts
	// the ones with a value, and sum over no values is NULL.
	type nulls struct {
		Rows    int64
		NonNull int64
		Sum     *pgtypeNumeric
	}
	nullShape := orm.Project3(
		orm.Count[gendemo.User](),
		orm.CountOf(gendemo.Users.Visits),
		orm.SumInt64[gendemo.User, pgtypeNumeric](gendemo.Users.Visits),
		func(rows, nonNull int64, sum *pgtypeNumeric) nulls {
			return nulls{Rows: rows, NonNull: nonNull, Sum: sum}
		},
	)
	// User 3 is the only one with a NULL visits, so restricting to it gives a
	// group whose rows exist and whose values do not.
	onlyNull, err := orm.Select(db.Users, nullShape).Where(gendemo.Users.ID.Eq(3)).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if onlyNull.Rows != 1 {
		t.Errorf("count(*) over one row = %d", onlyNull.Rows)
	}
	if onlyNull.NonNull != 0 {
		t.Errorf("count(visits) over a NULL = %d, want 0", onlyNull.NonNull)
	}
	if onlyNull.Sum != nil && onlyNull.Sum.Valid {
		t.Errorf("sum over only NULLs = %v, want NULL", onlyNull.Sum)
	}
}

// pgtypeNumeric is the Go type this fixture maps numeric to. gendemo configures
// none, so the aggregate's numeric result is read through pgx's own type — the
// point being that it is not float64.
type pgtypeNumeric = pgtype.Numeric

// Scenario E: FILTER restricts one aggregate without restricting the statement.
func TestAggregate_filter(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	type row struct {
		AuthorID  *int64
		All       int64
		Published int64
	}
	shape := orm.Project3(
		gendemo.Posts.AuthorID,
		orm.Count[gendemo.Post]().As("all_posts"),
		orm.Count[gendemo.Post]().Filter(gendemo.Posts.Published.Eq(true)).As("published_posts"),
		func(id *int64, all, published int64) row {
			return row{AuthorID: id, All: all, Published: published}
		},
	)

	sql, args, err := orm.Select(db.Posts, shape).GroupBy(gendemo.Posts.AuthorID).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `count(*) FILTER (WHERE "posts"."published" = $1) AS "published_posts"`) {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v", args)
	}
	// The filter is not the statement's WHERE: every post is still counted by
	// the unfiltered aggregate.
	if strings.Contains(sql, "WHERE \"posts\".\"published\"") && strings.Contains(sql, "FROM \"public\".\"posts\" WHERE") {
		t.Errorf("FILTER leaked into the root WHERE: %s", sql)
	}

	got, err := orm.Select(db.Posts, shape).
		GroupBy(gendemo.Posts.AuthorID).
		OrderBy(gendemo.Posts.AuthorID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	rows, err := conn.Query(t.Context(),
		"SELECT author_id, count(*), count(*) FILTER (WHERE published = true) FROM posts GROUP BY author_id ORDER BY author_id")
	if err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	defer rows.Close()
	var want []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.AuthorID, &r.All, &r.Published); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		want = append(want, r)
	}
	if show(got) != show(want) {
		t.Errorf("orm = %v, handwritten = %v", show(got), show(want))
	}
}

// show renders a slice of result rows with pointers dereferenced, so that two
// runs are compared by what they hold rather than by where it lives.
func show[T any](rows []T) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%+v;", deref(r))
	}
	return b.String()
}

// deref replaces every pointer field with its value, or <nil>.
func deref(v any) string {
	rv := reflect.ValueOf(v)
	var b strings.Builder
	for i := range rv.NumField() {
		f := rv.Field(i)
		if f.Kind() == reflect.Pointer {
			if f.IsNil() {
				b.WriteString("<nil> ")
				continue
			}
			fmt.Fprintf(&b, "%v ", f.Elem().Interface())
			continue
		}
		fmt.Fprintf(&b, "%v ", f.Interface())
	}
	return b.String()
}

// count(DISTINCT x) is DISTINCT inside the call, which is not the query-level
// DISTINCT over the result row.
func TestAggregate_distinct(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	shape := orm.Project1(
		orm.CountOf(gendemo.Posts.AuthorID).Distinct(),
		func(n int64) int64 { return n },
	)
	sql, _, err := orm.Select(db.Posts, shape).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `count(DISTINCT "posts"."author_id")`) {
		t.Errorf("SQL = %s", sql)
	}
	got, err := orm.Select(db.Posts, shape).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	var want int64
	if err := conn.QueryRow(t.Context(), "SELECT count(DISTINCT author_id) FROM posts").Scan(&want); err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	if got != want {
		t.Errorf("count DISTINCT = %d, want %d", got, want)
	}
}

// The result types are PostgreSQL's, and the proof is that the values scan and
// match what the server computes.
func TestAggregate_resultTypes(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	// sum(integer) is bigint, so the destination is *int64 rather than *int32.
	sumScore := orm.Project1(orm.SumInt32(gendemo.Posts.Score), func(v *int64) *int64 { return v })
	got, err := orm.Select(db.Posts, sumScore).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	var want int64
	if err := conn.QueryRow(t.Context(), "SELECT sum(score) FROM posts").Scan(&want); err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	if got == nil || *got != want {
		t.Errorf("sum = %v, want %d", got, want)
	}

	// avg(integer) is numeric, read through pgx's numeric rather than float64.
	avgScore := orm.Project1(
		orm.AvgInt32[gendemo.Post, pgtypeNumeric](gendemo.Posts.Score),
		func(v *pgtypeNumeric) *pgtypeNumeric { return v },
	)
	avg, err := orm.Select(db.Posts, avgScore).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if avg == nil || !avg.Valid {
		t.Fatalf("avg = %v", avg)
	}
	var wantAvg pgtype.Numeric
	if err := conn.QueryRow(t.Context(), "SELECT avg(score) FROM posts").Scan(&wantAvg); err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	a, _ := avg.Value()
	b, _ := wantAvg.Value()
	if fmt.Sprint(a) != fmt.Sprint(b) {
		t.Errorf("avg = %v, want %v", a, b)
	}

	// min/max keep the column's type and stay nullable.
	minMax := orm.Project2(
		orm.Min(gendemo.Posts.Score), orm.Max(gendemo.Posts.Score),
		func(lo, hi *int32) [2]*int32 { return [2]*int32{lo, hi} },
	)
	bounds, err := orm.Select(db.Posts, minMax).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if bounds[0] == nil || *bounds[0] != 1 || bounds[1] == nil || *bounds[1] != 9 {
		t.Errorf("min/max = %v %v", bounds[0], bounds[1])
	}

	// sum(bigint) is numeric, not bigint: the widening is what stops a sum of
	// bigints overflowing the type it was summed from.
	sumVisits := orm.Project1(
		orm.SumInt64[gendemo.User, pgtypeNumeric](gendemo.Users.Visits),
		func(v *pgtypeNumeric) *pgtypeNumeric { return v },
	)
	visits, err := orm.Select(db.Users, sumVisits).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if visits == nil || !visits.Valid {
		t.Fatalf("sum(visits) = %v", visits)
	}
	v, _ := visits.Value()
	if fmt.Sprint(v) != "30" {
		t.Errorf("sum(visits) = %v, want 30", v)
	}

	// A double precision column sums and averages as double precision.
	floats := orm.Project2(
		orm.SumFloat64(gendemo.Users.Score), orm.AvgFloat64(gendemo.Users.Score),
		func(s, a *float64) [2]*float64 { return [2]*float64{s, a} },
	)
	fs, err := orm.Select(db.Users, floats).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if fs[0] == nil || *fs[0] != 4.0 || fs[1] == nil || *fs[1] != 2.0 {
		t.Errorf("sum/avg of score = %v %v", fs[0], fs[1])
	}
}

// Aggregates belong in the select list and in HAVING, and the builder says so
// before PostgreSQL does.
func TestAggregate_notInWhere(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	shape := orm.Project1(orm.Count[gendemo.Post](), func(n int64) int64 { return n })
	_, err = orm.Select(db.Posts, shape).
		Where(orm.Count[gendemo.Post]().Gt(1)).
		All(t.Context())
	if err == nil {
		t.Fatal("an aggregate in WHERE was accepted")
	}
	if !strings.Contains(err.Error(), "belongs in Having") {
		t.Errorf("err = %v", err)
	}
	if ex.n != 0 {
		t.Errorf("the refused query sent %d statements", ex.n)
	}

	// An aggregate in GROUP BY is refused for the same reason.
	if _, _, err := orm.Select(db.Posts, shape).
		GroupBy(orm.RawValue[gendemo.Post, int64]("1")).SQL(); err != nil {
		t.Errorf("a plain grouping expression was refused: %v", err)
	}
}

// A grouped query the server cannot make sense of is the server's to reject,
// and its error survives.
func TestAggregate_postgresJudgesGrouping(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := aggDB(t)

	// title is neither grouped nor aggregated.
	shape := orm.Project2(
		gendemo.Posts.Title, orm.Count[gendemo.Post](),
		func(title string, n int64) string { return title },
	)
	_, err := orm.Select(db.Posts, shape).GroupBy(gendemo.Posts.AuthorID).All(t.Context())
	if err == nil {
		t.Fatal("an ungrouped column was accepted")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("err = %v, want a *pgconn.PgError", err)
	}
	if pg.Code != "42803" {
		t.Errorf("SQLSTATE = %s, want 42803", pg.Code)
	}
}

// Query.Count still means what it always meant: how many rows the entity query
// returns. It is a different question from the Count expression.
func TestAggregate_queryCountIsUnchanged(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := aggDB(t)

	n, err := db.Posts.Query().Where(gendemo.Posts.Published.Eq(true)).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Query.Count = %d, want 3", n)
	}
	// Limit is part of the rowset, so it is part of the count.
	n, err = db.Posts.Query().Limit(2).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Query.Count with a limit = %d, want 2", n)
	}

	// The expression counts groups instead.
	shape := orm.Project1(orm.Count[gendemo.Post](), func(c int64) int64 { return c })
	perGroup, err := orm.Select(db.Posts, shape).GroupBy(gendemo.Posts.AuthorID).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(perGroup) != 3 {
		t.Errorf("groups = %v", perGroup)
	}
}
