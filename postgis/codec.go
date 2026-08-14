package postgis

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Teaching a connection about PostGIS.
//
// geometry and geography are extension types, so their OIDs are assigned when
// somebody ran CREATE EXTENSION and differ from database to database. A driver
// cannot know them in advance; it has to ask. That is why this is a connection
// hook rather than a package-level table, and why it is the same shape as the
// hook the generator emits for enums.
//
// pgx's own LoadType cannot do it. It reads pg_type.typtype, sees 'b', assumes
// a base type is an array, and fails looking for an element type geometry does
// not have. So the OIDs are looked up here and paired with a codec that speaks
// EWKB — binary in both directions, with hex-EWKB accepted for the text format
// because that is what PostGIS emits when a geometry is cast to text.

// Register teaches a connection the PostGIS types.
//
// Wire it into a pool so every connection is taught once:
//
//	cfg, err := pgxpool.ParseConfig(dsn)
//	if err != nil {
//	    return err
//	}
//	cfg.AfterConnect = postgis.Register
//	pool, err := pgxpool.NewWithConfig(ctx, cfg)
//
// A project that also has generated enum registration composes the two:
//
//	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
//	    if err := domain.RegisterTypes(ctx, c); err != nil {
//	        return err
//	    }
//	    return postgis.Register(ctx, c)
//	}
//
// It reports an error when PostGIS is not installed in the database, because a
// program that registers spatial types and then finds none is a program whose
// next query would fail with something much less specific.
func Register(ctx context.Context, conn *pgx.Conn) error {
	oids, err := lookupOIDs(ctx, conn)
	if err != nil {
		return err
	}
	registerOIDs(conn.TypeMap(), oids)
	return nil
}

// RegisterIfPresent is [Register] for a program that runs against databases
// with and without PostGIS.
//
// It reports whether the extension was found. When it was not, the connection
// is left exactly as it was and no error is returned — which is the right shape
// for an application whose spatial features are optional, and the wrong shape
// for one whose queries are all spatial.
func RegisterIfPresent(ctx context.Context, conn *pgx.Conn) (bool, error) {
	oids, err := lookupOIDs(ctx, conn)
	if err != nil {
		if _, missing := err.(*NotInstalledError); missing {
			return false, nil
		}
		return false, err
	}
	registerOIDs(conn.TypeMap(), oids)
	return true, nil
}

// NotInstalledError reports a database with no PostGIS extension.
type NotInstalledError struct{}

// Error explains that the database has no PostGIS, and how to add it.
func (*NotInstalledError) Error() string {
	return "postgis: this database has no geometry type, so the PostGIS extension is not installed" +
		" (CREATE EXTENSION postgis, or let the migration that adds it run first)"
}

type typeOIDs struct {
	geometry, geography       uint32
	geometryArr, geographyArr uint32
	box2d, box3d              uint32
}

func lookupOIDs(ctx context.Context, conn *pgx.Conn) (typeOIDs, error) {
	// to_regtype returns NULL for a type that does not exist, where a cast to
	// regtype raises. NULL is the answer wanted here: "PostGIS is not installed"
	// is a fact to report, not an error to propagate from three layers down.
	const q = `select
		to_regtype('geometry')::oid, to_regtype('geography')::oid,
		to_regtype('geometry[]')::oid, to_regtype('geography[]')::oid,
		to_regtype('box2d')::oid, to_regtype('box3d')::oid`
	var o typeOIDs
	var geom, geog, geomArr, geogArr, b2, b3 *uint32
	if err := conn.QueryRow(ctx, q).Scan(&geom, &geog, &geomArr, &geogArr, &b2, &b3); err != nil {
		return o, fmt.Errorf("postgis: looking up the PostGIS type OIDs: %w", err)
	}
	if geom == nil || geog == nil {
		return o, &NotInstalledError{}
	}
	o.geometry, o.geography = *geom, *geog
	o.geometryArr, o.geographyArr = deref(geomArr), deref(geogArr)
	o.box2d, o.box3d = deref(b2), deref(b3)
	return o, nil
}

func deref(p *uint32) uint32 {
	if p == nil {
		return 0
	}
	return *p
}

