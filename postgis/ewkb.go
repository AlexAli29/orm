package postgis

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// EWKB, which is how a geometry crosses the wire.
//
// PostGIS's binary format is WKB with three bits stolen from the type word: one
// says a SRID follows, and two say whether the coordinates carry Z and M. That
// is the whole difference, and it is the reason this package speaks EWKB rather
// than WKB — plain WKB has nowhere to put the SRID, so a round trip through it
// silently returns a geometry in an unspecified coordinate system.
//
// The encoding is little-endian because that is what every machine this runs on
// is; the decoder reads either, because the bytes come from the server and the
// server writes its own.
//
// Nothing here parses WKT. Text is a format for people, and a geometry that
// reaches PostgreSQL as text is a geometry assembled by string concatenation —
// which is how coordinates become syntax. Values in this package are bind
// parameters carrying binary, and the one place text is accepted, [ParseEWKT],
// is an explicit parser that produces a value rather than a fragment of SQL.

// The flag bits PostGIS sets in the type word.
const (
	ewkbZ    uint32 = 0x80000000
	ewkbM    uint32 = 0x40000000
	ewkbSRID uint32 = 0x20000000
	// ewkbBBox is the flag saying a cached bounding box precedes the body.
	//
	// PostGIS never sets it on the wire — geometry_send writes no box — but the
	// format defines it, and a decoder that ignored it would read the box's
	// floats as the geometry's coordinates and return something plausible and
	// wrong. So it is rejected rather than skipped: this package has never seen
	// one, and guessing its layout would be guessing.
	ewkbBBox uint32 = 0x10000000
	// ewkbTypeMask keeps the geometry type and drops the flags.
	ewkbTypeMask uint32 = 0x0fffffff
)

// maxPoints bounds what the decoder will allocate from a length it has not yet
// read the bytes for.
//
// A corrupt or hostile EWKB header can claim four billion positions in nine
// bytes. Without a bound the decoder allocates from that number and the process
// dies before it can report anything; with one it returns an error naming the
// count. The limit is far above any real geometry — a country boundary at full
// resolution is a few hundred thousand points — and the check is against the
// bytes actually available as well, so a truthful large geometry still decodes.
const maxPoints = 1 << 26

// AppendEWKB appends the geometry's EWKB encoding to dst and returns it.
//
// The SRID is written whenever the geometry has one, which is what makes the
// round trip lossless: a geometry that went in as 4326 comes back as 4326
// rather than as an unlabelled set of numbers.
func (g Geometry) AppendEWKB(dst []byte) []byte {
	return g.appendEWKB(dst, true)
}

// EWKB returns the geometry's EWKB encoding.
func (g Geometry) EWKB() []byte {
	return g.AppendEWKB(make([]byte, 0, 9+len(g.coords)*8))
}

// appendEWKB writes one geometry. writeSRID is false for the members of a
// collection, which inherit the collection's — PostGIS writes the SRID once, on
// the outermost geometry, and repeating it inside is not a form it accepts.
func (g Geometry) appendEWKB(dst []byte, writeSRID bool) []byte {
	dst = append(dst, 1) // little-endian
	t := uint32(g.kind)
	if g.dim.HasZ() {
		t |= ewkbZ
	}
	if g.dim.HasM() {
		t |= ewkbM
	}
	withSRID := writeSRID && g.srid != UnknownSRID
	if withSRID {
		t |= ewkbSRID
	}
	dst = binary.LittleEndian.AppendUint32(dst, t)
	if withSRID {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(g.srid))
	}
	return g.appendBody(dst)
}

func (g Geometry) appendBody(dst []byte) []byte {
	n := g.dim.ordinates()
	switch g.kind {
	case KindPoint:
		if g.IsEmpty() {
			// PostGIS writes POINT EMPTY as a point whose ordinates are all
			// NaN, because the format has no length to set to zero. Reading it
			// back gives the empty point again.
			for range n {
				dst = appendFloat(dst, math.NaN())
			}
			return dst
		}
		return appendFloats(dst, g.coords[:n])

	case KindLineString:
		return appendRun(dst, n, g.coords)

	case KindPolygon:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(g.parts)))
		for _, p := range g.parts {
			dst = appendRun(dst, n, g.coords[p.start:p.end])
		}
		return dst

	case KindMultiPoint, KindMultiLineString, KindMultiPolygon:
		members := g.Geometries()
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(members)))
		for _, m := range members {
			// A multi geometry's members are full WKB geometries with their own
			// byte order and type word, and without their own SRID.
			dst = m.appendEWKB(dst, false)
		}
		return dst

	case KindCollection:
		dst = binary.LittleEndian.AppendUint32(dst, uint32(len(g.sub)))
		for _, m := range g.sub {
			dst = m.appendEWKB(dst, false)
		}
		return dst
	}
	return dst
}

// appendRun writes a length-prefixed sequence of positions. The length is
// positions rather than ordinates, which is why it needs to know how wide one
// position is.
func appendRun(dst []byte, ordinates int, coords []float64) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(coords)/ordinates))
	return appendFloats(dst, coords)
}

