package postgis_test

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/postgis"
)

// The codec is where every spatial value enters and leaves the program, so it
// is tested on its own before anything is tested through a server: a round trip
// that is wrong here is wrong everywhere, and finding it in a query error
// message is much harder than finding it in a byte comparison.

// cases covers one of each shape, each dimensionality, empties, and the two
// spatial references that behave differently — a real one and none at all.
func cases(t *testing.T) []struct {
	name string
	g    postgis.Geometry
} {
	t.Helper()
	collection, err := postgis.NewCollection(4326,
		postgis.NewPoint(4326, 1, 2),
		postgis.NewLineString(4326, postgis.XY, postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 3, Y: 4}),
	)
	if err != nil {
		t.Fatalf("building a collection: %v", err)
	}
	nested, err := postgis.NewCollection(4326, collection, postgis.NewPoint(4326, 9, 9))
	if err != nil {
		t.Fatalf("building a nested collection: %v", err)
	}
	return []struct {
		name string
		g    postgis.Geometry
	}{
		{"point", postgis.NewPoint(4326, -73.985, 40.748)},
		{"point no SRID", postgis.NewPoint(postgis.UnknownSRID, 1, 2)},
		{"point Z", postgis.NewPointZ(4326, 1, 2, 3)},
		{"point M", postgis.NewPointM(4326, 1, 2, 3)},
		{"point ZM", postgis.NewPointZM(4326, 1, 2, 3, 4)},
		{"point empty", postgis.EmptyPoint(4326)},
		{"point negative zero", postgis.NewPoint(4326, math.Copysign(0, -1), 0)},
		{"line", postgis.NewLineString(3857, postgis.XY,
			postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 1, Y: 1}, postgis.Coord{X: 2, Y: 0})},
		{"line Z", postgis.NewLineString(4326, postgis.XYZ,
			postgis.Coord{X: 0, Y: 0, Z: 10}, postgis.Coord{X: 1, Y: 1, Z: 20})},
		{"line empty", postgis.NewLineString(4326, postgis.XY)},
		{"polygon", postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10))},
		{"polygon with hole", postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10), square(3, 3, 4))},
		{"polygon empty", postgis.NewPolygon(4326, postgis.XY)},
		{"multipoint", postgis.NewMultiPoint(4326, postgis.XY,
			postgis.Coord{X: 1, Y: 2}, postgis.Coord{X: 3, Y: 4})},
		{"multipoint empty", postgis.NewMultiPoint(4326, postgis.XY)},
		{"multiline", postgis.NewMultiLineString(4326, postgis.XY,
			[]postgis.Coord{{X: 0, Y: 0}, {X: 1, Y: 1}},
			[]postgis.Coord{{X: 5, Y: 5}, {X: 6, Y: 6}, {X: 7, Y: 5}})},
		{"multipolygon", postgis.NewMultiPolygon(4326, postgis.XY,
			[][]postgis.Coord{square(0, 0, 1)},
			[][]postgis.Coord{square(10, 10, 2), square(10.5, 10.5, 0.5)})},
		{"collection", collection},
		{"nested collection", nested},
	}
}

// square returns a closed ring, which is what a polygon needs and what PostGIS
// refuses to store without.
func square(x, y, size float64) []postgis.Coord {
	return []postgis.Coord{
		{X: x, Y: y}, {X: x + size, Y: y}, {X: x + size, Y: y + size}, {X: x, Y: y + size}, {X: x, Y: y},
	}
}

func TestEWKB_roundTrip(t *testing.T) {
	for _, c := range cases(t) {
		t.Run(c.name, func(t *testing.T) {
			got, err := postgis.DecodeEWKB(c.g.EWKB())
			if err != nil {
				t.Fatalf("decoding %s: %v", c.g, err)
			}
			if !got.Equal(c.g) {
				t.Errorf("round trip changed the value:\n got %s %v\nwant %s %v",
					got, got.Coords(), c.g, c.g.Coords())
			}
			if got.SRID() != c.g.SRID() {
				t.Errorf("round trip changed the SRID: got %d, want %d", got.SRID(), c.g.SRID())
			}
			if got.Dim() != c.g.Dim() {
				t.Errorf("round trip changed the dimensionality: got %v, want %v", got.Dim(), c.g.Dim())
			}
			if got.IsEmpty() != c.g.IsEmpty() {
				t.Errorf("round trip changed emptiness: got %v, want %v", got.IsEmpty(), c.g.IsEmpty())
			}
		})
	}
}

