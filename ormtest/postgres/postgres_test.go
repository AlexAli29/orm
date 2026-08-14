package postgres_test

import (
	"strings"
	"testing"

	ormtestpostgres "github.com/AlexAli29/orm/ormtest/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// M15.1: the container path.
//
// These need Docker and are the slowest tests in the repository, so they are
// behind -short. What they prove is that the wrapper starts a real server, that
// initialisation runs, that the pool is configurable, and that PostGIS is
// reachable through the image rather than through a runtime install.

func TestRun(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	pg := ormtestpostgres.Run(t, ormtestpostgres.PostgreSQL17(),
		ormtestpostgres.InitSQL(`CREATE TABLE widgets (id bigint PRIMARY KEY, name text NOT NULL)`),
		ormtestpostgres.PoolConfig(func(c *pgxpool.Config) { c.MaxConns = 3 }),
	)

	// The configuration is not hidden: a test can see and set it.
	if pg.DSN == "" {
		t.Error("no DSN")
	}
	if pg.Config == nil || pg.Config.MaxConns != 3 {
		t.Errorf("the pool configuration did not take: %+v", pg.Config)
	}

	var version string
	if err := pg.Pool().QueryRow(t.Context(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if !strings.HasPrefix(version, "17") {
		t.Errorf("server_version = %s, want 17.x", version)
	}

	// The init statement ran.
	pg.Exec(t, `INSERT INTO widgets (id, name) VALUES (1, 'a')`)
	var n int64
	if err := pg.Pool().QueryRow(t.Context(), "SELECT count(*) FROM widgets").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("widgets = %d", n)
	}
}

// Scenario C: PostGIS through the image, with a real spatial query.
func TestRun_postGIS(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	pg := ormtestpostgres.Run(t, ormtestpostgres.PostGIS(),
		ormtestpostgres.InitSQL(`CREATE EXTENSION IF NOT EXISTS postgis`))

	var dist float64
	if err := pg.Pool().QueryRow(t.Context(),
		`SELECT ST_Distance(
			ST_SetSRID(ST_MakePoint(0, 0), 4326)::geography,
			ST_SetSRID(ST_MakePoint(0, 1), 4326)::geography)`).Scan(&dist); err != nil {
		t.Fatalf("a spatial query: %v", err)
	}
	// One degree of latitude is about 110 km. The point is that PostGIS
	// answered, not the exact figure.
	if dist < 100_000 || dist > 120_000 {
		t.Errorf("ST_Distance = %v metres", dist)
	}
}

// A different supported version, to prove the version options are real rather
// than decorative.
func TestRun_postgres14(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	pg := ormtestpostgres.Run(t, ormtestpostgres.PostgreSQL14())
	var version string
	if err := pg.Pool().QueryRow(t.Context(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("querying: %v", err)
	}
	if !strings.HasPrefix(version, "14") {
		t.Errorf("server_version = %s, want 14.x", version)
	}
}

// Parallel tests each get their own container, which is the isolation model this
// package advertises. Nothing here shares one, because a shared container's
// isolation semantics would have to be proven before being offered.
func TestRun_parallel(t *testing.T) {
	if testing.Short() {
		t.Skip("needs Docker")
	}
	for _, name := range []string{"a", "b"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pg := ormtestpostgres.Run(t, ormtestpostgres.PostgreSQL17(),
				ormtestpostgres.InitSQL(`CREATE TABLE t (id int)`))
			pg.Exec(t, `INSERT INTO t VALUES (1)`)
			var n int64
			if err := pg.Pool().QueryRow(t.Context(), "SELECT count(*) FROM t").Scan(&n); err != nil {
				t.Fatalf("counting: %v", err)
			}
			if n != 1 {
				t.Errorf("the containers are not isolated: %d rows", n)
			}
		})
	}
}
