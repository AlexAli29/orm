package postgis_test

import (
	"context"
	"math"
	"testing"

	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5"
)

// Against a real PostGIS.
//
// Everything above this line is this package agreeing with itself. What matters
// is whether PostGIS agrees: whether the bytes sent are the geometry meant,
// whether the SRID survives, whether an empty geometry comes back empty rather
// than NULL. Those are the server's answers, and asking it is the only way to
// have them.
//
// The comparisons are against what PostGIS says about the value — ST_AsEWKT,
// ST_SRID, GeometryType, ST_NDims — rather than against bytes this package
// produced, because comparing an encoder with itself proves nothing.

// gisConn opens a throwaway database with PostGIS installed, skipping when
// there is no server or the extension is not available.
func gisConn(t *testing.T) *pgx.Conn {
	t.Helper()
	admin := testdb.AdminDSN(t)

	// Ask before creating anything: a PostgreSQL without PostGIS is a normal
	// machine to run this suite on, and it should skip rather than fail.
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatalf("parsing %s: %v", testdb.EnvAdminDSN, err)
	}
	probe, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	var available bool
	err = probe.QueryRow(t.Context(),
		`select exists (select 1 from pg_available_extensions where name = 'postgis')`).Scan(&available)
	_ = probe.Close(context.WithoutCancel(t.Context()))
	if err != nil {
		t.Fatalf("asking whether PostGIS is available: %v", err)
	}
	if !available {
		t.Skip("this PostgreSQL has no PostGIS extension available; skipping the spatial tests")
	}

	dsn := testdb.Create(t, "CREATE EXTENSION postgis;")
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the test DSN: %v", err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), connCfg)
	if err != nil {
		t.Fatalf("connecting to the spatial test database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })

	if err := postgis.Register(t.Context(), conn); err != nil {
		t.Fatalf("registering the PostGIS types: %v", err)
	}
	return conn
}

// gisVersion reports what the server is, so a failure names the version it
// happened on rather than leaving that to be worked out later.
func TestPostGIS_version(t *testing.T) {
	conn := gisConn(t)
	var lib, pg string
	if err := conn.QueryRow(t.Context(), `select postgis_lib_version(), version()`).Scan(&lib, &pg); err != nil {
		t.Fatalf("reading the versions: %v", err)
	}
	t.Logf("PostGIS %s on %s", lib, pg)
}

// The central claim of the type model: a geometry built in Go, sent as a
// parameter and read back is the same geometry, and PostGIS agrees about what
// it is.
func TestPostGIS_roundTrip(t *testing.T) {
	conn := gisConn(t)
	for _, c := range cases(t) {
		t.Run(c.name, func(t *testing.T) {
			var (
				back   postgis.Geometry
				ewkt   string
				srid   int32
				gtype  string
				ndims  int
				empty  bool
				coords int
			)
			// The geometry goes out as a parameter and every answer comes back
			// from the server's own functions.
			const q = `select $1::geometry, ST_AsEWKT($1::geometry), ST_SRID($1::geometry),
				GeometryType($1::geometry), ST_NDims($1::geometry),
				ST_IsEmpty($1::geometry), ST_NPoints($1::geometry)`
			err := conn.QueryRow(t.Context(), q, c.g).Scan(&back, &ewkt, &srid, &gtype, &ndims, &empty, &coords)
			if err != nil {
				t.Fatalf("sending %s: %v", c.g, err)
			}
			t.Logf("PostGIS read it as %s", ewkt)

			if !back.Equal(c.g) {
				t.Errorf("the value changed crossing the wire:\n sent %s %v\n back %s %v",
					c.g, c.g.Coords(), back, back.Coords())
			}
			if srid != c.g.SRID() {
				t.Errorf("PostGIS says SRID %d; the value has %d", srid, c.g.SRID())
			}
			if empty != c.g.IsEmpty() {
				t.Errorf("PostGIS says empty=%v; the value says %v", empty, c.g.IsEmpty())
			}
			if coords != c.g.NumPoints() {
				t.Errorf("PostGIS counts %d points; the value counts %d", coords, c.g.NumPoints())
			}
			// GeometryType appends M for an XYM geometry and nothing for XYZ
			// or XYZM — an inconsistency in PostGIS itself rather than a
			// choice here, and the test records what the server actually does.
			want := upper(c.g.Kind().String())
			if c.g.Dim() == postgis.XYM {
				want += "M"
			}
			if gtype != want {
				t.Errorf("PostGIS says the shape is %s; the value says %s", gtype, want)
			}
			// ST_NDims counts the ordinates PostGIS thinks are present.
			wantDims := 2
			if c.g.Dim().HasZ() {
				wantDims++
			}
			if c.g.Dim().HasM() {
				wantDims++
			}
			if ndims != wantDims {
				t.Errorf("PostGIS counts %d dimensions; the value is %v (%d)", ndims, c.g.Dim(), wantDims)
			}
		})
	}
}

func upper(s string) string {
	out := []byte(s)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - 32
		}
	}
	return string(out)
}

