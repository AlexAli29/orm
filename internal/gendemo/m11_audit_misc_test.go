package gendemo_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// The remaining M11 audit: alias namespaces, CTE reuse, savepoints, window
// placement in nested scopes, and the composed nullability graph end to end.

// PostgreSQL's alias namespaces are not one namespace, and the builder must
// neither merge them nor over-reject.
func TestAudit_aliasNamespaces(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))

	t.Run("a CTE may be named after a table it does not shadow in its own body", func(t *testing.T) {
		// The body reads the physical table; the outer query reads the CTE.
		cte := orm.CTE("users", orm.Rows(id).From(gendemo.Users.Source()))
		shape := orm.Project1(orm.Ref(cte, id), func(v int64) int64 { return v })
		got, err := orm.Compose(db.Executor(), shape).With(cte).From(cte).
			OrderBy(orm.Ref(cte, id).Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		assertIDs(t, conn, `WITH users AS (SELECT id FROM users) SELECT id FROM users ORDER BY id`, got)
	})

	t.Run("a table alias and a CTE name may coincide across levels", func(t *testing.T) {
		// The CTE is called "u"; a table occurrence in the outer query is also
		// aliased "u" — but they are in one FROM clause, so it is a collision.
		cte := orm.CTE("u", orm.Rows(id).From(gendemo.Users.Source()))
		alias := gendemo.Users.As("u")
		shape := orm.Project1(orm.Ref(cte, id), func(v int64) int64 { return v })
		_, _, err := orm.Compose(nil, shape).
			With(cte).From(cte).
			Join(alias.Source(), orm.Eq(alias.ID, orm.Ref(cte, id))).
			SQL()
		if err == nil {
			t.Fatal("a CTE reference and a table alias claimed one name in one FROM clause")
		}
		if !strings.Contains(err.Error(), "alias collision") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("a result alias does not collide with a source alias", func(t *testing.T) {
		// Naming a result column "users" is legal: result names and FROM-item
		// names are different namespaces, and over-rejecting would refuse SQL
		// PostgreSQL runs.
		shape := orm.Project1(orm.Of(gendemo.Users.ID).As("users"), func(v int64) int64 { return v })
		if _, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Users.Source()).All(t.Context()); err != nil {
			t.Fatalf("a result alias matching a table name was refused: %v", err)
		}
	})

	t.Run("a derived alias and a table alias collide", func(t *testing.T) {
		sub := orm.Sub("u", orm.Rows(id).From(gendemo.Users.Source()))
		alias := gendemo.Users.As("u")
		shape := orm.Project1(orm.Ref(sub, id), func(v int64) int64 { return v })
		_, _, err := orm.Compose(nil, shape).
			From(sub).
			Join(alias.Source(), orm.Eq(alias.ID, orm.Ref(sub, id))).
			SQL()
		if err == nil {
			t.Fatal("a derived table and a table alias claimed one name")
		}
	})
}

// One CTE referenced twice is one WITH item in the SQL, not two subqueries.
func TestAudit_cteIsDeclaredOnceAndReferencedTwice(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	cte := orm.CTE("c", orm.Rows(id).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))))
	a, b := cte.As("a"), cte.As("b")

	shape := orm.Project2(orm.Ref(a, id), orm.Ref(b, id),
		func(x, y int64) [2]int64 { return [2]int64{x, y} })
	sql, args, err := orm.Compose(db.Executor(), shape).
		With(cte).From(a).
		Join(b, orm.Cond(orm.Expr[orm.Composed](`"b"."id" > "a"."id"`))).
		OrderBy(orm.Ref(a, id).Asc(), orm.Ref(b, id).Asc()).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	// The body appears once, and the argument inside it is bound once.
	if n := strings.Count(sql, `FROM "public"."users"`); n != 1 {
		t.Errorf("the CTE body appears %d times in the statement:\n%s", n, sql)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want the body's single argument bound once", args)
	}

	got, err := orm.Compose(db.Executor(), shape).
		With(cte).From(a).
		Join(b, orm.Cond(orm.Expr[orm.Composed](`"b"."id" > "a"."id"`))).
		OrderBy(orm.Ref(a, id).Asc(), orm.Ref(b, id).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	assertPairs(t, "reused CTE", got, handwrittenPairs(t, conn, `
		WITH c AS (SELECT id FROM users WHERE active = true)
		SELECT a.id, b.id FROM c a JOIN c b ON b.id > a.id ORDER BY a.id, b.id`))
}

