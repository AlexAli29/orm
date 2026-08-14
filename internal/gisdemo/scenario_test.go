package gisdemo_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
)

// The scenarios.
//
// Each of these is a query somebody would really write, built entirely from the
// committed generated descriptors, and each is checked against the same
// question asked in hand-written SQL. Comparing against hand-written SQL rather
// than against an expected list is what makes these tests about PostGIS rather
// than about arithmetic somebody did in a comment.

// Nearby places: the query every spatial application starts with.
//
// The column is a geography, so the radius is metres and the ordering is a real
// distance rather than a number that happens to sort the same way most of the
// time.
func TestScenario_nearbyPlaces(t *testing.T) {
	db, pool := openBoth(t)
	here := postgis.GeographyPoint(0, 0)

	type row struct {
		name   string
		metres float64
	}
	distance := postgis.OfGeog(gisdemo.Places.Spot).
		Distance(postgis.GeogValue[orm.Composed](here))

	shape := orm.Project2(
		orm.Of(gisdemo.Places.Name),
		orm.Of(distance),
		func(name string, metres float64) row { return row{name, metres} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(postgis.OfGeog(gisdemo.Places.Spot).
			DWithin(postgis.GeogValue[orm.Composed](here), 120_000)).
		OrderBy(orm.Of(distance).Asc()).
		Limit(50).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// The same question, written by hand.
	type want struct {
		name   string
		metres float64
	}
	var expected []want
	rows, err := pool.Query(t.Context(), `
		SELECT name, ST_Distance(spot, ST_GeogFromText('SRID=4326;POINT(0 0)'))
		FROM places
		WHERE ST_DWithin(spot, ST_GeogFromText('SRID=4326;POINT(0 0)'), 120000)
		ORDER BY 2 ASC LIMIT 50`)
	if err != nil {
		t.Fatalf("the hand-written query: %v", err)
	}
	for rows.Next() {
		var w want
		if err := rows.Scan(&w.name, &w.metres); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != len(expected) {
		t.Fatalf("the ORM read %d rows and the hand-written query read %d", len(got), len(expected))
	}
	for i := range got {
		if got[i].name != expected[i].name || got[i].metres != expected[i].metres {
			t.Errorf("row %d: ORM %+v, hand-written %+v", i, got[i], expected[i])
		}
	}
	if len(got) != 3 {
		t.Fatalf("read %d places within 120 km, want 3", len(got))
	}
	if got[0].name != "origin" || got[0].metres != 0 {
		t.Errorf("the nearest is %+v, want origin at 0 metres", got[0])
	}
}

// Delivery zones, including the boundary case that separates Contains from
// Covers.
//
// A point exactly on a zone's edge is in the zone by any reasonable reading, and
// ST_Contains says it is not. That is the distinction, and it is the one that
// silently loses an order.
func TestScenario_deliveryZones(t *testing.T) {
	db := openDB(t)

	// The inner zone is the square from (-2,-2) to (2,2). This point is exactly
	// on its edge.
	onEdge := postgis.NewPoint(4326, 2, 0)

	contains := names(t, db.Executor(), gisdemo.Zones.Source(),
		orm.Of(gisdemo.Zones.Name),
		orm.Of(gisdemo.Zones.ID).Asc(),
		postgis.Of(gisdemo.Zones.Area).Contains(postgis.Value[orm.Composed](onEdge)))
	if len(contains) != 0 {
		t.Errorf("ST_Contains matched %v for a point on the boundary; it should match nothing", contains)
	}

	covers := names(t, db.Executor(), gisdemo.Zones.Source(),
		orm.Of(gisdemo.Zones.Name),
		orm.Of(gisdemo.Zones.ID).Asc(),
		postgis.Of(gisdemo.Zones.Area).Covers(postgis.Value[orm.Composed](onEdge)))
	if want := []string{"inner"}; !equalStrings(covers, want) {
		t.Errorf("ST_Covers matched %v for a point on the boundary, want %v", covers, want)
	}

	// A point well inside matches both.
	inside := postgis.NewPoint(4326, 0, 0)
	for _, tc := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
	}{
		{"Contains", postgis.Of(gisdemo.Zones.Area).Contains(postgis.Value[orm.Composed](inside))},
		{"Covers", postgis.Of(gisdemo.Zones.Area).Covers(postgis.Value[orm.Composed](inside))},
	} {
		got := names(t, db.Executor(), gisdemo.Zones.Source(),
			orm.Of(gisdemo.Zones.Name), orm.Of(gisdemo.Zones.ID).Asc(), tc.pred)
		if want := []string{"inner"}; !equalStrings(got, want) {
			t.Errorf("%s matched %v for an interior point, want %v", tc.name, got, want)
		}
	}
}

// A map viewport: build an envelope, find what falls in it, render GeoJSON.
func TestScenario_viewport(t *testing.T) {
	db := openDB(t)
	viewport := postgis.MakeEnvelope[orm.Composed](-2, -2, 2, 2, 4326)

	type row struct {
		name    string
		geojson string
	}
	shape := orm.Project2(
		orm.Of(gisdemo.Places.Name),
		orm.Of(postgis.Of(gisdemo.Places.Location).AsGeoJSON()),
		func(name, geojson string) row { return row{name, geojson} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(postgis.Of(gisdemo.Places.Location).Intersects(viewport)).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the viewport contains %d places, want 3", len(got))
	}
	if got[0].name != "origin" {
		t.Errorf("the first is %s", got[0].name)
	}
	// ST_AsGeoJSON returns text, and the contract here is that it reads back as
	// a Go string rather than as a parsed document.
	if got[0].geojson != `{"type":"Point","coordinates":[0,0]}` {
		t.Errorf("the GeoJSON is %q", got[0].geojson)
	}
}

// Roads through a district, where Intersects and Crosses give different answers
// — which is the reason both exist.
//
// A line that runs through a polygon crosses it. A line that lies along the
// polygon's boundary intersects it and does not cross it, because crossing
// requires passing through the interior.
func TestScenario_roadsAndDistrict(t *testing.T) {
	db := openDB(t)
	district := postgis.NewPolygon(4326, postgis.XY, []postgis.Coord{
		{X: -2, Y: -2}, {X: 2, Y: -2}, {X: 2, Y: 2}, {X: -2, Y: 2}, {X: -2, Y: -2},
	})

	intersects := names(t, db.Executor(), gisdemo.Roads.Source(),
		orm.Of(gisdemo.Roads.Name), orm.Of(gisdemo.Roads.ID).Asc(),
		postgis.Of(gisdemo.Roads.Path).Intersects(postgis.Value[orm.Composed](district)))
	crosses := names(t, db.Executor(), gisdemo.Roads.Source(),
		orm.Of(gisdemo.Roads.Name), orm.Of(gisdemo.Roads.ID).Asc(),
		postgis.Of(gisdemo.Roads.Path).Crosses(postgis.Value[orm.Composed](district)))

	if want := []string{"through", "inside", "touching"}; !equalStrings(intersects, want) {
		t.Errorf("Intersects matched %v, want %v", intersects, want)
	}
	// "inside" lies wholly within the district, so it does not cross the
	// boundary; "touching" runs along the edge and never enters the interior.
	if want := []string{"through"}; !equalStrings(crosses, want) {
		t.Errorf("Crosses matched %v, want %v", crosses, want)
	}
	if equalStrings(intersects, crosses) {
		t.Error("Intersects and Crosses gave the same answer, so the corpus does not separate them")
	}
}

// Transforming between coordinate systems, and the metadata that follows.
func TestScenario_transform(t *testing.T) {
	db := openDB(t)

	// The 4326 column projected into 3857 becomes comparable with the column
	// that is already 3857.
	transformed := postgis.Of(gisdemo.Places.Location).Transform(3857)
	if transformed.DeclaredSRID() != 3857 {
		t.Fatalf("the transformed expression reports SRID %d", transformed.DeclaredSRID())
	}

	type row struct {
		name string
		x    *float64
		y    *float64
	}
	shape := orm.Project3(
		orm.Of(gisdemo.Places.Name),
		orm.Of(transformed.X()),
		orm.Of(transformed.Y()),
		func(name string, x, y *float64) row { return row{name, x, y} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(orm.Cond(gisdemo.Places.Name.Eq("east"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].x == nil {
		t.Fatalf("read %+v", got)
	}
	// One degree of longitude at the equator is about 111 km in web Mercator.
	if *got[0].x < 111_000 || *got[0].x > 112_000 {
		t.Errorf("transforming (1, 0) from 4326 to 3857 gave x=%v", *got[0].x)
	}
	if *got[0].y > 0.001 && *got[0].y < -0.001 {
		t.Errorf("the y ordinate moved to %v", *got[0].y)
	}

	// And the transformed expression now relates to the 3857 column, which the
	// untransformed one would not.
	matched := names(t, db.Executor(), gisdemo.Places.Source(),
		orm.Of(gisdemo.Places.Name), orm.Of(gisdemo.Places.ID).Asc(),
		postgis.Of(gisdemo.Places.Location).Transform(3857).
			DWithin(postgis.Compose(gisdemo.Places.Projected.Expr()), 1))
	if want := []string{"origin", "east"}; !equalStrings(matched, want) {
		t.Errorf("the transformed column matched %v against the 3857 column, want %v", matched, want)
	}
}

// Relating a 4326 column to a 3857 one without transforming is refused while
// the query is being built.
func TestScenario_mixedSRIDRefused(t *testing.T) {
	db := openDB(t)
	shape := orm.Project1(orm.Of(gisdemo.Places.Name), func(s string) string { return s })
	_, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(postgis.Of(gisdemo.Places.Location).
			Intersects(postgis.Compose(gisdemo.Places.Projected.Expr()))).
		All(t.Context())
	if err == nil {
		t.Fatal("relating the 4326 column to the 3857 column built a query")
	}
	for _, want := range []string{"4326", "3857"} {
		if !contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

// Nearest neighbour: a DWithin prefilter the index can use, then KNN ordering.
//
// The KNN operator's number is not the geography distance, and the test does
// not pretend it is: what is asserted is the order, which is what KNN is for.
func TestScenario_nearestNeighbour(t *testing.T) {
	db, pool := openBoth(t)
	here := postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0))

	shape := orm.Project1(orm.Of(gisdemo.Places.Name), func(s string) string { return s })
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(postgis.Of(gisdemo.Places.Location).DWithin(here, 20)).
		OrderBy(orm.Of(postgis.Of(gisdemo.Places.Location).KNNDistance(here)).Asc()).
		Limit(3).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	var expected []string
	rows, err := pool.Query(t.Context(), `
		SELECT name FROM places
		WHERE ST_DWithin(location, 'SRID=4326;POINT(0 0)'::geometry, 20)
		ORDER BY location <-> 'SRID=4326;POINT(0 0)'::geometry
		LIMIT 3`)
	if err != nil {
		t.Fatalf("the hand-written query: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		expected = append(expected, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(got, expected) {
		t.Errorf("the ORM ordered %v and the hand-written query ordered %v", got, expected)
	}
	if want := []string{"origin", "north", "east"}; !equalStrings(got, want) {
		t.Errorf("nearest first gave %v, want %v", got, want)
	}
}

// DWithin has to reach the server as ST_DWithin.
//
// Rewriting it as ST_Distance(...) < d gives the same rows and a completely
// different plan: the index expands a bounding box for the first and cannot
// help with the second. A query builder that quietly rewrote it would make
// every spatial query a sequential scan.
func TestScenario_dwithinStaysDWithin(t *testing.T) {
	db := openDB(t)
	shape := orm.Project1(orm.Of(gisdemo.Places.Name), func(s string) string { return s })
	sql, _, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(postgis.Of(gisdemo.Places.Location).
			DWithin(postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)), 1)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !contains(sql, "ST_DWithin(") {
		t.Errorf("the statement does not call ST_DWithin:\n%s", sql)
	}
	if contains(sql, "ST_Distance") {
		t.Errorf("the statement was rewritten to a distance comparison:\n%s", sql)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
