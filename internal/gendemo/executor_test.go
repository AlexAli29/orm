package gendemo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A repository reads through whatever satisfies Executor. These tests run the
// same query through each of pgx's three, because "it compiles" and "it reads
// what that connection can see" are different claims, and only the second one
// matters for a transaction.

func TestExecutor_conn(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	users, err := gendemo.New(conn).Users.Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 3 {
		t.Errorf("read %d users through a *pgx.Conn, want 3", len(users))
	}
}

func TestExecutor_pool(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	defer pool.Close()

	users, err := gendemo.New(pool).Users.Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 3 {
		t.Errorf("read %d users through a *pgxpool.Pool, want 3", len(users))
	}
}

func TestExecutor_transaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()

	// Insert inside the transaction. Nothing outside it can see this row, so
	// reading it back proves the repository really is using the transaction
	// rather than a connection that merely came from the same database.
	if _, err := tx.Exec(t.Context(),
		`INSERT INTO users (id, email, age, active, state, tags, settings, created_at)
		 VALUES (99, 'ghost@example.com', 21, true, 'active', '{}', '{}', now())`); err != nil {
		t.Fatalf("inserting inside the transaction: %v", err)
	}

	inTx := gendemo.New(tx)
	users, err := inTx.Users.Query().Where(gendemo.Users.Email.Eq("ghost@example.com")).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 1 || users[0].Age != 21 {
		t.Fatalf("the transaction cannot see its own uncommitted row: %+v", users)
	}

	// A second session cannot. Note that conn itself would see the row: the
	// transaction is open *on* it, so a query through conn runs inside the
	// transaction too. Isolation is only observable from somewhere else.
	other, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting a second session: %v", err)
	}
	defer func() { _ = other.Close(context.WithoutCancel(t.Context())) }()

	outside, err := gendemo.New(other).Users.Query().
		Where(gendemo.Users.Email.Eq("ghost@example.com")).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(outside) != 0 {
		t.Errorf("another session read the uncommitted row: %+v", outside)
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	after, err := gendemo.New(conn).Users.Query().
		Where(gendemo.Users.Email.Eq("ghost@example.com")).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("the rolled-back row survived: %+v", after)
	}
}

func TestExecutor_queriesInsideAndOutsideATransactionAgreeOnSQL(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()

	// Which executor a repository holds changes where the statement runs, not
	// what the statement is.
	build := func(db *gendemo.DB) string {
		sql, _, err := db.Users.Query().Where(gendemo.Users.Active.Eq(true)).Limit(5).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		return sql
	}
	if a, b := build(gendemo.New(conn)), build(gendemo.New(tx)); a != b {
		t.Errorf("the statement differs by executor:\n%s\n%s", a, b)
	}
	if !strings.Contains(build(gendemo.New(tx)), `FROM "public"."users"`) {
		t.Error("the statement does not name the table")
	}
}