// Encoding twice has to produce the same bytes, or a checksum, a cache key or a
// test comparing two encodings of the same value would disagree with itself.
func TestEWKB_deterministic(t *testing.T) {
	for _, c := range cases(t) {
		if a, b := c.g.EWKB(), c.g.EWKB(); !bytes.Equal(a, b) {
			t.Errorf("%s encodes differently each time", c.name)
		}
	}
}

// PostGIS writes big-endian when the machine that produced the value was, and
// the members of a collection may each state their own order. A decoder that
// only reads little-endian works on every machine anyone tests it on and fails
// against a value that crossed architectures.
func TestEWKB_bigEndian(t *testing.T) {
	// SRID=4326;POINT(1 2) as PostGIS writes it big-endian.
	const be = "0020000001000010e63ff00000000000004000000000000000"
	raw, err := hex.DecodeString(be)
	if err != nil {
		t.Fatal(err)
	}
	g, err := postgis.DecodeEWKB(raw)
	if err != nil {
		t.Fatalf("decoding big-endian EWKB: %v", err)
	}
	want := postgis.NewPoint(4326, 1, 2)
	if !g.Equal(want) {
		t.Errorf("decoded %s %v, want %s %v", g, g.Coords(), want, want.Coords())
	}
}

// A geometry with no SRID and one with SRID 0 are the same thing, and neither
// is a geometry in 4326. Inferring one would put every unlabelled coordinate
// somewhere on Earth.
func TestEWKB_noSRIDIsNotFourThreeTwoSix(t *testing.T) {
	g := postgis.NewPoint(postgis.UnknownSRID, 1, 2)
	raw := g.EWKB()
	// Without an SRID the encoding has no SRID word: a header of five bytes.
	if len(raw) != 1+4+16 {
		t.Fatalf("the encoding is %d bytes, so it carries an SRID it should not", len(raw))
	}
	back, err := postgis.DecodeEWKB(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.SRID() != postgis.UnknownSRID {
		t.Errorf("decoded SRID %d from a geometry that had none", back.SRID())
	}
}

// Truncation at every length has to be an error rather than a panic or a
// geometry made of whatever was left in the buffer.
func TestEWKB_truncated(t *testing.T) {
	for _, c := range cases(t) {
		raw := c.g.EWKB()
		for n := range len(raw) {
			g, err := postgis.DecodeEWKB(raw[:n])
			if err == nil {
				t.Errorf("%s truncated to %d of %d bytes decoded as %s", c.name, n, len(raw), g)
			}
		}
	}
}

// A hostile encoding claims more elements than the bytes could hold. Allocating
// from that number kills the process; the decoder has to refuse first.
func TestEWKB_hugeClaimedLength(t *testing.T) {
	// A LINESTRING header claiming 0xffffffff positions, with no positions.
	raw := []byte{1, 2, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}
	if _, err := postgis.DecodeEWKB(raw); err == nil {
		t.Fatal("a linestring claiming four billion points decoded without error")
	}
	// The same claim inside a collection, so the check is not only at the top.
	raw = []byte{1, 7, 0, 0, 0, 0xff, 0xff, 0xff, 0xff}
	if _, err := postgis.DecodeEWKB(raw); err == nil {
		t.Fatal("a collection claiming four billion members decoded without error")
	}
}

// Nesting collections deep enough would run the stack out during decoding.
func TestEWKB_deepNesting(t *testing.T) {
	var raw []byte
	for range 1000 {
		raw = append(raw, 1, 7, 0, 0, 0, 1, 0, 0, 0) // GEOMETRYCOLLECTION with one member
	}
	raw = append(raw, 1, 1, 0, 0, 0)
	raw = append(raw, make([]byte, 16)...)
	_, err := postgis.DecodeEWKB(raw)
	if err == nil {
		t.Fatal("a thousand nested collections decoded without error")
	}
	if !strings.Contains(err.Error(), "nests") {
		t.Errorf("the error does not name the nesting: %v", err)
	}
}

// Trailing bytes mean the value is not what it claims to be, and accepting it
// would mean accepting a geometry followed by anything at all.
func TestEWKB_trailingBytes(t *testing.T) {
	raw := append(postgis.NewPoint(4326, 1, 2).EWKB(), 0xde, 0xad)
	if _, err := postgis.DecodeEWKB(raw); err == nil {
		t.Fatal("a geometry with two trailing bytes decoded without error")
	}
}

// The curved and surface types are real PostGIS geometries this package does
// not model. Decoding one into the nearest shape it does model would return
// coordinates under a name that is not theirs.
func TestEWKB_unmodelledType(t *testing.T) {
	raw := []byte{1, 8, 0, 0, 0, 0, 0, 0, 0} // CircularString, empty
	_, err := postgis.DecodeEWKB(raw)
	if err == nil {
		t.Fatal("a CircularString decoded as something this package models")
	}
	if !strings.Contains(err.Error(), "not one this package models") {
		t.Errorf("the error does not say the type is unmodelled: %v", err)
	}
}

// A garbage first byte is not a byte-order marker, and the message should say
// so rather than reporting a truncated geometry.
func TestEWKB_badByteOrder(t *testing.T) {
	if _, err := postgis.DecodeEWKB([]byte{9, 1, 0, 0, 0}); err == nil {
		t.Fatal("a value with no byte-order marker decoded without error")
	}
}

// A geometry built from a caller's slice must not change when the caller reuses
// it. An expression holding a geometry that changed underneath it is an
// expression that compiles to something nobody wrote.
func TestGeometry_constructorCopies(t *testing.T) {
	ring := square(0, 0, 10)
	g := postgis.NewPolygon(4326, postgis.XY, ring)
	before := g.Coords()
	for i := range ring {
		ring[i].X = 999
	}
	after := g.Coords()
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("reusing the caller's slice changed the geometry at %d: %v became %v",
				i, before[i], after[i])
		}
	}
}

