package postgis

import (
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"
)

// Bounding boxes.
//
// ST_Extent aggregates a set of geometries into the smallest box containing
// them all, and returns a box2d rather than a geometry. It is a different type
// with a different text form and no binary send function, so it gets its own
// small codec — and it is worth having as a type rather than a string, because
// the four numbers are the answer and parsing them at every call site is how
// they get parsed wrong.
//
// A box carries no SRID. PostGIS's box2d has no room for one, which is why
// [Box2D.Geometry] asks for the SRID rather than inventing it: the numbers came
// from geometries in some coordinate system, and only the caller knows which.

// Box2D is a rectangle in the plane, as PostGIS's box2d.
type Box2D struct {
	// The corners of the rectangle, in the reference system of whatever
	// produced it. Min is the lower-left, Max the upper-right.
	MinX, MinY float64
	MaxX, MaxY float64
	// Valid distinguishes a box from the absence of one. ST_Extent over no rows
	// is NULL, and a zero-size box at the origin is a different answer.
	Valid bool
}

// Box3D is a box in space, as PostGIS's box3d.
type Box3D struct {
	// The corners of the box, in the reference system of whatever produced it.
	MinX, MinY, MinZ float64
	MaxX, MaxY, MaxZ float64
	// Valid distinguishes a box from the absence of one: ST_3DExtent over no
	// rows is NULL, and a zero-size box at the origin is a different answer.
	Valid bool
}

// Width is the box's extent along X.
func (b Box2D) Width() float64 { return b.MaxX - b.MinX }

// Height is the box's extent along Y.
func (b Box2D) Height() float64 { return b.MaxY - b.MinY }

// Geometry returns the box as a closed polygon in the given SRID.
//
// The SRID is an argument because a box does not carry one — see the note at
// the top of this file. A degenerate box, one with no width or no height,
// still produces the five-vertex ring PostGIS's own ST_Envelope would for a
// line or a point, rather than a shape that is not a polygon.
func (b Box2D) Geometry(srid int32) Geometry {
	if !b.Valid {
		return Geometry{kind: KindPolygon, dim: XY, srid: srid, empty: true}
	}
	ring := []Coord{
		{X: b.MinX, Y: b.MinY},
		{X: b.MaxX, Y: b.MinY},
		{X: b.MaxX, Y: b.MaxY},
		{X: b.MinX, Y: b.MaxY},
		{X: b.MinX, Y: b.MinY},
	}
	return NewPolygon(srid, XY, ring)
}

// String renders the box the way PostGIS does.
func (b Box2D) String() string {
	if !b.Valid {
		return "NULL"
	}
	return fmt.Sprintf("BOX(%s %s,%s %s)", f(b.MinX), f(b.MinY), f(b.MaxX), f(b.MaxY))
}

// String renders the box the way PostGIS does.
func (b Box3D) String() string {
	if !b.Valid {
		return "NULL"
	}
	return fmt.Sprintf("BOX3D(%s %s %s,%s %s %s)",
		f(b.MinX), f(b.MinY), f(b.MinZ), f(b.MaxX), f(b.MaxY), f(b.MaxZ))
}

func f(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }

// boxCodec reads box2d and box3d, which PostgreSQL sends as text.
//
// PostGIS gives these types no binary send function, so the text form is the
// only form — the codec declares that rather than asking for binary and
// getting a protocol error at query time.
type boxCodec struct{ dims int }

func (c boxCodec) FormatSupported(format int16) bool { return format == pgtype.TextFormatCode }
func (c boxCodec) PreferredFormat() int16            { return pgtype.TextFormatCode }

func (c boxCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	if format != pgtype.TextFormatCode {
		return nil
	}
	switch value.(type) {
	case Box2D, *Box2D, Box3D, *Box3D:
		return encodeBoxText{}
	}
	return nil
}

type encodeBoxText struct{}

