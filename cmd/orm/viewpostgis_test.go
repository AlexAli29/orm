package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 G1: managed PostGIS views, through the real workflow.
//
// The M16 audit found a PostGIS job that was green because every spatial test
// skipped. So this does not skip when it is meant to run: ORM_REQUIRE_POSTGIS
// turns an absent extension into a failure, exactly as the spatial suite does,
// and the job whose purpose is proving PostGIS sets it.

const postgisProject = `package domain

import "github.com/AlexAli29/orm/postgis"

//orm:table public.places
//orm:extension postgis
type Place struct {
	ID   int64            ` + "`orm:\"pk,identity\"`" + `
	Name string
	Geom postgis.Geometry ` + "`orm:\"pgtype:geometry(Point,4326)\"`" + `
	Area postgis.Geography ` + "`orm:\"pgtype:geography(Point,4326)\"`" + `
}

//orm:view public.place_shapes
//orm:definition ` + "`SELECT id, geom, ST_Centroid(geom) AS center, area FROM places`" + `
//orm:depends-on public.places
type PlaceShape struct {
	ID     int64
	Geom   postgis.Geometry  ` + "`orm:\"pgtype:geometry(Point,4326)\"`" + `
	// ST_Centroid returns bare geometry: PostgreSQL does not carry the shape or
	// the SRID constraint through a function, so the column is unconstrained and
	// the declaration says so. Claiming geometry(Point,4326) here would be a
	// guarantee the catalog cannot support — and orm check refuses it.
	Center postgis.Geometry  ` + "`orm:\"pgtype:geometry\"`" + `
	Area   postgis.Geography ` + "`orm:\"pgtype:geography(Point,4326)\"`" + `
}
`

func requireManagedPostGIS(t *testing.T, p *project) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var ok bool
	if err := conn.QueryRow(t.Context(),
		`SELECT exists (SELECT 1 FROM pg_available_extensions WHERE name = 'postgis')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if ok {
		return
	}
	if os.Getenv("ORM_REQUIRE_POSTGIS") != "" {
		t.Fatal("ORM_REQUIRE_POSTGIS is set and this PostgreSQL has no PostGIS: " +
			"the job meant to prove managed PostGIS views cannot run them")
	}
	t.Skip("this PostgreSQL has no PostGIS available")
}

// The whole workflow, with geometry and geography in one managed view.
//
// This test spent the whole of G1 and G2 skipping. A t.Skip("disabled") sat on
// its first line, above the call to requireManagedPostGIS — so the guard written
// to turn an absent extension into a failure never ran, and ORM_REQUIRE_POSTGIS
// could not make it fail. The CI job whose entire purpose is proving managed
// PostGIS views was green because the proof returned before doing anything.
//
// That is the M16 finding again, one level in: the audit found a job green
// because its spatial tests skipped, this file was written to stop that, and the
// stop was itself disabled in the same commit. A guard below an unconditional
// skip is decoration.
//
// Re-enabled here, and it passes: plan, migrate, check, generate, compile,
// query, converge. Nothing about the product was wrong — only the evidence.
func TestPostGIS_managedViewRoundtrip(t *testing.T) {
	p := newProject(t, postgisProject)
	requireManagedPostGIS(t, p)

	out := p.MustRun("makemigrations", "--name", "initial")
	if !strings.Contains(out, "create view public.place_shapes") {
		t.Fatalf("no view operation was planned:\n%s", out)
	}
	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	// A read-only repository, and the spatial types survived the view.
	gen := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
	if !strings.Contains(strings.Join(strings.Fields(gen), " "), "PlaceShapes *orm.ViewRepo[PlaceShape]") {
		t.Errorf("the managed PostGIS view did not get a read-only repository:\n%s", gen)
	}
	// That the spatial types survived is proved at runtime below, by scanning
	// real geometry and geography out of the view — not by looking for a
	// substring in generated text, which would pass on a comment.

	// The extension's own catalog views are not the project's schema.
	for _, forbidden := range []string{"GeometryColumns", "GeographyColumns", "geometry_columns"} {
		if strings.Contains(gen, forbidden) {
			t.Errorf("%s became a generated repository:\n%s", forbidden, gen)
		}
	}

	writeFile(t, filepath.Join(p.Dir, "main.go"), `package main

import (
	"context"
	"fmt"
	"os"

	"example.com/managed/domain"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/AlexAli29/orm/postgis"
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

	if _, err := pool.Exec(ctx, `+"`"+`INSERT INTO places (name, geom, area)
		VALUES ('one', ST_SetSRID(ST_MakePoint(1,2),4326),
		        ST_SetSRID(ST_MakePoint(3,4),4326)::geography)`+"`"+`); err != nil {
		panic(err)
	}

	rows, err := domain.New(pool).PlaceShapes.Query().All(ctx)
	if err != nil {
		panic(err)
	}
	if len(rows) != 1 {
		panic("want one row")
	}
	r := rows[0]
	fmt.Printf("geom_srid=%d\n", r.Geom.SRID())
	fmt.Printf("center_srid=%d\n", r.Center.SRID())
	fmt.Printf("geog_srid=%d\n", r.Area.SRID())
	c := r.Center.Coords()
	fmt.Printf("center=%v,%v,%v\n", len(c) == 1, c[0].X, c[0].Y)
}
`)
	got := runProgram(t, p)
	for _, want := range []string{
		"geom_srid=4326",
		"center_srid=4326",
		"geog_srid=4326",
		"center=true,1,2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the spatial roundtrip did not produce %q:\n%s", want, got)
		}
	}

	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the PostGIS workflow did not converge:\n%s", out)
	}
}

// No test in this package may skip before its guard runs.
//
// The managed PostGIS proof skipped for two milestones because an unconditional
// t.Skip sat above the call that was supposed to decide whether skipping was
// allowed. Nothing detected it: a skipped test is green, and a guard that never
// runs reports nothing at all.
//
// So the shape is refused rather than the instance. Every test in this package
// is read, and a t.Skip appearing as a test's first statement fails this one —
// an unconditional skip is always either a guard in the wrong order or a test
// somebody meant to come back to.
func TestNoTestInThisPackageSkipsUnconditionally(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	funcStart := regexp.MustCompile(`^func (Test\w+)\(t \*testing\.T\) \{$`)
	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(src), "\n")
		for i, line := range lines {
			m := funcStart.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			checked++
			// The first statement, skipping blank lines and comments.
			for j := i + 1; j < len(lines) && j < i+6; j++ {
				stmt := strings.TrimSpace(lines[j])
				if stmt == "" || strings.HasPrefix(stmt, "//") {
					continue
				}
				if strings.HasPrefix(stmt, "t.Skip") {
					t.Errorf("%s:%d: %s skips unconditionally as its first statement (%s). "+
						"A guard below it never runs, so no environment variable can turn "+
						"the skip into a failure and the test is green everywhere",
						e.Name(), j+1, m[1], stmt)
				}
				break
			}
		}
	}
	if checked == 0 {
		t.Fatal("no test functions were read, so this proves nothing")
	}
	t.Logf("%d test functions checked", checked)
}
