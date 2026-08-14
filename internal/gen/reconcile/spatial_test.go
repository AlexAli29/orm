package reconcile_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// Reconciling spatial columns.
//
// The two questions the reconciler can answer about a spatial column are which
// storage family it is and whether the tag agrees with it. Both are worth
// asking: a geography read into a Geometry gives distances in the wrong units,
// and a tag that says one shape over a column that holds another produces a
// managed schema that would recreate the wrong column.

const spatialSchema = `
CREATE EXTENSION IF NOT EXISTS postgis;
CREATE TABLE places (
    id        bigint PRIMARY KEY,
    location  geometry(Point, 4326) NOT NULL,
    projected geometry(Point, 3857),
    area      geometry(MultiPolygon, 4326),
    track     geometry(LineStringZ, 4326),
    spot      geography(Point, 4326) NOT NULL,
    sketch    geometry
);`

func TestSpatialReconcile_correctMappings(t *testing.T) {
	src := `package demo

import "github.com/AlexAli29/orm/postgis"

//orm:table places
type Place struct {
	ID        int64
	Location  postgis.Geometry  ` + "`orm:\"pgtype:geometry(Point,4326)\"`" + `
	Projected *postgis.Geometry ` + "`orm:\"pgtype:geometry(Point,3857)\"`" + `
	Area      *postgis.Geometry ` + "`orm:\"pgtype:geometry(MultiPolygon,4326)\"`" + `
	Track     *postgis.Geometry ` + "`orm:\"pgtype:geometry(LineStringZ,4326)\"`" + `
	Spot      postgis.Geography ` + "`orm:\"pgtype:geography(Point,4326)\"`" + `
	Sketch    *postgis.Geometry
}`
	report := reconcileSpatial(t, src)
	if report.Failed(diag.SeverityError) {
		t.Errorf("a correct spatial mapping produced errors:\n%s", findingsText(report))
	}
}

// Every way the two halves can disagree, each with its own diagnostic.
func TestSpatialReconcile_mismatches(t *testing.T) {
	tests := []struct {
		name  string
		field string
		// want is a fragment of the diagnostic, asserted so the case fails for
		// the reason it is about.
		want string
	}{
		{
			"a geography column read as a geometry",
			"Spot postgis.Geometry",
			"different surfaces",
		},
		{
			"a geometry column read as a geography",
			"Location postgis.Geography",
			"different surfaces",
		},
		{
			"a spatial column read as a string",
			"Location string",
			"postgis.Geometry",
		},
		{
			"the tag says the wrong shape",
			"Location postgis.Geometry `orm:\"pgtype:geometry(Polygon,4326)\"`",
			"the tag says Polygon and the column holds Point",
		},
		{
			"the tag says the wrong SRID",
			"Location postgis.Geometry `orm:\"pgtype:geometry(Point,3857)\"`",
			"the tag says SRID 3857 and the column is SRID 4326",
		},
		{
			"the tag says the wrong dimensionality",
			"Location postgis.Geometry `orm:\"pgtype:geometry(PointZ,4326)\"`",
			"the tag says XYZ and the column is XY",
		},
		{
			"the tag says the wrong family",
			"Location postgis.Geometry `orm:\"pgtype:geography(Point,4326)\"`",
			"the tag says geography and the column is geometry",
		},
		{
			"the tag constrains nothing and the column does",
			"Location postgis.Geometry `orm:\"pgtype:geometry\"`",
			"accepts anything",
		},
		{
			"a nullable column read as a value",
			"Area postgis.Geometry `orm:\"pgtype:geometry(MultiPolygon,4326)\"`",
			"cannot be represented",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := `package demo

import "github.com/AlexAli29/orm/postgis"

//orm:table places
type Place struct {
	ID int64
	` + tc.field + `
}

// The import is kept live for the cases whose field does not use it, so that a
// type error cannot stand in for the diagnostic being tested.
var _ = postgis.XY
`
			report := reconcileSpatial(t, src)
			if !report.Failed(diag.SeverityError) {
				t.Fatalf("this mapping produced no error:\n%s", src)
			}
			text := findingsText(report)
			if !strings.Contains(text, tc.want) {
				t.Errorf("the diagnostic does not say %q:\n%s", tc.want, text)
			}
		})
	}
}

// The diagnostics have to be deterministic, or a CI failure would name a
// different problem on each run.
func TestSpatialReconcile_diagnosticsAreDeterministic(t *testing.T) {
	src := `package demo

import "github.com/AlexAli29/orm/postgis"

//orm:table places
type Place struct {
	ID        int64
	Location  postgis.Geometry ` + "`orm:\"pgtype:geometry(Polygon,3857)\"`" + `
	Spot      postgis.Geometry ` + "`orm:\"pgtype:geography(Point,4326)\"`" + `
}`
	first := findingsText(reconcileSpatial(t, src))
	for range 4 {
		if again := findingsText(reconcileSpatial(t, src)); again != first {
			t.Fatalf("the diagnostics differ between runs:\n%s\n---\n%s", first, again)
		}
	}
	// And the SRID, shape and dimensionality mismatches are each reported
	// separately rather than collapsed into one.
	for _, want := range []string{"Polygon", "3857"} {
		if !strings.Contains(first, want) {
			t.Errorf("the diagnostics do not mention %s:\n%s", want, first)
		}
	}
}

// reconcileSpatial builds a throwaway PostGIS database, writes src into a
// temporary module, and reconciles the two.
//
// The entity source is a string rather than a fixture directory because these
// cases differ by one field each, and twenty near-identical directories would
// hide the one line that matters in each.
func reconcileSpatial(t *testing.T, src string) *diag.Report {
	t.Helper()
	requirePostGISFor(t)
	dsn := testdb.Create(t, spatialSchema)

	dir := t.TempDir()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	ownMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	mod := strings.Replace(string(ownMod), "module github.com/AlexAli29/orm", "module spatialrecon", 1) +
		"\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => " + root + "\n"
	writeFile(t, filepath.Join(dir, "go.mod"), mod)
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.sum"), string(sum))
	writeFile(t, filepath.Join(dir, "entities.go"), src)
	writeFile(t, filepath.Join(dir, "orm.yaml"),
		"version: 1\nschema:\n  dsn: ${ORM_SPATIAL_DSN}\n  search_path:\n    - public\npackages:\n  - path: .\n    output: same\n")

	t.Setenv("ORM_SPATIAL_DSN", dsn)
	cfg, err := config.Load(filepath.Join(dir, "orm.yaml"))
	if err != nil {
		t.Fatalf("loading the configuration: %v", err)
	}
	result, err := gen.Check(t.Context(), cfg)
	if err != nil {
		t.Fatalf("gen.Check: %v", err)
	}
	return result.Report
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// requirePostGISFor skips when the server has no PostGIS.
func requirePostGISFor(t *testing.T) {
	t.Helper()
	admin := testdb.AdminDSN(t)
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	var ok bool
	if err := conn.QueryRow(t.Context(),
		`select exists (select 1 from pg_available_extensions where name = 'postgis')`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Skip("this PostgreSQL has no PostGIS extension available")
	}
}

func findingsText(r *diag.Report) string {
	var b strings.Builder
	for _, f := range r.Findings() {
		b.WriteString(string(f.Code))
		b.WriteString(" ")
		b.WriteString(f.Message)
		b.WriteString("\n  ")
		b.WriteString(f.Reason)
		b.WriteString("\n  fix: ")
		b.WriteString(f.Fix)
		b.WriteString("\n")
	}
	return b.String()
}