func registerOIDs(m *pgtype.Map, o typeOIDs) {
	geom := &pgtype.Type{Name: "geometry", OID: o.geometry, Codec: spatialCodec{geography: false}}
	geog := &pgtype.Type{Name: "geography", OID: o.geography, Codec: spatialCodec{geography: true}}
	m.RegisterType(geom)
	m.RegisterType(geog)
	if o.geometryArr != 0 {
		m.RegisterType(&pgtype.Type{Name: "_geometry", OID: o.geometryArr,
			Codec: &pgtype.ArrayCodec{ElementType: geom}})
	}
	if o.geographyArr != 0 {
		m.RegisterType(&pgtype.Type{Name: "_geography", OID: o.geographyArr,
			Codec: &pgtype.ArrayCodec{ElementType: geog}})
	}
	// box2d and box3d come back from ST_Extent and ST_3DExtent. They have no
	// binary send function in PostGIS, so they are read as text and parsed.
	if o.box2d != 0 {
		m.RegisterType(&pgtype.Type{Name: "box2d", OID: o.box2d, Codec: boxCodec{dims: 2}})
	}
	if o.box3d != 0 {
		m.RegisterType(&pgtype.Type{Name: "box3d", OID: o.box3d, Codec: boxCodec{dims: 3}})
	}
	// Registering by name as well means a value produced by an expression whose
	// type pgx resolves by name — a CTE column, a composite field — finds the
	// same codec.
	m.RegisterDefaultPgType(Geometry{}, "geometry")
	m.RegisterDefaultPgType(&Geometry{}, "geometry")
	m.RegisterDefaultPgType([]Geometry(nil), "_geometry")
	m.RegisterDefaultPgType(Geography{}, "geography")
	m.RegisterDefaultPgType(&Geography{}, "geography")
	m.RegisterDefaultPgType([]Geography(nil), "_geography")
	m.RegisterDefaultPgType(Box2D{}, "box2d")
	m.RegisterDefaultPgType(Box3D{}, "box3d")
}

// spatialCodec moves geometry and geography values in EWKB.
//
// One codec serves both because the wire form is identical — a geography is a
// geometry the server agreed to measure differently. What differs is which Go
// type it decodes into, so that a geography column cannot be scanned into a
// Geometry without saying so.
type spatialCodec struct {
	geography bool
}

func (c spatialCodec) FormatSupported(format int16) bool {
	return format == pgtype.TextFormatCode || format == pgtype.BinaryFormatCode
}

// PreferredFormat is binary, which is what makes a polygon one copy of the
// coordinates rather than a hex string twice their size that then has to be
// parsed back into floats.
func (c spatialCodec) PreferredFormat() int16 { return pgtype.BinaryFormatCode }

func (c spatialCodec) PlanEncode(m *pgtype.Map, oid uint32, format int16, value any) pgtype.EncodePlan {
	switch value.(type) {
	case Geometry, *Geometry, Geography, *Geography:
	default:
		return nil
	}
	switch format {
	case pgtype.BinaryFormatCode:
		return encodeBinary{}
	case pgtype.TextFormatCode:
		return encodeHex{}
	}
	return nil
}

type encodeBinary struct{}

func (encodeBinary) Encode(value any, buf []byte) ([]byte, error) {
	g, ok := geometryOf(value)
	if !ok {
		return nil, nil // NULL
	}
	return g.AppendEWKB(buf), nil
}

type encodeHex struct{}

func (encodeHex) Encode(value any, buf []byte) ([]byte, error) {
	g, ok := geometryOf(value)
	if !ok {
		return nil, nil
	}
	b := g.EWKB()
	out := make([]byte, hex.EncodedLen(len(b)))
	hex.Encode(out, b)
	return append(buf, out...), nil
}

// geometryOf unwraps whichever spatial value was passed. A nil pointer is SQL
// NULL, which is how a *Geometry field holding no geometry is written.
func geometryOf(value any) (Geometry, bool) {
	switch v := value.(type) {
	case Geometry:
		return v, true
	case *Geometry:
		if v == nil {
			return Geometry{}, false
		}
		return *v, true
	case Geography:
		return v.geom, true
	case *Geography:
		if v == nil {
			return Geometry{}, false
		}
		return v.geom, true
	}
	return Geometry{}, false
}

