package production_test

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"example.com/production/service"
	"example.com/production/transport/httpapi"
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/ormtest"
	ormpg "github.com/AlexAli29/orm/ormtest/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The example's test harness.
//
// Every test here runs against a real PostgreSQL in a container, with the
// schema created by the same migration files the application ships — not by a
// copy of the DDL pasted into a test, which is how a test suite comes to pass
// against a schema the application does not have.
//
// It is also the proof that the examples are external consumers. This module's
// path is outside the ORM's — example.com/production, not
// github.com/AlexAli29/orm/examples/production — so Go itself refuses an import
// of the ORM's internal packages, and a CI job checks that the refusal is for
// that reason rather than for some unrelated build failure.
//
// The path matters more than the separate go.mod. Go's internal rule is
// lexical: a module nested under github.com/AlexAli29/orm/ would have been
// allowed to import github.com/AlexAli29/orm/internal/..., separate module or
// not.

// env is one prepared database and the things built on it.
type env struct {
	pool *pgxpool.Pool
	ex   orm.Executor
	svc  *service.Service
	api  *httpapi.API
	log  *slog.Logger
}

// newEnv starts PostgreSQL, applies the migrations and wires the application.
//
// The container is shared per test binary by ormtest/postgres; each test gets a
// clean database by truncating rather than by starting another server, which is
// the difference between a suite that takes seconds and one that takes minutes.
func newEnv(t *testing.T) *env {
	t.Helper()
	pg := ormpg.Run(t, postgresImage()...)

	// The schema comes from the migration files, applied by the real engine.
	conn, err := pgx.Connect(t.Context(), pg.DSN)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	ormtest.Migrate(t, conn, "migrations")

	pool := pg.Pool()
	log := slog.New(slog.NewTextHandler(&testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	ex := orm.Executor(pool)
	svc := service.New(ex)

	e := &env{pool: pool, ex: ex, svc: svc, api: httpapi.New(svc, log), log: log}
	e.reset(t)
	return e
}

// reset empties the tables so each test starts from nothing.
func (e *env) reset(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(t.Context(),
		`TRUNCATE audit_entries, tasks, projects, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncating: %v", err)
	}
}

// testWriter sends log output to the test log, so a failing test shows what the
// application said and a passing one prints nothing.
type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// postgresImage lets CI choose which PostgreSQL the example is tested against.
//
// Without it the matrix in the workflow would be decorative: every entry would
// run the toolkit's default image and the job would report a version it never
// used. With it, ORM_TEST_POSTGRES_IMAGE=postgres:14 means the example really
// ran on 14.
func postgresImage() []ormpg.Option {
	if ref := os.Getenv("ORM_TEST_POSTGRES_IMAGE"); ref != "" {
		return []ormpg.Option{ormpg.Image(ref)}
	}
	return nil
}
