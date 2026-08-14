package postgis_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

// Spatial queries through the ORM's own compiler.
//
// The claim being tested is that there is one compiler. A spatial predicate is
// built by this package, rendered by the ORM's writer, checked by the ORM's
// scope and nullability rules, and executed by the ORM's executor — and the
// rows come back through M10's typed projections with no reflection and no
// []any anywhere.
//
// The descriptors below are what generated code would emit for a spatial table.
// They are written by hand here because the generator's spatial support is a
// later stage; what they exercise is the runtime contract, which is the same
// either way.

type place struct{}

var (
	placesSrc  = orm.NewSource("public", "places")
	placeID    = orm.NewCol[place, int64](placesSrc, "id")
	placeName  = orm.NewCol[place, string](placesSrc, "name")
	placeLoc   = postgis.NewGeomCol[place](placesSrc, "location", 4326, postgis.KindPoint, postgis.XY)
	placeArea  = postgis.NewNullGeomCol[place](placesSrc, "area", 4326, postgis.KindPolygon, postgis.XY)
	placeWhere = postgis.NewGeogCol[place](placesSrc, "spot", 4326, postgis.KindPoint, postgis.XY)
)

const placesDDL = `
create table places (
	id       bigint primary key,
	name     text not null,
	location geometry(Point, 4326) not null,
	area     geometry(Polygon, 4326),
	spot     geography(Point, 4326) not null
)`

func setupPlaces(t *testing.T) *ormExec {
	t.Helper()
	conn := gisConn(t)
	if _, err := conn.Exec(t.Context(), placesDDL); err != nil {
		t.Fatalf("creating the places table: %v", err)
	}
	rows := []struct {
		id       int64
		name     string
		lon, lat float64
	}{
		{1, "origin", 0, 0},
		{2, "east", 1, 0},
		{3, "far", 10, 10},
		{4, "north", 0, 0.5},
	}
	for _, r := range rows {
		p := postgis.NewPoint(4326, r.lon, r.lat)
		g, err := p.AsGeography()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(t.Context(),
			`insert into places (id, name, location, area, spot) values ($1, $2, $3, null, $4)`,
			r.id, r.name, p, g); err != nil {
			t.Fatalf("seeding %s: %v", r.name, err)
		}
	}
	// One place with an area, so the nullable geometry column has both cases.
	if _, err := conn.Exec(t.Context(),
		`update places set area = $1 where id = 1`,
		postgis.NewPolygon(4326, postgis.XY, square(-1, -1, 2))); err != nil {
		t.Fatalf("setting an area: %v", err)
	}
	return &ormExec{conn}
}

// ormExec adapts the test connection to the ORM's Executor, which is the same
// interface a *pgx.Conn already satisfies — the wrapper exists only to keep the
// test's helper types in one place.
type ormExec struct{ orm.Executor }

