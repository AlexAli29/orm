package postgis_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The spatial compile-fail suite.
//
// The claims this package makes about types are only worth something if the Go
// compiler enforces them, and the only way to assert that a program does not
// compile is to try to compile it. Each case below is a whole program built
// against this working tree.
//
// What is being asserted is the part of the design that PostgreSQL cannot
// enforce and a run-time check should not have to: that a geography is not a
// geometry, that a nullable spatial column does not read into a destination
// that cannot hold NULL, and that a spatial predicate over one table does not
// reach another table's query.

const spatialPreamble = `package main

import (
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

type Place struct{}
type Region struct{}

var placesSrc = orm.NewSource("public", "places")
var regionsSrc = orm.NewSource("public", "regions")

var (
	PlaceID   = orm.NewOrdCol[Place, int64](placesSrc, "id")
	PlaceName = orm.NewTextCol[Place](placesSrc, "name")
	// A NOT NULL geometry column, constrained to points in 4326.
	PlaceLoc = postgis.NewGeomCol[Place](placesSrc, "location", 4326, postgis.KindPoint, postgis.XY)
	// A nullable geometry column.
	PlaceArea = postgis.NewNullGeomCol[Place](placesSrc, "area", 4326, postgis.KindPolygon, postgis.XY)
	// A geography column, which is a different PostgreSQL type.
	PlaceSpot = postgis.NewGeogCol[Place](placesSrc, "spot", 4326, postgis.KindPoint, postgis.XY)

	RegionArea = postgis.NewGeomCol[Region](regionsSrc, "area", 4326, postgis.KindPolygon, postgis.XY)
)

func main() {}
`

func TestSpatialCompileFails(t *testing.T) {
	dir := spatialModule(t)

	tests := []struct {
		name string
		body string
		// want is a fragment of the compiler's complaint, asserted so that the
		// case fails for the reason it is about rather than for a typo.
		want string
	}{
		{
			name: "a geometry related to a geography",
			body: `func bad() { _ = PlaceLoc.Expr().Intersects(PlaceSpot.Expr()) }`,
			want: "cannot use",
		},
		{
			name: "a geography related to a geometry",
			body: `func bad() { _ = PlaceSpot.Expr().Intersects(PlaceLoc.Expr()) }`,
			want: "cannot use",
		},
		{
			name: "a geography value handed to a geometry predicate",
			body: `func bad() {
	g, _ := postgis.NewPoint(4326, 1, 2).AsGeography()
	_ = PlaceLoc.Expr().Within(postgis.GeogValue[Place](g))
}`,
			want: "cannot use",
		},
		{
			name: "a geometry value handed to a geography predicate",
			body: `func bad() {
	_ = PlaceSpot.Expr().Covers(postgis.Value[Place](postgis.NewPoint(4326, 1, 2)))
}`,
			want: "cannot use",
		},
		{
			name: "a predicate over one entity handed to another entity's query",
			body: `func bad() {
	var p orm.Predicate[Region] = PlaceLoc.Expr().Intersects(postgis.Value[Place](postgis.NewPoint(4326, 1, 2)))
	_ = p
}`,
			want: "cannot use",
		},
		{
			name: "columns of two entities related without composing",
			body: `func bad() { _ = PlaceLoc.Expr().Intersects(RegionArea.Expr()) }`,
			want: "cannot use",
		},
		{
			name: "a nullable geometry column read into a non-nullable destination",
			body: `func bad() {
	_ = orm.Project1(PlaceArea, func(g postgis.Geometry) postgis.Geometry { return g })
}`,
			want: "",
		},
		{
			name: "a nullable column's area read as a plain float",
			body: `func bad() {
	var v orm.Value[Place, float64] = PlaceArea.Area()
	_ = v
}`,
			want: "cannot use",
		},
		{
			name: "a NOT NULL column's area read as a pointer",
			body: `func bad() {
	var v orm.Value[Place, *float64] = PlaceLoc.Area()
	_ = v
}`,
			want: "cannot use",
		},
		{
			name: "a geometry value used where a geography value is expected",
			body: `func bad() {
	var g postgis.Geography = postgis.NewPoint(4326, 1, 2)
	_ = g
}`,
			want: "cannot use",
		},
		{
			name: "a geography value used where a geometry value is expected",
			body: `func bad() {
	var g postgis.Geometry = postgis.GeographyPoint(1, 2)
	_ = g
}`,
			want: "cannot use",
		},
		{
			name: "an SRID read back as a float",
			body: `func bad() {
	var v orm.Value[Place, float64] = PlaceLoc.Expr().SRID()
	_ = v
}`,
			want: "cannot use",
		},
		{
			name: "ST_CoordDim read back as an int32",
			body: `func bad() {
	var v orm.Value[Place, int32] = PlaceLoc.Expr().CoordDim()
	_ = v
}`,
			want: "cannot use",
		},
		{
			name: "ST_X read back as a plain float, when it can be NULL",
			body: `func bad() {
	var v orm.Value[Place, float64] = PlaceLoc.Expr().X()
	_ = v
}`,
			want: "cannot use",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := buildSpatialCase(t, dir, tc.body)
			if err == nil {
				t.Fatalf("this compiled, and it should not have:\n%s", tc.body)
			}
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("it failed for the wrong reason; wanted %q in:\n%s", tc.want, out)
			}
		})
	}
}