// WithSRID must not modify the receiver, for the same reason.
func TestGeometry_withSRIDCopies(t *testing.T) {
	a := postgis.NewPoint(4326, 1, 2)
	b := a.WithSRID(3857)
	if a.SRID() != 4326 {
		t.Errorf("WithSRID changed the receiver's SRID to %d", a.SRID())
	}
	if b.SRID() != 3857 {
		t.Errorf("WithSRID produced SRID %d", b.SRID())
	}
}

// NULL and EMPTY are different answers. A package that renders them the same
// way makes "there is no geometry here" and "the geometry here covers nothing"
// indistinguishable, and they lead to different code.
func TestGeometry_emptyIsNotNull(t *testing.T) {
	empty := postgis.EmptyPoint(4326)
	if !empty.IsEmpty() {
		t.Error("EmptyPoint is not empty")
	}
	if empty.NumPoints() != 0 {
		t.Errorf("POINT EMPTY has %d points", empty.NumPoints())
	}
	// An empty point is a value, and a value round-trips.
	back, err := postgis.DecodeEWKB(empty.EWKB())
	if err != nil {
		t.Fatalf("decoding POINT EMPTY: %v", err)
	}
	if !back.IsEmpty() || back.Kind() != postgis.KindPoint || back.SRID() != 4326 {
		t.Errorf("POINT EMPTY round-tripped to %s", back)
	}
}

// A geography is a different Go type from a geometry, and getting one requires
// saying which coordinate system the numbers are in.
func TestGeography_needsSRID(t *testing.T) {
	if _, err := postgis.NewPoint(postgis.UnknownSRID, 1, 2).AsGeography(); err == nil {
		t.Fatal("a geometry with no SRID became a geography")
	}
	g, err := postgis.NewPoint(4326, 1, 2).AsGeography()
	if err != nil {
		t.Fatalf("AsGeography: %v", err)
	}
	if g.SRID() != 4326 {
		t.Errorf("the geography has SRID %d", g.SRID())
	}
	if !g.AsGeometry().Equal(postgis.NewPoint(4326, 1, 2)) {
		t.Error("converting back produced a different geometry")
	}
}

// A collection whose members disagree about the coordinate system is not a
// value PostGIS can store, so building one has to fail here rather than at the
// server.
func TestCollection_mismatchedSRID(t *testing.T) {
	_, err := postgis.NewCollection(4326, postgis.NewPoint(3857, 1, 2))
	if err == nil {
		t.Fatal("a collection accepted a member in another coordinate system")
	}
}

// The same goes for dimensionality: PostGIS stores one per value.
func TestCollection_mismatchedDim(t *testing.T) {
	_, err := postgis.NewCollection(4326,
		postgis.NewPoint(4326, 1, 2),
		postgis.NewPointZ(4326, 1, 2, 3),
	)
	if err == nil {
		t.Fatal("a collection accepted members of two dimensionalities")
	}
}
