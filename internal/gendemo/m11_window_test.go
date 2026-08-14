package gendemo_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M11.5: window functions.
//
// The claims are that a window function is typed the way PostgreSQL types it —
// row_number is a non-nullable bigint, lag is nullable whatever it reads — that
// a window's own ordering never leaks into the statement's, and that filtering
// on a window result works the way PostgreSQL allows it to: by computing it in
// a derived table and filtering outside.

// byAuthor is the window most of these tests compute over: posts grouped by
// their author, newest first.
func byAuthor() *orm.WindowDef {
	return orm.Window().
		PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(orm.Of(gendemo.Posts.CreatedAt).Desc())
}

func TestWindow_rankingFunctionsMatchHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, score, created_at) VALUES
	    (10, 1, 'a', 5, '2024-04-01T09:00:00Z'),
	    (11, 1, 'b', 5, '2024-05-01T09:00:00Z')`)

	type row struct {
		ID   int64
		N    int64
		Rank int64
		Dupe int64
	}
	shape := orm.Project4(
		orm.Of(gendemo.Posts.ID),
		orm.RowNumber().Over(byAuthor()),
		orm.Rank().Over(orm.Window().
			PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
			OrderBy(orm.Of(gendemo.Posts.Score).Desc())),
		orm.DenseRank().Over(orm.Window().
			PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
			OrderBy(orm.Of(gendemo.Posts.Score).Desc())),
		func(id, n, rank, dense int64) row {
			return row{ID: id, N: n, Rank: rank, Dupe: dense}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	rows, err := conn.Query(t.Context(), `
		SELECT id,
		       row_number() OVER (PARTITION BY author_id ORDER BY created_at DESC),
		       rank()       OVER (PARTITION BY author_id ORDER BY score DESC),
		       dense_rank() OVER (PARTITION BY author_id ORDER BY score DESC)
		FROM posts ORDER BY id`)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var want row
		if err := rows.Scan(&want.ID, &want.N, &want.Rank, &want.Dupe); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(got) {
			t.Fatalf("the ORM returned only %d rows", len(got))
		}
		if got[i] != want {
			t.Errorf("row %d = %+v, handwritten SQL returned %+v", i, got[i], want)
		}
		i++
	}
	if i != len(got) {
		t.Errorf("the ORM returned %d rows, handwritten SQL %d", len(got), i)
	}
}

// lag and lead read a row that may not exist, so both are nullable whatever
// they read — here, a NOT NULL column.
func TestWindow_lagAndLeadAreNullable(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	type row struct {
		ID   int64
		Prev *int64
		Next *int64
	}
	shape := orm.Project3(
		orm.Of(gendemo.Posts.ID),
		orm.Lag(gendemo.Posts.ID).Over(byAuthor()),
		orm.Lead(gendemo.Posts.ID).Over(byAuthor()),
		func(id int64, prev, next *int64) row { return row{ID: id, Prev: prev, Next: next} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	nulls := 0
	for _, r := range got {
		if r.Prev == nil {
			nulls++
		}
	}
	if nulls == 0 {
		t.Error("no row had a NULL lag; the first row of every partition should have one")
	}

	rows, err := conn.Query(t.Context(), `
		SELECT id,
		       lag(id)  OVER (PARTITION BY author_id ORDER BY created_at DESC),
		       lead(id) OVER (PARTITION BY author_id ORDER BY created_at DESC)
		FROM posts ORDER BY id`)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var want row
		if err := rows.Scan(&want.ID, &want.Prev, &want.Next); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got[i].ID != want.ID || !samePtr(got[i].Prev, want.Prev) || !samePtr(got[i].Next, want.Next) {
			t.Errorf("row %d = %+v, handwritten SQL returned %+v", i, got[i], want)
		}
		i++
	}
}

func samePtr(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// A windowed aggregate keeps its own nullability and does not collapse rows.
func TestWindow_aggregateOverKeepsItsRowsAndItsNullability(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	type row struct {
		ID    int64
		Total int64
		Seen  int64
	}
	shape := orm.Project3(
		orm.Of(gendemo.Posts.ID),
		orm.OfNull(orm.SumInt32(gendemo.Posts.Score).Over(byAuthor())),
		orm.Of(orm.Count[gendemo.Post]().Over(byAuthor())),
		func(id int64, total *int64, seen int64) row {
			var out row
			out.ID, out.Seen = id, seen
			if total != nil {
				out.Total = *total
			}
			return out
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Three posts are seeded and a windowed aggregate collapses none of them.
	if len(got) != 3 {
		t.Fatalf("a windowed aggregate returned %d rows, want one per post", len(got))
	}
	want := handwrittenIDs(t, conn, `
		SELECT count(*) OVER (PARTITION BY author_id ORDER BY created_at DESC)
		FROM posts ORDER BY id`)
	for i, r := range got {
		if r.Seen != want[i] {
			t.Errorf("row %d counted %d, handwritten SQL counted %d", i, r.Seen, want[i])
		}
	}
}

func TestWindow_frames(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	running := orm.Window().
		PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		Rows(orm.UnboundedPreceding(), orm.CurrentRow())

	shape := orm.Project2(
		orm.Of(gendemo.Posts.ID),
		orm.Of(orm.Count[gendemo.Post]().Over(running)),
		func(id, n int64) int64 { return n },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		SELECT count(*) OVER (
		    PARTITION BY author_id ORDER BY id
		    ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW
		) FROM posts ORDER BY id`)
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %d, handwritten SQL = %d", i, got[i], want[i])
		}
	}

	// A RANGE frame compiles too, and the two are different clauses.
	ranged := orm.Window().
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		Range(orm.UnboundedPreceding(), orm.UnboundedFollowing())
	sql, _, err := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Count[gendemo.Post]().Over(ranged)), func(n int64) int64 { return n },
	)).From(gendemo.Posts.Source()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, "RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING") {
		t.Errorf("SQL = %s", sql)
	}
}

