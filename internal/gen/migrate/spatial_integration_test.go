package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// The spatial migration workflow, against a real PostGIS.
//
// A migration engine that gets spatial columns nearly right is worse than one
// that refuses them: the failure is a migration generated on every run for a
// column nobody touched, or a type change that succeeds and reinterprets the
// data. Both are checked here.

func gisMigrateConn(t *testing.T) (*pgx.Conn, string) {
	t.Helper()
	admin := testdb.AdminDSN(t)
	cfg, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatalf("parsing %s: %v", testdb.EnvAdminDSN, err)
	}
	probe, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	var ok bool
	err = probe.QueryRow(t.Context(),
		`select exists (select 1 from pg_available_extensions where name = 'postgis')`).Scan(&ok)
	_ = probe.Close(context.WithoutCancel(t.Context()))
	if err != nil {
		t.Fatalf("asking whether PostGIS is available: %v", err)
	}
	if !ok {
		t.Skip("this PostgreSQL has no PostGIS extension available")
	}

	dsn := testdb.Create(t, "")
	connCfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the test DSN: %v", err)
	}
	conn, err := pgx.ConnectConfig(t.Context(), connCfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn, dsn
}

// spatialDesired is a schema with every spatial shape the model shows, declared
// the way a managed project would declare it.
func spatialDesired() *schema.Schema {
	col := func(name, typ string, nullable bool) schema.Column {
		return schema.Column{Name: name, Type: schema.Type{Name: typ}.Canonical(), Nullable: nullable}
	}
	return &schema.Schema{
		Extensions: []schema.Extension{{Name: "postgis"}},
		Tables: []schema.Table{{
			Schema: "public",
			Name:   "places",
			Columns: []schema.Column{
				{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
				col("location", "geometry(Point,4326)", false),
				col("projected", "geometry(Point,3857)", true),
				col("area", "geometry(MultiPolygon,4326)", true),
				col("track", "geometry(LineStringZ,4326)", true),
				col("marked", "geometry(PointM,4326)", true),
				col("full", "geometry(PointZM,4326)", true),
				col("spot", "geography(Point,4326)", false),
				col("sketch", "geometry", true),
			},
			PrimaryKey: &schema.PrimaryKey{Name: "places_pkey", Columns: []string{"id"}},
			Indexes: []schema.Index{
				{Name: "places_location_gist", Method: "gist",
					Columns: []schema.IndexColumn{{Name: "location"}}},
				{Name: "places_spot_gist", Method: "gist",
					Columns: []schema.IndexColumn{{Name: "spot"}}},
				// A non-default operator class, which the catalog records and a
				// round trip has to keep: gist_geometry_ops_nd indexes every
				// dimension where the default indexes two, and losing it would
				// silently turn a 3D index into a 2D one.
				//
				// The default class is deliberately not declared anywhere here.
				// PostgreSQL omits it from pg_get_indexdef, so a schema that
				// spelled it out would differ from the catalog forever — which
				// is true of every type and not something spatial.
				{Name: "places_area_gist", Method: "gist",
					Columns: []schema.IndexColumn{{Name: "area", OpClass: "gist_geometry_ops_nd"}}},
				// A partial index, which is the generic index model applied to a
				// spatial column rather than anything spatial in itself.
				{Name: "places_partial_gist", Method: "gist",
					Columns: []schema.IndexColumn{{Name: "projected"}},
					Where:   "projected IS NOT NULL"},
			},
		}},
	}
}

// The whole managed loop: create from nothing, then find nothing left to do.
//
// The second half is the part that matters. A round trip that loses the shape,
// the SRID or the dimensionality produces a diff on every run — a migration
// tool that always has work to do is a migration tool nobody trusts.
func TestSpatialMigrate_managedRoundTrip(t *testing.T) {
	conn, _ := gisMigrateConn(t)
	want := spatialDesired()

	// makemigrations, against an empty database.
	empty := &schema.Schema{}
	first, err := migrate.Compute(empty, want, migrate.Options{})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(first.Operations) == 0 {
		t.Fatal("the first diff is empty")
	}
	// The extension has to be created before the table that uses its types.
	if _, ok := first.Operations[0].(migrate.CreateExtension); !ok {
		t.Fatalf("the first operation is %T, and the extension has to come first", first.Operations[0])
	}

	// migrate.
	applySpatial(t, conn, first)

	// inspect, and compare with what was asked for.
	actual, err := pgintro.Canonical(t.Context(), conn, []string{"public"})
	if err != nil {
		t.Fatalf("introspecting: %v", err)
	}

	// makemigrations --check: nothing left to do.
	second, err := migrate.Compute(actual, want, migrate.Options{})
	if err != nil {
		t.Fatalf("the second Diff: %v", err)
	}
	if len(second.Operations) != 0 {
		var b strings.Builder
		for _, op := range second.Operations {
			b.WriteString("\n  " + op.Describe())
		}
		t.Errorf("the round trip drifted:%s", b.String())
	}

	// And the metadata really did survive, rather than both sides being wrong
	// in the same way.
	byName := map[string]schema.Column{}
	for _, tbl := range actual.Tables {
		if tbl.Name != "places" {
			continue
		}
		for _, c := range tbl.Columns {
			byName[c.Name] = c
		}
	}
	for name, wantType := range map[string]string{
		"location":  "geometry(Point,4326)",
		"projected": "geometry(Point,3857)",
		"area":      "geometry(MultiPolygon,4326)",
		"track":     "geometry(LineStringZ,4326)",
		"marked":    "geometry(PointM,4326)",
		"full":      "geometry(PointZM,4326)",
		"spot":      "geography(Point,4326)",
		"sketch":    "geometry",
	} {
		got, ok := byName[name]
		if !ok {
			t.Errorf("the column %s was not created", name)
			continue
		}
		if got.Type.Name != wantType {
			t.Errorf("%s came back as %s, want %s", name, got.Type.Name, wantType)
		}
	}

	// The GiST indexes round-tripped too, over both storage families.
	var gist int
	for _, tbl := range actual.Tables {
		for _, ix := range tbl.Indexes {
			if ix.Method == "gist" {
				gist++
			}
		}
	}
	if gist != 4 {
		t.Errorf("%d GiST indexes came back, want 4", gist)
	}

	// The operator class and the predicate survived, which is what makes the
	// round trip meaningful rather than merely quiet.
	var sawOpClass, sawPartial bool
	for _, tbl := range actual.Tables {
		for _, ix := range tbl.Indexes {
			for _, c := range ix.Columns {
				if c.OpClass == "gist_geometry_ops_nd" {
					sawOpClass = true
				}
			}
			if ix.Name == "places_partial_gist" && !ix.Where.Empty() {
				sawPartial = true
			}
		}
	}
	if !sawOpClass {
		t.Error("the explicit operator class did not survive the round trip")
	}
	if !sawPartial {
		t.Error("the partial index's predicate did not survive the round trip")
	}
}

// Adding and dropping a spatial column, which is the routine change.
func TestSpatialMigrate_addAndDropColumn(t *testing.T) {
	conn, _ := gisMigrateConn(t)
	base := spatialDesired()
	applySpatial(t, conn, mustDiff(t, &schema.Schema{}, base))

	// Add one.
	withExtra := cloneSchema(base)
	withExtra.Tables[0].Columns = append(withExtra.Tables[0].Columns, schema.Column{
		Name: "centre", Type: schema.Type{Name: "geometry(Point,4326)"}.Canonical(), Nullable: true,
	})
	applySpatial(t, conn, mustDiff(t, base, withExtra))

	actual, err := pgintro.Canonical(t.Context(), conn, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if d := mustDiff(t, actual, withExtra); len(d.Operations) != 0 {
		t.Errorf("adding a spatial column drifted: %v", d.Operations[0].Describe())
	}

	// And drop it again.
	applySpatial(t, conn, mustDiff(t, withExtra, base))
	actual, err = pgintro.Canonical(t.Context(), conn, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	if d := mustDiff(t, actual, base); len(d.Operations) != 0 {
		t.Errorf("dropping a spatial column drifted: %v", d.Operations[0].Describe())
	}
}

// The dangerous changes: PostgreSQL refuses two of them and accepts the third
// without a word, and the engine's job is to invent nothing and warn about the
// one that goes through.
func TestSpatialMigrate_dangerousTypeChanges(t *testing.T) {
	conn, _ := gisMigrateConn(t)
	base := spatialDesired()
	applySpatial(t, conn, mustDiff(t, &schema.Schema{}, base))
	if _, err := conn.Exec(t.Context(),
		`INSERT INTO places (id, location, spot) VALUES (1, 'SRID=4326;POINT(1 2)', 'SRID=4326;POINT(1 2)')`); err != nil {
		t.Fatalf("seeding a row: %v", err)
	}

	for _, tc := range []struct {
		name    string
		column  string
		newType string
		// refused says PostgreSQL rejects the change outright.
		refused bool
		// warns says the engine warns about it, because PostgreSQL will not.
		warns bool
	}{
		{"SRID 4326 to 3857", "location", "geometry(Point,3857)", true, false},
		{"Point to Polygon", "location", "geometry(Polygon,4326)", true, false},
		{"geometry to geography", "location", "geography(Point,4326)", false, true},
		{"geography to geometry", "spot", "geometry(Point,4326)", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := cloneSchema(base)
			for i := range target.Tables[0].Columns {
				if target.Tables[0].Columns[i].Name == tc.column {
					target.Tables[0].Columns[i].Type = schema.Type{Name: tc.newType}.Canonical()
				}
			}
			d := mustDiff(t, base, target)
			if len(d.Operations) == 0 {
				t.Fatal("the change produced no operation")
			}

			// Nothing invented a USING clause.
			for _, op := range d.Operations {
				alter, ok := op.(migrate.AlterColumn)
				if !ok {
					continue
				}
				if !alter.Using.Empty() {
					t.Errorf("the engine invented a cast: USING %s", alter.Using)
				}
			}

			warnings := migrate.Warnings(d.Operations)
			var reinterpret bool
			for _, w := range warnings {
				if w.Code == migrate.WSpatialReinterpret {
					reinterpret = true
				}
			}
			if reinterpret != tc.warns {
				t.Errorf("the reinterpretation warning is %v, want %v (warnings: %v)",
					reinterpret, tc.warns, warnings)
			}

			// And what the server does with it.
			stmts, err := d.Operations[len(d.Operations)-1].SQL()
			if err != nil {
				t.Fatalf("rendering: %v", err)
			}
			tx, err := conn.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(t.Context())) }()
			var execErr error
			for _, stmt := range stmts {
				if _, execErr = tx.Exec(t.Context(), stmt); execErr != nil {
					break
				}
			}
			if tc.refused && execErr == nil {
				t.Errorf("PostgreSQL accepted %s; the test expected it to refuse", tc.name)
			}
			if !tc.refused && execErr != nil {
				t.Errorf("PostgreSQL refused %s: %v", tc.name, execErr)
			}
		})
	}
}

