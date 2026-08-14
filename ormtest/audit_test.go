package ormtest_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/ormtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The M15 final audit: attacks on ormtest.
//
// The helper's whole value is that the rollback is unconditional, so these go
// after the conditions under which it might not be — a cancelled context, a
// quoted identifier, a caller who names half a reference.

// Release-critical: the cleanup must not depend on the test's context, which
// testing cancels before cleanups run.
//
// A rollback issued on a cancelled context does not reach the server: the
// transaction stays open until the connection is returned and reset, and in the
// meantime it holds its locks. This asserts the database state rather than the
// helper's internals, so it fails for the reason it is about.
func TestAudit_rollbackSurvivesACancelledContext(t *testing.T) {
	p := auditPool(t)
	const email = "cancelled-context@example.com"

	// The real sequence: the test body runs with a live context, testing cancels
	// it, and only then do the cleanups run. Cancelling before Begin would be a
	// different test — and not the one that matters, because a rollback issued
	// on a dead context is the failure that leaves a transaction holding locks
	// until the connection is reset.
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeT(ctx)
	db := gendemo.New(ormtest.Tx(f, p))
	if _, err := db.Users.Insert(ctx, gendemo.User{
		Email: email, Age: 1, Active: true, State: gendemo.UserStateActive,
		Settings: map[string]any{}, Tags: []string{},
	}); err != nil {
		t.Fatalf("inserting: %v", err)
	}
	cancel()
	f.runCleanups()

	if len(f.errs) > 0 {
		t.Errorf("the rollback reported an error on a cancelled context: %v", f.errs)
	}
	n, err := gendemo.New(p).Users.Query().Where(gendemo.Users.Email.Eq(email)).Count(t.Context())
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows survived: the rollback did not reach the server", n)
	}
}

// Identifiers that need quoting must be quoted. A truncate assembled by
// concatenation breaks on the first one, and the failure is a syntax error at
// runtime rather than anything a type could catch.
func TestAudit_truncateQuotesIdentifiers(t *testing.T) {
	testdb.AdminDSN(t)
	// A table whose name is a reserved word, and one with an uppercase letter:
	// both are legal and both need quoting.
	dsn := testdb.Create(t, `
		CREATE TABLE "order" (id bigint PRIMARY KEY);
		CREATE TABLE "MixedCase" (id bigint PRIMARY KEY);
		INSERT INTO "order" VALUES (1);
		INSERT INTO "MixedCase" VALUES (1);
	`)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(p.Close)

	if err := ormtest.Truncate(t.Context(), p,
		tableAt("public", "order"), tableAt("public", "MixedCase")); err != nil {
		t.Fatalf("Truncate over identifiers needing quotes: %v", err)
	}
	for _, name := range []string{"order", "MixedCase"} {
		var n int64
		if err := p.QueryRow(t.Context(), `SELECT count(*) FROM "`+name+`"`).Scan(&n); err != nil {
			t.Fatalf("counting %s: %v", name, err)
		}
		if n != 0 {
			t.Errorf("%s still has %d rows", name, n)
		}
	}
}

// tableAt builds the minimal descriptor Truncate needs, standing in for
// generated code.
type auditTable struct{ src *orm.Source }

func (t auditTable) Source() *orm.Source { return t.src }

func tableAt(schema, table string) ormtest.Table {
	return auditTable{src: orm.NewSource(schema, table)}
}

// The same table name in two schemas: truncating one must not touch the other.
func TestAudit_truncateIsSchemaQualified(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, `
		CREATE SCHEMA other;
		CREATE TABLE public.thing (id bigint PRIMARY KEY);
		CREATE TABLE other.thing  (id bigint PRIMARY KEY);
		INSERT INTO public.thing VALUES (1);
		INSERT INTO other.thing  VALUES (1);
	`)
	p, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(p.Close)

	if err := ormtest.Truncate(t.Context(), p, tableAt("public", "thing")); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	var pub, other int64
	if err := p.QueryRow(t.Context(), "SELECT count(*) FROM public.thing").Scan(&pub); err != nil {
		t.Fatalf("counting public: %v", err)
	}
	if err := p.QueryRow(t.Context(), "SELECT count(*) FROM other.thing").Scan(&other); err != nil {
		t.Fatalf("counting other: %v", err)
	}
	if pub != 0 {
		t.Errorf("public.thing still has %d rows", pub)
	}
	if other != 1 {
		t.Errorf("other.thing was truncated too: %d rows left", other)
	}
}

// Cascade is never implicit: emptying a referenced table without it is
// PostgreSQL's error, not a silent wider truncation.
func TestAudit_cascadeIsNeverImplicit(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, `
		CREATE TABLE parent (id bigint PRIMARY KEY);
		CREATE TABLE child  (id bigint PRIMARY KEY, parent_id bigint REFERENCES parent(id));
		INSERT INTO parent VALUES (1);
		INSERT INTO child  VALUES (1, 1);
	`)
	p, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(p.Close)

	err = ormtest.Truncate(t.Context(), p, tableAt("public", "parent"))
	if err == nil {
		t.Fatal("truncating a referenced table succeeded without Cascade")
	}
	if !strings.Contains(err.Error(), "foreign key") {
		t.Errorf("error = %v", err)
	}
	// The child is untouched: nothing was emptied on the way to the error.
	var n int64
	if err := p.QueryRow(t.Context(), "SELECT count(*) FROM child").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("child lost rows to a truncate that failed: %d left", n)
	}

	// And with Cascade it works, emptying both — which is what the option says.
	if err := ormtest.TruncateWith(t.Context(), p,
		[]ormtest.TruncateOption{ormtest.Cascade()}, tableAt("public", "parent")); err != nil {
		t.Fatalf("TruncateWith(Cascade): %v", err)
	}
	if err := p.QueryRow(t.Context(), "SELECT count(*) FROM child").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 0 {
		t.Errorf("Cascade left %d child rows", n)
	}
}

// RestartIdentity is not on by default: without it the sequence keeps going.
func TestAudit_restartIdentityIsNotImplicit(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, `
		CREATE TABLE seqd (id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY, v text NOT NULL);
		INSERT INTO seqd (v) VALUES ('a'), ('b'), ('c');
	`)
	p, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(p.Close)

	if err := ormtest.Truncate(t.Context(), p, tableAt("public", "seqd")); err != nil {
		t.Fatalf("Truncate: %v", err)
	}
	var id int64
	if err := p.QueryRow(t.Context(), "INSERT INTO seqd (v) VALUES ('d') RETURNING id").Scan(&id); err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if id == 1 {
		t.Error("the identity restarted without RestartIdentity being asked for")
	}

	if err := ormtest.TruncateWith(t.Context(), p,
		[]ormtest.TruncateOption{ormtest.RestartIdentity()}, tableAt("public", "seqd")); err != nil {
		t.Fatalf("TruncateWith(RestartIdentity): %v", err)
	}
	if err := p.QueryRow(t.Context(), "INSERT INTO seqd (v) VALUES ('e') RETURNING id").Scan(&id); err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if id != 1 {
		t.Errorf("RestartIdentity produced id %d, want 1", id)
	}
}

func auditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	cfg.AfterConnect = gendemo.RegisterTypes
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}