func TestWindow_refusesAnImpossibleFrame(t *testing.T) {
	for _, tt := range []struct {
		name   string
		window *orm.WindowDef
		want   string
	}{
		{
			"starting at unbounded following",
			orm.Window().Rows(orm.UnboundedFollowing(), orm.CurrentRow()),
			"cannot start at UNBOUNDED FOLLOWING",
		},
		{
			"ending at unbounded preceding",
			orm.Window().Rows(orm.CurrentRow(), orm.UnboundedPreceding()),
			"cannot end at UNBOUNDED PRECEDING",
		},
		{
			"starting after it ends",
			orm.Window().Rows(orm.Following(1), orm.Preceding(1)),
			"cannot start after it ends",
		},
		{
			"a negative offset",
			orm.Window().Rows(orm.Preceding(-1), orm.CurrentRow()),
			"cannot be negative",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shape := orm.Project1(orm.RowNumber().Over(tt.window), func(n int64) int64 { return n })
			_, _, err := orm.Compose(nil, shape).From(gendemo.Posts.Source()).SQL()
			if err == nil {
				t.Fatal("an impossible frame compiled")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// A window's ordering belongs to the window. It never joins the statement's.
func TestWindow_orderingDoesNotLeakIntoTheStatement(t *testing.T) {
	shape := orm.Project1(orm.RowNumber().Over(byAuthor()), func(n int64) int64 { return n })
	sql, _, err := orm.Compose(nil, shape).
		From(gendemo.Posts.Source()).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `ORDER BY "posts"."id" ASC`) {
		t.Errorf("the statement's ORDER BY is not the one that was asked for: %s", sql)
	}
	if strings.Count(sql, "ORDER BY") != 2 {
		t.Errorf("the window ordering and the statement ordering are not separate: %s", sql)
	}
}

// PostgreSQL computes windows after WHERE, GROUP BY and HAVING have chosen the
// rows, so a window function cannot appear in any of them.
func TestWindow_isRefusedWherePostgreSQLForbidsIt(t *testing.T) {
	rn := orm.RowNumber().Over(byAuthor())
	shape := orm.Project1(orm.Of(gendemo.Posts.ID), func(id int64) int64 { return id })

	for _, tt := range []struct {
		name  string
		build func() *orm.ComposedQuery[int64]
	}{
		{"where", func() *orm.ComposedQuery[int64] {
			return orm.Compose(nil, shape).From(gendemo.Posts.Source()).Where(rn.Eq(int64(1)))
		}},
		{"having", func() *orm.ComposedQuery[int64] {
			return orm.Compose(nil, shape).From(gendemo.Posts.Source()).Having(rn.Eq(int64(1)))
		}},
		{"group by", func() *orm.ComposedQuery[int64] {
			return orm.Compose(nil, shape).From(gendemo.Posts.Source()).GroupBy(rn)
		}},
		{"join condition", func() *orm.ComposedQuery[int64] {
			return orm.Compose(nil, shape).
				From(gendemo.Posts.Source()).
				Join(gendemo.Users.Source(), rn.Eq(int64(1)))
		}},
		{"distinct on", func() *orm.ComposedQuery[int64] {
			return orm.Compose(nil, shape).From(gendemo.Posts.Source()).DistinctOn(rn)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.build().SQL()
			if err == nil {
				t.Fatal("a window function was accepted in a clause PostgreSQL forbids")
			}
			var clause *orm.WindowClauseError
			if !errors.As(err, &clause) {
				t.Fatalf("error = %v, want a *orm.WindowClauseError", err)
			}
			if !strings.Contains(err.Error(), "derived table") {
				t.Errorf("error = %v, want it to say how to filter on a window", err)
			}
		})
	}
}

// Scenario K: the newest post per author, computed with a window inside a
// derived table and filtered outside — which is how PostgreSQL allows it, and
// the proof that the M11 abstractions compose.
func TestWindow_derivedTableFiltering(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	postID := orm.Named("id", orm.Of(gendemo.Posts.ID))
	author := orm.Named("author_id", orm.Of(gendemo.Posts.AuthorID))
	rn := orm.Named("rn", orm.RowNumber().Over(byAuthor()))

	ranked := orm.Sub("p", orm.Rows(postID, author, rn).From(gendemo.Posts.Source()))

	shape := orm.Project2(
		orm.Ref(ranked, author), orm.Ref(ranked, postID),
		func(a *int64, id int64) [2]int64 {
			var out [2]int64
			if a != nil {
				out[0] = *a
			}
			out[1] = id
			return out
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(ranked).
		Where(orm.Ref(ranked, rn).Eq(int64(1))).
		OrderBy(orm.Ref(ranked, postID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		SELECT id FROM (
		    SELECT id, author_id,
		           row_number() OVER (PARTITION BY author_id ORDER BY created_at DESC) AS rn
		    FROM posts
		) p WHERE rn = 1 ORDER BY id`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	for i := range got {
		if got[i][1] != want[i] {
			t.Errorf("row %d kept post %d, handwritten SQL kept %d", i, got[i][1], want[i])
		}
	}
}

// Every window result type this package claims, against the server.
func TestPGTypeOf_M11Windows(t *testing.T) {
	testdb.AdminDSN(t)
	_, conn := m11env(t)

	for _, tt := range []struct {
		what string
		sql  string
		want string
	}{
		{"row_number", `SELECT pg_typeof(row_number() OVER ()) FROM posts LIMIT 1`, "bigint"},
		{"rank", `SELECT pg_typeof(rank() OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"dense_rank", `SELECT pg_typeof(dense_rank() OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"percent_rank", `SELECT pg_typeof(percent_rank() OVER (ORDER BY id)) FROM posts LIMIT 1`, "double precision"},
		{"cume_dist", `SELECT pg_typeof(cume_dist() OVER (ORDER BY id)) FROM posts LIMIT 1`, "double precision"},
		{"ntile", `SELECT pg_typeof(ntile(2) OVER (ORDER BY id)) FROM posts LIMIT 1`, "integer"},
		{"lag", `SELECT pg_typeof(lag(id, 1) OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"lead", `SELECT pg_typeof(lead(id, 1) OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"first_value", `SELECT pg_typeof(first_value(id) OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"last_value", `SELECT pg_typeof(last_value(id) OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"nth_value", `SELECT pg_typeof(nth_value(id, 2) OVER (ORDER BY id)) FROM posts LIMIT 1`, "bigint"},
		{"windowed count", `SELECT pg_typeof(count(*) OVER ()) FROM posts LIMIT 1`, "bigint"},
		{"windowed sum of integer", `SELECT pg_typeof(sum(score) OVER ()) FROM posts LIMIT 1`, "bigint"},
	} {
		t.Run(tt.what, func(t *testing.T) {
			if got := pgTypeOf(t, conn, tt.sql); got != tt.want {
				t.Errorf("pg_typeof = %q, this package claims %q", got, tt.want)
			}
		})
	}
}

// The SQL the window constructors emit is the SQL the pg_typeof matrix was
// written against.
func TestWindow_compilesToTheCheckedSQL(t *testing.T) {
	for _, tt := range []struct {
		what string
		expr orm.Selectable[orm.Composed, int64]
		want string
	}{
		{"row_number", orm.RowNumber().Over(nil), `row_number() OVER ()`},
		{"rank", orm.Rank().Over(orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc())),
			`rank() OVER (ORDER BY "posts"."id" ASC)`},
		{"dense_rank", orm.DenseRank().Over(orm.Window()), `dense_rank() OVER ()`},
		{"windowed count", orm.Of(orm.Count[gendemo.Post]().Over(nil)), `count(*) OVER ()`},
	} {
		t.Run(tt.what, func(t *testing.T) {
			shape := orm.Project1(tt.expr, func(n int64) int64 { return n })
			sql, _, err := orm.Compose(nil, shape).From(gendemo.Posts.Source()).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if !strings.Contains(sql, tt.want) {
				t.Errorf("SQL = %s\nwant it to contain %s", sql, tt.want)
			}
		})
	}

	lag := orm.Lag(gendemo.Posts.ID).Over(orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc()))
	shape := orm.Project1(lag, func(v *int64) *int64 { return v })
	sql, args, err := orm.Compose(nil, shape).From(gendemo.Posts.Source()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `lag("posts"."id", $1) OVER (ORDER BY "posts"."id" ASC)`) {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 1 || args[0] != int32(1) {
		t.Errorf("args = %v, want the default offset as a parameter", args)
	}
}