func (c spatialCodec) PlanScan(m *pgtype.Map, oid uint32, format int16, target any) pgtype.ScanPlan {
	switch target.(type) {
	case *Geometry, **Geometry, *Geography, **Geography, *[]byte, **[]byte, *string:
		return scanSpatial{format: format, geography: c.geography}
	}
	return nil
}

type scanSpatial struct {
	format    int16
	geography bool
}

func (s scanSpatial) Scan(src []byte, target any) error {
	if src == nil {
		return s.scanNull(target)
	}
	raw, err := s.bytes(src)
	if err != nil {
		return err
	}
	switch t := target.(type) {
	case *[]byte:
		// The raw EWKB, copied: src is only valid during this call.
		*t = append([]byte(nil), raw...)
		return nil
	case **[]byte:
		b := append([]byte(nil), raw...)
		*t = &b
		return nil
	case *string:
		*t = hex.EncodeToString(raw)
		return nil
	}
	g, err := DecodeEWKB(raw)
	if err != nil {
		return err
	}
	switch t := target.(type) {
	case *Geometry:
		if s.geography {
			return errWrongSpatialType("geography", "Geometry", "Geography")
		}
		*t = g
	case **Geometry:
		if s.geography {
			return errWrongSpatialType("geography", "*Geometry", "*Geography")
		}
		*t = &g
	case *Geography:
		if !s.geography {
			return errWrongSpatialType("geometry", "Geography", "Geometry")
		}
		*t = Geography{geom: g}
	case **Geography:
		if !s.geography {
			return errWrongSpatialType("geometry", "*Geography", "*Geometry")
		}
		v := Geography{geom: g}
		*t = &v
	}
	return nil
}

// errWrongSpatialType refuses to decode a geography into a Geometry or the
// other way round.
//
// The bytes would decode: the wire form is the same. What would not survive is
// the meaning — a Geometry read out of a geography column measures in degrees
// everywhere it is used afterwards, and nothing downstream would notice.
func errWrongSpatialType(column, got, want string) error {
	return fmt.Errorf("postgis: this is a %s column and it was read into a %s;"+
		" read it into a %s, or cast the column in the query if the other meaning is what you want",
		column, got, want)
}

func (s scanSpatial) scanNull(target any) error {
	switch t := target.(type) {
	case **Geometry:
		*t = nil
		return nil
	case **Geography:
		*t = nil
		return nil
	case **[]byte:
		*t = nil
		return nil
	case *[]byte:
		*t = nil
		return nil
	}
	return fmt.Errorf("postgis: this row has NULL where a %T was expected;"+
		" read a nullable spatial column into a pointer, which is what tells NULL from an empty geometry", target)
}

// bytes turns the wire representation into EWKB. The text format PostGIS uses
// for geometry is hex-EWKB, so the only difference is the decoding.
func (s scanSpatial) bytes(src []byte) ([]byte, error) {
	if s.format == pgtype.BinaryFormatCode {
		return src, nil
	}
	out := make([]byte, hex.DecodedLen(len(src)))
	if _, err := hex.Decode(out, src); err != nil {
		return nil, fmt.Errorf("postgis: this value is not hex-encoded geometry: %w", err)
	}
	return out, nil
}

func (c spatialCodec) DecodeDatabaseSQLValue(m *pgtype.Map, oid uint32, format int16, src []byte) (driver.Value, error) {
	if src == nil {
		return nil, nil
	}
	// database/sql has no spatial value, so it gets what psql would show: the
	// hex-EWKB text PostGIS renders a geometry as.
	raw, err := scanSpatial{format: format}.bytes(src)
	if err != nil {
		return nil, err
	}
	return hex.EncodeToString(raw), nil
}

func (c spatialCodec) DecodeValue(m *pgtype.Map, oid uint32, format int16, src []byte) (any, error) {
	if src == nil {
		return nil, nil
	}
	raw, err := scanSpatial{format: format}.bytes(src)
	if err != nil {
		return nil, err
	}
	g, err := DecodeEWKB(raw)
	if err != nil {
		return nil, err
	}
	if c.geography {
		return Geography{geom: g}, nil
	}
	return g, nil
}