func appendFloats(dst []byte, fs []float64) []byte {
	for _, f := range fs {
		dst = appendFloat(dst, f)
	}
	return dst
}

func appendFloat(dst []byte, f float64) []byte {
	return binary.LittleEndian.AppendUint64(dst, math.Float64bits(f))
}

// ErrShortEWKB reports an encoding that ended before the geometry it described
// did.
var ErrShortEWKB = errors.New("postgis: the geometry data ends in the middle of a value")

// DecodeEWKB reads a geometry from its EWKB encoding.
//
// It accepts either byte order and both the SRID-bearing and plain forms, since
// what arrives depends on which function produced it: ST_AsBinary drops the
// SRID and ST_AsEWKB keeps it. A geometry decoded from plain WKB has
// [UnknownSRID], which is the truth about it rather than a default.
func DecodeEWKB(b []byte) (Geometry, error) {
	d := &decoder{buf: b}
	g, err := d.geometry(UnknownSRID)
	if err != nil {
		return Geometry{}, err
	}
	if d.pos != len(d.buf) {
		return Geometry{}, fmt.Errorf("postgis: %d bytes of trailing data after the geometry", len(d.buf)-d.pos)
	}
	return g, nil
}

type decoder struct {
	buf    []byte
	pos    int
	endian binary.ByteOrder
	// depth bounds nesting, because a collection may contain collections and a
	// hostile encoding can nest them until the stack runs out.
	depth int
}

// maxDepth is well past anything PostGIS produces; a real collection of
// collections is one or two levels.
const maxDepth = 32

// geometry reads one geometry. inherited is the SRID of the enclosing geometry,
// which members take when they carry none of their own.
func (d *decoder) geometry(inherited int32) (Geometry, error) {
	if d.depth++; d.depth > maxDepth {
		return Geometry{}, fmt.Errorf("postgis: the geometry nests more than %d levels deep", maxDepth)
	}
	defer func() { d.depth-- }()

	order, err := d.byteOrder()
	if err != nil {
		return Geometry{}, err
	}
	// Each member states its own byte order, and PostGIS is entitled to mix
	// them; the decoder honours whatever the innermost header said.
	saved := d.endian
	d.endian = order
	defer func() { d.endian = saved }()

	t, err := d.uint32()
	if err != nil {
		return Geometry{}, err
	}
	g := Geometry{srid: inherited}
	switch {
	case t&ewkbZ != 0 && t&ewkbM != 0:
		g.dim = XYZM
	case t&ewkbZ != 0:
		g.dim = XYZ
	case t&ewkbM != 0:
		g.dim = XYM
	}
	if t&ewkbBBox != 0 {
		return Geometry{}, fmt.Errorf(
			"postgis: this geometry carries a cached bounding box, which this decoder does not read;" +
				" PostGIS does not send one, so the value did not come from a PostGIS connection")
	}
	if t&ewkbSRID != 0 {
		s, err := d.uint32()
		if err != nil {
			return Geometry{}, err
		}
		// A member's SRID has to be the enclosing geometry's. PostGIS writes the
		// SRID once, on the outermost geometry, and a collection whose members
		// disagree is not a value it can store — nor one this package could
		// re-encode, since the inner SRID has nowhere to go.
		if d.depth > 1 && int32(s) != inherited {
			return Geometry{}, fmt.Errorf(
				"postgis: a member is in SRID %d and the geometry containing it is in SRID %d;"+
					" every part of one geometry is in one coordinate system", int32(s), inherited)
		}
		g.srid = int32(s)
	}
	code := t & ewkbTypeMask
	if code < uint32(KindPoint) || code > uint32(KindCollection) {
		// 8 and above are the curved and surface types — CircularString,
		// CompoundCurve, PolyhedralSurface, TIN and the rest. PostGIS stores
		// them; this package does not model them, and saying so is better than
		// decoding the coordinates into a shape they are not.
		return Geometry{}, fmt.Errorf("postgis: geometry type %d is not one this package models"+
			" (it models Point through GeometryCollection; curves, surfaces and TINs are not included)", code)
	}
	g.kind = Kind(code)

	if err := d.body(&g); err != nil {
		return Geometry{}, err
	}
	return g, nil
}

