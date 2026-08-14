package postgis

import "fmt"

// Geography is a geometry on the spheroid.
//
// It holds the same coordinates a [Geometry] does and is a different Go type,
// because PostgreSQL treats it as a different type and the difference is not
// cosmetic:
//
//	ST_Distance over geometry(4326)   degrees, and not a distance anyone wants
//	ST_Distance over geography(4326)  metres along the spheroid
//
// Both compile, both return a float8, and only one of them answers "how far
// apart are these two places". Nothing in this package converts between them on
// your behalf; [Geometry.AsGeography] and [Geography.AsGeometry] are how the
// conversion is written down, and they are the only way it happens.
//
// The coordinates are longitude then latitude, in that order, because that is
// the order PostGIS reads them in: X is longitude. A pair written the other way
// round compiles, stores and returns wrong answers.
type Geography struct {
	geom Geometry
}

// AsGeography reinterprets the geometry's coordinates as positions on the
// spheroid.
//
// This is a cast, exactly as `geom::geography` is: the numbers do not change,
// only what they are taken to mean. It requires an SRID, because a geography
// with no spatial reference is a set of numbers with no surface to be on — and
// PostGIS's own cast would silently supply 4326, which is a guess this package
// will not make for you.
func (g Geometry) AsGeography() (Geography, error) {
	if g.srid == UnknownSRID {
		return Geography{}, fmt.Errorf("postgis: this geometry has no SRID, and a geography needs one" +
			" (use WithSRID to say which coordinate system the numbers are in — 4326 for longitude and latitude)")
	}
	return Geography{geom: g.clone()}, nil
}

// AsGeometry reinterprets the geography's positions as plane coordinates.
//
// The numbers are unchanged; what changes is that distances and areas over the
// result are in the SRID's units rather than in metres.
func (g Geography) AsGeometry() Geometry { return g.geom.clone() }

// NewGeography is [Geometry.AsGeography] for a geometry built inline, which is
// the common case.
func NewGeography(g Geometry) (Geography, error) { return g.AsGeography() }

// GeographyPoint returns a point on the spheroid at the given longitude and
// latitude, in SRID 4326.
//
// The argument order is the one PostGIS uses and the opposite of how a place is
// usually spoken: longitude first.
func GeographyPoint(lon, lat float64) Geography {
	return Geography{geom: NewPoint(4326, lon, lat)}
}

// Kind reports the shape.
func (g Geography) Kind() Kind { return g.geom.kind }

// Dim reports the dimensionality.
func (g Geography) Dim() Dim { return g.geom.dim }

// SRID reports the spatial reference, which for a geography is almost always
// 4326.
func (g Geography) SRID() int32 { return g.geom.srid }

// IsEmpty reports whether this is one of PostGIS's empty values, which is not
// the same as being NULL.
func (g Geography) IsEmpty() bool { return g.geom.IsEmpty() }

// NumPoints reports how many positions the value holds.
func (g Geography) NumPoints() int { return g.geom.NumPoints() }

// Coords returns every position, in order.
func (g Geography) Coords() []Coord { return g.geom.Coords() }

// Geometries returns the members of a collection or multi value.
func (g Geography) Geometries() []Geometry { return g.geom.Geometries() }

// Equal reports structural equality, with the same meaning it has on
// [Geometry.Equal]: same shape, same coordinates, same SRID, and not the
// topological question ST_Equals answers.
func (g Geography) Equal(other Geography) bool { return g.geom.Equal(other.geom) }

// EWKB returns the value's EWKB encoding, which is the same encoding a geometry
// with these coordinates has — on the wire the two types are identical, and
// only the column tells them apart.
func (g Geography) EWKB() []byte { return g.geom.EWKB() }

// AppendEWKB appends the value's EWKB encoding to dst.
func (g Geography) AppendEWKB(dst []byte) []byte { return g.geom.AppendEWKB(dst) }

// String renders the value for a person.
func (g Geography) String() string { return "geography(" + g.geom.String() + ")" }
