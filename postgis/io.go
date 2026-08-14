package postgis

import (
	"github.com/AlexAli29/orm"
)

// Spatial input and output.
//
// The output side is a set of renderings PostGIS does better than Go can: EWKT
// with its SRID prefix, GeoJSON in the form a map library expects, the binary
// forms. Each returns what the server actually returns rather than what the
// name suggests — ST_AsGeoJSON returns text and not jsonb, which is the sort of
// thing a package finds out by asking rather than by assuming, and the type
// matrix asks.
//
// The input side is a parsing boundary and is treated as one. WKT and GeoJSON
// arriving from outside the program are data, they cross as bind parameters,
// and malformed input comes back as PostGIS's own error. There is no path here
// by which a caller's text becomes SQL — the security corpus in the tests sends
// quotes, backslashes, and text shaped like a statement, and asserts the
// generated SQL does not change.
//
// None of this replaces the binary codec. A geometry sent as a parameter
// travels as EWKB and needs none of these; they exist for the cases where the
// text form is the point, which is rendering for a client and accepting what a
// client sent.

// AsText renders the geometry as WKT, which is the OGC's text form and carries
// no SRID.
//
// Use [GeomExpr.AsEWKT] when the coordinate system has to survive the
// rendering, which for anything leaving the program it usually does.
func (g GeomExpr[E]) AsText() orm.Value[E, string] {
	return g.text("ST_AsText", "AsTextNull")
}

// AsTextNull is [GeomExpr.AsText] over a geometry that can be NULL.
func (g GeomExpr[E]) AsTextNull() orm.Value[E, *string] {
	return g.textNull("ST_AsText", "AsText")
}

// AsEWKT renders the geometry as PostGIS's extended WKT, which prefixes the
// SRID: SRID=4326;POINT(1 2).
func (g GeomExpr[E]) AsEWKT() orm.Value[E, string] {
	return g.text("ST_AsEWKT", "AsEWKTNull")
}

// AsEWKTNull is [GeomExpr.AsEWKT] over a geometry that can be NULL.
func (g GeomExpr[E]) AsEWKTNull() orm.Value[E, *string] {
	return g.textNull("ST_AsEWKT", "AsEWKT")
}

// AsGeoJSON renders the geometry as a GeoJSON geometry object.
//
// It reads back as a string rather than as a parsed document, because that is
// what PostGIS returns: ST_AsGeoJSON's result type is text, not json and not
// jsonb. Claiming otherwise would put a jsonb codec in front of bytes the
// server never marked as one.
//
// GeoJSON is defined in WGS 84, so a geometry in another coordinate system
// should be transformed before rendering — PostGIS does not do it and neither
// does this.
func (g GeomExpr[E]) AsGeoJSON() orm.Value[E, string] {
	return g.text("ST_AsGeoJSON", "AsGeoJSONNull")
}

// AsGeoJSONNull is [GeomExpr.AsGeoJSON] over a geometry that can be NULL.
func (g GeomExpr[E]) AsGeoJSONNull() orm.Value[E, *string] {
	return g.textNull("ST_AsGeoJSON", "AsGeoJSON")
}

// AsBinary renders the geometry as OGC WKB, which carries no SRID.
func (g GeomExpr[E]) AsBinary() orm.Value[E, []byte] {
	return g.bytes("ST_AsBinary", "AsBinaryNull")
}

// AsBinaryNull is [GeomExpr.AsBinary] over a geometry that can be NULL.
func (g GeomExpr[E]) AsBinaryNull() orm.Value[E, *[]byte] {
	return g.bytesNull("ST_AsBinary", "AsBinary")
}

// AsEWKB renders the geometry as PostGIS's extended WKB, which carries the
// SRID.
//
// It is the same encoding [Geometry.EWKB] produces and the codec sends, so a
// query needs it only when the bytes themselves are the result — a cache key, a
// checksum, a payload for something else that speaks EWKB.
func (g GeomExpr[E]) AsEWKB() orm.Value[E, []byte] {
	return g.bytes("ST_AsEWKB", "AsEWKBNull")
}

// AsEWKBNull is [GeomExpr.AsEWKB] over a geometry that can be NULL.
func (g GeomExpr[E]) AsEWKBNull() orm.Value[E, *[]byte] {
	return g.bytesNull("ST_AsEWKB", "AsEWKB")
}

// The geography renderings, which are the same functions over the other type.

// AsText renders the geography as WKT.
func (g GeogExpr[E]) AsText() orm.Value[E, string] { return g.e.text("ST_AsText", "AsTextNull") }

// AsTextNull is [GeogExpr.AsText] over a geography that can be NULL.
func (g GeogExpr[E]) AsTextNull() orm.Value[E, *string] {
	return g.e.textNull("ST_AsText", "AsText")
}

// AsEWKT renders the geography as PostGIS's extended WKT.
func (g GeogExpr[E]) AsEWKT() orm.Value[E, string] { return g.e.text("ST_AsEWKT", "AsEWKTNull") }

// AsGeoJSON renders the geography as a GeoJSON geometry object, as text.
func (g GeogExpr[E]) AsGeoJSON() orm.Value[E, string] {
	return g.e.text("ST_AsGeoJSON", "AsGeoJSONNull")
}