func (d *decoder) body(g *Geometry) error {
	n := g.dim.ordinates()
	switch g.kind {
	case KindPoint:
		cs, err := d.floats(n)
		if err != nil {
			return err
		}
		// All-NaN is PostGIS's POINT EMPTY. A point with a NaN in only some
		// ordinates is not empty — it is a point with a NaN in it, which is a
		// value PostGIS will store and hand back.
		allNaN := true
		for _, f := range cs {
			if !math.IsNaN(f) {
				allNaN = false
				break
			}
		}
		if allNaN {
			g.empty = true
			return nil
		}
		g.coords = cs
		return nil

	case KindLineString:
		coords, err := d.run(n)
		if err != nil {
			return err
		}
		g.coords = coords
		if len(coords) == 0 {
			g.empty = true
		} else {
			g.parts = []part{{start: 0, end: len(coords)}}
		}
		return nil

	case KindPolygon:
		rings, err := d.count()
		if err != nil {
			return err
		}
		for range rings {
			coords, err := d.run(n)
			if err != nil {
				return err
			}
			g.parts = append(g.parts, part{start: len(g.coords), end: len(g.coords) + len(coords)})
			g.coords = append(g.coords, coords...)
		}
		g.empty = len(g.coords) == 0
		return nil

	case KindMultiPoint, KindMultiLineString, KindMultiPolygon:
		members, err := d.count()
		if err != nil {
			return err
		}
		want := memberKind(g.kind)
		for i := range members {
			m, err := d.geometry(g.srid)
			if err != nil {
				return err
			}
			if m.kind != want {
				return fmt.Errorf("postgis: %s member %d is a %s", g.kind, i, m.kind)
			}
			if m.dim != g.dim {
				return fmt.Errorf("postgis: %s member %d is %s and the geometry is %s",
					g.kind, i, dimName(m.dim), dimName(g.dim))
			}
			switch {
			case m.IsEmpty():
				// An empty member still occupies a position in the multi
				// geometry — MULTIPOINT(EMPTY, (1 2)) has two members — so it
				// gets an empty run rather than nothing, which is what keeps
				// the member count through a round trip.
				g.parts = append(g.parts, part{group: i, start: len(g.coords), end: len(g.coords)})
			case m.kind == KindPoint:
				// A point has no run of its own; its position is its run.
				g.parts = append(g.parts, part{group: i, start: len(g.coords), end: len(g.coords) + n})
			default:
				for _, p := range m.parts {
					g.parts = append(g.parts, part{
						group: i,
						start: len(g.coords) + p.start,
						end:   len(g.coords) + p.end,
					})
				}
			}
			g.coords = append(g.coords, m.coords...)
		}
		g.empty = members == 0
		return nil

	case KindCollection:
		members, err := d.count()
		if err != nil {
			return err
		}
		for i := range members {
			m, err := d.geometry(g.srid)
			if err != nil {
				return err
			}
			// Every member carries the collection's dimensionality. PostGIS
			// answers "Dimensions mismatch in lwcollection" and refuses to read
			// bytes that say otherwise, so accepting them here would produce a
			// Go value that re-encodes into something the server rejects — and
			// a collection whose members disagree has no single answer to what
			// its own dimensionality is.
			if m.dim != g.dim {
				return fmt.Errorf("postgis: collection member %d is %s and the collection is %s;"+
					" every part of one geometry has the same dimensionality",
					i, dimName(m.dim), dimName(g.dim))
			}
			g.sub = append(g.sub, m)
		}
		g.empty = members == 0
		return nil
	}
	return nil
}

// run reads a length-prefixed sequence of positions.
func (d *decoder) run(ordinates int) ([]float64, error) {
	n, err := d.count()
	if err != nil {
		return nil, err
	}
	return d.floats(n * ordinates)
}

// count reads a length and checks it against what the remaining bytes can
// possibly hold, so a claimed length never becomes an allocation on its own.
func (d *decoder) count() (int, error) {
	v, err := d.uint32()
	if err != nil {
		return 0, err
	}
	n := int(v)
	if n < 0 || n > maxPoints {
		return 0, fmt.Errorf("postgis: the geometry claims %d elements, which is more than this decoder accepts", v)
	}
	// The cheapest possible element is a byte order and a type word, so a claim
	// larger than the remaining bytes allow is a lie the decoder can catch
	// before allocating for it.
	if n > len(d.buf)-d.pos {
		return 0, fmt.Errorf("%w: it claims %d elements with %d bytes left",
			ErrShortEWKB, n, len(d.buf)-d.pos)
	}
	return n, nil
}

func (d *decoder) byteOrder() (binary.ByteOrder, error) {
	if d.pos >= len(d.buf) {
		return nil, ErrShortEWKB
	}
	b := d.buf[d.pos]
	d.pos++
	switch b {
	case 0:
		return binary.BigEndian, nil
	case 1:
		return binary.LittleEndian, nil
	default:
		return nil, fmt.Errorf("postgis: %#x is not a byte-order marker, so this is not geometry data", b)
	}
}

func (d *decoder) uint32() (uint32, error) {
	if d.pos+4 > len(d.buf) {
		return 0, ErrShortEWKB
	}
	v := d.endian.Uint32(d.buf[d.pos:])
	d.pos += 4
	return v, nil
}

func (d *decoder) floats(n int) ([]float64, error) {
	if n == 0 {
		return nil, nil
	}
	if d.pos+n*8 > len(d.buf) {
		return nil, ErrShortEWKB
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(d.endian.Uint64(d.buf[d.pos:]))
		d.pos += 8
	}
	return out, nil
}
