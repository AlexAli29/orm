package main

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/examples/managed/internal/domain"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The example is code people copy, so it is tested like code people copy.
//
// The schema comes from the committed migrations rather than from a DDL file,
// applied through the same engine the command-line tool uses. That is worth
// doing here rather than shelling out to orm: it shows that a program can own
// its own migrations, which is what a service that migrates on deploy needs.

func newDB(t *testing.T) *domain.DB {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "")

	// One connection owns the migration run, because the advisory lock it takes
	// lives on a connection.
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	set, err := migrate.NewStore("migrations").Load()
	if err != nil {
		t.Fatalf("loading the migrations: %v", err)
	}
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("migrating: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.AfterConnect = domain.RegisterTypes
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return domain.New(pool)
}

func TestSeedAndFeed(t *testing.T) {
	db := newDB(t)

	if err := seed(t.Context(), db); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Seeding twice writes once, which is the property that lets the command be
	// run against a database that already has the rows.
	if err := seed(t.Context(), db); err != nil {
		t.Fatalf("seed again: %v", err)
	}
	n, err := db.Authors.Query().Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Fatalf("seeding twice produced %d authors", n)
	}

	var out strings.Builder
	if err := feed(t.Context(), db, &out); err != nil {
		t.Fatalf("feed: %v", err)
	}
	want := "Ada <ada@example.com>\n" +
		"    2024-04-01  On engines\n" +
		"    2024-03-01  On notation\n"
	if out.String() != want {
		t.Errorf("feed =\n%s\nwant\n%s", out.String(), want)
	}
	// The draft is not in the feed, which is the relation predicate doing its
	// job rather than the loop filtering afterwards.
	if strings.Contains(out.String(), "Unfinished") {
		t.Error("a draft reached the feed")
	}
}

// The migrations really do build the schema the declarations describe: if they
// did not, the generated metadata would not match what the queries above run
// against, and this would fail rather than the example rotting quietly.
func TestMigrationsBuildTheSchema(t *testing.T) {
	db := newDB(t)
	if _, err := db.Articles.Query().Limit(1).All(t.Context()); err != nil {
		t.Errorf("reading articles from the migrated schema: %v", err)
	}
}
