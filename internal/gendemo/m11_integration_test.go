package gendemo_test

import (
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// Scenario L: everything at once.
//
// One statement using a CTE, a derived table, two outer joins, a correlated
// EXISTS, a CASE, a grouping aggregate, a window function, a HAVING clause and
// an ORDER BY, with five bind arguments spread across five clauses. It is here
// because each of those was tested on its own, and a milestone about
// composition is not finished until they compose — with one parameter list,
// numbered in the order the SQL is written, and the same rows a handwritten
// statement returns.

func TestMaximalComposition(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO comments (id, post_id, author_id, body) VALUES
	    (1, 1, 1, 'a'), (2, 1, 1, 'b'), (3, 2, 3, 'c')`)

	// The CTE: the active users, by their own definition.
	cteID := orm.Named("id", orm.Of(gendemo.Users.ID))
	cteEmail := orm.Named("email", orm.Of(gendemo.Users.Email))
	active := orm.CTE("active", orm.Rows(cteID, cteEmail).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))))

	// The derived table: how many posts each author has, above a score.
	statsAuthor := orm.Named("user_id", orm.Of(gendemo.Posts.AuthorID))
	statsPosts := orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))
	stats := orm.Sub("s", orm.Rows(statsAuthor, statsPosts).
		From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Posts.Score.Gte(int32(0)))).
		GroupBy(orm.Of(gendemo.Posts.AuthorID)))

	// The correlated subquery: does this user have a post since a cutoff?
	recent := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, orm.Ref(active, cteID)),
			orm.Cond(gendemo.Posts.CreatedAt.Gte(t0)))

	comments := orm.Of(orm.CountOf(gendemo.Comments.ID))
	mood := orm.Case(comments.Gt(int64(1)), orm.Val("busy")).Else(orm.Val("quiet"))

	type row struct {
		ID    int64
		Posts int64
		Mood  string
		N     int64
	}
	shape := orm.Project4(
		orm.Ref(active, cteID),
		orm.Coalesce(orm.Val(int64(0)), orm.OptRef(stats, statsPosts)),
		mood,
		orm.RowNumber().Over(orm.Window().OrderBy(orm.Ref(active, cteID).Asc())),
		func(id, posts int64, mood string, n int64) row {
			return row{ID: id, Posts: posts, Mood: mood, N: n}
		},
	)

	q := orm.Compose(db.Executor(), shape).
		With(active).
		From(active).
		LeftJoin(stats, orm.Eq(orm.Ref(stats, statsAuthor), orm.Ref(active, cteID))).
		LeftJoin(gendemo.Comments.Source(),
			orm.Cond(orm.Expr[orm.Composed](`"comments"."author_id" = "active"."id"`))).
		Where(orm.Exists[orm.Composed](recent)).
		GroupBy(orm.Ref(active, cteID), orm.OptRef(stats, statsPosts)).
		Having(comments.Gte(int64(0))).
		OrderBy(orm.Ref(active, cteID).Asc())

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	// The parameter list is one namespace, numbered in the order the SQL is
	// written: the WITH item, then the select list, then the FROM clause's
	// derived table, then WHERE, then HAVING. Compiling any part separately
	// would restart the numbering and bind the wrong values.
	// The order is the compiler's traversal, which is the order the SQL reads:
	// the WITH item, then the select list left to right, then the FROM clause's
	// derived table, then WHERE, then HAVING.
	wantArgs := []any{true, int64(0), int64(1), "busy", "quiet", int32(0), t0, int64(0)}
	if len(args) != len(wantArgs) {
		t.Fatalf("the statement binds %d parameters, want %d:\n%s\n%v", len(args), len(wantArgs), sql, args)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("parameter $%d = %v, want %v\n%s", i+1, args[i], wantArgs[i], sql)
		}
	}
	for i := 1; i <= len(args); i++ {
		if n := countPlaceholder(sql, i); n != 1 {
			t.Errorf("$%d appears %d times in\n%s", i, n, sql)
		}
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// The same question, written by hand, with the arguments in the same
	// order and bound to the same values.
	rows, err := conn.Query(t.Context(), `
		WITH active AS (
		    SELECT id, email FROM users WHERE active = $1
		)
		SELECT
		    active.id,
		    coalesce(s.post_count, $2),
		    CASE WHEN count(comments.id) > $3 THEN $4 ELSE $5 END,
		    row_number() OVER (ORDER BY active.id)
		FROM active
		LEFT JOIN (
		    SELECT author_id AS user_id, count(*) AS post_count
		    FROM posts WHERE score >= $6 GROUP BY author_id
		) s ON s.user_id = active.id
		LEFT JOIN comments ON comments.author_id = active.id
		WHERE EXISTS (
		    SELECT 1 FROM posts WHERE posts.author_id = active.id AND posts.created_at >= $7
		)
		GROUP BY active.id, s.post_count
		HAVING count(comments.id) >= $8
		ORDER BY active.id`,
		wantArgs...)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var want row
		if err := rows.Scan(&want.ID, &want.Posts, &want.Mood, &want.N); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(got) {
			t.Fatalf("the ORM returned %d rows, handwritten SQL returned more", len(got))
		}
		if got[i] != want {
			t.Errorf("row %d = %+v, handwritten SQL returned %+v", i, got[i], want)
		}
		i++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	if i != len(got) {
		t.Errorf("the ORM returned %d rows, handwritten SQL %d", len(got), i)
	}
	if i == 0 {
		t.Error("the scenario returned no rows, so it proved nothing about the values")
	}
}

// countPlaceholder counts occurrences of $n that are not the prefix of a longer
// number, so that $1 is not found inside $10.
func countPlaceholder(sql string, n int) int {
	want := "$" + itoa(n)
	count := 0
	for i := 0; i+len(want) <= len(sql); i++ {
		if sql[i:i+len(want)] != want {
			continue
		}
		if i+len(want) < len(sql) {
			if c := sql[i+len(want)]; c >= '0' && c <= '9' {
				continue
			}
		}
		count++
	}
	return count
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Scenario B: a correlated scalar subquery, whose result is nullable because
// the statement can match no row.
func TestScenarioB_correlatedScalarSubquery(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	latest := orm.Scalar[gendemo.User, time.Time](
		orm.Rows(orm.NamedNull("m", orm.Max(gendemo.Posts.CreatedAt))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)),
	)
	shape := orm.Project2(gendemo.Users.ID, latest,
		func(id int64, at *time.Time) int64 {
			if at == nil {
				return -1
			}
			return id
		})
	got, err := orm.Select(db.Users, shape).OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		SELECT CASE WHEN (SELECT max(created_at) FROM posts WHERE posts.author_id = users.id) IS NULL
		            THEN -1 ELSE id END
		FROM users ORDER BY users.id`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("row %d = %d, handwritten SQL = %d", i, got[i], want[i])
		}
	}
}

