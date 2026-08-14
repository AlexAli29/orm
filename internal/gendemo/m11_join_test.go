package gendemo_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M11.2: public joins.
//
// Two claims are under test. The first is that the SQL is the SQL — a condition
// in ON is not a condition in WHERE, and the compiler does not move it. The
// second is the release-critical one: a value read through an outer join is
// nullable in this query whatever its column's constraint says, and the types
// say so rather than the scanner finding out.

func TestJoin_innerAndLeftCompileAsWritten(t *testing.T) {
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Profiles.Bio),
		func(id int64, bio *string) string { return fmt.Sprint(id, bio) },
	)
	sql, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "profiles"."bio" FROM "public"."users" ` +
		`LEFT JOIN "public"."profiles" ON "profiles"."user_id" = "users"."id"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
}

// A join condition may name the sources introduced before it and the one it
// attaches. Naming one joined later is a mistake about order, and the message
// says so rather than reporting a column PostgreSQL cannot resolve.
func TestJoin_scopeIsSequential(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		// This condition names comments, which the next join introduces.
		Join(gendemo.Posts.Source(), orm.Eq(gendemo.Comments.ID, gendemo.Posts.ID)).
		Join(gendemo.Comments.Source(), orm.Eq(gendemo.Comments.ID, gendemo.Posts.ID)).
		SQL()
	if err == nil {
		t.Fatal("a join condition naming a source joined later compiled")
	}
	if !strings.Contains(err.Error(), "ON condition") || !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %v", err)
	}
}

