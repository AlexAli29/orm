package gisdemo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The adversarial audit of the generated spatial surface.
//
// Everything here goes through the committed generated descriptors, because
// that is what a project actually uses. A property that holds for a
// hand-written descriptor and not for a generated one is a property that does
// not hold.

// The generated metadata has to be what the database says, not what the tag
// said. A descriptor claiming 4326 over a 3857 column would make every
// build-time SRID check agree with itself and disagree with the server.
func TestAudit_generatedMetadataMatchesTheDatabase(t *testing.T) {
	db, pool := openBoth(t)
	_ = db

	rows, err := pool.Query(t.Context(), `
		SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND a.attnum > 0 AND NOT a.attisdropped
		  -- Ordinary and partitioned tables only. PostGIS also brings composite
		  -- types with geometry fields (geometry_dump, valid_detail); they have
		  -- relkind 'c' and are not tables anybody maps.
		  AND c.relkind IN ('r', 'p')
		  AND format_type(a.atttypid, a.atttypmod) LIKE 'geo%'
		  -- PostGIS brings composite types of its own (geometry_dump,
		  -- valid_detail) with geometry fields. They are the extension's, not
		  -- this schema's, which is exactly what introspection excludes.
		  AND NOT EXISTS (SELECT 1 FROM pg_depend dep
		                  WHERE dep.classid = 'pg_class'::regclass
		                    AND dep.objid = c.oid AND dep.deptype = 'e')
		ORDER BY c.relname, a.attnum`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	type dbCol struct {
		table, column, declared string
		nullable                bool
	}
	var cols []dbCol
	for rows.Next() {
		var c dbCol
		if err := rows.Scan(&c.table, &c.column, &c.declared, &c.nullable); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cols) < 12 {
		t.Fatalf("the schema has %d spatial columns; the fixture is too small to prove anything", len(cols))
	}

	// What the generated descriptors claim, keyed the same way.
	claimed := map[string]postgis.TypeMod{
		"places.spot":      modOf(gisdemo.Places.Spot.SRID(), gisdemo.Places.Spot.Kind(), gisdemo.Places.Spot.Dim(), postgis.FamilyGeography),
		"places.location":  modOf(gisdemo.Places.Location.SRID(), gisdemo.Places.Location.Kind(), gisdemo.Places.Location.Dim(), postgis.FamilyGeometry),
		"places.projected": modOf(gisdemo.Places.Projected.SRID(), gisdemo.Places.Projected.Kind(), gisdemo.Places.Projected.Dim(), postgis.FamilyGeometry),
		"places.footprint": modOf(gisdemo.Places.Footprint.SRID(), gisdemo.Places.Footprint.Kind(), gisdemo.Places.Footprint.Dim(), postgis.FamilyGeometry),
		"zones.area":       modOf(gisdemo.Zones.Area.SRID(), gisdemo.Zones.Area.Kind(), gisdemo.Zones.Area.Dim(), postgis.FamilyGeometry),
		"zones.centre":     modOf(gisdemo.Zones.Centre.SRID(), gisdemo.Zones.Centre.Kind(), gisdemo.Zones.Centre.Dim(), postgis.FamilyGeometry),
		"roads.path":       modOf(gisdemo.Roads.Path.SRID(), gisdemo.Roads.Path.Kind(), gisdemo.Roads.Path.Dim(), postgis.FamilyGeometry),
		"readings.flat":    modOf(gisdemo.Readings.Flat.SRID(), gisdemo.Readings.Flat.Kind(), gisdemo.Readings.Flat.Dim(), postgis.FamilyGeometry),
		"readings.raised":  modOf(gisdemo.Readings.Raised.SRID(), gisdemo.Readings.Raised.Kind(), gisdemo.Readings.Raised.Dim(), postgis.FamilyGeometry),
		"readings.marked":  modOf(gisdemo.Readings.Marked.SRID(), gisdemo.Readings.Marked.Kind(), gisdemo.Readings.Marked.Dim(), postgis.FamilyGeometry),
		"readings.zm":      modOf(gisdemo.Readings.ZM.SRID(), gisdemo.Readings.ZM.Kind(), gisdemo.Readings.ZM.Dim(), postgis.FamilyGeometry),
		"readings.line3d":  modOf(gisdemo.Readings.Line3D.SRID(), gisdemo.Readings.Line3D.Kind(), gisdemo.Readings.Line3D.Dim(), postgis.FamilyGeometry),
		"places.sketch":    modOf(gisdemo.Places.Sketch.SRID(), gisdemo.Places.Sketch.Kind(), gisdemo.Places.Sketch.Dim(), postgis.FamilyGeometry),
	}

	for _, c := range cols {
		key := c.table + "." + c.column
		want, err := postgis.ParseTypeMod(c.declared)
		if err != nil {
			t.Fatalf("%s: the database says %q, which does not parse: %v", key, c.declared, err)
		}
		got, ok := claimed[key]
		if !ok {
			t.Errorf("%s is a spatial column with no generated descriptor in this test", key)
			continue
		}
		if got.Family != want.Family {
			t.Errorf("%s: the descriptor says %s and the column is %s", key, got.Family, want.Family)
		}
		if got.Kind != want.Kind {
			t.Errorf("%s: the descriptor says %s and the column is %s", key, got.Kind, want.Kind)
		}
		if got.SRID != want.SRID {
			t.Errorf("%s: the descriptor says SRID %d and the column is SRID %d", key, got.SRID, want.SRID)
		}
		if got.Dim != want.Dim {
			t.Errorf("%s: the descriptor says %v and the column is %v", key, got.Dim, want.Dim)
		}
	}
}

func modOf(srid int32, kind postgis.Kind, dim postgis.Dim, fam postgis.Family) postgis.TypeMod {
	return postgis.TypeMod{Family: fam, Kind: kind, SRID: srid, Dim: dim}
}

// A NOT NULL spatial column read through a LEFT JOIN, then through a spatial
// function, must still be nullable at the end.
//
// This is the attack the nullability system exists for, and the spatial layer
// is where it could quietly stop working: the function wrapper builds a new
// node, and a node that forgot which source it came from would let a
// non-nullable destination through.
func TestAudit_outerJoinThroughSpatialFunctions(t *testing.T) {
	db := openDB(t)

	// Every one of these reads a NOT NULL column of a left-joined source and
	// claims the result cannot be NULL. Each has to be refused.
	refusals := map[string]orm.Projection[orm.Composed, int]{
		"the column itself": orm.Project1(
			orm.Of(gisdemo.Places.Location), func(postgis.Geometry) int { return 0 }),
		"Distance": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).Distance(
				postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)))),
			func(float64) int { return 0 }),
		"Area": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).Area()), func(float64) int { return 0 }),
		"Centroid": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).Centroid().Value()),
			func(postgis.Geometry) int { return 0 }),
		"Transform": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).Transform(3857).Value()),
			func(postgis.Geometry) int { return 0 }),
		"AsGeoJSON": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).AsGeoJSON()), func(string) int { return 0 }),
		"AsEWKT": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).AsEWKT()), func(string) int { return 0 }),
		"SRID": orm.Project1(
			postgis.Of(gisdemo.Places.Location).SRID(), func(int32) int { return 0 }),
		"a chain of four": orm.Project1(
			orm.Of(postgis.Of(gisdemo.Places.Location).
				Transform(3857).Buffer(1).Centroid().Transform(4326).Value()),
			func(postgis.Geometry) int { return 0 }),
		"the geography column": orm.Project1(
			orm.Of(gisdemo.Places.Spot), func(postgis.Geography) int { return 0 }),
		"geography Distance": orm.Project1(
			orm.Of(postgis.OfGeog(gisdemo.Places.Spot).Distance(
				postgis.GeogValue[orm.Composed](postgis.GeographyPoint(0, 0)))),
			func(float64) int { return 0 }),
	}

	for name, shape := range refusals {
		t.Run(name, func(t *testing.T) {
			_, err := orm.Compose(db.Executor(), shape).
				From(gisdemo.Zones.Source()).
				LeftJoin(gisdemo.Places.Source(),
					orm.Cond(gisdemo.Zones.Name.Eq("nothing matches"))).
				All(t.Context())
			if err == nil {
				t.Fatalf("%s read a left-joined NOT NULL column into a destination that cannot hold NULL", name)
			}
			if !strings.Contains(err.Error(), "outer join") {
				t.Errorf("the refusal is not about the join: %v", err)
			}
		})
	}
}

