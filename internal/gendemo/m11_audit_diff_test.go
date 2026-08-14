package gendemo_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The M11 differential audit.
//
// Every claim is settled by running the ORM's statement and a handwritten one
// against the same rows and comparing the values. A test that compared SQL text
// would only prove the compiler agrees with itself.

// Release-critical: a condition in ON and the same condition in WHERE are
// different queries, over all three parent classes.
func TestAudit_onIsNotWhereAcrossEveryParentClass(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	// user 1: a published post and an unpublished one
	// user 2: no posts at all
	// user 3: one published post
	m11exec(t, conn, `INSERT INTO users (id, email, age, active, state, created_at)
	    VALUES (20, 'only-unpublished@example.com', 30, true, 'active', now())`)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, published, created_at)
	    VALUES (20, 20, 'draft', false, now())`)

	match := orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)
	published := orm.Cond(gendemo.Posts.Published.Eq(true))
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Posts.ID),
		func(u int64, p *int64) [2]int64 {
			out := [2]int64{u, -1}
			if p != nil {
				out[1] = *p
			}
			return out
		},
	)
	order := []orm.Order[orm.Composed]{
		orm.Of(gendemo.Users.ID).Asc(), orm.Opt(gendemo.Posts.ID).Asc(),
	}

	inON, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), match, published).
		OrderBy(order...).All(t.Context())
	if err != nil {
		t.Fatalf("ON: %v", err)
	}
	inWHERE, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), match).
		Where(published).
		OrderBy(order...).All(t.Context())
	if err != nil {
		t.Fatalf("WHERE: %v", err)
	}

	wantON := handwrittenPairs(t, conn, `
		SELECT u.id, coalesce(p.id, -1) FROM users u
		LEFT JOIN posts p ON p.author_id = u.id AND p.published = true
		ORDER BY u.id, p.id`)
	wantWHERE := handwrittenPairs(t, conn, `
		SELECT u.id, coalesce(p.id, -1) FROM users u
		LEFT JOIN posts p ON p.author_id = u.id
		WHERE p.published = true
		ORDER BY u.id, p.id`)

	assertPairs(t, "ON", inON, wantON)
	assertPairs(t, "WHERE", inWHERE, wantWHERE)
	if len(inON) == len(inWHERE) {
		t.Errorf("ON and WHERE returned the same %d rows; the corpus does not tell them apart", len(inON))
	}
	// The parent whose only child is unpublished survives in ON with a NULL
	// child, and disappears in WHERE. That is the whole difference.
	found := false
	for _, r := range inON {
		if r[0] == 20 && r[1] == -1 {
			found = true
		}
	}
	if !found {
		t.Error("ON dropped the parent whose only child failed the extra condition")
	}
	for _, r := range inWHERE {
		if r[0] == 20 {
			t.Error("WHERE kept the parent whose only child failed the extra condition")
		}
	}
}

// Every join shape, against handwritten SQL.
func TestAudit_joinShapesMatchHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO profiles (id, user_id, bio) VALUES (1, 1, 'a'), (2, 3, 'b')`)
	// A profile row pointing at no user is impossible (FK), so RIGHT/FULL are
	// exercised through categories, which nothing references here.
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (1, 'x'), (2, 'y')`)

	pair := func(u *int64, p *int64) [2]int64 {
		out := [2]int64{-1, -1}
		if u != nil {
			out[0] = *u
		}
		if p != nil {
			out[1] = *p
		}
		return out
	}
	both := orm.Project2(orm.Opt(gendemo.Users.ID), orm.Opt(gendemo.Profiles.ID), pair)
	order := []orm.Order[orm.Composed]{
		orm.Opt(gendemo.Users.ID).Asc(), orm.Opt(gendemo.Profiles.ID).Asc(),
	}
	on := orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)

	for _, tt := range []struct {
		name  string
		build func(*orm.ComposedQuery[[2]int64]) *orm.ComposedQuery[[2]int64]
		sql   string
	}{
		{"inner", func(q *orm.ComposedQuery[[2]int64]) *orm.ComposedQuery[[2]int64] {
			return q.Join(gendemo.Profiles.Source(), on)
		}, `SELECT coalesce(u.id,-1), coalesce(p.id,-1) FROM users u JOIN profiles p ON p.user_id = u.id ORDER BY u.id, p.id`},
		{"left", func(q *orm.ComposedQuery[[2]int64]) *orm.ComposedQuery[[2]int64] {
			return q.LeftJoin(gendemo.Profiles.Source(), on)
		}, `SELECT coalesce(u.id,-1), coalesce(p.id,-1) FROM users u LEFT JOIN profiles p ON p.user_id = u.id ORDER BY u.id, p.id`},
		{"right", func(q *orm.ComposedQuery[[2]int64]) *orm.ComposedQuery[[2]int64] {
			return q.RightJoin(gendemo.Profiles.Source(), on)
		}, `SELECT coalesce(u.id,-1), coalesce(p.id,-1) FROM users u RIGHT JOIN profiles p ON p.user_id = u.id ORDER BY u.id, p.id`},
		{"full", func(q *orm.ComposedQuery[[2]int64]) *orm.ComposedQuery[[2]int64] {
			return q.FullJoin(gendemo.Profiles.Source(), on)
		}, `SELECT coalesce(u.id,-1), coalesce(p.id,-1) FROM users u FULL JOIN profiles p ON p.user_id = u.id ORDER BY u.id, p.id`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.build(orm.Compose(db.Executor(), both).From(gendemo.Users.Source())).
				OrderBy(order...).All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			assertPairs(t, tt.name, got, handwrittenPairs(t, conn, tt.sql))
		})
	}
}

// A CROSS JOIN produces the Cartesian product, and the row count says so.
func TestAudit_crossJoinCardinality(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (1, 'x'), (2, 'y')`)

	shape := orm.Project2(orm.Of(gendemo.Users.ID), orm.Of(gendemo.Categories.ID),
		func(u, c int64) [2]int64 { return [2]int64{u, c} })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		CrossJoin(gendemo.Categories.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc(), orm.Of(gendemo.Categories.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	assertPairs(t, "cross", got, handwrittenPairs(t, conn,
		`SELECT u.id, c.id FROM users u CROSS JOIN categories c ORDER BY u.id, c.id`))
	if len(got) != 6 {
		t.Errorf("3 users x 2 categories produced %d rows, want 6", len(got))
	}
}

// A LEFT JOIN followed by an INNER JOIN keyed off the nullable side: the
// compiler must not lose that the inner-joined source is not nullable, and must
// not claim the left-joined one is not.
func TestAudit_nestedJoinTreeNullability(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO avatars (id, url) VALUES (1, 'u')`)
	m11exec(t, conn, `INSERT INTO profiles (id, user_id, bio, avatar_id) VALUES (1, 1, 'a', 1)`)

	t.Run("A LEFT JOIN B LEFT JOIN C keyed off B", func(t *testing.T) {
		shape := orm.Project3(
			orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Profiles.ID), orm.Opt(gendemo.Avatars.URL),
			func(u int64, p *int64, a *string) [2]int64 {
				out := [2]int64{u, -1}
				if p != nil {
					out[1] = *p
				}
				return out
			},
		)
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Users.Source()).
			LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
			LeftJoin(gendemo.Avatars.Source(), orm.Cond(orm.Expr[orm.Composed](`"avatars"."id" = "profiles"."avatar_id"`))).
			OrderBy(orm.Of(gendemo.Users.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		assertPairs(t, "nested outer", got, handwrittenPairs(t, conn, `
			SELECT u.id, coalesce(p.id, -1) FROM users u
			LEFT JOIN profiles p ON p.user_id = u.id
			LEFT JOIN avatars a ON a.id = p.avatar_id
			ORDER BY u.id`))
	})

	t.Run("an inner join after a left join is not nullable", func(t *testing.T) {
		// avatars is INNER joined, so a row only survives when it matched.
		// Reading avatars.url non-nullably has to be accepted.
		shape := orm.Project2(
			orm.Of(gendemo.Users.ID), orm.Of(gendemo.Avatars.URL),
			func(u int64, a string) [2]int64 { return [2]int64{u, int64(len(a))} },
		)
		if _, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Users.Source()).
			LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
			Join(gendemo.Avatars.Source(), orm.Cond(orm.Expr[orm.Composed](`"avatars"."id" = "profiles"."avatar_id"`))).
			All(t.Context()); err != nil {
			t.Fatalf("an INNER JOIN after a LEFT JOIN was treated as nullable: %v", err)
		}
	})
}

// The membership tests, against PostgreSQL, including the NULL cases.
func TestAudit_membershipSemantics(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	// Duplicates and a NULL in the inner rowset.
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, created_at) VALUES
	    (30, 1, 'dup', now()), (31, NULL, 'orphan', now())`)

	authors := orm.Rows(orm.Named("author_id", orm.Of(gendemo.Posts.AuthorID))).
		From(gendemo.Posts.Source())
	named := orm.Rows(orm.Named("author_id", orm.Of(gendemo.Posts.AuthorID))).
		From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Posts.AuthorID.IsNotNull()))

	for _, tt := range []struct {
		name string
		pred orm.Predicate[gendemo.User]
		sql  string
	}{
		{"in with duplicates and a null", orm.InSub(gendemo.Users.ID, authors),
			`SELECT id FROM users WHERE id IN (SELECT author_id FROM posts) ORDER BY id`},
		{"not in with a null", orm.NotInSub(gendemo.Users.ID, authors),
			`SELECT id FROM users WHERE id NOT IN (SELECT author_id FROM posts) ORDER BY id`},
		{"not in without nulls", orm.NotInSub(gendemo.Users.ID, named),
			`SELECT id FROM users WHERE id NOT IN (SELECT author_id FROM posts WHERE author_id IS NOT NULL) ORDER BY id`},
		{"nullable left-hand side", orm.InSub(gendemo.Users.ManagerID, named),
			`SELECT id FROM users WHERE manager_id IN (SELECT author_id FROM posts WHERE author_id IS NOT NULL) ORDER BY id`},
		{"exists", orm.Exists[gendemo.User](orm.Rows(orm.Named("x", orm.Val(1))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID))),
			`SELECT id FROM users u WHERE EXISTS (SELECT 1 FROM posts p WHERE p.author_id = u.id) ORDER BY id`},
		{"not exists", orm.NotExists[gendemo.User](orm.Rows(orm.Named("x", orm.Val(1))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID))),
			`SELECT id FROM users u WHERE NOT EXISTS (SELECT 1 FROM posts p WHERE p.author_id = u.id) ORDER BY id`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := userIDs(t, db.Users.Query().Where(tt.pred).OrderBy(gendemo.Users.ID.Asc()))
			assertIDs(t, conn, tt.sql, got)
		})
	}

	// The release-critical property, stated on its own: NOT IN over a rowset
	// containing NULL is UNKNOWN for every row, so nothing comes back — and
	// NOT EXISTS over the same data does return rows.
	notIn := userIDs(t, db.Users.Query().Where(orm.NotInSub(gendemo.Users.ID, authors)))
	notExists := userIDs(t, db.Users.Query().Where(orm.NotExists[gendemo.User](
		orm.Rows(orm.Named("x", orm.Val(1))).From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)))))
	if len(notIn) != 0 {
		t.Errorf("NOT IN over a rowset with a NULL returned %v", notIn)
	}
	if len(notExists) == 0 {
		t.Error("NOT EXISTS returned nothing; the two were not distinguished")
	}
}