// names runs a composed query returning the names it selects, in order.
func names(t *testing.T, ex orm.Executor, where orm.Predicate[orm.Composed]) []string {
	t.Helper()
	shape := orm.Project1(orm.Of(placeName), func(n string) string { return n })
	got, err := orm.Compose(ex, shape).
		From(placesSrc).
		Where(where).
		OrderBy(orm.Of(placeID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	return got
}

func TestSpatial_predicatesInAQuery(t *testing.T) {
	ex := setupPlaces(t)

	// A polygon covering the origin and the point one degree east, but not the
	// far one.
	box := postgis.NewPolygon(4326, postgis.XY, square(-2, -2, 5))

	tests := []struct {
		name  string
		where orm.Predicate[orm.Composed]
		want  []string
	}{
		{
			"within a polygon",
			postgis.Of(placeLoc).Within(postgis.Value[orm.Composed](box)),
			[]string{"origin", "east", "north"},
		},
		{
			"intersects a polygon",
			postgis.Of(placeLoc).Intersects(postgis.Value[orm.Composed](box)),
			[]string{"origin", "east", "north"},
		},
		{
			"disjoint from a polygon",
			postgis.Of(placeLoc).Disjoint(postgis.Value[orm.Composed](box)),
			[]string{"far"},
		},
		{
			"bounding boxes overlap",
			postgis.Of(placeLoc).BBoxIntersects(postgis.Value[orm.Composed](box)),
			[]string{"origin", "east", "north"},
		},
		{
			"within 0.6 degrees of the origin",
			postgis.Of(placeLoc).DWithin(
				postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)), 0.6),
			[]string{"origin", "north"},
		},
		{
			"equal to a point",
			postgis.Of(placeLoc).EqualsGeom(
				postgis.Value[orm.Composed](postgis.NewPoint(4326, 1, 0))),
			[]string{"east"},
		},
		{
			"a DE-9IM pattern for interior intersection",
			postgis.Of(placeLoc).Relate(postgis.Value[orm.Composed](box), "T********"),
			[]string{"origin", "east", "north"},
		},
		{
			"valid geometries",
			postgis.Of(placeLoc).IsValid(),
			[]string{"origin", "east", "far", "north"},
		},
		{
			"not empty",
			orm.Not(postgis.Of(placeLoc).IsEmptyGeom()),
			[]string{"origin", "east", "far", "north"},
		},
		{
			"composed with an ordinary predicate",
			orm.And(
				postgis.Of(placeLoc).Within(postgis.Value[orm.Composed](box)),
				orm.Cond(placeName.Ne("north")),
			),
			[]string{"origin", "east"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := names(t, ex, tc.where)
			if !equalStrings(got, tc.want) {
				t.Errorf("selected %v, want %v", got, tc.want)
			}
		})
	}
}

// The geography predicates answer in metres, which is the whole reason the
// second type exists.
func TestSpatial_geographyPredicates(t *testing.T) {
	ex := setupPlaces(t)

	origin, err := postgis.NewPoint(4326, 0, 0).AsGeography()
	if err != nil {
		t.Fatal(err)
	}
	// A degree of longitude at the equator is about 111 km, so 60 km reaches
	// the point half a degree north and not the one a full degree east.
	got := names(t, ex, postgis.OfGeog(placeWhere).DWithin(
		postgis.GeogValue[orm.Composed](origin), 60_000))
	if want := []string{"origin", "north"}; !equalStrings(got, want) {
		t.Errorf("within 60 km: %v, want %v", got, want)
	}

	got = names(t, ex, postgis.OfGeog(placeWhere).DWithin(
		postgis.GeogValue[orm.Composed](origin), 120_000))
	if want := []string{"origin", "east", "north"}; !equalStrings(got, want) {
		t.Errorf("within 120 km: %v, want %v", got, want)
	}
}

// The same distance written against the geometry column is in degrees, and the
// two disagreeing is the point.
func TestSpatial_unitsDifferByType(t *testing.T) {
	ex := setupPlaces(t)
	// 60000 degrees selects everything, which is what a query written for
	// metres against a geometry column does.
	got := names(t, ex, postgis.Of(placeLoc).DWithin(
		postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)), 60_000))
	if want := []string{"origin", "east", "far", "north"}; !equalStrings(got, want) {
		t.Errorf("60000 degrees selected %v, want everything (%v)", got, want)
	}
}

// A geometry in another coordinate system is refused while the query is being
// built, with a message naming both SRIDs — rather than by the server, at run
// time, with a message naming neither operand.
func TestSpatial_mixedSRIDRefused(t *testing.T) {
	ex := setupPlaces(t)
	shape := orm.Project1(orm.Of(placeName), func(n string) string { return n })
	_, err := orm.Compose(ex, shape).
		From(placesSrc).
		Where(postgis.Of(placeLoc).Intersects(
			postgis.Value[orm.Composed](postgis.NewPoint(3857, 0, 0)))).
		All(t.Context())
	if err == nil {
		t.Fatal("a predicate mixing SRID 4326 and 3857 built a query")
	}
	if !contains(err.Error(), "4326") || !contains(err.Error(), "3857") {
		t.Errorf("the error does not name both coordinate systems: %v", err)
	}
}