// And the same values, read through the nullable form, really do come back
// NULL for an unmatched row rather than as a zero.
func TestAudit_outerJoinProducesRealNulls(t *testing.T) {
	db := openDB(t)

	type row struct {
		zone     string
		distance *float64
		area     *float64
		geojson  *string
		centroid *postgis.Geometry
		srid     *int32
	}
	shape := orm.Project6(
		orm.Of(gisdemo.Zones.Name),
		orm.OfNull(orm.FnNull[orm.Composed, float64]("ST_Distance",
			orm.ArgOpt(gisdemo.Places.Location), orm.ArgOf(orm.Of(gisdemo.Zones.Area)))),
		orm.OfNull(orm.FnNull[orm.Composed, float64]("ST_Area", orm.ArgOpt(gisdemo.Places.Location))),
		orm.OfNull(orm.FnNull[orm.Composed, string]("ST_AsGeoJSON", orm.ArgOpt(gisdemo.Places.Location))),
		orm.Opt(gisdemo.Places.Location),
		orm.OfNull(orm.FnNull[orm.Composed, int32]("ST_SRID", orm.ArgOpt(gisdemo.Places.Location))),
		func(z string, d, a *float64, j *string, c *postgis.Geometry, s *int32) row {
			return row{z, d, a, j, c, s}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Zones.Source()).
		LeftJoin(gisdemo.Places.Source(), orm.Cond(gisdemo.Zones.Name.Eq("nothing matches"))).
		OrderBy(orm.Of(gisdemo.Zones.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d rows, want 2", len(got))
	}
	for _, r := range got {
		if r.distance != nil || r.area != nil || r.geojson != nil || r.centroid != nil || r.srid != nil {
			t.Errorf("%s matched nothing and came back with %+v", r.zone, r)
		}
	}
}

// A CASE whose branches are in different coordinate systems must not report one
// of them as the result's SRID.
//
// The architecture's answer is that a CASE produces an orm.Expression rather
// than a spatial expression, so there is no spatial metadata to be wrong about
// — a CASE result cannot be fed back into a spatial predicate without going
// through a constructor that states what it is. This test holds that door shut.
func TestAudit_caseHasNoSpatialMetadataToLieWith(t *testing.T) {
	db := openDB(t)

	// Two branches in genuinely different coordinate systems.
	chosen := orm.Case(
		orm.Cond(gisdemo.Places.Name.Eq("origin")),
		orm.Of(gisdemo.Places.Location.Expr().Value()),
	).Else(postgis.Value[orm.Composed](postgis.NewPoint(3857, 1, 2)).Value())

	type row struct {
		name string
		srid int32
	}
	shape := orm.Project2(
		orm.Of(gisdemo.Places.Name),
		orm.Of(orm.Fn[orm.Composed, int32]("ST_SRID", orm.ArgOf(chosen))),
		func(n string, s int32) row { return row{n, s} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(orm.Cond(gisdemo.Places.Name.In("origin", "east"))).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d rows", len(got))
	}
	// The two rows really are in different coordinate systems, which is the
	// point: a single claimed SRID for this expression would be false for one
	// of them.
	if got[0].srid == got[1].srid {
		t.Fatalf("both branches gave SRID %d; the corpus does not exercise the mismatch", got[0].srid)
	}
	if got[0].srid != 4326 || got[1].srid != 3857 {
		t.Errorf("the branches gave SRID %d and %d", got[0].srid, got[1].srid)
	}
}

// The index must not change the answer, and dropping and recreating it must
// not change it either. The planner's choices are not a correctness property of
// this package, and a test that only ever ran with an index would not know.
func TestAudit_indexDoesNotChangeResults(t *testing.T) {
	db, pool := openBoth(t)

	corpus := func() []string {
		t.Helper()
		var out []string
		for _, p := range []orm.Predicate[orm.Composed]{
			postgis.Of(gisdemo.Places.Location).DWithin(
				postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)), 1),
			postgis.Of(gisdemo.Places.Location).Intersects(
				postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, ring(-2, -2, 5)))),
			postgis.Of(gisdemo.Places.Location).BBoxIntersects(
				postgis.Value[orm.Composed](postgis.NewPolygon(4326, postgis.XY, ring(-2, -2, 5)))),
			postgis.OfGeog(gisdemo.Places.Spot).DWithin(
				postgis.GeogValue[orm.Composed](postgis.GeographyPoint(0, 0)), 60_000),
		} {
			out = append(out, strings.Join(names(t, db.Executor(), gisdemo.Places.Source(),
				orm.Of(gisdemo.Places.Name), orm.Of(gisdemo.Places.ID).Asc(), p), ","))
		}
		// And the KNN ordering.
		shape := orm.Project1(orm.Of(gisdemo.Places.Name), func(s string) string { return s })
		ordered, err := orm.Compose(db.Executor(), shape).
			From(gisdemo.Places.Source()).
			OrderBy(orm.Of(postgis.Of(gisdemo.Places.Location).KNNDistance(
				postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0)))).Asc(),
				orm.Of(gisdemo.Places.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join(ordered, ","))
		return out
	}

	withIndex := corpus()

	for _, stmt := range []string{
		`DROP INDEX places_location_gist`,
		`DROP INDEX places_spot_gist`,
	} {
		if _, err := pool.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	withoutIndex := corpus()

	for _, stmt := range []string{
		`CREATE INDEX places_location_gist ON places USING gist (location)`,
		`CREATE INDEX places_spot_gist ON places USING gist (spot)`,
	} {
		if _, err := pool.Exec(t.Context(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	recreated := corpus()

	for i := range withIndex {
		if withIndex[i] != withoutIndex[i] {
			t.Errorf("query %d gave %q with a GiST index and %q without", i, withIndex[i], withoutIndex[i])
		}
		if withIndex[i] != recreated[i] {
			t.Errorf("query %d gave %q before and %q after recreating the index", i, withIndex[i], recreated[i])
		}
	}
	if withIndex[0] == "" {
		t.Fatal("the corpus selected nothing, so it proves nothing")
	}
}

func ring(x, y, size float64) []postgis.Coord {
	return []postgis.Coord{
		{X: x, Y: y}, {X: x + size, Y: y}, {X: x + size, Y: y + size}, {X: x, Y: y + size}, {X: x, Y: y},
	}
}

// A COPY that violates a spatial constraint has to report the server's error,
// copy nothing, and leave the pool usable.
func TestAudit_copyFailurePreservesTheError(t *testing.T) {
	db, pool := openBoth(t)

	before := countPlaces(t, pool)

	// A point in the wrong coordinate system for the column.
	bad := newPlace(90_001, 1, 2)
	bad.Location = postgis.NewPoint(3857, 1, 2)
	rows := []gisdemo.Place{newPlace(90_000, 0, 0), bad, newPlace(90_002, 2, 2)}

	n, err := db.Places.CopyFrom(t.Context(), rows)
	if err == nil {
		t.Fatal("a COPY with a geometry in the wrong SRID succeeded")
	}
	if n != 0 {
		t.Errorf("the failed COPY reports %d rows copied", n)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("the error is not a PostgreSQL error: %v", err)
	} else if !strings.Contains(pgErr.Message, "SRID") {
		t.Errorf("the message does not name the problem: %q", pgErr.Message)
	}

	// Nothing landed, and the pool still works.
	if after := countPlaces(t, pool); after != before {
		t.Errorf("the failed COPY left %d rows behind", after-before)
	}
	if _, err := db.Places.Query().Where(gisdemo.Places.Name.Eq("origin")).One(t.Context()); err != nil {
		t.Fatalf("after a failed COPY the pool is unusable: %v", err)
	}
}

func countPlaces(t *testing.T, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM places`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A spatial write inside a savepoint, rolled back, with the outer transaction
// committing. M12's nesting and M13's writes have to compose.
func TestAudit_savepoint(t *testing.T) {
	db, pool := openBoth(t)

	outer := postgis.NewPoint(4326, 11, 11)
	inner := postgis.NewPoint(4326, 22, 22)

	err := db.Tx(t.Context(), func(tx *gisdemo.DB) error {
		if _, err := tx.Places.Update().
			Set(gisdemo.Places.Location.Set(outer)).
			Where(gisdemo.Places.Name.Eq("origin")).
			Exec(t.Context()); err != nil {
			return err
		}
		// A nested transaction is a savepoint. Rolling it back must undo only
		// the write inside it.
		inErr := tx.Tx(t.Context(), func(inner2 *gisdemo.DB) error {
			if _, err := inner2.Places.Update().
				Set(gisdemo.Places.Location.Set(inner)).
				Where(gisdemo.Places.Name.Eq("origin")).
				Exec(t.Context()); err != nil {
				return err
			}
			return errSavepoint
		})
		if !errors.Is(inErr, errSavepoint) {
			t.Errorf("the savepoint returned %v", inErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	var ewkt string
	if err := pool.QueryRow(t.Context(),
		`SELECT ST_AsEWKT(location) FROM places WHERE name = 'origin'`).Scan(&ewkt); err != nil {
		t.Fatal(err)
	}
	if ewkt != "SRID=4326;POINT(11 11)" {
		t.Errorf("after the savepoint rolled back, the location is %q", ewkt)
	}
}

var errSavepoint = errors.New("rolling back the savepoint")

// FOR UPDATE SKIP LOCKED alongside a spatial filter: the lock semantics are
// M12's and must not change because the WHERE clause mentions PostGIS.
func TestAudit_skipLockedWithSpatialFilter(t *testing.T) {
	_, pool := openBoth(t)

	near := postgis.Value[orm.Composed](postgis.NewPoint(4326, 0, 0))
	_ = near

	// One worker takes the nearest matching row and holds it.
	tx1, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx1.Rollback(context.WithoutCancel(t.Context())) }()

	first, err := gisdemo.New(tx1).Places.Query().
		Where(gisdemo.Places.ID.Lte(2)).
		OrderBy(gisdemo.Places.ID.Asc()).
		Limit(1).
		ForUpdate().
		All(t.Context())
	if err != nil {
		t.Fatalf("the first worker: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("the first worker took %d rows", len(first))
	}

	// A second worker, skipping what is locked, must get a different row —
	// and the spatial predicate must not change that.
	tx2, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(context.WithoutCancel(t.Context())) }()

	shape := orm.Project2(orm.Of(gisdemo.Places.ID), orm.Of(gisdemo.Places.Name),
		func(id int64, n string) string { return n })
	second, err := orm.Compose(tx2, shape).
		From(gisdemo.Places.Source()).
		Where(
			orm.Cond(gisdemo.Places.ID.Lte(2)),
			postgis.Of(gisdemo.Places.Location).DWithin(near, 100),
		).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
		Limit(1).
		Lock(orm.ForUpdateStrong, orm.SkipLocked).
		All(t.Context())
	if err != nil {
		t.Fatalf("the second worker: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("the second worker took %d rows, want 1", len(second))
	}
	if second[0] == first[0].Name {
		t.Errorf("both workers claimed %s; SKIP LOCKED did not apply", second[0])
	}
}