// The engine does not install PostGIS because a spatial column appeared. The
// extension is a declaration like any other, and a schema that uses its types
// without declaring it produces a migration that fails at the server rather
// than one that quietly changes what is installed.
func TestSpatialMigrate_extensionIsNotImplicit(t *testing.T) {
	conn, _ := gisMigrateConn(t)
	want := spatialDesired()
	want.Extensions = nil

	d := mustDiff(t, &schema.Schema{}, want)
	for _, op := range d.Operations {
		if _, ok := op.(migrate.CreateExtension); ok {
			t.Fatal("the engine created the PostGIS extension nobody declared")
		}
	}

	// And the migration fails at the server, which is the honest outcome.
	stmts, err := d.Operations[0].SQL()
	if err != nil {
		t.Fatal(err)
	}
	var execErr error
	for _, stmt := range stmts {
		if _, execErr = conn.Exec(t.Context(), stmt); execErr != nil {
			break
		}
	}
	if execErr == nil {
		t.Error("a table using geometry was created in a database with no PostGIS")
	}
}

// Migration state has to preserve every spatial fact, or drift detection
// against it is unsound from the next migration onwards.
func TestSpatialMigrate_statePreservesMetadata(t *testing.T) {
	want := spatialDesired()
	d := mustDiff(t, &schema.Schema{}, want)

	// Replay the operations into a fresh state, which is what the engine does
	// to know what the database should look like without connecting to it.
	state := &schema.Schema{}
	for _, op := range d.Operations {
		if err := op.Apply(state); err != nil {
			t.Fatalf("applying %s: %v", op.Describe(), err)
		}
	}
	if again, err := migrate.Compute(state, want, migrate.Options{}); err != nil {
		t.Fatalf("Diff: %v", err)
	} else if len(again.Operations) != 0 {
		t.Errorf("the replayed state differs from what was asked for: %v", again.Operations[0].Describe())
	}

	// And the state really holds the modifiers rather than a flattened name.
	for _, tbl := range state.Tables {
		for _, c := range tbl.Columns {
			if c.Name == "track" && c.Type.Name != "geometry(LineStringZ,4326)" {
				t.Errorf("the state holds %s for track", c.Type.Name)
			}
			if c.Name == "full" && c.Type.Name != "geometry(PointZM,4326)" {
				t.Errorf("the state holds %s for full", c.Type.Name)
			}
		}
	}
}