// The forms that are meant to work have to compile, or the suite above would be
// asserting that spatial queries cannot be written at all.
func TestSpatialCompileFails_theValidFormsCompile(t *testing.T) {
	dir := spatialModule(t)
	body := `func good() {
	pt := postgis.NewPoint(4326, 1, 2)
	poly := postgis.NewPolygon(4326, postgis.XY, []postgis.Coord{{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 0}})

	// Predicates over one entity.
	var p orm.Predicate[Place]
	p = PlaceLoc.Expr().Intersects(postgis.Value[Place](poly))
	p = PlaceLoc.Expr().Within(postgis.Value[Place](poly))
	p = PlaceLoc.Expr().DWithin(postgis.Value[Place](pt), 0.5)
	p = PlaceLoc.Expr().BBoxIntersects(postgis.Value[Place](poly))
	p = PlaceLoc.Expr().Relate(postgis.Value[Place](poly), "T********")
	p = PlaceLoc.Expr().IsValid()
	p = PlaceArea.Expr().Covers(postgis.Value[Place](pt))
	_ = p

	// The geography type, in metres.
	g, _ := pt.AsGeography()
	p = PlaceSpot.Expr().DWithin(postgis.GeogValue[Place](g), 500)
	_ = p

	// Crossing between the two, written out.
	p = PlaceLoc.Expr().AsGeography().DWithin(postgis.GeogValue[Place](g), 500)
	p = PlaceSpot.Expr().AsGeometry().Within(postgis.Value[Place](poly))
	_ = p

	// Measurements, with the nullability the column implies.
	var f orm.Value[Place, float64] = PlaceLoc.Area()
	var fn orm.Value[Place, *float64] = PlaceArea.Area()
	var i orm.Value[Place, int32] = PlaceLoc.Expr().SRID()
	var s orm.Value[Place, int16] = PlaceLoc.Expr().CoordDim()
	var x orm.Value[Place, *float64] = PlaceLoc.Expr().X()
	_, _, _, _, _ = f, fn, i, s, x

	// Composed, where the entity tag is dropped and the sources are checked.
	var c orm.Predicate[orm.Composed] = postgis.Of(PlaceLoc).Intersects(postgis.Of(RegionArea))
	_ = c

	// Selecting a spatial column, with the right Go type on each side.
	_ = orm.Project2(orm.Of(PlaceName), orm.Of(PlaceLoc),
		func(string, postgis.Geometry) int { return 0 })
	_ = orm.Project1(orm.Of(PlaceArea), func(*postgis.Geometry) int { return 0 })
	_ = orm.Project1(orm.Of(PlaceSpot), func(postgis.Geography) int { return 0 })
}`
	if out, err := buildSpatialCase(t, dir, body); err != nil {
		t.Fatalf("a valid spatial program does not compile, so every case above proves nothing:\n%s", out)
	}
}

// The preamble has to compile on its own, or every case would be failing for a
// reason that has nothing to do with what it asserts.
func TestSpatialCompileFails_thePreambleCompiles(t *testing.T) {
	dir := spatialModule(t)
	if out, err := buildSpatialCase(t, dir, "func good() {}"); err != nil {
		t.Fatalf("the preamble does not compile:\n%s", out)
	}
}

func spatialModule(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	dir := t.TempDir()

	// The module file is this one with its path swapped, so the case inherits
	// every requirement the runtime already has without a resolution step that
	// could reach the network.
	ownMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	mod := strings.Replace(string(ownMod), "module github.com/AlexAli29/orm", "module ormspatialcompilefail", 1) +
		"\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => " + root + "\n"
	write(t, filepath.Join(dir, "go.mod"), mod)

	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	write(t, filepath.Join(dir, "go.sum"), string(sum))
	write(t, filepath.Join(dir, "main.go"), spatialPreamble)
	return dir
}

// spatialBuildOnce serialises the builds, which share one module directory.
var spatialBuildOnce sync.Mutex

func buildSpatialCase(t *testing.T, dir, body string) (string, error) {
	t.Helper()
	spatialBuildOnce.Lock()
	defer spatialBuildOnce.Unlock()

	path := filepath.Join(dir, "case.go")
	header := "package main\n\nimport (\n\t\"github.com/AlexAli29/orm\"\n\t\"github.com/AlexAli29/orm/postgis\"\n)\n\n" +
		"var _ = orm.And[Place]\nvar _ = postgis.XY\n\n"
	write(t, path, header+body+"\n")
	defer func() { _ = os.Remove(path) }()

	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", os.DevNull, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