// AsGeoJSONNull is [GeogExpr.AsGeoJSON] over a geography that can be NULL.
func (g GeogExpr[E]) AsGeoJSONNull() orm.Value[E, *string] {
	return g.e.textNull("ST_AsGeoJSON", "AsGeoJSON")
}

func (g GeomExpr[E]) text(fn, alternative string) orm.Value[E, string] {
	if g.nullable {
		return orm.Fn[E, string](fn, orm.ArgFail(nullMismatch(fn, alternative)))
	}
	return orm.Fn[E, string](fn, g.arg)
}

func (g GeomExpr[E]) textNull(fn, alternative string) orm.Value[E, *string] {
	if !g.nullable {
		return orm.FnNull[E, string](fn, orm.ArgFail(notNullMismatch(fn, alternative)))
	}
	return orm.FnNull[E, string](fn, g.arg)
}

func (g GeomExpr[E]) bytes(fn, alternative string) orm.Value[E, []byte] {
	if g.nullable {
		return orm.Fn[E, []byte](fn, orm.ArgFail(nullMismatch(fn, alternative)))
	}
	return orm.Fn[E, []byte](fn, g.arg)
}

func (g GeomExpr[E]) bytesNull(fn, alternative string) orm.Value[E, *[]byte] {
	if !g.nullable {
		return orm.FnNull[E, []byte](fn, orm.ArgFail(notNullMismatch(fn, alternative)))
	}
	return orm.FnNull[E, []byte](fn, g.arg)
}

// The parsing boundary.
//
// Each of these takes text from outside the program and hands it to PostGIS to
// parse. The text is a bind parameter — there is no formatting step, no
// quoting, and nothing a caller writes reaches the statement as syntax — so
// input containing quotes, backslashes or something that looks like SQL is
// input, and PostGIS rejects it as malformed geometry rather than running it.

// GeomFromText parses WKT into a geometry, in the given coordinate system.
//
// The SRID is required rather than optional, for the same reason
// [MakeEnvelope]'s is: PostGIS's one-argument form produces SRID 0, and a
// geometry in SRID 0 silently fails to relate to anything in a table that has
// one. Pass [UnknownSRID] deliberately when that really is what is meant.
//
// Malformed WKT is PostGIS's error, unchanged. This package does not parse WKT
// and so has no second opinion about what is malformed.
func GeomFromText[E any](wkt string, srid int32) GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOf(orm.Fn[E, Geometry]("ST_GeomFromText",
			orm.ArgValue(wkt), orm.ArgCast(srid, "int4"))),
		srid: srid,
	}
}

// GeomFromTextExpr is [GeomFromText] where the text comes from a column.
func GeomFromTextExpr[E any](wkt orm.Selectable[E, string], srid int32) GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOf(orm.Fn[E, Geometry]("ST_GeomFromText",
			orm.ArgOf(wkt), orm.ArgCast(srid, "int4"))),
		srid: srid,
	}
}

// GeogFromText parses WKT or EWKT into a geography.
//
// PostGIS's geography parser takes the SRID from an EWKT prefix and otherwise
// assumes 4326, which is the one place in this package a default coordinate
// system is not this package's choice — it is the function's own documented
// behaviour, and the expression records 4326 so that later checks see what
// PostGIS will see.
func GeogFromText[E any](wkt string) GeogExpr[E] {
	return GeogExpr[E]{e: GeomExpr[E]{
		arg:  orm.ArgOf(orm.Fn[E, Geography]("ST_GeogFromText", orm.ArgValue(wkt))),
		srid: 4326,
	}}
}

// GeomFromGeoJSON parses a GeoJSON geometry object.
//
// The document is a bind parameter. GeoJSON is defined in WGS 84 and PostGIS
// tags the result 4326, which is what the expression records.
func GeomFromGeoJSON[E any](doc string) GeomExpr[E] {
	return GeomExpr[E]{
		arg:  orm.ArgOf(orm.Fn[E, Geometry]("ST_GeomFromGeoJSON", orm.ArgValue(doc))),
		srid: 4326,
	}
}

// GeomFromGeoJSONExpr is [GeomFromGeoJSON] where the document comes from a
// column — a jsonb column rendered to text, or a text column holding one.
func GeomFromGeoJSONExpr[E any](doc orm.Selectable[E, string]) GeomExpr[E] {
	return GeomExpr[E]{
		arg:  orm.ArgOf(orm.Fn[E, Geometry]("ST_GeomFromGeoJSON", orm.ArgOf(doc))),
		srid: 4326,
	}
}

// GeomFromEWKB parses PostGIS's extended WKB.
//
// It exists for bytes that arrived from somewhere else already encoded — a
// cache, a message queue, another system's export. A geometry this program
// holds needs none of it: [Value] sends the same encoding through the codec
// with no function call in the way.
func GeomFromEWKB[E any](ewkb []byte) GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOf(orm.Fn[E, Geometry]("ST_GeomFromEWKB", orm.ArgValue(ewkb))),
	}
}
