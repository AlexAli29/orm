package ormtest_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/ormtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M15.1: the test toolkit.
//
// The claims are that a transaction always rolls back — whatever the test does,
// including failing and panicking — that truncation quotes its identifiers
// through the writer rather than by hand, and that the schema assertion runs the
// production reconciliation rather than something that agrees with it.

func pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.AfterConnect = gendemo.RegisterTypes
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// fakeT records what a helper did to a test without failing this one.
//
// It is how the failure paths are asserted: a helper that calls Fatalf has to
// be observed calling it, and observing that with a real *testing.T means
// failing the run.
type fakeT struct {
	ctx      context.Context
	cleanups []func()
	errs     []string
	fatals   []string
	failed   bool
}

func newFakeT(ctx context.Context) *fakeT { return &fakeT{ctx: ctx} }

func (f *fakeT) Helper()           {}
func (f *fakeT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) Errorf(format string, args ...any) {
	f.errs = append(f.errs, sprintf(format, args))
	f.failed = true
}
func (f *fakeT) Fatalf(format string, args ...any) {
	f.fatals = append(f.fatals, sprintf(format, args))
	f.failed = true
}
func (f *fakeT) Failed() bool             { return f.failed }
func (f *fakeT) Context() context.Context { return f.ctx }

// runCleanups runs them in reverse, the way testing does.
func (f *fakeT) runCleanups() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

func sprintf(format string, args []any) string {
	if len(args) == 0 {
		return format
	}
	return fmtSprintf(format, args...)
}

// Scenario A: a row created inside the helper's transaction is gone afterwards.
func TestTx_rollsBack(t *testing.T) {
	p := pool(t)
	const email = "rolled-back@example.com"

	// The nested test is where the helper's cleanup runs, so the assertion
	// afterwards observes what survived it.
	t.Run("inside", func(t *testing.T) {
		db := gendemo.New(ormtest.Tx(t, p))
		if _, err := db.Users.Insert(t.Context(), gendemo.User{
			Email: email, Age: 30, Active: true, State: gendemo.UserStateActive,
			Settings: map[string]any{}, Tags: []string{},
		}); err != nil {
			t.Fatalf("inserting: %v", err)
		}
		got, err := db.Users.Query().Where(gendemo.Users.Email.Eq(email)).Count(t.Context())
		if err != nil {
			t.Fatalf("counting inside: %v", err)
		}
		if got != 1 {
			t.Fatalf("the row is not visible inside its own transaction: %d", got)
		}
	})

	after := gendemo.New(p)
	n, err := after.Users.Query().Where(gendemo.Users.Email.Eq(email)).Count(t.Context())
	if err != nil {
		t.Fatalf("counting after: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows survived the test transaction", n)
	}
}

// Release-critical: the rollback is registered before anything can go wrong, so
// it happens even when the test fails or panics. A cleanup registered one line
// later would leak the transaction in exactly these two cases.
func TestTx_rollsBackAfterFailureAndPanic(t *testing.T) {
	p := pool(t)

	t.Run("after a fatal", func(t *testing.T) {
		const email = "fatal@example.com"
		f := newFakeT(t.Context())
		func() {
			db := gendemo.New(ormtest.Tx(f, p))
			if _, err := db.Users.Insert(t.Context(), gendemo.User{
				Email: email, Age: 1, Active: true, State: gendemo.UserStateActive,
				Settings: map[string]any{}, Tags: []string{},
			}); err != nil {
				t.Fatalf("inserting: %v", err)
			}
			// The test would call Fatal here; what matters is that the cleanup
			// was already registered.
			f.Fatalf("the test failed")
		}()
		f.runCleanups()

		n, err := gendemo.New(p).Users.Query().Where(gendemo.Users.Email.Eq(email)).Count(t.Context())
		if err != nil {
			t.Fatalf("counting: %v", err)
		}
		if n != 0 {
			t.Errorf("%d rows survived a failing test", n)
		}
	})

	t.Run("after a panic", func(t *testing.T) {
		const email = "panic@example.com"
		f := newFakeT(t.Context())
		func() {
			defer func() {
				_ = recover()
				f.runCleanups()
			}()
			db := gendemo.New(ormtest.Tx(f, p))
			if _, err := db.Users.Insert(t.Context(), gendemo.User{
				Email: email, Age: 1, Active: true, State: gendemo.UserStateActive,
				Settings: map[string]any{}, Tags: []string{},
			}); err != nil {
				t.Fatalf("inserting: %v", err)
			}
			panic("the test panicked")
		}()

		n, err := gendemo.New(p).Users.Query().Where(gendemo.Users.Email.Eq(email)).Count(t.Context())
		if err != nil {
			t.Fatalf("counting: %v", err)
		}
		if n != 0 {
			t.Errorf("%d rows survived a panicking test", n)
		}
	})
}

// The helper hands back the real transaction, which is what makes it faithful:
// COPY works inside it and the ORM knows it is in a transaction. The documented
// consequence is that a caller who type-asserts their way out can commit — so
// the contract is "this package offers no commit", not "committing is
// impossible", and the documentation says exactly that.
func TestTx_isTheRealTransaction(t *testing.T) {
	p := pool(t)
	ex := ormtest.Tx(t, p)
	if ex == nil {
		t.Fatal("no executor")
	}

	// Faithful: COPY needs the underlying transaction and would fail through a
	// wrapper that only forwarded Query.
	db := gendemo.New(ex)
	n, err := db.Categories.CopyFrom(t.Context(), []gendemo.Category{{Name: "copied-in-a-test-tx"}})
	if err != nil {
		t.Fatalf("COPY inside the test transaction: %v", err)
	}
	if n != 1 {
		t.Errorf("COPY copied %d rows", n)
	}

	// And the documented consequence, asserted so that nobody later "fixes" the
	// documentation by weakening it: the real transaction is reachable.
	if _, ok := ex.(interface {
		Commit(context.Context) error
	}); !ok {
		t.Error("the executor is not the real transaction; COPY and transaction detection depend on it being one")
	}
}

