package gendemo_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The M11 composition audit: placeholders, derived-column binding, builder
// immutability and transaction behaviour.

// Release-critical: every argument reaches the clause it was written for.
//
// Counting placeholders proves nothing — a statement whose arguments are all
// bound one position out has exactly the right count. So the values are chosen
// so that swapping any two changes the answer, and the answer is compared with
// a handwritten statement whose placeholders are numbered by hand.
func TestAudit_everyArgumentReachesItsOwnClause(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO posts (id, author_id, title, published, score, created_at)
	    VALUES (70, 1, 'kept', false, 5, now()), (71, 3, 'zzz', true, 1, now())`)

	cteID := orm.Named("id", orm.Of(gendemo.Users.ID))
	// $1 — inside the WITH item.
	active := orm.CTE("c", orm.Rows(cteID).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Age.Gte(int32(18)))))

	u := gendemo.Users.As("u")
	p := gendemo.Posts.As("p")

	// $2, $3, $4 — the CASE, in the select list.
	label := orm.Case(orm.Cond(u.Age.Gt(int32(40))), orm.Val("old")).Else(orm.Val("young"))
	// $5 — a correlated scalar subquery, in the select list.
	scored := orm.Scalar[orm.Composed, int64](
		orm.Rows(orm.Named("n", orm.Of(orm.Count[orm.Composed]()))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(gendemo.Posts.AuthorID, orm.Ref(active, cteID)),
				orm.Cond(gendemo.Posts.Score.Gte(int32(3)))))
	// $6 — an aggregate FILTER, in the select list.
	drafts := orm.Of(orm.CountOf(p.ID).Filter(p.Published.Eq(false)))

	// $7 — a join condition. $8 — inside a LATERAL. $9 — WHERE. $10 — HAVING.
	latestID := orm.Named("id", orm.Of(gendemo.Posts.ID))
	lateral := orm.Sub("l", orm.Rows(latestID).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, orm.Ref(active, cteID)),
			orm.Cond(gendemo.Posts.Title.Ne("zzz"))).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc()).
		Limit(1))

	type row struct {
		ID     int64
		Label  string
		Scored *int64
		Drafts int64
	}
	shape := orm.Project4(
		orm.Ref(active, cteID), label, scored, drafts,
		func(id int64, label string, scored *int64, drafts int64) row {
			return row{ID: id, Label: label, Scored: scored, Drafts: drafts}
		},
	)

	q := orm.Compose(db.Executor(), shape).
		With(active).
		From(active).
		Join(u.Source(), orm.Eq(u.ID, orm.Ref(active, cteID)), orm.Cond(u.Active.Eq(true))).
		LeftJoinLateral(lateral).
		LeftJoin(p.Source(), orm.Eq(p.AuthorID, orm.Ref(active, cteID))).
		Where(orm.Cond(u.Email.Ne("nobody@example.com"))).
		GroupBy(orm.Ref(active, cteID), orm.Of(u.Age)).
		Having(orm.Of(orm.CountOf(p.ID)).Gte(int64(0))).
		OrderBy(orm.Ref(active, cteID).Asc())

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := []any{
		int32(18),            // $1  WITH
		int32(40),            // $2  CASE condition
		"old",                // $3  CASE then
		"young",              // $4  CASE else
		int32(3),             // $5  scalar subquery
		false,                // $6  aggregate FILTER
		true,                 // $7  join ON
		"zzz",                // $8  LATERAL
		"nobody@example.com", // $9  WHERE
		int64(0),             // $10 HAVING
	}
	if len(args) != len(want) {
		t.Fatalf("the statement binds %d arguments, want %d\n%s\n%v", len(args), len(want), sql, args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("argument $%d = %v, want %v — an argument reached the wrong clause\n%s",
				i+1, args[i], want[i], sql)
		}
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// The same question by hand, with the placeholders numbered by hand.
	rows, err := conn.Query(t.Context(), `
		WITH c AS (SELECT id FROM users WHERE age >= $1)
		SELECT c.id,
		       CASE WHEN u.age > $2 THEN $3 ELSE $4 END,
		       (SELECT count(*) FROM posts WHERE posts.author_id = c.id AND posts.score >= $5),
		       count(p.id) FILTER (WHERE p.published = $6)
		FROM c
		JOIN users u ON u.id = c.id AND u.active = $7
		LEFT JOIN LATERAL (
		    SELECT id FROM posts
		    WHERE posts.author_id = c.id AND posts.title <> $8
		    ORDER BY id LIMIT 1
		) l ON true
		LEFT JOIN posts p ON p.author_id = c.id
		WHERE u.email <> $9
		GROUP BY c.id, u.age
		HAVING count(p.id) >= $10
		ORDER BY c.id`, want...)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var w row
		if err := rows.Scan(&w.ID, &w.Label, &w.Scored, &w.Drafts); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(got) {
			t.Fatalf("the ORM returned %d rows, handwritten SQL returned more", len(got))
		}
		if got[i].ID != w.ID || got[i].Label != w.Label || got[i].Drafts != w.Drafts ||
			!sameInt(got[i].Scored, w.Scored) {
			t.Errorf("row %d = %+v, handwritten SQL returned %+v", i, got[i], w)
		}
		i++
	}
	if i != len(got) {
		t.Errorf("the ORM returned %d rows, handwritten SQL %d", len(got), i)
	}
	if i == 0 {
		t.Fatal("the statement returned no rows, so it proved nothing about the values")
	}
}

func sameInt(a, b *int64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

// A derived column is bound by name, and the name is exactly the one declared —
// including its case. Two same-typed outputs must not be able to swap places.
func TestAudit_derivedColumnBinding(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	t.Run("same-typed outputs do not swap", func(t *testing.T) {
		// Both outputs are bigint, so a positional mistake would be invisible
		// to the type system and to PostgreSQL.
		id := orm.Named("the_id", orm.Of(gendemo.Users.ID))
		age := orm.Named("the_age", orm.Cast(gendemo.Users.Age, orm.BigInt))
		src := orm.Sub("s", orm.Rows(id, age).From(gendemo.Users.Source()))

		// Read them back in the opposite order from the declaration.
		shape := orm.Project2(orm.Ref(src, age), orm.Ref(src, id),
			func(a, i int64) [2]int64 { return [2]int64{a, i} })
		got, err := orm.Compose(db.Executor(), shape).
			From(src).
			OrderBy(orm.Ref(src, id).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		want := handwrittenPairs(t, conn,
			`SELECT s.the_age, s.the_id FROM (SELECT id AS the_id, age::int8 AS the_age FROM users) s ORDER BY s.the_id`)
		assertPairs(t, "swapped read", got, want)
		for _, r := range got {
			if r[0] == r[1] {
				t.Fatal("the two columns are indistinguishable in this data, so the test proves nothing")
			}
		}
	})

	t.Run("names are case sensitive", func(t *testing.T) {
		lower := orm.Named("v", orm.Of(gendemo.Users.ID))
		upper := orm.Named("V", orm.Cast(gendemo.Users.Age, orm.BigInt))
		src := orm.Sub("s", orm.Rows(lower, upper).From(gendemo.Users.Source()))

		sql, _, err := orm.Compose(nil, orm.Project2(orm.Ref(src, lower), orm.Ref(src, upper),
			func(a, b int64) [2]int64 { return [2]int64{a, b} })).From(src).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, `"s"."v"`) || !strings.Contains(sql, `"s"."V"`) {
			t.Errorf("the two names were folded together: %s", sql)
		}
		shape := orm.Project2(orm.Ref(src, lower), orm.Ref(src, upper),
			func(a, b int64) [2]int64 { return [2]int64{a, b} })
		got, err := orm.Compose(db.Executor(), shape).From(src).
			OrderBy(orm.Ref(src, lower).Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		want := handwrittenPairs(t, conn,
			`SELECT s."v", s."V" FROM (SELECT id AS "v", age::int8 AS "V" FROM users) s ORDER BY s."v"`)
		assertPairs(t, "case sensitive", got, want)
	})

	t.Run("a name the source does not provide is refused", func(t *testing.T) {
		id := orm.Named("the_id", orm.Of(gendemo.Users.ID))
		other := orm.Named("elsewhere", orm.Of(gendemo.Users.ID))
		src := orm.Sub("s", orm.Rows(id).From(gendemo.Users.Source()))
		shape := orm.Project1(orm.Ref(src, other), func(v int64) int64 { return v })
		_, _, err := orm.Compose(nil, shape).From(src).SQL()
		if err == nil {
			t.Fatal("a column the derived table does not provide compiled")
		}
	})
}

// Cloning isolates every piece of builder state M11 added.
func TestAudit_cloneIsolatesEveryM11Clause(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	cte := orm.CTE("c", orm.Rows(id).From(gendemo.Users.Source()))
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })

	base := orm.Compose(nil, shape).From(gendemo.Users.Source())
	before, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	branch := base.Clone().
		With(cte).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		DistinctOn(orm.Of(gendemo.Users.ID)).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))).
		GroupBy(orm.Of(gendemo.Users.ID)).
		Having(orm.Of(orm.Count[orm.Composed]()).Gt(int64(0))).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		Limit(5).
		Offset(2)
	if _, _, err := branch.SQL(); err != nil {
		t.Fatalf("the branch does not compile: %v", err)
	}

	after, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if before != after {
		t.Errorf("cloning leaked into the base query:\n%s\n%s", before, after)
	}

	// And two clones of one base do not see each other.
	a := base.Clone().Where(orm.Cond(gendemo.Users.ID.Eq(int64(1))))
	b := base.Clone().Where(orm.Cond(gendemo.Users.ID.Eq(int64(2))))
	sqlA, argsA, _ := a.SQL()
	sqlB, argsB, _ := b.SQL()
	if sqlA != sqlB {
		t.Errorf("two clones produced different statements:\n%s\n%s", sqlA, sqlB)
	}
	if len(argsA) != 1 || len(argsB) != 1 || argsA[0] == argsB[0] {
		t.Errorf("clone arguments = %v and %v", argsA, argsB)
	}
}

// A window specification, a CASE and a subquery definition are reusable: using
// one in a statement does not change it for the next.
func TestAudit_sharedDefinitionsAreNotConsumed(t *testing.T) {
	w := orm.Window().
		PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(orm.Of(gendemo.Posts.ID).Asc())
	label := orm.Case(orm.Cond(gendemo.Posts.Published.Eq(true)), orm.Val("yes")).Else(orm.Val("no"))
	def := orm.Rows(orm.Named("id", orm.Of(gendemo.Posts.ID))).From(gendemo.Posts.Source())

	compile := func() (string, []any) {
		t := orm.Sub("s", def)
		shape := orm.Project3(
			orm.RowNumber().Over(w), label, orm.Ref(t, orm.Named("id", orm.Of(gendemo.Posts.ID))),
			func(n int64, l string, id int64) int64 { return n },
		)
		sql, args, err := orm.Compose(nil, shape).
			From(gendemo.Posts.Source()).
			CrossJoin(t).
			SQL()
		if err != nil {
			panic(err)
		}
		return sql, args
	}
	first, firstArgs := compile()
	second, secondArgs := compile()
	if first != second {
		t.Errorf("reusing shared definitions produced two statements:\n%s\n%s", first, second)
	}
	if len(firstArgs) != len(secondArgs) {
		t.Errorf("argument counts differ: %d and %d", len(firstArgs), len(secondArgs))
	}
}

// 64 goroutines compiling from one set of shared definitions, with the globals
// checked afterwards.
func TestAudit_sharedDefinitionsAreRaceFree(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	cte := orm.CTE("c", orm.Rows(id).From(gendemo.Users.Source()))
	def := orm.Rows(id).From(gendemo.Users.Source())
	sub := orm.Sub("s", def)
	manager := gendemo.Users.As("manager")
	w := orm.Window().PartitionBy(orm.Opt(manager.ID))
	label := orm.Case(orm.Cond(manager.Active.Eq(true)), orm.Val("y")).Else(orm.Val("n"))

	shape := orm.Project4(
		orm.Ref(cte, id), orm.Ref(sub, id), orm.RowNumber().Over(w), label,
		func(a, b, n int64, l string) int64 { return a },
	)
	build := func() (string, error) {
		sql, _, err := orm.Compose(nil, shape).
			With(cte).
			From(cte).
			Join(sub, orm.Eq(orm.Ref(sub, id), orm.Ref(cte, id))).
			LeftJoin(manager.Source(), orm.Eq(manager.ID, orm.Ref(cte, id))).
			Where(orm.Exists[orm.Composed](def)).
			SQL()
		return sql, err
	}
	want, err := build()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	var wg sync.WaitGroup
	bad := make([]string, 64)
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 8 {
				got, err := build()
				switch {
				case err != nil:
					bad[i] = err.Error()
				case got != want:
					bad[i] = "different SQL"
				}
			}
		}()
	}
	wg.Wait()
	for i, b := range bad {
		if b != "" {
			t.Fatalf("goroutine %d: %s", i, b)
		}
	}
	if gendemo.Users.Source().Ref() != "users" || cte.Name() != "c" || sub.Ref() != "s" {
		t.Error("a shared descriptor was mutated by compiling it")
	}
}

// Composed queries see the transaction they run in, and not otherwise.
func TestAudit_transactionVisibility(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	outside := gendemo.New(pool)
	other := testdb.Connect(t, dsn)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	cte := orm.CTE("c", orm.Rows(id).From(gendemo.Users.Source()))
	shape := orm.Project1(orm.Ref(cte, id), func(v int64) int64 { return v })
	count := func(ex orm.Executor) int {
		t.Helper()
		got, err := orm.Compose(ex, shape).With(cte).From(cte).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return len(got)
	}
	before := count(outside.Executor())

	err := outside.Tx(t.Context(), func(tx *gendemo.DB) error {
		ins, err := tx.Executor().Query(t.Context(),
			`INSERT INTO users (id, email, age, active, state, created_at)
			 VALUES (90, 'tx@example.com', 30, true, 'active', now())`)
		if err != nil {
			return err
		}
		ins.Close()
		if err := ins.Err(); err != nil {
			return err
		}
		// Inside the transaction, every M11 shape sees the new row.
		if n := count(tx.Executor()); n != before+1 {
			t.Errorf("inside the transaction a CTE query saw %d rows, want %d", n, before+1)
		}
		joined, err2 := orm.Compose(tx.Executor(),
			orm.Project2(orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Posts.ID),
				func(u int64, p *int64) int64 { return u })).
			From(gendemo.Users.Source()).
			LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
			All(t.Context())
		if err2 != nil {
			t.Fatalf("join inside the transaction: %v", err2)
		}
		found := false
		for _, u := range joined {
			if u == 90 {
				found = true
			}
		}
		if !found {
			t.Error("a join inside the transaction did not see the uncommitted row")
		}

		// A separate connection does not.
		var n int
		if err := other.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
			return err
		}
		if n != before {
			t.Errorf("another connection saw %d rows, want %d", n, before)
		}
		return errors.New("roll back")
	})
	if err == nil {
		t.Fatal("the transaction committed")
	}
	if n := count(outside.Executor()); n != before {
		t.Errorf("after the rollback the query saw %d rows, want %d", n, before)
	}
}

// A data-modifying WITH item is a write, and a read-only transaction refuses it
// with PostgreSQL's own error.
func TestAudit_writingCTEUnderAReadOnlyTransaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	shape := orm.Project1(gendemo.Users.ID.As("id"), func(id int64) int64 { return id })
	err := db.TxOptions(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx *gendemo.DB) error {
		// A plain composed read works.
		read := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
		if _, err := orm.Compose(tx.Executor(), read).From(gendemo.Users.Source()).All(t.Context()); err != nil {
			return err
		}
		changed := orm.WritingCTE("changed", orm.UpdateReturning(
			tx.Users.Update().Set(gendemo.Users.Age.Set(int32(1))).All(), shape))
		out := orm.Project1(orm.Ref(changed, orm.Named("id", orm.Of(gendemo.Users.ID))),
			func(v int64) int64 { return v })
		_, err := orm.Compose(tx.Executor(), out).With(changed).From(changed).All(t.Context())
		return err
	})
	if err == nil {
		t.Fatal("a data-modifying CTE ran in a read-only transaction")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
	}
	if pg.Code != "25006" {
		t.Errorf("SQLSTATE = %s, want 25006 (read-only sql transaction)", pg.Code)
	}
}

// Streaming a composed query releases its connection, and stopping early does
// too — otherwise a pool drains one row at a time.
func TestAudit_composedQueriesReleaseTheirConnections(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	sub := orm.Sub("s", orm.Rows(id).From(gendemo.Users.Source()))
	shape := orm.Project2(
		orm.Ref(sub, id), orm.RowNumber().Over(orm.Window().OrderBy(orm.Ref(sub, id).Asc())),
		func(a, n int64) int64 { return a },
	)

	for range 200 {
		q := orm.Compose(db.Executor(), shape).From(sub)
		if _, err := q.All(t.Context()); err != nil {
			t.Fatalf("All: %v", err)
		}
		// Stop after the first row.
		for _, err := range q.Rows(t.Context()) {
			if err != nil {
				t.Fatalf("Rows: %v", err)
			}
			break
		}
		if _, err := q.Count(t.Context()); err != nil {
			t.Fatalf("Count: %v", err)
		}
		// A statement that fails still releases.
		bad := orm.Compose(db.Executor(), shape).From(sub).
			Where(orm.Cond(orm.Expr[orm.Composed]("no_such_column = 1")))
		if _, err := bad.All(t.Context()); err == nil {
			t.Fatal("a broken statement succeeded")
		}
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections are still acquired after 200 composed queries", n)
	}
}

// poolFor opens a two-connection pool, so that a single leaked rowset makes
// the loops above hang rather than pass quietly.
func poolFor(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.MaxConns = 2
	cfg.AfterConnect = gendemo.RegisterTypes
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