// The other direction: a geometry PostGIS built, read into Go, must equal the
// one this package builds from the same description. This is what catches a
// decoder that is self-consistent and wrong.
func TestPostGIS_serverBuilt(t *testing.T) {
	conn := gisConn(t)
	// The EWKT here is a constant in this test, not a value from anywhere: it
	// is the description both sides are built from.
	tests := []struct {
		ewkt string
		want postgis.Geometry
	}{
		{"SRID=4326;POINT(-73.985 40.748)", postgis.NewPoint(4326, -73.985, 40.748)},
		{"SRID=4326;POINTZ(1 2 3)", postgis.NewPointZ(4326, 1, 2, 3)},
		{"SRID=4326;POINTM(1 2 3)", postgis.NewPointM(4326, 1, 2, 3)},
		{"SRID=4326;POINTZM(1 2 3 4)", postgis.NewPointZM(4326, 1, 2, 3, 4)},
		{"SRID=4326;POINT EMPTY", postgis.EmptyPoint(4326)},
		{"SRID=3857;LINESTRING(0 0,1 1,2 0)", postgis.NewLineString(3857, postgis.XY,
			postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 1, Y: 1}, postgis.Coord{X: 2, Y: 0})},
		{"SRID=4326;POLYGON((0 0,10 0,10 10,0 10,0 0))",
			postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10))},
		{"SRID=4326;POLYGON((0 0,10 0,10 10,0 10,0 0),(3 3,7 3,7 7,3 7,3 3))",
			postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10), square(3, 3, 4))},
		{"SRID=4326;MULTIPOINT((1 2),(3 4))", postgis.NewMultiPoint(4326, postgis.XY,
			postgis.Coord{X: 1, Y: 2}, postgis.Coord{X: 3, Y: 4})},
		{"SRID=4326;MULTILINESTRING((0 0,1 1),(5 5,6 6,7 5))",
			postgis.NewMultiLineString(4326, postgis.XY,
				[]postgis.Coord{{X: 0, Y: 0}, {X: 1, Y: 1}},
				[]postgis.Coord{{X: 5, Y: 5}, {X: 6, Y: 6}, {X: 7, Y: 5}})},
		{"SRID=4326;MULTIPOLYGON(((0 0,1 0,1 1,0 1,0 0)),((10 10,12 10,12 12,10 12,10 10),(10.5 10.5,11 10.5,11 11,10.5 11,10.5 10.5)))",
			postgis.NewMultiPolygon(4326, postgis.XY,
				[][]postgis.Coord{square(0, 0, 1)},
				[][]postgis.Coord{square(10, 10, 2), square(10.5, 10.5, 0.5)})},
		{"SRID=4326;GEOMETRYCOLLECTION(POINT(1 2),LINESTRING(0 0,3 4))", mustCollection(t, 4326,
			postgis.NewPoint(4326, 1, 2),
			postgis.NewLineString(4326, postgis.XY, postgis.Coord{X: 0, Y: 0}, postgis.Coord{X: 3, Y: 4}))},
	}
	for _, tc := range tests {
		t.Run(tc.ewkt, func(t *testing.T) {
			var got postgis.Geometry
			if err := conn.QueryRow(t.Context(), `select ST_GeomFromEWKT($1)`, tc.ewkt).Scan(&got); err != nil {
				t.Fatalf("reading %s: %v", tc.ewkt, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("PostGIS built %s %v; this package builds %s %v",
					got, got.Coords(), tc.want, tc.want.Coords())
			}
			if got.Dim() != tc.want.Dim() {
				t.Errorf("dimensionality: PostGIS %v, this package %v", got.Dim(), tc.want.Dim())
			}
			if got.IsEmpty() != tc.want.IsEmpty() {
				t.Errorf("emptiness: PostGIS %v, this package %v", got.IsEmpty(), tc.want.IsEmpty())
			}
		})
	}
}

