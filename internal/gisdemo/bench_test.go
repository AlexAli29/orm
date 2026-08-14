package gisdemo_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
)

// What the spatial path costs through generated code.
//
// Compiling a query is the part an application does on every request, so it is
// the one worth measuring: a builder that allocated per predicate would be
// invisible in a test and expensive in a loop. The numbers here are not a
// target, they are a tripwire — a change that doubles them is a change worth
// looking at.

// Building and rendering a realistic spatial query, with no server involved.
func BenchmarkSpatial_compile(b *testing.B) {
	here := postgis.GeographyPoint(0, 0)
	b.ReportAllocs()
	for b.Loop() {
		distance := postgis.OfGeog(gisdemo.Places.Spot).
			Distance(postgis.GeogValue[orm.Composed](here))
		shape := orm.Project2(
			orm.Of(gisdemo.Places.Name),
			orm.Of(distance),
			func(n string, d float64) string { return n },
		)
		_, _, err := orm.Compose(nil, shape).
			From(gisdemo.Places.Source()).
			Where(postgis.OfGeog(gisdemo.Places.Spot).
				DWithin(postgis.GeogValue[orm.Composed](here), 5000)).
			OrderBy(orm.Of(distance).Asc()).
			Limit(50).
			SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// A predicate-heavy query, which is where a per-operand allocation would show.
func BenchmarkSpatial_compilePredicates(b *testing.B) {
	poly := postgis.NewPolygon(4326, postgis.XY, []postgis.Coord{
		{X: 0, Y: 0}, {X: 1, Y: 0}, {X: 1, Y: 1}, {X: 0, Y: 1}, {X: 0, Y: 0},
	})
	b.ReportAllocs()
	for b.Loop() {
		v := postgis.Value[orm.Composed](poly)
		shape := orm.Project1(orm.Of(gisdemo.Places.Name), func(n string) string { return n })
		_, _, err := orm.Compose(nil, shape).
			From(gisdemo.Places.Source()).
			Where(
				postgis.Of(gisdemo.Places.Location).Intersects(v),
				postgis.Of(gisdemo.Places.Location).Within(v),
				postgis.Of(gisdemo.Places.Location).DWithin(v, 1),
				postgis.Of(gisdemo.Places.Location).BBoxIntersects(v),
			).
			SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// A chain of transformations, which is the composition an application builds
// when it projects and buffers on the way out.
func BenchmarkSpatial_compileTransformChain(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		g := gisdemo.Places.Location.Expr().
			Transform(3857).
			Buffer(100).
			Centroid().
			Transform(4326)
		shape := orm.Project2(gisdemo.Places.Name, g.Value(),
			func(n string, x postgis.Geometry) string { return n })
		if _, _, err := orm.Compose(nil, orm.Project2(
			orm.Of(gisdemo.Places.Name), orm.Of(postgis.Compose(g).Value()),
			func(n string, x postgis.Geometry) string { return n },
		)).From(gisdemo.Places.Source()).SQL(); err != nil {
			b.Fatal(err)
		}
		_ = shape
	}
}
