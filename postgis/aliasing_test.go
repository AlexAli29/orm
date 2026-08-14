package postgis_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/postgis"
)

// Ownership of a geometry's coordinates.
//
// The M12 audit found that a multirange built from a caller's slice kept that
// slice, so an expression already compiled into a query changed meaning when
// the caller reused the backing array. A geometry is the same shape of value —
// coordinates and structure in slices — so it is the same attack.
//
// Every case below does the same three things: build a value from a slice,
// build an expression from the value, mutate the slice, and check the
// expression is what it was. The mutation is at every level, because cloning
// only the outer slice header is the bug that looks fixed.

// mutate scribbles over every coordinate in a slice.
func mutate(cs []postgis.Coord) {
	for i := range cs {
		cs[i] = postgis.Coord{X: 99999, Y: -99999, Z: 12345, M: -12345}
	}
}

func TestAliasing_lineString(t *testing.T) {
	ring := []postgis.Coord{{X: 0, Y: 0}, {X: 1, Y: 1}, {X: 2, Y: 0}}
	g := postgis.NewLineString(4326, postgis.XY, ring...)
	before := g.EWKB()

	mutate(ring)

	if got := g.EWKB(); !equalBytes(got, before) {
		t.Errorf("mutating the caller's slice changed the line: %v", g.Coords())
	}
}

func TestAliasing_polygonRings(t *testing.T) {
	outer := square(0, 0, 10)
	inner := square(3, 3, 4)
	rings := [][]postgis.Coord{outer, inner}
	g := postgis.NewPolygon(4326, postgis.XY, rings...)
	before := g.EWKB()

	// The outer slice, an inner ring, and one coordinate inside a ring — the
	// three levels a partial clone gets wrong in three different ways.
	rings[0] = square(100, 100, 1)
	mutate(outer)
	inner[0] = postgis.Coord{X: -1, Y: -1}

	if got := g.EWKB(); !equalBytes(got, before) {
		t.Errorf("mutating the caller's rings changed the polygon: %v", g.Coords())
	}
}

func TestAliasing_multiLineString(t *testing.T) {
	a := []postgis.Coord{{X: 0, Y: 0}, {X: 1, Y: 1}}
	b := []postgis.Coord{{X: 5, Y: 5}, {X: 6, Y: 6}}
	lines := [][]postgis.Coord{a, b}
	g := postgis.NewMultiLineString(4326, postgis.XY, lines...)
	before := g.EWKB()

	lines[1] = nil
	mutate(a)
	mutate(b)

	if got := g.EWKB(); !equalBytes(got, before) {
		t.Errorf("mutating the caller's lines changed the multi-line: %v", g.Coords())
	}
}

func TestAliasing_multiPolygon(t *testing.T) {
	first := square(0, 0, 1)
	second := square(10, 10, 2)
	hole := square(10.5, 10.5, 0.5)
	polys := [][][]postgis.Coord{{first}, {second, hole}}
	g := postgis.NewMultiPolygon(4326, postgis.XY, polys...)
	before := g.EWKB()

	polys[0] = nil
	polys[1][1] = nil
	mutate(first)
	mutate(second)
	mutate(hole)

	if got := g.EWKB(); !equalBytes(got, before) {
		t.Errorf("mutating the caller's polygons changed the multi-polygon: %v", g.Coords())
	}
}

func TestAliasing_multiPoint(t *testing.T) {
	pts := []postgis.Coord{{X: 1, Y: 2}, {X: 3, Y: 4}}
	g := postgis.NewMultiPoint(4326, postgis.XY, pts...)
	before := g.EWKB()
	mutate(pts)
	if got := g.EWKB(); !equalBytes(got, before) {
		t.Errorf("mutating the caller's points changed the multi-point: %v", g.Coords())
	}
}