func mustCollection(t *testing.T, srid int32, members ...postgis.Geometry) postgis.Geometry {
	t.Helper()
	g, err := postgis.NewCollection(srid, members...)
	if err != nil {
		t.Fatalf("building a collection: %v", err)
	}
	return g
}

// A geometry stored in a column and read back must be unchanged, which is a
// different claim from surviving a cast in a select list: the column has a type
// modifier PostGIS enforces.
func TestPostGIS_storedInAColumn(t *testing.T) {
	conn := gisConn(t)
	const ddl = `create table places (
		id int primary key,
		location geometry(Point, 4326) not null,
		area geometry(Polygon, 4326),
		track geometry(LineStringZ, 4326)
	)`
	if _, err := conn.Exec(t.Context(), ddl); err != nil {
		t.Fatalf("creating the table: %v", err)
	}

	loc := postgis.NewPoint(4326, -73.985, 40.748)
	area := postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10))
	track := postgis.NewLineString(4326, postgis.XYZ,
		postgis.Coord{X: 0, Y: 0, Z: 5}, postgis.Coord{X: 1, Y: 1, Z: 6})

	if _, err := conn.Exec(t.Context(),
		`insert into places (id, location, area, track) values (1, $1, $2, $3), (2, $1, null, null)`,
		loc, area, track); err != nil {
		t.Fatalf("inserting: %v", err)
	}

	var gotLoc postgis.Geometry
	var gotArea, gotTrack *postgis.Geometry
	err := conn.QueryRow(t.Context(),
		`select location, area, track from places where id = 1`).Scan(&gotLoc, &gotArea, &gotTrack)
	if err != nil {
		t.Fatalf("reading row 1: %v", err)
	}
	if !gotLoc.Equal(loc) {
		t.Errorf("location came back as %s %v", gotLoc, gotLoc.Coords())
	}
	if gotArea == nil || !gotArea.Equal(area) {
		t.Errorf("area came back as %v", gotArea)
	}
	if gotTrack == nil || !gotTrack.Equal(track) {
		t.Errorf("track came back as %v", gotTrack)
	}

	// The row with NULLs. A nullable geometry reads into a pointer, and NULL is
	// a nil pointer rather than an empty geometry — the distinction this whole
	// design turns on.
	var nullArea *postgis.Geometry
	if err := conn.QueryRow(t.Context(), `select area from places where id = 2`).Scan(&nullArea); err != nil {
		t.Fatalf("reading the NULL geometry: %v", err)
	}
	if nullArea != nil {
		t.Errorf("a NULL geometry read back as %s", nullArea)
	}
}

