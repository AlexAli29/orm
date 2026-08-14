// Package ormtest is the testing toolkit for applications built on this ORM.
//
// It exists because the two things most tests need — a transaction that is
// always rolled back, and a way to empty a set of tables — are easy to get
// subtly wrong, and getting them wrong makes a suite that passes for the wrong
// reasons. A cleanup registered one line too late leaks a transaction when a
// test calls t.Fatal; a truncate written with string concatenation breaks on
// the first table whose name needs quoting.
//
// # It uses the same paths a production caller does
//
// Nothing here reaches into the ORM's internals to go faster. The transaction
// helper runs the ORM's own transaction; the truncate helper compiles through
// the ORM's own writer and identifier quoting; the migration helper runs the
// ORM's own migration engine; the schema check runs the same reconciliation the
// generator's check command runs.
//
// That is deliberate and it is the point. A test toolkit that took a shortcut
// past the thing it is testing would report green for code that does not work,
// which is worse than having no toolkit — and it is exactly the shortcut a
// toolkit is tempted into.
//
// # It never commits
//
// [Tx] rolls back, always, and this package offers no way to ask it not to.
//
// What it returns is the real pgx transaction, behind the ORM's [orm.Executor]
// interface. That is deliberate: a wrapper that hid everything but Query would
// make the helper's transaction behave differently from a production one — COPY
// would stop working inside it, and the ORM would stop recognising that it is in
// a transaction at all — and a toolkit whose transaction is not the real thing
// is testing the wrong object.
//
// The consequence is worth stating plainly rather than glossing: a caller who
// type-asserts the returned executor back to a pgx.Tx can commit it. That is
// leaving this package's API, not using it, and no interface could prevent it
// without giving up the fidelity above.
//
// # It adds no dependencies
//
// This package needs the ORM, pgx and the standard library. Running PostgreSQL
// in a container is a separate module — see ormtest/postgres — so that a
// project which supplies its own database never compiles Testcontainers or
// pulls its dependency tree.
package ormtest

import (
	"context"
	"fmt"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
)

// TB is the part of testing.TB this package uses.
//
// It is an interface rather than *testing.T so that the helpers work from a
// benchmark and from a fuzz target, and so that this package's own tests can
// prove what happens on failure without failing.
type TB interface {
	Helper()
	Cleanup(func())
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Failed() bool
	Context() context.Context
}

// Beginner is what [Tx] needs of a database handle: the ability to start a
// transaction.
//
// A *pgxpool.Pool and a *pgx.Conn both satisfy it, which is what makes the
// helper work with whatever the test already has. It is deliberately not the
// ORM's Executor: an Executor can run a statement, and starting a transaction
// is a different capability.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Tx starts a transaction and rolls it back when the test finishes.
//
// The rollback is registered before the transaction is returned, so it happens
// whatever the test does next — returning early, calling t.Fatal, or panicking.
// That ordering is the whole reason to use this rather than a defer, and it is
// what a test that calls Fatal from a helper goroutine needs.
//
//	func TestSomething(t *testing.T) {
//	    tx := ormtest.Tx(t, pool)
//	    db := domain.New(tx)
//
//	    user, err := db.Users.Insert(t.Context(), domain.User{Email: "a@b.c"})
//	    // ... assertions ...
//	}   // the insert is rolled back here
//
// The returned value is an [orm.Executor], so a generated database handle binds
// to it exactly as it binds to a pool — and it is the real transaction
// underneath, so COPY, savepoints and the ORM's own transaction detection all
// behave as they do in production. Everything the test does through it is
// inside the transaction and none of it survives.
//
// This package offers no way to commit. See the package documentation for what
// that does and does not guarantee.
func Tx(t TB, db Beginner) orm.Executor {
	t.Helper()
	if db == nil {
		t.Fatalf("ormtest.Tx: no database")
		return nil
	}

	ctx := t.Context()
	tx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("ormtest.Tx: beginning a transaction: %v", err)
		return nil
	}

	// Registered before anything can fail, and using a context of its own: the
	// test's context is cancelled before cleanups run, and a rollback on a
	// cancelled context would leave the transaction open on the server until
	// the connection went back to the pool.
	t.Cleanup(func() {
		if err := tx.Rollback(context.WithoutCancel(ctx)); err != nil && err != pgx.ErrTxClosed {
			t.Errorf("ormtest.Tx: rolling back: %v", err)
		}
	})
	return tx
}