// Truncation empties what it is given, quotes through the writer, and is scoped
// by the executor it is handed.
func TestTruncate(t *testing.T) {
	p := pool(t)
	db := gendemo.New(p)

	seedOne := func() {
		t.Helper()
		if _, err := db.Users.Insert(t.Context(), gendemo.User{
			Email: "truncate@example.com", Age: 1, Active: true, State: gendemo.UserStateActive,
			Settings: map[string]any{}, Tags: []string{},
		}); err != nil {
			t.Fatalf("seeding: %v", err)
		}
	}

	t.Run("empties the tables", func(t *testing.T) {
		seedOne()
		// users is referenced by several tables, so emptying it means saying
		// what to do about them. Cascade is that answer and is opt-in for
		// exactly this reason: it empties tables the caller did not name.
		if err := ormtest.TruncateWith(t.Context(), p,
			[]ormtest.TruncateOption{ormtest.Cascade()}, gendemo.Users); err != nil {
			t.Fatalf("Truncate: %v", err)
		}
		n, err := db.Users.Query().Count(t.Context())
		if err != nil {
			t.Fatalf("counting: %v", err)
		}
		if n != 0 {
			t.Errorf("%d rows survived", n)
		}
	})

	t.Run("is scoped by the executor", func(t *testing.T) {
		seedOne()
		before, err := db.Users.Query().Count(t.Context())
		if err != nil {
			t.Fatalf("counting: %v", err)
		}
		t.Run("inside a rolled-back transaction", func(t *testing.T) {
			ex := ormtest.Tx(t, p)
			if err := ormtest.TruncateWith(t.Context(), ex,
				[]ormtest.TruncateOption{ormtest.Cascade()}, gendemo.Users); err != nil {
				t.Fatalf("Truncate: %v", err)
			}
		})
		after, err := db.Users.Query().Count(t.Context())
		if err != nil {
			t.Fatalf("counting: %v", err)
		}
		if after != before {
			t.Errorf("a truncate inside a rolled-back transaction changed the table: %d -> %d", before, after)
		}
	})

	t.Run("naming half of a reference is PostgreSQL's error, not a silent success", func(t *testing.T) {
		err := ormtest.Truncate(t.Context(), p, gendemo.Users)
		if err == nil {
			t.Fatal("truncating a referenced table alone succeeded")
		}
		if !strings.Contains(err.Error(), "foreign key") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("restart identity", func(t *testing.T) {
		if err := ormtest.TruncateWith(t.Context(), p,
			[]ormtest.TruncateOption{ormtest.RestartIdentity(), ormtest.Cascade()},
			gendemo.Users); err != nil {
			t.Fatalf("TruncateWith: %v", err)
		}
		inserted, err := db.Users.Insert(t.Context(), gendemo.User{
			Email: "first-again@example.com", Age: 1, Active: true, State: gendemo.UserStateActive,
			Settings: map[string]any{}, Tags: []string{},
		})
		if err != nil {
			t.Fatalf("inserting: %v", err)
		}
		if inserted.ID != 1 {
			t.Errorf("the identity restarted at %d, want 1", inserted.ID)
		}
	})

	t.Run("no tables is not an error", func(t *testing.T) {
		if err := ormtest.Truncate(t.Context(), p); err != nil {
			t.Errorf("Truncate with no tables: %v", err)
		}
	})

	t.Run("no executor is an error", func(t *testing.T) {
		if err := ormtest.Truncate(t.Context(), nil, gendemo.Users); err == nil {
			t.Error("Truncate with no executor succeeded")
		}
	})
}

// The schema assertion runs the production reconciliation: a clean project
// passes, and drift fails with the diagnostics the check command prints.
func TestRequireSchemaClean(t *testing.T) {
	testdb.AdminDSN(t)

	t.Run("a project whose schema matches passes", func(t *testing.T) {
		dsn := testdb.Create(t, schema(t))
		t.Setenv("ORM_GENDEMO_DSN", dsn)
		if err := ormtest.CheckSchema(t.Context(), gendemoConfig(t)); err != nil {
			t.Fatalf("a matching schema was reported as drifted:\n%v", err)
		}
	})

	t.Run("drift fails with the check command's own findings", func(t *testing.T) {
		// One column removed is drift the reconciler has a diagnostic for.
		dsn := testdb.Create(t, schema(t)+"\nALTER TABLE users DROP COLUMN nickname;")
		t.Setenv("ORM_GENDEMO_DSN", dsn)

		err := ormtest.CheckSchema(t.Context(), gendemoConfig(t))
		if err == nil {
			t.Fatal("a dropped column was not reported as drift")
		}
		var se *ormtest.SchemaError
		if !errors.As(err, &se) {
			t.Fatalf("error = %T (%v), want a *ormtest.SchemaError", err, err)
		}
		if se.Report == nil || se.Report.Len() == 0 {
			t.Fatal("the error carries no findings")
		}
		if !strings.Contains(se.Rendered, "nickname") {
			t.Errorf("the rendering does not name the column that moved:\n%s", se.Rendered)
		}
		// And it read the database rather than changing it.
		if !strings.Contains(se.Error(), "disagree") {
			t.Errorf("error = %v", se)
		}
	})
}