// NULL and EMPTY have to stay apart through the server as well as in Go.
func TestPostGIS_nullIsNotEmpty(t *testing.T) {
	conn := gisConn(t)
	if _, err := conn.Exec(t.Context(),
		`create table shapes (id int primary key, g geometry(Polygon, 4326))`); err != nil {
		t.Fatalf("creating the table: %v", err)
	}
	empty := postgis.NewPolygon(4326, postgis.XY)
	if _, err := conn.Exec(t.Context(),
		`insert into shapes values (1, null), (2, $1)`, empty); err != nil {
		t.Fatalf("inserting: %v", err)
	}

	rows, err := conn.Query(t.Context(), `select id, g, g is null, ST_IsEmpty(g) from shapes order by id`)
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	defer rows.Close()

	type row struct {
		id      int
		g       *postgis.Geometry
		isNull  bool
		isEmpty *bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.g, &r.isNull, &r.isEmpty); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading rows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d rows, want 2", len(got))
	}

	if got[0].g != nil {
		t.Errorf("the NULL row read back a geometry: %s", got[0].g)
	}
	if !got[0].isNull {
		t.Error("PostgreSQL does not think the NULL row is NULL")
	}
	if got[0].isEmpty != nil {
		t.Errorf("ST_IsEmpty(NULL) came back as %v, and it is NULL", *got[0].isEmpty)
	}

	if got[1].g == nil {
		t.Fatal("the empty polygon read back as NULL, which is the confusion this design exists to prevent")
	}
	if got[1].isNull {
		t.Error("PostgreSQL thinks the empty polygon is NULL")
	}
	if got[1].isEmpty == nil || !*got[1].isEmpty {
		t.Errorf("PostGIS does not think the empty polygon is empty: %v", got[1].isEmpty)
	}
	if !got[1].g.IsEmpty() {
		t.Error("the empty polygon read back as non-empty")
	}
}

// A geography column is not a geometry column, and reading one into the other
// silently would mean measuring in degrees where metres were meant.
func TestPostGIS_geographyIsDistinct(t *testing.T) {
	conn := gisConn(t)
	if _, err := conn.Exec(t.Context(),
		`create table sites (id int primary key, where_ geography(Point, 4326) not null)`); err != nil {
		t.Fatalf("creating the table: %v", err)
	}
	site := postgis.GeographyPoint(-73.985, 40.748)
	if _, err := conn.Exec(t.Context(), `insert into sites values (1, $1)`, site); err != nil {
		t.Fatalf("inserting a geography: %v", err)
	}

	var back postgis.Geography
	if err := conn.QueryRow(t.Context(), `select where_ from sites`).Scan(&back); err != nil {
		t.Fatalf("reading the geography: %v", err)
	}
	if !back.Equal(site) {
		t.Errorf("the geography came back as %s %v", back, back.Coords())
	}

	// Reading it into a Geometry has to be refused rather than quietly done.
	var wrong postgis.Geometry
	if err := conn.QueryRow(t.Context(), `select where_ from sites`).Scan(&wrong); err == nil {
		t.Error("a geography column was read into a Geometry without complaint")
	}
	// And a geometry column into a Geography.
	var wrong2 postgis.Geography
	if err := conn.QueryRow(t.Context(), `select ST_MakePoint(1, 2)`).Scan(&wrong2); err == nil {
		t.Error("a geometry was read into a Geography without complaint")
	}
}

// The measurements are the reason the two types are kept apart, so the test
// asserts the difference rather than only the type.
func TestPostGIS_geographyMeasuresInMetres(t *testing.T) {
	conn := gisConn(t)
	a := postgis.GeographyPoint(-73.985, 40.748) // New York
	b := postgis.GeographyPoint(-0.1276, 51.5072)

	// The casts are not decoration. ST_Distance is overloaded, and a bare
	// parameter leaves PostgreSQL to resolve the overload with no type to go on
	// — it picks the deprecated text form and fails on the binary it is handed.
	// The expression layer never emits a bare parameter for this reason.
	var metres float64
	if err := conn.QueryRow(t.Context(),
		`select ST_Distance($1::geography, $2::geography)`, a, b).Scan(&metres); err != nil {
		t.Fatalf("measuring on the spheroid: %v", err)
	}
	// London is about 5570 km from New York. The assertion is loose because the
	// point of it is the unit, not the spheroid model.
	if metres < 5_500_000 || metres > 5_600_000 {
		t.Errorf("ST_Distance over geography returned %v, which is not metres between New York and London", metres)
	}

	var degrees float64
	err := conn.QueryRow(t.Context(), `select ST_Distance($1::geometry, $2::geometry)`,
		a.AsGeometry(), b.AsGeometry()).Scan(&degrees)
	if err != nil {
		t.Fatalf("measuring in the plane: %v", err)
	}
	if degrees > 100 {
		t.Errorf("ST_Distance over geometry returned %v, which is not degrees", degrees)
	}
}