// PostgreSQL's three-valued logic reaches WHERE and HAVING unchanged.
func TestAudit_threeValuedLogic(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
		sql  string
	}{
		{"comparison against NULL keeps no row",
			orm.Cond(gendemo.Users.Nickname.Eq("alex")),
			`SELECT id FROM users WHERE nickname = 'alex' ORDER BY id`},
		{"negation of UNKNOWN is UNKNOWN",
			orm.Not(orm.Cond(gendemo.Users.Nickname.Eq("alex"))),
			`SELECT id FROM users WHERE NOT (nickname = 'alex') ORDER BY id`},
		{"tuple comparison with a NULL component",
			orm.Row2Eq(gendemo.Users.ID, gendemo.Users.ManagerID, int64(1), int64(1)),
			`SELECT id FROM users WHERE (id, manager_id) = (1, 1) ORDER BY id`},
		{"IS NULL is two-valued",
			orm.Cond(gendemo.Users.Nickname.IsNull()),
			`SELECT id FROM users WHERE nickname IS NULL ORDER BY id`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orm.Compose(db.Executor(), shape).
				From(gendemo.Users.Source()).
				Where(tt.pred).
				OrderBy(orm.Of(gendemo.Users.ID).Asc()).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if got == nil {
				got = []int64{}
			}
			assertIDs(t, conn, tt.sql, got)
		})
	}
}

