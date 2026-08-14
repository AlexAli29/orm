package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 compatibility: materialized views over PostGIS types.
//
// The managed PostGIS evidence covers an ordinary view. A materialized view is
// the harder case for spatial columns, because it stores them: the geometry and
// the geography are written into a relation of their own, indexed, refreshed and
// read back, rather than recomputed on every select.
//
// The support matrix is not PG14 through PG18. The project claims three
// combinations and CI runs exactly those, so this runs exactly those — and fails
// rather than skips when one is missing, because a PostGIS claim proven by
// whichever server happened to be up is the false green this milestone has been
// chasing since the M16 audit.

// postgisCombination is one declared PostgreSQL/PostGIS pairing.
type postgisCombination struct {
	postgres string
	postgis  string
	env      string
}

// postgisMatrix is the frozen support matrix, mirroring the CI job's rows.
func postgisMatrix() []postgisCombination {
	return []postgisCombination{
		{"17", "3.5", "ORM_TEST_DSN_POSTGIS_17_35"},
		{"16", "3.4", "ORM_TEST_DSN_POSTGIS_16_34"},
		{"14", "3.4", "ORM_TEST_DSN_POSTGIS_14_34"},
	}
}

// compatGisProject declares a table with spatial columns and a materialized view
// that stores them, indexed the way a spatial rollup would be.
const compatGisProject = `package domain

import "github.com/AlexAli29/orm/postgis"

//orm:table public.places
//orm:extension postgis
type Place struct {
	ID    int64             ` + "`orm:\"pk,identity\"`" + `
	Name  string
	Geom  postgis.Geometry  ` + "`orm:\"pgtype:geometry(Point,4326)\"`" + `
	Area  postgis.Geography ` + "`orm:\"pgtype:geography(Point,4326)\"`" + `
}

//orm:materialized-view public.place_rollups
//orm:definition ` + "`SELECT id, name, geom, area FROM places`" + `
//orm:depends-on public.places
//orm:index place_rollup_id_key (ID) unique
//orm:index place_rollup_geom_gist (Geom) using gist
type PlaceRollup struct {
	ID   int64
	Name string
	Geom postgis.Geometry  ` + "`orm:\"pgtype:geometry(Point,4326)\"`" + `
	Area postgis.Geography ` + "`orm:\"pgtype:geography(Point,4326)\"`" + `
}
`

// compatGisProbe writes real spatial values, refreshes and reads them back.
const compatGisProbe = `package main

import (
	"context"
	"fmt"
	"os"

	"example.com/managed/domain"
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("DSN"))
	if err != nil {
		panic(err)
	}
	cfg.AfterConnect = postgis.Register
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	db := domain.New(pool)

	if _, err := pool.Exec(ctx, ` + "`" + `INSERT INTO places (name, geom, area)
		VALUES ('one', ST_SetSRID(ST_MakePoint(1,2),4326),
		        ST_SetSRID(ST_MakePoint(3,4),4326)::geography)` + "`" + `); err != nil {
		panic(err)
	}
	if err := db.PlaceRollups.Refresh(ctx); err != nil {
		panic("refresh: " + err.Error())
	}
	if err := db.PlaceRollups.Refresh(ctx, orm.Concurrently()); err != nil {
		panic("concurrent refresh: " + err.Error())
	}

	rows, err := db.PlaceRollups.Query().All(ctx)
	if err != nil {
		panic("query: " + err.Error())
	}
	if len(rows) != 1 {
		panic(fmt.Sprintf("want one row, got %d", len(rows)))
	}
	r := rows[0]
	c := r.Geom.Coords()
	fmt.Printf("geom_srid=%d geog_srid=%d coords=%d,%v,%v\n",
		r.Geom.SRID(), r.Area.SRID(), len(c), c[0].X, c[0].Y)
}
`