func (encodeBoxText) Encode(value any, buf []byte) ([]byte, error) {
	switch v := value.(type) {
	case Box2D:
		if !v.Valid {
			return nil, nil
		}
		return append(buf, v.String()...), nil
	case *Box2D:
		if v == nil || !v.Valid {
			return nil, nil
		}
		return append(buf, v.String()...), nil
	case Box3D:
		if !v.Valid {
			return nil, nil
		}
		return append(buf, v.String()...), nil
	case *Box3D:
		if v == nil || !v.Valid {
			return nil, nil
		}
		return append(buf, v.String()...), nil
	}
	return nil, fmt.Errorf("postgis: cannot encode %T as a box", value)
}

func (c boxCodec) PlanScan(m *pgtype.Map, oid uint32, format int16, target any) pgtype.ScanPlan {
	switch target.(type) {
	case *Box2D, *Box3D, *string:
		return scanBox{dims: c.dims}
	}
	return nil
}

type scanBox struct{ dims int }

func (s scanBox) Scan(src []byte, target any) error {
	if src == nil {
		// A box is its own null: ST_Extent over no rows is NULL, and that is a
		// routine answer rather than an error, so the zero value carries it.
		switch t := target.(type) {
		case *Box2D:
			*t = Box2D{}
			return nil
		case *Box3D:
			*t = Box3D{}
			return nil
		case *string:
			*t = ""
			return nil
		}
	}
	switch t := target.(type) {
	case *string:
		*t = string(src)
		return nil
	case *Box2D:
		n, err := parseBox(string(src), 2)
		if err != nil {
			return err
		}
		*t = Box2D{MinX: n[0], MinY: n[1], MaxX: n[2], MaxY: n[3], Valid: true}
		return nil
	case *Box3D:
		// A box3d over two-dimensional geometries still prints three ordinates,
		// with zero for Z, so the parser is told how many to expect rather than
		// counting them.
		n, err := parseBox(string(src), 3)
		if err != nil {
			return err
		}
		*t = Box3D{MinX: n[0], MinY: n[1], MinZ: n[2], MaxX: n[3], MaxY: n[4], MaxZ: n[5], Valid: true}
		return nil
	}
	return fmt.Errorf("postgis: cannot scan a box into %T", target)
}

// parseBox reads BOX(minx miny,maxx maxy) and its three-dimensional form.
func parseBox(s string, dims int) ([]float64, error) {
	body := s
	if i := strings.IndexByte(body, '('); i >= 0 && strings.HasSuffix(body, ")") {
		body = body[i+1 : len(body)-1]
	} else {
		return nil, fmt.Errorf("postgis: %q is not a box", s)
	}
	corners := strings.Split(body, ",")
	if len(corners) != 2 {
		return nil, fmt.Errorf("postgis: %q is not a box: it has %d corners", s, len(corners))
	}
	out := make([]float64, 0, dims*2)
	for _, corner := range corners {
		fields := strings.Fields(corner)
		if len(fields) != dims {
			return nil, fmt.Errorf("postgis: %q is not a %d-dimensional box: a corner has %d ordinates",
				s, dims, len(fields))
		}
		for _, field := range fields {
			v, err := strconv.ParseFloat(field, 64)
			if err != nil {
				return nil, fmt.Errorf("postgis: %q is not a box: %w", s, err)
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func (c boxCodec) DecodeDatabaseSQLValue(m *pgtype.Map, oid uint32, format int16, src []byte) (driver.Value, error) {
	if src == nil {
		return nil, nil
	}
	return string(src), nil
}

func (c boxCodec) DecodeValue(m *pgtype.Map, oid uint32, format int16, src []byte) (any, error) {
	if src == nil {
		return nil, nil
	}
	if c.dims == 3 {
		var b Box3D
		if err := (scanBox{dims: 3}).Scan(src, &b); err != nil {
			return nil, err
		}
		return b, nil
	}
	var b Box2D
	if err := (scanBox{dims: 2}).Scan(src, &b); err != nil {
		return nil, err
	}
	return b, nil
}
