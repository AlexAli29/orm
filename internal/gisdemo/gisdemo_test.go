package gisdemo_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The spatial demo's shared fixtures.
//
// Everything in this package queries through the committed generated code, so a
// test that passes here is a test that passes for somebody who ran the
// generator against their own PostGIS schema. That is the difference between a
// runtime that works and a feature that ships.

// schemaSQL is the DDL the generator was run against, read from the same file
// so that the two cannot drift apart.
func schemaSQL(t testing.TB) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join("schema.sql"))
	if err != nil {
		t.Fatalf("reading schema.sql: %v", err)
	}
	return string(ddl)
}

// EnvRequirePostGIS makes the absence of PostGIS a failure instead of a skip.
//
// Skipping is right on a developer's machine, where a plain PostgreSQL is the
// normal thing to have. It is wrong in CI, and silently so: a job configured
// with an image that has no PostGIS skips every spatial test and reports
// success, so the whole support matrix can be green without one spatial
// assertion having run. That is exactly what was happening — the matrix used
// the plain postgres image and 49 tests skipped in every job.
//
// The CI jobs that exist to prove PostGIS set this. A wrong image tag then
// fails loudly rather than quietly proving nothing.
const EnvRequirePostGIS = "ORM_REQUIRE_POSTGIS"

// requirePostGIS skips when the server has no PostGIS, unless the environment
// says PostGIS is the point of this run.
func requirePostGIS(t testing.TB) {
	t.Helper()
	admin := testdb.AdminDSN(t)
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatalf("parsing %s: %v", testdb.EnvAdminDSN, err)
	}
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var ok bool
	if err := conn.QueryRow(context.Background(),
		`select exists (select 1 from pg_available_extensions where name = 'postgis')`).Scan(&ok); err != nil {
		t.Fatalf("asking whether PostGIS is available: %v", err)
	}
	if msg, fatal := absentPostGIS(ok, os.Getenv(EnvRequirePostGIS)); msg != "" {
		if fatal {
			t.Fatal(msg)
		}
		t.Skip(msg)
	}
}

// absentPostGIS decides what a missing PostGIS means, given whether the
// environment declared PostGIS to be the point of this run.
//
// It is a function of its two inputs so that it can be tested on a machine that
// does have PostGIS. That is not a detail: the branch that matters is the one
// taken when the extension is missing, and on any server able to run this suite
// that branch is unreachable. Left inline, the gate against a silently skipping
// CI job would itself have been the thing nothing checked.
func absentPostGIS(available bool, required string) (msg string, fatal bool) {
	if available {
		return "", false
	}
	if required != "" {
		return EnvRequirePostGIS + " is set but this PostgreSQL has no PostGIS extension available: " +
			"the job meant to prove PostGIS support is connected to a server that cannot run it", true
	}
	return "this PostgreSQL has no PostGIS extension available; skipping the spatial demo", false
}

// openDB builds a throwaway spatial database, seeds it, and returns a generated
// DB over a pool that knows the PostGIS types.
//
// The pool is the interesting part: registration is an AfterConnect hook, so
// every connection the pool opens — now and after a backend dies — learns the
// types. A program that registered once on one connection would work in a test
// and fail in production the first time the pool grew.
func openDB(t testing.TB) *gisdemo.DB {
	t.Helper()
	db, _ := openBoth(t)
	return db
}

// openBoth returns the generated DB and the pool underneath it.
//
// The pool is what the tests use to ask PostGIS a question directly — the
// hand-written half of every differential comparison. Going through the ORM for
// both halves would prove only that it agrees with itself.
func openBoth(t testing.TB) (*gisdemo.DB, *pgxpool.Pool) {
	t.Helper()
	pool := openPool(t)
	return gisdemo.New(pool), pool
}

func openPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	requirePostGIS(t)
	dsn := testdb.Create(t, schemaSQL(t)+seed)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the test DSN: %v", err)
	}
	cfg.AfterConnect = postgis.Register
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seed inserts the fixture rows with plain SQL, so that what the ORM reads was
// not written by the thing under test.
//
// The places are laid out around the origin at distances that separate the
// predicates: a degree of longitude at the equator is about 111 km, so 60 km
// reaches "north" and not "east", and the two are far enough apart that a
// rounding difference cannot flip a result.
const seed = `
INSERT INTO places (id, name, spot, location, projected, footprint, sketch) VALUES
  (1, 'origin', 'SRID=4326;POINT(0 0)',      'SRID=4326;POINT(0 0)',
      ST_Transform('SRID=4326;POINT(0 0)'::geometry, 3857),
      'SRID=4326;POLYGON((-1 -1,1 -1,1 1,-1 1,-1 -1))',
      'SRID=4326;LINESTRING(0 0,1 1)'),
  (2, 'east',   'SRID=4326;POINT(1 0)',      'SRID=4326;POINT(1 0)',
      ST_Transform('SRID=4326;POINT(1 0)'::geometry, 3857), NULL, NULL),
  (3, 'far',    'SRID=4326;POINT(10 10)',    'SRID=4326;POINT(10 10)', NULL, NULL, NULL),
  (4, 'north',  'SRID=4326;POINT(0 0.5)',    'SRID=4326;POINT(0 0.5)', NULL,
      'SRID=4326;POLYGON EMPTY', NULL);

INSERT INTO zones (id, name, area, centre) VALUES
  (1, 'inner', 'SRID=4326;MULTIPOLYGON(((-2 -2,2 -2,2 2,-2 2,-2 -2)))', 'SRID=4326;POINT(0 0)'),
  (2, 'outer', 'SRID=4326;MULTIPOLYGON(((5 5,15 5,15 15,5 15,5 5)))',   NULL);

INSERT INTO roads (id, name, path) VALUES
  (1, 'through',  'SRID=4326;LINESTRING(-5 0,5 0)'),
  (2, 'inside',   'SRID=4326;LINESTRING(-1 -1,1 1)'),
  (3, 'outside',  'SRID=4326;LINESTRING(20 20,30 30)'),
  (4, 'touching', 'SRID=4326;LINESTRING(-2 -2,-2 2)');

INSERT INTO readings (id, flat, raised, marked, zm, line3d) VALUES
  (1, 'SRID=4326;POINT(1 2)', 'SRID=4326;POINTZ(1 2 3)', 'SRID=4326;POINTM(1 2 4)',
      'SRID=4326;POINTZM(1 2 3 4)', 'SRID=4326;LINESTRINGZ(0 0 0,1 1 1)');
`

// names runs a composed query returning one string column, in the order given.
func names(t *testing.T, ex orm.Executor, src *orm.Source, name orm.Expression[string, *string], order orm.Order[orm.Composed], where orm.Predicate[orm.Composed]) []string {
	t.Helper()
	shape := orm.Project1(name, func(s string) string { return s })
	got, err := orm.Compose(ex, shape).
		From(src).
		Where(where).
		OrderBy(order).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	return got
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
