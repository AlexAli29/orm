package gisdemo_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
)

// Spatial values inside the rest of the expression language.
//
// M11 gave the ORM derived tables, CTEs, subqueries, CASE and LATERAL; M12 gave
// it the PostgreSQL types. The claim under test here is that spatial values are
// values in that language rather than a parallel one — that a geometry can be
// projected out of a CTE, ordered by a window function, chosen by a CASE and
// aggregated, with the same types and the same checks it has anywhere else.

// A derived table projecting spatial values and scalars, consumed outside.
func TestCompose_derivedTable(t *testing.T) {
	db := openDB(t)

	name := orm.Named("name", orm.Of(gisdemo.Places.Name))
	centroid := orm.Named("centroid", orm.Of(gisdemo.Places.Location.Expr().Centroid().Value()))
	metres := orm.Named("metres", orm.Of(postgis.OfGeog(gisdemo.Places.Spot).
		Distance(postgis.GeogValue[orm.Composed](postgis.GeographyPoint(0, 0)))))
	geojson := orm.Named("geojson", orm.Of(postgis.Of(gisdemo.Places.Location).AsGeoJSON()))

	inner := orm.Sub("nearby", orm.Rows(name, centroid, metres, geojson).
		From(gisdemo.Places.Source()).
		Where(postgis.OfGeog(gisdemo.Places.Spot).
			DWithin(postgis.GeogValue[orm.Composed](postgis.GeographyPoint(0, 0)), 120_000)))

	type row struct {
		name     string
		centroid postgis.Geometry
		metres   float64
		geojson  string
	}
	shape := orm.Project4(
		orm.Ref(inner, name), orm.Ref(inner, centroid), orm.Ref(inner, metres), orm.Ref(inner, geojson),
		func(n string, c postgis.Geometry, m float64, g string) row { return row{n, c, m, g} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(inner).
		OrderBy(orm.Ref(inner, metres).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d rows, want 3", len(got))
	}
	if got[0].name != "origin" {
		t.Errorf("the nearest is %s", got[0].name)
	}
	// The geometry survived the derived table with its coordinate system.
	if got[0].centroid.SRID() != 4326 {
		t.Errorf("the projected centroid is SRID %d", got[0].centroid.SRID())
	}
	if !got[0].centroid.Equal(postgis.NewPoint(4326, 0, 0)) {
		t.Errorf("the projected centroid is %s %v", got[0].centroid, got[0].centroid.Coords())
	}
}

// The same through a CTE.
func TestCompose_cte(t *testing.T) {
	db := openDB(t)

	name := orm.Named("name", orm.Of(gisdemo.Places.Name))
	area := orm.Named("area", orm.Of(gisdemo.Places.Location.Expr().Buffer(1).Value()))

	cte := orm.CTE("buffered", orm.Rows(name, area).From(gisdemo.Places.Source()))

	type row struct {
		name string
		area postgis.Geometry
	}
	shape := orm.Project2(orm.Ref(cte, name), orm.Ref(cte, area),
		func(n string, a postgis.Geometry) row { return row{n, a} })
	got, err := orm.Compose(db.Executor(), shape).
		With(cte).
		From(cte).
		OrderBy(orm.Ref(cte, name).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d rows, want 4", len(got))
	}
	for _, r := range got {
		if r.area.Kind() != postgis.KindPolygon {
			t.Errorf("%s buffered to a %s", r.name, r.area.Kind())
		}
		if r.area.SRID() != 4326 {
			t.Errorf("%s buffered into SRID %d", r.name, r.area.SRID())
		}
	}
}

// A correlated subquery whose spatial predicate names the outer source.
func TestCompose_correlatedSubquery(t *testing.T) {
	db := openDB(t)

	// Zones that contain at least one place.
	occupied := orm.Exists[orm.Composed](orm.Rows(orm.Named("one", orm.Val(1))).
		From(gisdemo.Places.Source()).
		Where(postgis.Of(gisdemo.Zones.Area).Covers(postgis.Of(gisdemo.Places.Location))))

	got := names(t, db.Executor(), gisdemo.Zones.Source(),
		orm.Of(gisdemo.Zones.Name), orm.Of(gisdemo.Zones.ID).Asc(), occupied)
	if want := []string{"inner", "outer"}; !equalStrings(got, want) {
		t.Errorf("occupied zones are %v, want %v", got, want)
	}
}

// A correlated subquery naming a source the statement does not have is
// refused, which is the scope check the rest of the ORM already does — applied
// unchanged to a spatial predicate.
func TestCompose_unrelatedSourceRefused(t *testing.T) {
	db := openDB(t)

	stray := gisdemo.Roads.As("stray")
	bad := orm.Exists[orm.Composed](orm.Rows(orm.Named("one", orm.Val(1))).
		From(gisdemo.Places.Source()).
		Where(postgis.Of(gisdemo.Zones.Area).Intersects(postgis.Of(stray.Path))))

	shape := orm.Project1(orm.Of(gisdemo.Zones.Name), func(s string) string { return s })
	_, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Zones.Source()).
		Where(bad).
		All(t.Context())
	if err == nil {
		t.Fatal("a spatial predicate naming a source no statement has built a query")
	}
}

// CASE over spatial values, which widens to a general geometry rather than
// claiming a shape only one branch has.
func TestCompose_caseOverGeometry(t *testing.T) {
	db := openDB(t)

	// A point when the place has no footprint, the footprint's centroid when it
	// has one. The two branches are different shapes going in and the same type
	// coming out.
	chosen := orm.Case(
		orm.Cond(gisdemo.Places.Name.Eq("origin")),
		orm.Of(gisdemo.Places.Location.Expr().Buffer(0.5).Value()),
	).Else(orm.Of(gisdemo.Places.Location.Expr().Value()))

	type row struct {
		name string
		kind string
		geom postgis.Geometry
	}
	shape := orm.Project3(
		orm.Of(gisdemo.Places.Name),
		orm.Of(orm.Fn[orm.Composed, string]("ST_GeometryType", orm.ArgOf(chosen))),
		orm.Of(chosen),
		func(n, k string, g postgis.Geometry) row { return row{n, k, g} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d rows", len(got))
	}
	if got[0].kind != "ST_Polygon" {
		t.Errorf("the buffered branch is %s", got[0].kind)
	}
	if got[1].kind != "ST_Point" {
		t.Errorf("the other branch is %s", got[1].kind)
	}
	// Both branches read into the same Go type, and each value reports its own
	// shape — which is the reason the result is a general geometry.
	if got[0].geom.Kind() != postgis.KindPolygon || got[1].geom.Kind() != postgis.KindPoint {
		t.Errorf("the shapes came back as %s and %s", got[0].geom.Kind(), got[1].geom.Kind())
	}
}

// LATERAL: the nearest place to each zone's centre.
//
// This is the query LATERAL exists for — a per-row lookup that a plain join
// cannot express — and it is worth having spatially because "the nearest one"
// is the spatial question.
func TestCompose_lateralNearest(t *testing.T) {
	db := openDB(t)

	placeName := orm.Named("place", orm.Of(gisdemo.Places.Name))
	nearest := orm.Sub("nearest", orm.Rows(placeName).
		From(gisdemo.Places.Source()).
		OrderBy(orm.Of(postgis.Of(gisdemo.Places.Location).
			KNNDistance(postgis.Of(gisdemo.Zones.Centre))).Asc()).
		Limit(1))

	type row struct {
		zone  string
		place string
	}
	shape := orm.Project2(orm.Of(gisdemo.Zones.Name), orm.Ref(nearest, placeName),
		func(z, p string) row { return row{z, p} })
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Zones.Source()).
		CrossJoinLateral(nearest).
		Where(orm.Cond(gisdemo.Zones.Centre.IsNotNull())).
		OrderBy(orm.Of(gisdemo.Zones.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	if got[0].zone != "inner" || got[0].place != "origin" {
		t.Errorf("the nearest place to %s is %s", got[0].zone, got[0].place)
	}
}

// The spatial aggregates, through the existing aggregate framework.
func TestCompose_aggregates(t *testing.T) {
	db := openDB(t)

	type row struct {
		collected *postgis.Geometry
		merged    *postgis.Geometry
		extent    *postgis.Box2D
		n         int64
	}
	shape := orm.Project4(
		orm.OfNull(postgis.Collect(gisdemo.Places.Location.Expr())),
		orm.OfNull(postgis.UnionAgg(gisdemo.Places.Location.Expr())),
		orm.OfNull(postgis.Extent(gisdemo.Places.Location.Expr())),
		orm.Of(orm.Count[gisdemo.Place]()),
		func(c, m *postgis.Geometry, e *postgis.Box2D, n int64) row { return row{c, m, e, n} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows", len(got))
	}
	r := got[0]
	if r.n != 4 {
		t.Fatalf("counted %d places", r.n)
	}
	if r.collected == nil || r.collected.NumPoints() != 4 {
		t.Errorf("ST_Collect gave %v", r.collected)
	}
	if r.collected != nil && r.collected.Kind() != postgis.KindMultiPoint {
		t.Errorf("ST_Collect of four points gave a %s", r.collected.Kind())
	}
	// The four seeded places span (0,0) to (10,10).
	if r.extent == nil || !r.extent.Valid {
		t.Fatalf("ST_Extent gave %v", r.extent)
	}
	if r.extent.MinX != 0 || r.extent.MinY != 0 || r.extent.MaxX != 10 || r.extent.MaxY != 10 {
		t.Errorf("ST_Extent gave %s", r.extent)
	}
	if r.merged == nil {
		t.Error("ST_Union aggregate gave NULL over four rows")
	}
}

// An aggregate over no rows is NULL and not an empty box, which is why the
// aggregate result types are pointers.
func TestCompose_aggregateOverNoRows(t *testing.T) {
	db := openDB(t)

	shape := orm.Project2(
		orm.OfNull(postgis.Extent(gisdemo.Places.Location.Expr())),
		orm.OfNull(postgis.Collect(gisdemo.Places.Location.Expr())),
		func(e *postgis.Box2D, c *postgis.Geometry) [2]bool { return [2]bool{e != nil, c != nil} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(orm.Cond(gisdemo.Places.Name.Eq("nothing matches this"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	if got[0][0] || got[0][1] {
		t.Errorf("an aggregate over no rows gave a value: extent=%v collect=%v", got[0][0], got[0][1])
	}
}

// Grouping by a non-spatial column with a spatial aggregate per group.
func TestCompose_groupedExtent(t *testing.T) {
	db := openDB(t)

	type row struct {
		zone   string
		extent *postgis.Box2D
	}
	shape := orm.Project2(
		orm.Of(gisdemo.Zones.Name),
		orm.OfNull(postgis.Extent(gisdemo.Zones.Area.Expr())),
		func(n string, e *postgis.Box2D) row { return row{n, e} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Zones.Source()).
		GroupBy(orm.Of(gisdemo.Zones.Name)).
		OrderBy(orm.Of(gisdemo.Zones.Name).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d groups, want 2", len(got))
	}
	if got[0].extent == nil || got[0].extent.MaxX != 2 {
		t.Errorf("the inner zone's extent is %v", got[0].extent)
	}
	if got[1].extent == nil || got[1].extent.MinX != 5 {
		t.Errorf("the outer zone's extent is %v", got[1].extent)
	}
}

// A spatial measurement composed with a window function, which is the M11
// machinery applied to a number that happens to come from PostGIS.
func TestCompose_window(t *testing.T) {
	db := openDB(t)

	metres := postgis.OfGeog(gisdemo.Places.Spot).
		Distance(postgis.GeogValue[orm.Composed](postgis.GeographyPoint(0, 0)))
	rank := orm.RowNumber().Over(orm.Window().OrderBy(orm.Of(metres).Asc()))

	type row struct {
		name string
		rank int64
	}
	shape := orm.Project2(orm.Of(gisdemo.Places.Name), orm.Of(rank),
		func(n string, r int64) row { return row{n, r} })
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		OrderBy(orm.Of(rank).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []string{"origin", "north", "east", "far"}
	for i, w := range want {
		if got[i].name != w || got[i].rank != int64(i+1) {
			t.Errorf("rank %d is %+v, want %s", i+1, got[i], w)
		}
	}
}