// The same two joins in the right order compile, which is what makes the case
// above about order rather than about the sources involved.
func TestJoin_sequentialScopeAcceptsTheRightOrder(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		Join(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		Join(gendemo.Comments.Source(), orm.Eq(gendemo.Comments.ID, gendemo.Posts.ID)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

func TestJoin_crossJoinTakesNoCondition(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	sql, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		CrossJoin(gendemo.Categories.Source()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `CROSS JOIN "public"."categories"`) || strings.Contains(sql, " ON ") {
		t.Errorf("SQL = %s", sql)
	}
}

// Two occurrences claiming one name would make every column of the second
// ambiguous, so the collision is refused before PostgreSQL sees it.
func TestJoin_refusesADuplicateAlias(t *testing.T) {
	a := gendemo.Users.As("u")
	b := gendemo.Posts.As("u")
	shape := orm.Project1(orm.Of(a.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(a.Source()).
		Join(b.Source(), orm.Eq(b.AuthorID, a.ID)).
		SQL()
	if err == nil {
		t.Fatal("two sources aliased u compiled")
	}
	if !strings.Contains(err.Error(), "alias collision") {
		t.Errorf("error = %v", err)
	}
}

// Release-critical: a NOT NULL column read through a LEFT JOIN is nullable in
// this query, and a select list that says otherwise is refused.
func TestLeftJoin_refusesANonNullableResult(t *testing.T) {
	// profiles.id is NOT NULL, so its descriptor is not nullable and Of keeps
	// it that way — which is exactly the claim the join makes false.
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), orm.Of(gendemo.Profiles.ID),
		func(a, b int64) [2]int64 { return [2]int64{a, b} },
	)
	_, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
		SQL()
	if err == nil {
		t.Fatal("a NOT NULL column read through a LEFT JOIN compiled as non-nullable")
	}
	if !strings.Contains(err.Error(), "outer join") || !strings.Contains(err.Error(), "Opt") {
		t.Errorf("error = %v, want it to name the join and say how to fix it", err)
	}
}

// An INNER JOIN makes nothing nullable, so the same select list is fine.
func TestInnerJoin_changesNoNullability(t *testing.T) {
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), orm.Of(gendemo.Profiles.ID),
		func(a, b int64) [2]int64 { return [2]int64{a, b} },
	)
	if _, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		Join(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
		SQL(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

// A RIGHT JOIN nullifies the other side, and a FULL JOIN both.
func TestOuterJoins_nullifyTheSideTheyCanEmpty(t *testing.T) {
	left := orm.Of(gendemo.Users.ID)
	right := orm.Of(gendemo.Profiles.ID)
	build := func(a, b int64) int { return 0 }

	t.Run("right join nullifies the left", func(t *testing.T) {
		_, _, err := orm.Compose(nil, orm.Project2(left, right, build)).
			From(gendemo.Users.Source()).
			RightJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
			SQL()
		if err == nil || !strings.Contains(err.Error(), "public.users") {
			t.Fatalf("error = %v, want it to name users as the nullable side", err)
		}
	})
	t.Run("full join nullifies both", func(t *testing.T) {
		ok := orm.Project2(orm.Opt(gendemo.Users.ID), orm.Opt(gendemo.Profiles.ID),
			func(a, b *int64) int { return 0 })
		if _, _, err := orm.Compose(nil, ok).
			From(gendemo.Users.Source()).
			FullJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
			SQL(); err != nil {
			t.Fatalf("SQL: %v", err)
		}
		bad := orm.Project2(orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Profiles.ID),
			func(a int64, b *int64) int { return 0 })
		if _, _, err := orm.Compose(nil, bad).
			From(gendemo.Users.Source()).
			FullJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
			SQL(); err == nil {
			t.Fatal("a FULL JOIN left the first source non-nullable")
		}
	})
}

// Nullability survives being computed with. NULL + 1 is NULL, and the type of
// the result says so rather than the scanner discovering it.
func TestLeftJoin_nullabilityPropagatesThroughArithmetic(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	// posts.score is NOT NULL; read through a LEFT JOIN with no match it is
	// NULL, and so is score + 1.
	one := int32(1)
	score := orm.Opt(gendemo.Posts.Score)
	type row struct {
		ID    int64
		Score *int32
		Plus  *int32
	}
	shape := orm.Project3(
		orm.Of(gendemo.Users.ID), score, score.Add(&one),
		func(id int64, s, p *int32) row { return row{ID: id, Score: s, Plus: p} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		Where(orm.Cond(gendemo.Users.ID.Eq(int64(2)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want the one user with no posts", len(got))
	}
	if got[0].Score != nil || got[0].Plus != nil {
		t.Errorf("row = %+v, want both values NULL", got[0])
	}
}

// Scenario D: every column of the right-hand side is NOT NULL in the schema,
// the parent has no child, and every one of them has to arrive as nil.
func TestLeftJoin_everyNotNullColumnOfTheAbsentSideIsNull(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	// comments.id, body, visible, score and created_at are all NOT NULL. Posts
	// 2 and 3 deliberately have none.
	m11exec(t, conn, `INSERT INTO comments (id, post_id, body) VALUES (1, 1, 'hi')`)

	type row struct {
		PostID  int64
		ID      *int64
		Body    *string
		Visible *bool
		Score   *int32
		Created *time.Time
	}
	shape := orm.Project6(
		orm.Of(gendemo.Posts.ID),
		orm.Opt(gendemo.Comments.ID),
		orm.Opt(gendemo.Comments.Body),
		orm.Opt(gendemo.Comments.Visible),
		orm.Opt(gendemo.Comments.Score),
		orm.Opt(gendemo.Comments.CreatedAt),
		func(pid int64, id *int64, body *string, vis *bool, score *int32, at *time.Time) row {
			return row{PostID: pid, ID: id, Body: body, Visible: vis, Score: score, Created: at}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		LeftJoin(gendemo.Comments.Source(),
			orm.Cond(orm.Expr[orm.Composed](`"comments"."post_id" = "posts"."id"`))).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0].ID == nil || *got[0].Body != "hi" {
		t.Errorf("the matched row lost its values: %+v", got[0])
	}
	for _, r := range got[1:] {
		if r.ID != nil || r.Body != nil || r.Visible != nil || r.Score != nil || r.Created != nil {
			t.Errorf("post %d has no comment and read %+v", r.PostID, r)
		}
	}
}

// A condition in ON and the same condition in WHERE are different queries. The
// compiler puts each where it was written and moves neither.
func TestJoin_onIsNotWhere(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	published := orm.Cond(gendemo.Posts.Published.Eq(true))
	match := orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Posts.ID),
		func(u int64, p *int64) [2]int64 {
			var out [2]int64
			out[0] = u
			if p != nil {
				out[1] = *p
			}
			return out
		},
	)

	inOn, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), match, published).
		OrderBy(orm.Of(gendemo.Users.ID).Asc(), orm.Opt(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("ON: %v", err)
	}
	inWhere, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), match).
		Where(published).
		OrderBy(orm.Of(gendemo.Users.ID).Asc(), orm.Opt(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("WHERE: %v", err)
	}
	if len(inOn) == len(inWhere) {
		t.Errorf("ON and WHERE returned the same %d rows; they are different queries", len(inOn))
	}

	onWant := handwrittenIDs(t, conn, `
		SELECT u.id FROM users u
		LEFT JOIN posts p ON p.author_id = u.id AND p.published
		ORDER BY u.id, p.id`)
	whereWant := handwrittenIDs(t, conn, `
		SELECT u.id FROM users u
		LEFT JOIN posts p ON p.author_id = u.id
		WHERE p.published
		ORDER BY u.id, p.id`)
	if len(inOn) != len(onWant) {
		t.Errorf("ON returned %d rows, handwritten SQL %d", len(inOn), len(onWant))
	}
	if len(inWhere) != len(whereWant) {
		t.Errorf("WHERE returned %d rows, handwritten SQL %d", len(inWhere), len(whereWant))
	}
}

// A self join: two occurrences of one table, told apart by identity rather than
// by name, with the global descriptors untouched.
func TestJoin_selfJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `UPDATE users SET manager_id = 1 WHERE id IN (2, 3)`)

	employee := gendemo.Users.As("employee")
	manager := gendemo.Users.As("manager")

	type row struct {
		Employee string
		Manager  string
	}
	shape := orm.Project2(
		orm.Of(employee.Email), orm.Opt(manager.Email),
		func(e string, m *string) row {
			out := row{Employee: e}
			if m != nil {
				out.Manager = *m
			}
			return out
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(employee.Source()).
		LeftJoin(manager.Source(), orm.Eq(employee.ManagerID, manager.ID)).
		OrderBy(orm.Of(employee.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0].Manager != "" {
		t.Errorf("user 1 has no manager and read %q", got[0].Manager)
	}
	if got[1].Manager != "alex@example.com" {
		t.Errorf("user 2's manager = %q", got[1].Manager)
	}
	// The global descriptors still name the unaliased occurrence.
	if gendemo.Users.Source().Ref() != "users" {
		t.Errorf("aliasing changed the global source to %q", gendemo.Users.Source().Ref())
	}
}

// Scenario A: an aggregate derived table joined into users, whose count is
// non-NULL inside the subquery and nullable through the LEFT JOIN.
func TestLeftJoin_derivedAggregateSource(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	stats := orm.Sub("s", postStats())
	type row struct {
		ID    int64
		Email string
		Posts *int64
	}
	shape := orm.Project3(
		orm.Of(gendemo.Users.ID), orm.Of(gendemo.Users.Email), orm.OptRef(stats, statsCount),
		func(id int64, email string, n *int64) row { return row{ID: id, Email: email, Posts: n} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(stats, orm.Eq(orm.Ref(stats, statsUserID), gendemo.Users.ID)).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0].Posts == nil || *got[0].Posts != 2 {
		t.Errorf("user 1 = %+v, want 2 posts", got[0])
	}
	if got[1].Posts != nil {
		t.Errorf("user 2 has no posts and read %d; count(*) is never NULL but the join can leave no row", *got[1].Posts)
	}

	rows := handwrittenIDs(t, conn, `
		SELECT coalesce(s.post_count, -1) FROM users u
		LEFT JOIN (SELECT author_id AS user_id, count(*) AS post_count FROM posts GROUP BY author_id) s
		ON s.user_id = u.id
		ORDER BY u.id`)
	for i, r := range got {
		want := rows[i]
		switch {
		case r.Posts == nil && want != -1:
			t.Errorf("row %d: the ORM read NULL, handwritten SQL read %d", i, want)
		case r.Posts != nil && *r.Posts != want:
			t.Errorf("row %d: the ORM read %d, handwritten SQL read %d", i, *r.Posts, want)
		}
	}
}

// Aggregation over an outer join: count of a column is zero where the join
// matched nothing, and count(*) is one, because the row still exists.
func TestJoin_aggregateOverAnOuterJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	type row struct {
		ID    int64
		Rows  int64
		Posts int64
	}
	shape := orm.Project3(
		orm.Of(gendemo.Users.ID),
		orm.Of(orm.Count[orm.Composed]()),
		orm.Of(orm.CountOf(gendemo.Posts.ID)),
		func(id, all, posts int64) row { return row{ID: id, Rows: all, Posts: posts} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		GroupBy(orm.Of(gendemo.Users.ID)).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[1].Rows != 1 || got[1].Posts != 0 {
		t.Errorf("the user with no posts = %+v, want count(*) 1 and count(posts.id) 0", got[1])
	}
	want := handwrittenIDs(t, conn, `
		SELECT count(p.id) FROM users u
		LEFT JOIN posts p ON p.author_id = u.id
		GROUP BY u.id ORDER BY u.id`)
	for i, r := range got {
		if r.Posts != want[i] {
			t.Errorf("row %d counted %d, handwritten SQL counted %d", i, r.Posts, want[i])
		}
	}
}

// Scenario F: LATERAL, the shape PostgreSQL is unusually good at — one
// subquery per left-hand row, ordered and limited on its own terms.
func TestLeftJoinLateral_topChildPerParent(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	postID := orm.Named("id", orm.Of(gendemo.Posts.ID))
	latest := orm.Sub("p", orm.Rows(postID).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		OrderBy(orm.Of(gendemo.Posts.CreatedAt).Desc()).
		Limit(1))

	type row struct {
		User int64
		Post *int64
	}
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), orm.OptRef(latest, postID),
		func(u int64, p *int64) row { return row{User: u, Post: p} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoinLateral(latest).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	want := handwrittenIDs(t, conn, `
		SELECT coalesce(p.id, -1) FROM users u
		LEFT JOIN LATERAL (
		    SELECT id FROM posts WHERE posts.author_id = u.id ORDER BY created_at DESC LIMIT 1
		) p ON true
		ORDER BY u.id`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	for i, r := range got {
		switch {
		case r.Post == nil && want[i] != -1:
			t.Errorf("row %d: the ORM read NULL, handwritten SQL read %d", i, want[i])
		case r.Post != nil && *r.Post != want[i]:
			t.Errorf("row %d: the ORM read %d, handwritten SQL read %d", i, *r.Post, want[i])
		}
	}
	if got[1].Post != nil {
		t.Errorf("the user with no posts read %d", *got[1].Post)
	}
}

// A lateral subquery sees the sources written before it and no others.
func TestLateral_cannotNameASourceJoinedAfterIt(t *testing.T) {
	postID := orm.Named("id", orm.Of(gendemo.Posts.ID))
	latest := orm.Sub("p", orm.Rows(postID).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)))

	shape := orm.Project1(orm.OptRef(latest, postID), func(p *int64) *int64 { return p })
	_, _, err := orm.Compose(nil, shape).
		From(gendemo.Categories.Source()).
		LeftJoinLateral(latest).
		Join(gendemo.Users.Source(), orm.Cond(orm.Expr[orm.Composed]("TRUE"))).
		SQL()
	if err == nil {
		t.Fatal("a lateral subquery naming a source joined after it compiled")
	}
	if !strings.Contains(err.Error(), "introduces after it") {
		t.Errorf("error = %v", err)
	}
}

// Several joins in one statement, each attaching to what came before.
func TestJoin_multipleJoinsKeepTheirOwnIdentity(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO avatars (id, url) VALUES (1, 'http://a')`)
	m11exec(t, conn, `INSERT INTO profiles (id, user_id, bio, avatar_id) VALUES (1, 1, 'b', 1)`)

	type row struct {
		User   int64
		Bio    *string
		Avatar *string
		Post   *int64
	}
	shape := orm.Project4(
		orm.Of(gendemo.Users.ID),
		orm.Opt(gendemo.Profiles.Bio),
		orm.Opt(gendemo.Avatars.URL),
		orm.Opt(gendemo.Posts.ID),
		func(u int64, bio, av *string, p *int64) row {
			return row{User: u, Bio: bio, Avatar: av, Post: p}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
		LeftJoin(gendemo.Avatars.Source(),
			orm.Cond(orm.Expr[orm.Composed](`"avatars"."id" = "profiles"."avatar_id"`))).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		OrderBy(orm.Of(gendemo.Users.ID).Asc(), orm.Opt(gendemo.Posts.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		SELECT u.id FROM users u
		LEFT JOIN profiles pr ON pr.user_id = u.id
		LEFT JOIN avatars a ON a.id = pr.avatar_id
		LEFT JOIN posts p ON p.author_id = u.id
		ORDER BY u.id, p.id`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	if got[0].Avatar == nil || *got[0].Avatar != "http://a" {
		t.Errorf("the avatar two joins deep read %v", got[0].Avatar)
	}
}

// Nullability is a property of this query, not of the descriptor. The same
// column read without an outer join is non-nullable again.
func TestLeftJoin_nullabilityIsScopeLocal(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Profiles.ID), func(id int64) int64 { return id })
	if _, _, err := orm.Compose(nil, shape).From(gendemo.Profiles.Source()).SQL(); err != nil {
		t.Fatalf("a column that is nullable only through a join is not nullable without one: %v", err)
	}
}