// A nested transaction is a savepoint: an M11 query inside it sees the nested
// changes, and after the savepoint rolls back the outer query does not.
func TestAudit_savepointVisibility(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	sub := orm.Sub("s", orm.Rows(id).From(gendemo.Users.Source()))
	shape := orm.Project1(orm.Ref(sub, id), func(v int64) int64 { return v })
	count := func(ex orm.Executor) int {
		t.Helper()
		got, err := orm.Compose(ex, shape).From(sub).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return len(got)
	}

	sentinel := errors.New("roll back the savepoint")
	err := db.Tx(t.Context(), func(outer *gendemo.DB) error {
		before := count(outer.Executor())

		nested := outer.Tx(t.Context(), func(inner *gendemo.DB) error {
			rows, err := inner.Executor().Query(t.Context(),
				`INSERT INTO users (id, email, age, active, state, created_at)
				 VALUES (91, 'sp@example.com', 30, true, 'active', now())`)
			if err != nil {
				return err
			}
			rows.Close()
			if n := count(inner.Executor()); n != before+1 {
				t.Errorf("inside the savepoint a derived query saw %d rows, want %d", n, before+1)
			}
			return sentinel
		})
		if !errors.Is(nested, sentinel) {
			t.Fatalf("the nested transaction returned %v", nested)
		}
		// The savepoint rolled back; the outer transaction is still usable and
		// no longer sees the row.
		if n := count(outer.Executor()); n != before {
			t.Errorf("after the savepoint rolled back the query saw %d rows, want %d", n, before)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
}

// A window function is refused in the clauses PostgreSQL refuses it in, and
// accepted in the two it allows — the select list and ORDER BY.
func TestAudit_windowClausePlacement(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	rn := orm.RowNumber().Over(orm.Window().OrderBy(orm.Of(gendemo.Posts.ID).Asc()))
	shape := orm.Project2(orm.Of(gendemo.Posts.ID), rn,
		func(id, n int64) [2]int64 { return [2]int64{id, n} })

	t.Run("ORDER BY accepts one", func(t *testing.T) {
		if _, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Posts.Source()).
			OrderBy(rn.Desc()).
			All(t.Context()); err != nil {
			t.Fatalf("a window function in ORDER BY was refused: %v", err)
		}
	})
	t.Run("a nested query may use one where its own clause allows it", func(t *testing.T) {
		// The window lives in the derived table's select list, and the filter
		// on it lives outside — which is the only way PostgreSQL allows it.
		pid := orm.Named("id", orm.Of(gendemo.Posts.ID))
		num := orm.Named("rn", rn)
		inner := orm.Sub("p", orm.Rows(pid, num).From(gendemo.Posts.Source()))
		out := orm.Project1(orm.Ref(inner, pid), func(v int64) int64 { return v })
		if _, err := orm.Compose(db.Executor(), out).
			From(inner).
			Where(orm.Ref(inner, num).Eq(int64(1))).
			All(t.Context()); err != nil {
			t.Fatalf("filtering a window through a derived table was refused: %v", err)
		}
	})
	t.Run("a window inside a subquery does not poison the outer WHERE", func(t *testing.T) {
		// The EXISTS subquery selects a window function; the outer WHERE holds
		// an EXISTS, not a window, and must be accepted.
		pid := orm.Named("rn", rn)
		inner := orm.Rows(pid).From(gendemo.Posts.Source())
		out := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
		if _, err := orm.Compose(db.Executor(), out).
			From(gendemo.Users.Source()).
			Where(orm.Exists[orm.Composed](inner)).
			All(t.Context()); err != nil {
			t.Fatalf("an EXISTS whose subquery selects a window was refused: %v", err)
		}
	})
}

// LATERAL is only legal before a subquery. Attaching it to a table or a CTE
// reference is refused by the builder, which knows the source is not one —
// PostgreSQL reports it as "syntax error at end of input", which says nothing.
func TestAudit_lateralOnANonSubquerySource(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	cte := orm.CTE("c", orm.Rows(id).From(gendemo.Users.Source()))

	for _, tt := range []struct {
		name string
		src  *orm.Source
	}{
		{"a table", gendemo.Posts.Source()},
		{"a CTE reference", cte},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := orm.Compose(nil, shape).
				From(gendemo.Users.Source()).
				CrossJoinLateral(tt.src).
				SQL()
			if err == nil {
				t.Fatal("LATERAL before a source that is not a subquery was accepted")
			}
			if !strings.Contains(err.Error(), "LATERAL applies to a subquery") {
				t.Errorf("error = %v", err)
			}
		})
	}

	// A derived source is exactly what it does apply to.
	sub := orm.Sub("s", orm.Rows(id).From(gendemo.Users.Source()))
	if _, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		CrossJoinLateral(sub).
		SQL(); err != nil {
		t.Fatalf("LATERAL before a subquery was refused: %v", err)
	}
}