// TxFunc runs fn inside a transaction that is rolled back afterwards.
//
// It is [Tx] for callers who prefer the work to be visibly scoped. The two are
// the same helper; neither commits.
func TxFunc(t TB, db Beginner, fn func(ex orm.Executor)) {
	t.Helper()
	fn(Tx(t, db))
}

// Table is what [Truncate] needs of a generated table descriptor: the schema
// and the table it names.
//
// Every generated descriptor satisfies it, so a caller writes Truncate(ctx, ex,
// Users, Posts) with the descriptors already in scope. Taking descriptors
// rather than strings is what keeps a typo a compile error and what makes the
// identifiers come from the catalog rather than from a caller's memory.
type Table interface {
	Source() *orm.Source
}

// TruncateOption configures [Truncate].
type TruncateOption func(*truncateConfig)

type truncateConfig struct {
	restartIdentity bool
	cascade         bool
}

// RestartIdentity resets the tables' identity sequences, so the next row starts
// at one again. It is PostgreSQL's RESTART IDENTITY and means exactly that.
func RestartIdentity() TruncateOption {
	return func(c *truncateConfig) { c.restartIdentity = true }
}

// Cascade truncates the tables that reference these ones as well.
//
// It is PostgreSQL's CASCADE. It empties tables the caller did not name, which
// is occasionally what a fixture wants and is never what it wants by accident,
// so it is opt-in and says so.
func Cascade() TruncateOption {
	return func(c *truncateConfig) { c.cascade = true }
}

// Truncate empties the named tables.
//
// One statement names every table, which is what makes it work in the presence
// of foreign keys between them: PostgreSQL truncates the whole set together and
// does not check references within it. Truncating them one at a time would fail
// on the first table another names.
//
// The executor decides the scope. Pass a transaction and the truncation is
// inside it and rolls back with it; pass a pool and it is committed.
func Truncate(ctx context.Context, ex orm.Executor, tables ...Table) error {
	return TruncateWith(ctx, ex, nil, tables...)
}

// TruncateWith is [Truncate] with options.
func TruncateWith(ctx context.Context, ex orm.Executor, opts []TruncateOption, tables ...Table) error {
	if ex == nil {
		return fmt.Errorf("ormtest: truncate needs an executor")
	}
	if len(tables) == 0 {
		return nil
	}
	var cfg truncateConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	names := make([]string, 0, len(tables))
	for i, tbl := range tables {
		if tbl == nil {
			return fmt.Errorf("ormtest: truncate was given no table at position %d", i)
		}
		src := tbl.Source()
		if src == nil {
			return fmt.Errorf("ormtest: truncate was given a table with no source at position %d", i)
		}
		// The identifier is quoted by the ORM's own writer rather than by
		// anything written here, so a schema or table needing quotes gets it,
		// and gets it the same way every statement in the project does.
		name, err := src.QuotedName()
		if err != nil {
			return fmt.Errorf("ormtest: truncate: %w", err)
		}
		names = append(names, name)
	}

	stmt := "TRUNCATE TABLE " + join(names, ", ")
	if cfg.restartIdentity {
		stmt += " RESTART IDENTITY"
	}
	if cfg.cascade {
		stmt += " CASCADE"
	}

	rows, err := ex.Query(ctx, stmt)
	if err != nil {
		return fmt.Errorf("ormtest: truncating: %w", err)
	}
	rows.Close()
	return rows.Err()
}

// MustTruncate is [Truncate] with the error reported to the test.
func MustTruncate(t TB, ex orm.Executor, tables ...Table) {
	t.Helper()
	if err := Truncate(t.Context(), ex, tables...); err != nil {
		t.Fatalf("%v", err)
	}
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
