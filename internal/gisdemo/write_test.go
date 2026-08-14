package gisdemo_test

import (
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
)

// Writing spatial values through the generated entity.
//
// Every path here goes through the committed metadata: the value functions the
// generator emitted, the codec this package registers, and the write builders
// the rest of the ORM already had. Nothing spatial is special about any of
// them, which is the claim.

func newPlace(id int64, lon, lat float64) gisdemo.Place {
	spot, err := postgis.NewPoint(4326, lon, lat).AsGeography()
	if err != nil {
		panic(err)
	}
	return gisdemo.Place{
		ID:        id,
		Name:      "written",
		Spot:      spot,
		Location:  postgis.NewPoint(4326, lon, lat),
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestWrite_insert(t *testing.T) {
	db, pool := openBoth(t)
	p := newPlace(100, 3, 4)
	poly := postgis.NewPolygon(4326, postgis.XY, []postgis.Coord{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	})
	p.Footprint = &poly

	got, err := db.Places.Insert(t.Context(), p)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !got.Location.Equal(p.Location) {
		t.Errorf("the inserted location came back as %s", got.Location)
	}
	if !got.Spot.Equal(p.Spot) {
		t.Errorf("the inserted geography came back as %s", got.Spot)
	}
	if got.Footprint == nil || !got.Footprint.Equal(poly) {
		t.Errorf("the inserted footprint came back as %v", got.Footprint)
	}
	if got.Projected != nil {
		t.Errorf("a column left NULL came back as %s", got.Projected)
	}

	// And PostGIS agrees about what was stored.
	var ewkt string
	if err := pool.QueryRow(t.Context(),
		`SELECT ST_AsEWKT(location) FROM places WHERE id = 100`).Scan(&ewkt); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
	if ewkt != "SRID=4326;POINT(3 4)" {
		t.Errorf("PostGIS stored %q", ewkt)
	}
}

func TestWrite_updateValue(t *testing.T) {
	db, pool := openBoth(t)
	moved := postgis.NewPoint(4326, 7, 8)

	n, err := db.Places.Update().
		Set(gisdemo.Places.Location.Set(moved)).
		Where(gisdemo.Places.Name.Eq("origin")).
		Exec(t.Context())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated %d rows", n)
	}

	var ewkt string
	if err := pool.QueryRow(t.Context(),
		`SELECT ST_AsEWKT(location) FROM places WHERE name = 'origin'`).Scan(&ewkt); err != nil {
		t.Fatal(err)
	}
	if ewkt != "SRID=4326;POINT(7 8)" {
		t.Errorf("PostGIS stored %q", ewkt)
	}
}

// Assigning the result of a spatial expression, which is the write the
// transform scenario needs: reproject a column in place.
func TestWrite_updateExpression(t *testing.T) {
	db, pool := openBoth(t)

	n, err := db.Places.Update().
		Set(gisdemo.Places.Projected.SetExpr(
			orm.Nullable(gisdemo.Places.Location.Expr().Transform(3857).Value()))).
		Where(gisdemo.Places.Name.Eq("far")).
		Exec(t.Context())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 1 {
		t.Fatalf("updated %d rows", n)
	}

	var srid int32
	var x float64
	if err := pool.QueryRow(t.Context(),
		`SELECT ST_SRID(projected), ST_X(projected) FROM places WHERE name = 'far'`).Scan(&srid, &x); err != nil {
		t.Fatal(err)
	}
	if srid != 3857 {
		t.Errorf("the reprojected column is SRID %d", srid)
	}
	// Ten degrees of longitude is a bit over a million metres in web Mercator.
	if x < 1_000_000 || x > 1_200_000 {
		t.Errorf("the reprojected x is %v, which is not metres", x)
	}
}

// RETURNING with a spatial value, a spatial expression and a scalar
// measurement in one statement.
func TestWrite_returning(t *testing.T) {
	db := openDB(t)

	type row struct {
		name     string
		location postgis.Geometry
		centroid postgis.Geometry
		srid     int32
	}
	moved := postgis.NewPoint(4326, 2, 2)
	shape := orm.Project4(
		gisdemo.Places.Name,
		gisdemo.Places.Location,
		gisdemo.Places.Location.Expr().Centroid().Value(),
		gisdemo.Places.Location.Expr().SRID(),
		func(name string, loc, centroid postgis.Geometry, srid int32) row {
			return row{name, loc, centroid, srid}
		},
	)
	got, err := orm.UpdateReturning(db.Places.Update().
		Set(gisdemo.Places.Location.Set(moved)).
		Where(gisdemo.Places.Name.Eq("east")), shape).
		All(t.Context())
	if err != nil {
		t.Fatalf("UpdateReturning: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("returned %d rows", len(got))
	}
	if !got[0].location.Equal(moved) {
		t.Errorf("RETURNING gave location %s", got[0].location)
	}
	// The centroid of a point is the point.
	if !got[0].centroid.Equal(moved) {
		t.Errorf("RETURNING gave centroid %s", got[0].centroid)
	}
	if got[0].srid != 4326 {
		t.Errorf("RETURNING gave SRID %d", got[0].srid)
	}
}

// COPY through the generated metadata, with the confusable columns in place.
//
// places has a geometry(Point,4326), a geometry(Point,3857) and a
// geography(Point,4326) side by side. Three columns that all hold a point and
// mean different things is where a positional bug hides, so the values are
// chosen so that landing in the wrong column is visible.
func TestWrite_copyFrom(t *testing.T) {
	db := openDB(t)

	rows := make([]gisdemo.Place, 0, 200)
	for i := range 200 {
		p := newPlace(int64(1000+i), float64(i)/100, float64(i)/200)
		// The projected column holds numbers no 4326 column could plausibly
		// hold, so a value in the wrong place is not a rounding question.
		projected := postgis.NewPoint(3857, 100000+float64(i), 200000+float64(i))
		p.Projected = &projected
		poly := postgis.NewPolygon(4326, postgis.XY, []postgis.Coord{
			{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 0},
		})
		if i%2 == 0 {
			p.Footprint = &poly
		}
		rows = append(rows, p)
	}

	n, err := db.Places.CopyFrom(t.Context(), rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != int64(len(rows)) {
		t.Fatalf("copied %d rows, want %d", n, len(rows))
	}

	// Read them back through the generated entity and compare every spatial
	// field, which is what catches a value that landed one column over.
	back, err := db.Places.Query().
		Where(gisdemo.Places.ID.Gte(1000)).
		OrderBy(gisdemo.Places.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(back) != len(rows) {
		t.Fatalf("read back %d rows, want %d", len(back), len(rows))
	}
	for i := range rows {
		want, got := rows[i], back[i]
		if !got.Location.Equal(want.Location) {
			t.Fatalf("row %d: location %s, want %s", i, got.Location, want.Location)
		}
		if !got.Spot.Equal(want.Spot) {
			t.Fatalf("row %d: spot %s, want %s", i, got.Spot, want.Spot)
		}
		if got.Projected == nil || !got.Projected.Equal(*want.Projected) {
			t.Fatalf("row %d: projected %v, want %s", i, got.Projected, want.Projected)
		}
		if (got.Footprint == nil) != (want.Footprint == nil) {
			t.Fatalf("row %d: footprint nullability differs", i)
		}
		if got.Footprint != nil && !got.Footprint.Equal(*want.Footprint) {
			t.Fatalf("row %d: footprint %s", i, got.Footprint)
		}
	}
}

// COPY of the four dimensionalities, which is where a dropped Z or M would
// show.
func TestWrite_copyDimensions(t *testing.T) {
	db := openDB(t)

	want := gisdemo.Reading{
		ID:     100,
		Flat:   postgis.NewPoint(4326, 1, 2),
		Raised: postgis.NewPointZ(4326, 1, 2, 3),
		Marked: postgis.NewPointM(4326, 1, 2, 4),
		ZM:     postgis.NewPointZM(4326, 1, 2, 3, 4),
	}
	line := postgis.NewLineString(4326, postgis.XYZ,
		postgis.Coord{X: 0, Y: 0, Z: 5}, postgis.Coord{X: 1, Y: 1, Z: 6})
	want.Line3D = &line

	if _, err := db.Readings.CopyFrom(t.Context(), []gisdemo.Reading{want}); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	got, err := db.Readings.Query().Where(gisdemo.Readings.ID.Eq(100)).One(t.Context())
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}

	for _, tc := range []struct {
		name       string
		got, want  postgis.Geometry
		wantDim    postgis.Dim
		wantCoords postgis.Coord
	}{
		{"flat", got.Flat, want.Flat, postgis.XY, postgis.Coord{X: 1, Y: 2}},
		{"raised", got.Raised, want.Raised, postgis.XYZ, postgis.Coord{X: 1, Y: 2, Z: 3}},
		{"marked", got.Marked, want.Marked, postgis.XYM, postgis.Coord{X: 1, Y: 2, M: 4}},
		{"zm", got.ZM, want.ZM, postgis.XYZM, postgis.Coord{X: 1, Y: 2, Z: 3, M: 4}},
	} {
		if !tc.got.Equal(tc.want) {
			t.Errorf("%s came back as %s %v, want %s %v",
				tc.name, tc.got, tc.got.Coords(), tc.want, tc.want.Coords())
		}
		if tc.got.Dim() != tc.wantDim {
			t.Errorf("%s came back %v, want %v", tc.name, tc.got.Dim(), tc.wantDim)
		}
		if cs := tc.got.Coords(); len(cs) != 1 || cs[0] != tc.wantCoords {
			t.Errorf("%s coordinates are %v, want %v", tc.name, cs, tc.wantCoords)
		}
	}
	if got.Line3D == nil || !got.Line3D.Equal(line) {
		t.Errorf("the 3D line came back as %v", got.Line3D)
	}
}

// A transaction that locks a row, moves it, reads a measurement back, and rolls
// the whole thing away.
func TestWrite_transaction(t *testing.T) {
	db, pool := openBoth(t)
	moved := postgis.NewPoint(4326, 5, 5)

	err := db.Tx(t.Context(), func(tx *gisdemo.DB) error {
		locked, err := tx.Places.Query().
			Where(gisdemo.Places.Name.Eq("origin")).
			ForUpdate().
			One(t.Context())
		if err != nil {
			return err
		}
		if locked.Name != "origin" {
			t.Errorf("locked %s", locked.Name)
		}

		type row struct {
			name  string
			metre float64
		}
		shape := orm.Project2(
			gisdemo.Places.Name,
			gisdemo.Places.Location.Expr().
				Distance(postgis.Value[gisdemo.Place](postgis.NewPoint(4326, 0, 0))),
			func(name string, d float64) row { return row{name, d} },
		)
		got, err := orm.UpdateReturning(tx.Places.Update().
			Set(gisdemo.Places.Location.Set(moved)).
			Where(gisdemo.Places.Name.Eq("origin")), shape).
			All(t.Context())
		if err != nil {
			return err
		}
		if len(got) != 1 {
			t.Fatalf("returned %d rows", len(got))
		}
		// The measurement is over the new value, because RETURNING sees the row
		// as the update left it.
		if got[0].metre < 7.07 || got[0].metre > 7.08 {
			t.Errorf("the distance from the origin is %v, want about 7.071", got[0].metre)
		}
		// Roll the transaction back by returning an error.
		return errRollback
	})
	if err != errRollback {
		t.Fatalf("Tx: %v", err)
	}

	// Nothing survived.
	var ewkt string
	if err := pool.QueryRow(t.Context(),
		`SELECT ST_AsEWKT(location) FROM places WHERE name = 'origin'`).Scan(&ewkt); err != nil {
		t.Fatal(err)
	}
	if ewkt != "SRID=4326;POINT(0 0)" {
		t.Errorf("the rolled-back update survived: %q", ewkt)
	}
}

type rollbackError struct{}

func (rollbackError) Error() string { return "rolling back" }

var errRollback = rollbackError{}