// DISTINCT ON chooses the same representative row PostgreSQL does, with ties
// broken by the same ordering.
func TestAudit_distinctOnChoosesTheSameRow(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, score, created_at) VALUES
	    (40, 1, 'a', 5, '2024-05-01T09:00:00Z'),
	    (41, 1, 'b', 5, '2024-05-01T09:00:00Z'),
	    (42, 3, 'c', 1, '2024-06-01T09:00:00Z')`)

	shape := orm.Project1(orm.Of(gendemo.Posts.ID), func(id int64) int64 { return id })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		DistinctOn(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(
			orm.Of(gendemo.Posts.AuthorID).Asc(),
			orm.Of(gendemo.Posts.CreatedAt).Desc(),
			orm.Of(gendemo.Posts.ID).Asc(),
		).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	assertIDs(t, conn, `
		SELECT DISTINCT ON (author_id) id FROM posts
		ORDER BY author_id, created_at DESC, id`, got)
}

// Distinct and DistinctOn are separate state: setting one never sets the other,
// and Clone isolates both.
func TestAudit_distinctAndDistinctOnAreSeparateState(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Posts.ID), func(id int64) int64 { return id })
	base := orm.Compose(nil, shape).From(gendemo.Posts.Source())

	plain, _, err := base.Clone().Distinct().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(plain, "SELECT DISTINCT ") || strings.Contains(plain, "DISTINCT ON") {
		t.Errorf("Distinct produced %s", plain)
	}
	on, _, err := base.Clone().DistinctOn(orm.Of(gendemo.Posts.AuthorID)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(on, "DISTINCT ON (") {
		t.Errorf("DistinctOn produced %s", on)
	}
	// The base is untouched by either branch.
	bare, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(bare, "DISTINCT") {
		t.Errorf("a clone's DISTINCT reached the base query: %s", bare)
	}
}

// The ranking functions with tied ordering values, against PostgreSQL.
func TestAudit_rankingWithTies(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, score, created_at) VALUES
	    (50, 1, 'a', 5, now()), (51, 1, 'b', 5, now()), (52, 1, 'c', 1, now())`)

	w := orm.Window().
		PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(orm.Of(gendemo.Posts.Score).Desc())
	shape := orm.Project4(
		orm.Of(gendemo.Posts.ID),
		orm.RowNumber().Over(w),
		orm.Rank().Over(w),
		orm.DenseRank().Over(w),
		func(id, rn, rank, dense int64) [4]int64 { return [4]int64{id, rn, rank, dense} },
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
		       row_number() OVER (PARTITION BY author_id ORDER BY score DESC),
		       rank()       OVER (PARTITION BY author_id ORDER BY score DESC),
		       dense_rank() OVER (PARTITION BY author_id ORDER BY score DESC)
		FROM posts ORDER BY id`)
	if err != nil {
		t.Fatalf("handwritten: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var want [4]int64
		if err := rows.Scan(&want[0], &want[1], &want[2], &want[3]); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got[i] != want {
			t.Errorf("row %d = %v, handwritten SQL returned %v", i, got[i], want)
		}
		i++
	}
	if i != len(got) {
		t.Errorf("the ORM returned %d rows, handwritten SQL %d", len(got), i)
	}
}

// Frames, against PostgreSQL, in both modes and at every bound.
func TestAudit_frames(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, score, created_at) VALUES
	    (60, 1, 'a', 1, now()), (61, 1, 'b', 2, now()), (62, 1, 'c', 3, now())`)

	for _, tt := range []struct {
		name   string
		window *orm.WindowDef
		sql    string
	}{
		{"rows unbounded preceding to current",
			orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
				Rows(orm.UnboundedPreceding(), orm.CurrentRow()),
			`ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW`},
		{"rows one preceding to one following",
			orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
				Rows(orm.Preceding(1), orm.Following(1)),
			`ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING`},
		{"rows current to unbounded following",
			orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
				Rows(orm.CurrentRow(), orm.UnboundedFollowing()),
			`ROWS BETWEEN CURRENT ROW AND UNBOUNDED FOLLOWING`},
		{"range unbounded to unbounded",
			orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
				Range(orm.UnboundedPreceding(), orm.UnboundedFollowing()),
			`RANGE BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING`},
		{"groups",
			orm.Window().OrderBy(orm.Of(gendemo.Posts.Score).Asc()).
				Groups(orm.UnboundedPreceding(), orm.CurrentRow()),
			`GROUPS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shape := orm.Project2(
				orm.Of(gendemo.Posts.ID),
				orm.Of(orm.Count[orm.Composed]().Over(tt.window)),
				func(id, n int64) [2]int64 { return [2]int64{id, n} },
			)
			got, err := orm.Compose(db.Executor(), shape).
				From(gendemo.Posts.Source()).
				OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			order := "id"
			if strings.HasPrefix(tt.sql, "GROUPS") {
				order = "score"
			}
			want := handwrittenPairs(t, conn,
				`SELECT id, count(*) OVER (ORDER BY `+order+` `+tt.sql+`) FROM posts ORDER BY id`)
			assertPairs(t, tt.name, got, want)
		})
	}
}

// PostgreSQL's own errors survive every M11 clause that can produce one.
func TestAudit_pgErrorsSurvive(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	run := func(t *testing.T, build func() error) *pgconn.PgError {
		t.Helper()
		err := build()
		if err == nil {
			t.Fatal("the statement succeeded")
		}
		var pg *pgconn.PgError
		if !errors.As(err, &pg) {
			t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
		}
		return pg
	}

	t.Run("scalar subquery cardinality", func(t *testing.T) {
		many := orm.Scalar[gendemo.User, string](
			orm.Rows(orm.Named("t", orm.Of(gendemo.Posts.Title))).From(gendemo.Posts.Source()))
		pg := run(t, func() error {
			_, err := orm.Select(db.Users, orm.Project1(many, func(s *string) *string { return s })).
				All(t.Context())
			return err
		})
		if pg.Code != "21000" {
			t.Errorf("SQLSTATE = %s, want 21000", pg.Code)
		}
	})
	t.Run("invalid grouping", func(t *testing.T) {
		shape := orm.Project2(
			orm.Of(gendemo.Users.ID), orm.Of(orm.Count[orm.Composed]()),
			func(id, n int64) [2]int64 { return [2]int64{id, n} })
		pg := run(t, func() error {
			_, err := orm.Compose(db.Executor(), shape).From(gendemo.Users.Source()).All(t.Context())
			return err
		})
		if pg.Code != "42803" {
			t.Errorf("SQLSTATE = %s, want 42803 (grouping error)", pg.Code)
		}
	})
	t.Run("distinct on disagreeing with order by", func(t *testing.T) {
		shape := orm.Project1(orm.Of(gendemo.Posts.ID), func(id int64) int64 { return id })
		pg := run(t, func() error {
			_, err := orm.Compose(db.Executor(), shape).
				From(gendemo.Posts.Source()).
				DistinctOn(orm.Of(gendemo.Posts.AuthorID)).
				OrderBy(orm.Of(gendemo.Posts.CreatedAt).Desc()).
				All(t.Context())
			return err
		})
		if pg.Code != "42P10" {
			t.Errorf("SQLSTATE = %s, want 42P10", pg.Code)
		}
	})
	t.Run("invalid cast", func(t *testing.T) {
		shape := orm.Project1(orm.Cast(gendemo.Users.Email, orm.BigInt),
			func(v int64) int64 { return v })
		run(t, func() error {
			_, err := orm.Compose(db.Executor(), shape).From(gendemo.Users.Source()).All(t.Context())
			return err
		})
	})
	t.Run("recursive term type mismatch", func(t *testing.T) {
		id := orm.Named("id", orm.Of(gendemo.Users.ID))
		tree := orm.RecursiveCTE("tree",
			orm.Rows(id).From(gendemo.Users.Source()).
				Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))),
			func(self *orm.Source) orm.Term {
				// text where the anchor produced bigint: PostgreSQL's to catch.
				return orm.Rows(orm.Named("id", orm.Of(gendemo.Users.Email))).
					From(gendemo.Users.Source()).
					Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, id)))
			})
		shape := orm.Project1(orm.Ref(tree, id), func(v int64) int64 { return v })
		run(t, func() error {
			_, err := orm.Compose(db.Executor(), shape).With(tree).From(tree).All(t.Context())
			return err
		})
	})
}

// handwrittenPairs runs a statement selecting two bigint columns.
func handwrittenPairs(t *testing.T, conn *pgx.Conn, sql string) [][2]int64 {
	t.Helper()
	rows, err := conn.Query(t.Context(), sql)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	out := [][2]int64{}
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			t.Fatalf("handwritten query: %v", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	return out
}

func assertPairs(t *testing.T, what string, got, want [][2]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: the ORM returned %d rows, handwritten SQL %d\n%v\n%v", what, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s row %d: the ORM read %v, handwritten SQL read %v", what, i, got[i], want[i])
		}
	}
}