// Scenario C: NOT IN over a subquery that yields a NULL, which is UNKNOWN for
// every row and therefore returns nothing. It is not rewritten into NOT EXISTS,
// which would return rows.
func TestScenarioC_notInWithANull(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, created_at) VALUES (98, NULL, 'orphan', now())`)

	authors := orm.Rows(orm.Named("author_id", orm.Of(gendemo.Posts.AuthorID))).
		From(gendemo.Posts.Source())

	got := userIDs(t, db.Users.Query().
		Where(orm.NotInSub(gendemo.Users.ID, authors)).
		OrderBy(gendemo.Users.ID.Asc()))
	assertIDs(t, conn, `SELECT id FROM users WHERE id NOT IN (SELECT author_id FROM posts) ORDER BY id`, got)
	if len(got) != 0 {
		t.Errorf("NOT IN over a subquery yielding NULL returned %v", got)
	}

	// Excluding the NULLs inside the subquery is what makes it answer the
	// question people usually mean.
	named := orm.Rows(orm.Named("author_id", orm.Of(gendemo.Posts.AuthorID))).
		From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Posts.AuthorID.IsNotNull()))
	got = userIDs(t, db.Users.Query().
		Where(orm.NotInSub(gendemo.Users.ID, named)).
		OrderBy(gendemo.Users.ID.Asc()))
	assertIDs(t, conn, `
		SELECT id FROM users
		WHERE id NOT IN (SELECT author_id FROM posts WHERE author_id IS NOT NULL)
		ORDER BY id`, got)
	if len(got) == 0 {
		t.Error("excluding the NULLs still returned nothing")
	}
}