// The default operator class is PostgreSQL's to omit.
//
// pg_get_indexdef renders gist (geom) for the default class and
// gist (geom gist_geometry_ops_nd) for any other, so a desired schema that
// spelled the default out would differ from the catalog on every run. That is
// how PostgreSQL behaves for every type; the test records it so that somebody
// meeting the drift knows where it comes from.
func TestSpatialMigrate_defaultOpClassIsOmitted(t *testing.T) {
	conn, _ := gisMigrateConn(t)
	if _, err := conn.Exec(t.Context(), `
		CREATE EXTENSION postgis;
		CREATE TABLE opc (g geometry(PointZ, 4326));
		CREATE INDEX opc_default ON opc USING gist (g gist_geometry_ops_2d);
		CREATE INDEX opc_nd ON opc USING gist (g gist_geometry_ops_nd)`); err != nil {
		t.Fatal(err)
	}
	actual, err := pgintro.Canonical(t.Context(), conn, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tbl := range actual.Tables {
		for _, ix := range tbl.Indexes {
			for _, c := range ix.Columns {
				got[ix.Name] = c.OpClass
			}
		}
	}
	if got["opc_default"] != "" {
		t.Errorf("the default operator class came back as %q; PostgreSQL does not render it",
			got["opc_default"])
	}
	if got["opc_nd"] != "gist_geometry_ops_nd" {
		t.Errorf("the non-default operator class came back as %q", got["opc_nd"])
	}
}

// A spatial type modifier written in another case is the same column, and a
// migration engine that thought otherwise would generate an alteration on every
// run.
func TestSpatialMigrate_modifierCaseIsNormalised(t *testing.T) {
	for _, pair := range [][2]string{
		{"geometry(Point,4326)", "GEOMETRY(POINT,4326)"},
		{"geometry(Point,4326)", "geometry(point, 4326)"},
		{"geometry(PointZM,4326)", "geometry(pointzm,4326)"},
		{"geography(MultiPolygon,4326)", "GEOGRAPHY(multipolygon,4326)"},
	} {
		a := schema.Type{Name: pair[0]}.Canonical()
		b := schema.Type{Name: pair[1]}.Canonical()
		if a != b {
			t.Errorf("%q and %q canonicalise to %q and %q", pair[0], pair[1], a.Name, b.Name)
		}
	}
}

func mustDiff(t *testing.T, from, to *schema.Schema) migrate.Diff {
	t.Helper()
	d, err := migrate.Compute(from, to, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return d
}

func applySpatial(t *testing.T, conn *pgx.Conn, m migrate.Diff) {
	t.Helper()
	for _, op := range m.Operations {
		stmts, err := op.SQL()
		if err != nil {
			t.Fatalf("rendering %s: %v", op.Describe(), err)
		}
		for _, stmt := range stmts {
			if _, err := conn.Exec(t.Context(), stmt); err != nil {
				t.Fatalf("running %q: %v", stmt, err)
			}
		}
	}
}

func cloneSchema(s *schema.Schema) *schema.Schema {
	out := *s
	out.Extensions = append([]schema.Extension(nil), s.Extensions...)
	out.Tables = make([]schema.Table, len(s.Tables))
	for i, t := range s.Tables {
		out.Tables[i] = t
		out.Tables[i].Columns = append([]schema.Column(nil), t.Columns...)
		out.Tables[i].Indexes = append([]schema.Index(nil), t.Indexes...)
	}
	return &out
}