// A SRID that is not 4326 has to survive unchanged, because the failure mode of
// getting this wrong is coordinates that look plausible and are somewhere else.
func TestPostGIS_sridPreserved(t *testing.T) {
	conn := gisConn(t)
	for _, srid := range []int32{postgis.UnknownSRID, 4326, 3857, 2263, 27700} {
		g := postgis.NewPoint(srid, 100000, 200000)
		var back postgis.Geometry
		var reported int32
		err := conn.QueryRow(t.Context(),
			`select $1::geometry, ST_SRID($1::geometry)`, g).Scan(&back, &reported)
		if err != nil {
			t.Fatalf("SRID %d: %v", srid, err)
		}
		if reported != srid {
			t.Errorf("sent SRID %d and PostGIS reports %d", srid, reported)
		}
		if back.SRID() != srid {
			t.Errorf("sent SRID %d and read back %d", srid, back.SRID())
		}
	}
}

// The values are bind parameters. A coordinate that reached the server as text
// would be a coordinate that could carry syntax, so the test sends the shapes
// that would break a formatter and checks the value survives intact.
func TestPostGIS_coordinatesAreParameters(t *testing.T) {
	conn := gisConn(t)
	hostile := []float64{
		math.Inf(1), math.Inf(-1),
		1e308, -1e308, 5e-324,
		0.1 + 0.2, // not representable, and not equal to 0.3
	}
	for _, v := range hostile {
		g := postgis.NewPoint(4326, v, 1)
		var back postgis.Geometry
		if err := conn.QueryRow(t.Context(), `select $1::geometry`, g).Scan(&back); err != nil {
			t.Fatalf("sending X=%v: %v", v, err)
		}
		got := back.Coords()
		if len(got) != 1 || got[0].X != v {
			t.Errorf("X=%v came back as %v", v, got)
		}
	}
}

// Text is one of the two formats a connection may use, and a connection using
// it must move geometries just as correctly. pgx's simple protocol is where
// this happens in practice.
func TestPostGIS_simpleProtocol(t *testing.T) {
	conn := gisConn(t)

	g := postgis.NewPolygon(4326, postgis.XY, square(0, 0, 10), square(3, 3, 4))
	var back postgis.Geometry
	err := conn.QueryRow(t.Context(), `select $1::geometry`, pgx.QueryExecModeSimpleProtocol, g).Scan(&back)
	if err != nil {
		t.Fatalf("sending over the simple protocol: %v", err)
	}
	if !back.Equal(g) {
		t.Errorf("the simple protocol changed the value: %s %v", back, back.Coords())
	}
}

// Reading a spatial column requires the types to be registered. A program that
// forgets gets a clear failure rather than a scan into something unexpected.
func TestPostGIS_notInstalled(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	err = postgis.Register(t.Context(), conn)
	if err == nil {
		t.Fatal("registering succeeded against a database with no PostGIS")
	}
	var notInstalled *postgis.NotInstalledError
	if !errorsAs(err, &notInstalled) {
		t.Errorf("the error is not a NotInstalledError: %v", err)
	}

	found, err := postgis.RegisterIfPresent(t.Context(), conn)
	if err != nil {
		t.Errorf("RegisterIfPresent returned an error for a database with no PostGIS: %v", err)
	}
	if found {
		t.Error("RegisterIfPresent claims PostGIS is installed")
	}
}

// Kept small and local so the test file does not depend on the errors package
// for one call.
func errorsAs(err error, target **postgis.NotInstalledError) bool {
	for err != nil {
		if e, ok := err.(*postgis.NotInstalledError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
