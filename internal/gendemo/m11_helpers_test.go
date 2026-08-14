package gendemo_test

import (
	"slices"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The M11 differential harness.
//
// Every claim about composed SQL is settled the same way: the ORM builds a
// statement, a handwritten statement answers the same question, and the two
// rowsets are compared on one database. A test that only asserted the generated
// text would prove the compiler agrees with itself.

// m11env creates a database, seeds it, and returns a generated handle and the
// connection under it. Both talk to the same database, which is what makes the
// handwritten comparison a comparison.
func m11env(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	dsn := testdb.Create(t, schema(t)+seed)
	conn := testdb.Connect(t, dsn)
	return gendemo.New(conn), conn
}

// m11exec runs a statement for its effect, so that a test can seed the rows its
// claim needs without going through the thing under test.
func m11exec(t *testing.T, conn *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), sql, args...); err != nil {
		t.Fatalf("executing %s: %v", sql, err)
	}
}

// handwrittenIDs runs a statement selecting one bigint column.
func handwrittenIDs(t *testing.T, conn *pgx.Conn, sql string, args ...any) []int64 {
	t.Helper()
	rows, err := conn.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("handwritten query: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	return out
}

// assertIDs compares what the ORM returned against what the handwritten
// statement returns, on the same rows.
func assertIDs(t *testing.T, conn *pgx.Conn, sql string, got []int64) {
	t.Helper()
	want := handwrittenIDs(t, conn, sql)
	if !slices.Equal(got, want) {
		t.Errorf("the ORM returned %v; the handwritten statement\n%s\nreturned %v", got, sql, want)
	}
}

// userIDs runs an entity query and reports the ids it matched.
func userIDs(t *testing.T, q *orm.Query[gendemo.User]) []int64 {
	t.Helper()
	users, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	out := []int64{}
	for _, u := range users {
		out = append(out, u.ID)
	}
	return out
}

// pgTypeOf reports what PostgreSQL says the expression's type is.
//
// It is how every claim this package makes about a result type is checked
// against the server rather than against a comment: the ORM says row_number is
// bigint, and pg_typeof is asked whether it agrees.
func pgTypeOf(t *testing.T, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	var typ string
	if err := conn.QueryRow(t.Context(), sql, args...).Scan(&typ); err != nil {
		t.Fatalf("pg_typeof via %s: %v", sql, err)
	}
	return typ
}

// m12env is m11env with the identity sequences advanced past the seeded rows.
//
// The seed inserts explicit ids, which leaves every identity sequence at one —
// so the next row PostgreSQL numbers itself would collide. Any real database
// whose rows arrived through the identity is already past that point, and a
// COPY test should not be measuring the fixture.
func m12env(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	db, conn := m11env(t)
	for _, table := range []string{"users", "posts", "comments", "categories", "profiles", "avatars"} {
		m11exec(t, conn, `SELECT setval(pg_get_serial_sequence($1, 'id'),
			GREATEST(coalesce((SELECT max(id) FROM `+table+`), 0), 1))`, table)
	}
	return db, conn
}

// poolConfig builds a pool configuration with a chosen size, so a contention
// test can give every worker a connection rather than measuring the pool.
func poolConfig(t *testing.T, dsn string, max int32) *pgxpool.Config {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.MaxConns = max
	cfg.AfterConnect = gendemo.RegisterTypes
	return cfg
}

// poolFrom opens a pool and closes it when the test finishes.
func poolFrom(t *testing.T, cfg *pgxpool.Config) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// settledAcquired waits, briefly, for a pool to finish releasing.
//
// A connection broken by a cancellation is destroyed asynchronously, so reading
// the statistics the instant a call returns can catch a connection on its way
// out. Waiting distinguishes that from a leak: cleanup finishes in
// milliseconds, and a leak never does.
func settledAcquired(t *testing.T, pool *pgxpool.Pool) int32 {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var n int32
	for time.Now().Before(deadline) {
		if n = pool.Stat().AcquiredConns(); n == 0 {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
	return n
}