// A collection holds whole geometries, so the members have to be cloned too —
// and a member's own slices along with them.
func TestAliasing_collection(t *testing.T) {
	ring := square(0, 0, 10)
	member := postgis.NewPolygon(4326, postgis.XY, ring)
	pt := postgis.NewPoint(4326, 1, 2)

	c, err := postgis.NewCollection(4326, member, pt)
	if err != nil {
		t.Fatal(err)
	}
	before := c.EWKB()

	mutate(ring)
	// And mutating the member value itself, which a shallow copy would share.
	member = member.WithSRID(3857)
	_ = member

	if got := c.EWKB(); !equalBytes(got, before) {
		t.Errorf("mutating a member changed the collection")
	}
}

// The reads have to be copies too. Coords and Geometries hand a caller a slice,
// and a caller who edits it must not be editing the geometry.
func TestAliasing_readsAreCopies(t *testing.T) {
	g := postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10), square(3, 3, 4))
	before := g.EWKB()

	cs := g.Coords()
	mutate(cs)
	if got := g.EWKB(); !equalBytes(got, before) {
		t.Error("editing the slice Coords returned changed the geometry")
	}

	multi := postgis.NewMultiPolygon(4326, postgis.XY, [][]postgis.Coord{square(0, 0, 1)})
	beforeMulti := multi.EWKB()
	members := multi.Geometries()
	for i := range members {
		members[i] = postgis.NewPoint(9999, 0, 0)
	}
	if got := multi.EWKB(); !equalBytes(got, beforeMulti) {
		t.Error("editing the slice Geometries returned changed the geometry")
	}
}

// The whole point of the exercise: an expression already built into a query.
//
// This is the M12 failure mode exactly — the value went into an expression, the
// caller reused the slice, and the compiled statement meant something else.
func TestAliasing_expressionKeepsItsValue(t *testing.T) {
	ring := square(0, 0, 10)
	g := postgis.NewPolygon(4326, postgis.XY, ring)

	q := orm.Compose(nil, orm.Project1(
		postgis.Value[orm.Composed](g).Value(),
		func(x postgis.Geometry) postgis.Geometry { return x },
	)).From(oneRow)

	sqlBefore, argsBefore, err := q.SQL()
	if err != nil {
		t.Fatal(err)
	}
	firstBefore := argsBefore[0].(postgis.Geometry).EWKB()

	mutate(ring)

	sqlAfter, argsAfter, err := q.SQL()
	if err != nil {
		t.Fatal(err)
	}
	if sqlAfter != sqlBefore {
		t.Errorf("the statement changed:\n%s\n%s", sqlBefore, sqlAfter)
	}
	firstAfter := argsAfter[0].(postgis.Geometry).EWKB()
	if !equalBytes(firstBefore, firstAfter) {
		t.Error("the compiled expression's geometry changed when the caller reused their slice")
	}
}

// AppendEWKB writes into a caller's buffer, which must not be the geometry's
// own storage.
func TestAliasing_appendEWKBDoesNotShare(t *testing.T) {
	g := postgis.NewLineString(4326, postgis.XY,
		postgis.Coord{X: 1, Y: 2}, postgis.Coord{X: 3, Y: 4})
	buf := g.AppendEWKB(nil)
	before := g.EWKB()
	for i := range buf {
		buf[i] = 0
	}
	if got := g.EWKB(); !equalBytes(got, before) {
		t.Error("zeroing the buffer AppendEWKB wrote into changed the geometry")
	}
}

// A decoded geometry must not share storage with the bytes it came from, since
// pgx reuses its read buffer between rows.
func TestAliasing_decodeCopies(t *testing.T) {
	raw := postgis.NewLineString(4326, postgis.XY,
		postgis.Coord{X: 1, Y: 2}, postgis.Coord{X: 3, Y: 4}).EWKB()
	g, err := postgis.DecodeEWKB(raw)
	if err != nil {
		t.Fatal(err)
	}
	before := g.Coords()
	for i := range raw {
		raw[i] = 0xff
	}
	after := g.Coords()
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("overwriting the source bytes changed the decoded geometry at %d", i)
		}
	}
}

func equalBytes(a, b []byte) bool {
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