// Relating a geometry to a geography does not compile.
//
// GeomExpr and GeogExpr are different Go types, so
//
//	postgis.Of(placeLoc).Intersects(postgis.OfGeog(placeWhere))
//
// is rejected by the Go compiler rather than by anything this package wrote —
// which is the strongest form the rule can take, and the reason there is no
// run-time check for it. What this test asserts is that the cast between them
// exists and works, so the mistake has a correct thing to become.
func TestSpatial_crossingBetweenTypes(t *testing.T) {
	ex := setupPlaces(t)

	origin, err := postgis.NewPoint(4326, 0, 0).AsGeography()
	if err != nil {
		t.Fatal(err)
	}
	// The geometry column, cast to geography, measured in metres.
	got := names(t, ex, postgis.ComposeGeog(postgis.Of(placeLoc).AsGeography()).
		DWithin(postgis.GeogValue[orm.Composed](origin), 60_000))
	if want := []string{"origin", "north"}; !equalStrings(got, want) {
		t.Errorf("the cast geometry within 60 km: %v, want %v", got, want)
	}

	// And the geography column, cast to geometry, measured in degrees.
	got = names(t, ex, postgis.Compose(postgis.OfGeog(placeWhere).AsGeometry()).
		DWithin(postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)), 0.6))
	if want := []string{"origin", "north"}; !equalStrings(got, want) {
		t.Errorf("the cast geography within 0.6 degrees: %v, want %v", got, want)
	}
}

// A DE-9IM pattern that is not one fails while the query is being built.
func TestSpatial_badDE9IM(t *testing.T) {
	ex := setupPlaces(t)
	shape := orm.Project1(orm.Of(placeName), func(n string) string { return n })
	for _, pattern := range []string{"", "TTT", "T*******X", "123456789 "} {
		_, err := orm.Compose(ex, shape).
			From(placesSrc).
			Where(postgis.Of(placeLoc).Relate(
				postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)), pattern)).
			All(t.Context())
		if err == nil {
			t.Errorf("the pattern %q built a query", pattern)
		}
	}
}

// The measurements read back as the Go types this package claims, and the
// claims are checked against pg_typeof rather than asserted.
func TestSpatial_measurementTypes(t *testing.T) {
	conn := gisConn(t)
	tests := []struct {
		sql  string
		want string
	}{
		{`ST_Area('POLYGON((0 0,1 0,1 1,0 1,0 0))'::geometry)`, "double precision"},
		{`ST_Length('LINESTRING(0 0,1 1)'::geometry)`, "double precision"},
		{`ST_Perimeter('POLYGON((0 0,1 0,1 1,0 1,0 0))'::geometry)`, "double precision"},
		{`ST_Distance('POINT(0 0)'::geometry, 'POINT(1 1)'::geometry)`, "double precision"},
		{`ST_X('POINT(1 2)'::geometry)`, "double precision"},
		{`ST_Azimuth('POINT(0 0)'::geometry, 'POINT(1 1)'::geometry)`, "double precision"},
		{`ST_SRID('POINT(1 2)'::geometry)`, "integer"},
		{`ST_NPoints('POINT(1 2)'::geometry)`, "integer"},
		{`ST_NumGeometries('POINT(1 2)'::geometry)`, "integer"},
		{`ST_Dimension('POINT(1 2)'::geometry)`, "integer"},
		{`ST_CoordDim('POINT(1 2)'::geometry)`, "smallint"},
		{`ST_GeometryType('POINT(1 2)'::geometry)`, "text"},
		{`ST_Area('POLYGON((0 0,1 0,1 1,0 1,0 0))'::geography)`, "double precision"},
		{`'POINT(0 0)'::geometry <-> 'POINT(1 1)'::geometry`, "double precision"},
	}
	for _, tc := range tests {
		var got string
		if err := conn.QueryRow(t.Context(), `select pg_typeof(`+tc.sql+`)::text`).Scan(&got); err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		if got != tc.want {
			t.Errorf("%s returns %s; this package claims %s", tc.sql, got, tc.want)
		}
	}
}

