// Package gisdemo is a worked example of a generated spatial package.
//
// The entities here are hand-written and the orm_*.gen.go files beside them are
// not: they are committed output, so that `go build ./...` compiles generated
// spatial code on every run and the tests beside them query through it rather
// than through descriptors a test wrote.
//
// That distinction is the point of this package. It is possible to build a
// spatial runtime that works when a human hand-writes the descriptors and falls
// over when a generator has to derive them from a catalog; the only way to know
// which one this is, is to make the generator do it.
package gisdemo

import (
	"time"

	"github.com/AlexAli29/orm/postgis"
)

// Place is somewhere on the map, held five ways.
//
// The pgtype: tags are how the shape and the coordinate system reach the
// generator. A Go type says which of PostGIS's two storage families a column is
// — postgis.Geography and postgis.Geometry are different types — and it cannot
// say more than that, because no Go type carries a number. The tag says the
// rest, and reconciliation checks it against the column.
//
//orm:table places
type Place struct {
	ID   int64
	Name string

	// On the spheroid. ST_Distance over this is metres.
	Spot postgis.Geography `orm:"pgtype:geography(Point,4326)"`

	// In the plane, in degrees. ST_Distance over this is degrees, which is a
	// number and not a distance.
	Location postgis.Geometry `orm:"pgtype:geometry(Point,4326)"`

	// The same place in web Mercator. Relating it to Location without
	// transforming first is refused.
	Projected *postgis.Geometry `orm:"pgtype:geometry(Point,3857)"`

	// A footprint the place may not have: NULL and an empty polygon are both
	// possible here and mean different things.
	Footprint *postgis.Geometry `orm:"pgtype:geometry(Polygon,4326)"`

	// A column with no modifier, which accepts any shape in any coordinate
	// system — and constrains nothing, so nothing is checked against it.
	Sketch *postgis.Geometry

	CreatedAt time.Time
}

// Zone is a delivery area.
//
//orm:table zones
type Zone struct {
	ID     int64
	Name   string
	Area   postgis.Geometry  `orm:"pgtype:geometry(MultiPolygon,4326)"`
	Centre *postgis.Geometry `orm:"pgtype:geometry(Point,4326)"`
}

// Road is a line somebody drives along.
//
//orm:table roads
type Road struct {
	ID   int64
	Name string
	Path postgis.Geometry `orm:"pgtype:geometry(LineString,4326)"`
}

// Reading carries one position in each of PostGIS's four dimensionalities.
//
// They exist so that a Z or an M that gets dropped somewhere between the
// catalog, the generator, the codec and COPY has a test that notices.
//
//orm:table readings
type Reading struct {
	ID     int64
	Flat   postgis.Geometry  `orm:"pgtype:geometry(Point,4326)"`
	Raised postgis.Geometry  `orm:"pgtype:geometry(PointZ,4326)"`
	Marked postgis.Geometry  `orm:"pgtype:geometry(PointM,4326)"`
	ZM     postgis.Geometry  `orm:"pgtype:geometry(PointZM,4326)"`
	Line3D *postgis.Geometry `orm:"pgtype:geometry(LineStringZ,4326),column:line3d"`
}