// A derived source is a snapshot of the query it was built from. Changing the
// builder afterwards does not reach back into a source already made, which is
// what makes one definition safe to hand around.
func TestAudit_derivedSourceIsASnapshot(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	q := orm.Rows(id).From(gendemo.Users.Source())
	before := orm.Sub("s", q)

	q.Where(orm.Cond(gendemo.Users.Active.Eq(true)))
	after := orm.Sub("s", q)

	shape := orm.Project1(orm.Ref(before, id), func(v int64) int64 { return v })
	beforeSQL, beforeArgs, err := orm.Compose(nil, shape).From(before).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	afterSQL, afterArgs, err := orm.Compose(nil,
		orm.Project1(orm.Ref(after, id), func(v int64) int64 { return v })).From(after).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(beforeArgs) != 0 {
		t.Errorf("the earlier source picked up a later condition: %s %v", beforeSQL, beforeArgs)
	}
	if len(afterArgs) != 1 {
		t.Errorf("the later source did not pick up the condition: %s %v", afterSQL, afterArgs)
	}
}

// The nullability graph end to end: an outer-joined source, cast, wrapped in a
// CASE, carried through a window, put in a derived table, outer-joined again,
// and finally read. Nothing along the way may claim the value cannot be NULL.
func TestAudit_maximalNullabilityGraph(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (99, 'no match')`)

	child := gendemo.Users.As("child")
	// age is NOT NULL in the schema; every step below has to keep it nullable.
	age := orm.Opt(child.Age)
	cast := orm.CastNull(age, orm.BigInt)
	branch := orm.Case(orm.Cond(gendemo.Categories.ID.Gt(int64(0))), cast).Else(cast)
	lagged := orm.Lag(branch).Over(orm.Window().OrderBy(orm.Of(gendemo.Categories.ID).Asc()))

	catID := orm.Named("cat", orm.Of(gendemo.Categories.ID))
	value := orm.Named("v", lagged)
	inner := orm.Sub("inner", orm.Rows(catID, value).
		From(gendemo.Categories.Source()).
		LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)))

	shape := orm.Project2(
		orm.Of(gendemo.Categories.ID), orm.OptRef(inner, value),
		func(id int64, v *int64) *int64 { return v },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(inner, orm.Eq(orm.Ref(inner, catID), gendemo.Categories.ID)).
		OrderBy(orm.Of(gendemo.Categories.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no rows, so the graph proved nothing")
	}
	// Every value in this data is NULL — through the join, the cast, the CASE,
	// the window and the second join — and none of it was a scan error.
	for i, v := range got {
		if v != nil {
			t.Errorf("row %d read %d; every value in this data is NULL", i, *v)
		}
	}
}

// A composed query streaming a large result does not buffer it.
func TestAudit_largeResultStreams(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+`
		INSERT INTO users (id, email, age, tags, settings)
		  SELECT g, 'u'||g||'@example.com', 30, '{}', '{}' FROM generate_series(1, 100000) g;`)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	sub := orm.Sub("s", orm.Rows(id).From(gendemo.Users.Source()))
	shape := orm.Project2(
		orm.Ref(sub, id),
		orm.RowNumber().Over(orm.Window().OrderBy(orm.Ref(sub, id).Asc())),
		func(a, n int64) int64 { return a },
	)

	seen := 0
	for _, err := range orm.Compose(db.Executor(), shape).From(sub).Rows(t.Context()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		seen++
		if seen == 10 {
			break
		}
	}
	if seen != 10 {
		t.Fatalf("read %d rows before stopping, want 10", seen)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections still acquired after stopping a 100k-row stream", n)
	}
	// The whole thing still reads, and the connection comes back.
	all, err := orm.Compose(db.Executor(), shape).From(sub).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 100000 {
		t.Errorf("read %d rows, want 100000", len(all))
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections still acquired", n)
	}
}

var _ = pgx.TxOptions{}