// requirePostGISCombination returns the DSN for one declared combination, or
// fails when mandatory mode is on and it is absent.
func requirePostGISCombination(t *testing.T, c postgisCombination) string {
	t.Helper()
	dsn := os.Getenv(c.env)
	if dsn == "" {
		if os.Getenv("ORM_REQUIRE_POSTGIS") != "" {
			t.Fatalf("%s is unset and ORM_REQUIRE_POSTGIS is on: the job that exists to prove "+
				"PostgreSQL %s with PostGIS %s cannot run it", c.env, c.postgres, c.postgis)
		}
		t.Skipf("set %s to run the PostgreSQL %s / PostGIS %s combination",
			c.env, c.postgres, c.postgis)
	}
	// The server is the one this row claims. A moved image tag would otherwise
	// pass as something it is not, which is the check the CI job makes with its
	// expectation variables.
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("%s: connecting: %v", c.env, err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var major, gis string
	if err := conn.QueryRow(t.Context(),
		`SELECT (current_setting('server_version_num')::int / 10000)::text`).Scan(&major); err != nil {
		t.Fatalf("%s: reading the server version: %v", c.env, err)
	}
	if err := conn.QueryRow(t.Context(),
		`SELECT default_version FROM pg_available_extensions WHERE name = 'postgis'`).Scan(&gis); err != nil {
		t.Fatalf("%s: this server has no PostGIS available: %v", c.env, err)
	}
	if major != c.postgres {
		t.Fatalf("%s points at PostgreSQL %s, and this row is the %s row", c.env, major, c.postgres)
	}
	if !strings.HasPrefix(gis, c.postgis+".") && gis != c.postgis {
		t.Fatalf("%s offers PostGIS %s, and this row is the %s row", c.env, gis, c.postgis)
	}
	t.Logf("PostgreSQL %s, PostGIS %s", major, gis)
	return dsn
}

// A materialized view storing geometry and geography, through the whole managed
// workflow, on every declared combination.
func TestCompatGIS_materializedViewOverSpatialTypes(t *testing.T) {
	for _, c := range postgisMatrix() {
		t.Run(fmt.Sprintf("pg%s-postgis%s", c.postgres, c.postgis), func(t *testing.T) {
			dsn := requirePostGISCombination(t, c)
			t.Setenv("ORM_TEST_ADMIN_DSN", dsn)
			p := newProject(t, compatGisProject)

			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("check")
			p.MustRun("generate")
			p.MustRun("check", "--generated")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Fatalf("the spatial project did not converge:\n%s", out)
			}

			// The stored relation really is a materialized view with the
			// spatial types, and the GiST index really is GiST.
			kinds := p.Query(`SELECT c.relkind::text FROM pg_class c
			                   WHERE c.relname = 'place_rollups'`)
			if len(kinds) != 1 || kinds[0] != "m" {
				t.Fatalf("place_rollups is %v, want a materialized view", kinds)
			}
			types := p.Query(`SELECT a.attname || '=' || format_type(a.atttypid, a.atttypmod)
			                    FROM pg_attribute a
			                   WHERE a.attrelid = 'public.place_rollups'::regclass AND a.attnum > 0
			                   ORDER BY a.attnum`)
			joined := strings.Join(types, " ")
			for _, want := range []string{"geom=geometry(Point,4326)", "area=geography(Point,4326)"} {
				if !strings.Contains(joined, want) {
					t.Errorf("the stored relation does not carry %q: %v", want, types)
				}
			}
			am := p.Query(`SELECT am.amname FROM pg_class i
			                 JOIN pg_index x ON x.indexrelid = i.oid
			                 JOIN pg_am am ON am.oid = i.relam
			                WHERE i.relname = 'place_rollup_geom_gist'`)
			if len(am) != 1 || am[0] != "gist" {
				t.Errorf("the spatial index uses %v, want gist", am)
			}

			// The generated repository is a materialized view's, and the values
			// survive a write, a refresh, a concurrent refresh and a read.
			gen := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
			if !strings.Contains(strings.Join(strings.Fields(gen), " "),
				"PlaceRollups *orm.MaterializedViewRepo[PlaceRollup]") {
				t.Errorf("the spatial rollup did not get a materialized-view repository:\n%s", gen)
			}
			writeFile(t, filepath.Join(p.Dir, "main.go"), compatGisProbe)
			got := runProgram(t, p)
			for _, want := range []string{"geom_srid=4326", "geog_srid=4326", "coords=1,1,2"} {
				if !strings.Contains(got, want) {
					t.Errorf("the spatial roundtrip did not produce %q:\n%s", want, got)
				}
			}

			// An index-only change still leaves the relation alone, with spatial
			// columns stored in it.
			before := relationOID(t, p, "public.place_rollups")
			p.Entities(strings.Replace(compatGisProject,
				"//orm:index place_rollup_id_key (ID) unique\n",
				"//orm:index place_rollup_id_key (ID) unique\n//orm:index place_rollup_name_idx (Name)\n", 1))
			p.MustRun("makemigrations", "--name", "add-index")
			p.MustRun("migrate")
			p.MustRun("check")
			if after := relationOID(t, p, "public.place_rollups"); after != before {
				t.Errorf("an index change recreated the spatial materialized view: OID %d -> %d",
					before, after)
			}
		})
	}
}

// The declared matrix is what runs, and the count is asserted so a row silently
// dropped from the table is a failure rather than a smaller green run.
func TestCompatGIS_theDeclaredMatrixIsComplete(t *testing.T) {
	got := postgisMatrix()
	if len(got) != 3 {
		t.Fatalf("the matrix has %d combinations; the project claims three", len(got))
	}
	want := map[string]string{"17": "3.5", "16": "3.4", "14": "3.4"}
	for _, c := range got {
		if want[c.postgres] != c.postgis {
			t.Errorf("the matrix pairs PostgreSQL %s with PostGIS %s; the claim is %s",
				c.postgres, c.postgis, want[c.postgres])
		}
		delete(want, c.postgres)
	}
	if len(want) != 0 {
		t.Errorf("the matrix is missing %v", want)
	}
}