// The measurements in a real query, read into typed locals.
func TestSpatial_measurementsInAQuery(t *testing.T) {
	ex := setupPlaces(t)

	type row struct {
		name string
		x    float64
		y    float64
		srid int32
		kind string
	}
	shape := orm.Project5(
		orm.Of(placeName),
		orm.OfNull(placeLoc.Expr().X()),
		orm.OfNull(placeLoc.Expr().Y()),
		orm.Of(placeLoc.Expr().SRID()),
		orm.Of(placeLoc.Expr().GeometryType()),
		func(name string, x, y *float64, srid int32, kind string) row {
			r := row{name: name, srid: srid, kind: kind}
			if x != nil {
				r.x = *x
			}
			if y != nil {
				r.y = *y
			}
			return r
		},
	)
	got, err := orm.Compose(ex, shape).
		From(placesSrc).
		Where(orm.Cond(placeName.Eq("east"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	want := row{name: "east", x: 1, y: 0, srid: 4326, kind: "ST_Point"}
	if got[0] != want {
		t.Errorf("read %+v, want %+v", got[0], want)
	}
}

// A measurement over a nullable column reads back as a pointer, because
// ST_Area of a NULL is NULL and not zero.
func TestSpatial_nullableMeasurement(t *testing.T) {
	ex := setupPlaces(t)

	type row struct {
		name string
		area *float64
	}
	shape := orm.Project2(
		orm.Of(placeName),
		orm.OfNull(placeArea.Area()),
		func(name string, area *float64) row { return row{name, area} },
	)
	got, err := orm.Compose(ex, shape).
		From(placesSrc).
		OrderBy(orm.Of(placeID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d rows, want 4", len(got))
	}
	if got[0].area == nil {
		t.Error("the place with an area read back a NULL area")
	} else if *got[0].area != 4 {
		t.Errorf("the area is %v, want 4", *got[0].area)
	}
	for _, r := range got[1:] {
		if r.area != nil {
			t.Errorf("%s has no area and read back %v", r.name, *r.area)
		}
	}
}

// Ordering by KNN distance is what makes "the ten nearest" a query the index
// can answer, so the order it produces has to be the distance order.
func TestSpatial_nearestOrder(t *testing.T) {
	ex := setupPlaces(t)
	here := postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0))
	shape := orm.Project1(orm.Of(placeName), func(n string) string { return n })
	got, err := orm.Compose(ex, shape).
		From(placesSrc).
		OrderBy(orm.Of(postgis.Of(placeLoc).KNNDistance(here)).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []string{"origin", "north", "east", "far"}
	if !equalStrings(got, want) {
		t.Errorf("nearest first gave %v, want %v", got, want)
	}
}

// Asking a nullable expression for a non-nullable measurement is refused rather
// than answered, because the answer would be read into a destination that
// cannot hold the NULL it can produce.
func TestSpatial_nullabilityRefused(t *testing.T) {
	ex := setupPlaces(t)
	shape := orm.Project1(orm.Of(placeArea.Expr().Area()), func(a float64) float64 { return a })
	_, err := orm.Compose(ex, shape).From(placesSrc).All(t.Context())
	if err == nil {
		t.Fatal("a non-nullable area over a nullable column built a query")
	}
	if !contains(err.Error(), "AreaNull") {
		t.Errorf("the error does not name the form to use instead: %v", err)
	}
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
