package migrate_test

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M12.2: ranges, multiranges and intervals through the migration engine.
//
// Nothing here is range-aware. A range column is a column with a type name, a
// GiST index is an index with an access method, and the point of the test is
// that the generic engine already handles both — so what is checked is the
// round trip closing, not a new code path existing.

// rangeSchema declares one table holding every family, plus the GiST index a
// range column exists to be searched through.
func rangeSchema() *schema.Schema {
	s := &schema.Schema{
		Tables: []schema.Table{{
			Schema: "public", Name: "bookings",
			Columns: []schema.Column{
				{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
				{Name: "room", Type: schema.Type{Name: "text"}},
				// The three that share time.Time in Go and stay distinct here.
				{Name: "period", Type: schema.Type{Name: "tstzrange"}},
				{Name: "stay", Type: schema.Type{Name: "daterange"}},
				{Name: "shift", Type: schema.Type{Name: "tsrange"}},
				{Name: "quota", Type: schema.Type{Name: "int4range"}},
				{Name: "span", Type: schema.Type{Name: "int8range"}},
				{Name: "revised", Type: schema.Type{Name: "tstzrange"}, Nullable: true},
				{Name: "lease", Type: schema.Type{Name: "interval"}},
				{Name: "grace", Type: schema.Type{Name: "interval"}, Nullable: true},
				{Name: "holds", Type: schema.Type{Name: "tstzmultirange"}},
				{Name: "slots", Type: schema.Type{Name: "int4multirange"}, Nullable: true},
			},
			PrimaryKey: &schema.PrimaryKey{Name: "bookings_pkey", Columns: []string{"id"}},
			Indexes: []schema.Index{{
				Name: "bookings_period_gist", Method: "gist",
				Columns: []schema.IndexColumn{{Name: "period"}},
			}},
		}},
	}
	s.Normalize()
	return s
}

// Scenario E and Scenario N: desired, migrated, introspected, equal — and a
// second run finds nothing to do.
func TestRoundTrip_ranges(t *testing.T) {
	conn := connect(t)
	want := rangeSchema()

	set := newSet(t, migrationFor(t, "0001_ranges", &schema.Schema{}, want))
	m := migrate.New(conn, set)

	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertEqual(t, want, introspect(t, conn))

	// The offline state and the live database agree, which is what makes the
	// next migration computable without a connection.
	state, err := set.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	assertEqual(t, state, introspect(t, conn))

	// Zero drift: nothing left to do.
	plan, err := m.Migrate(t.Context(), "")
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if !plan.Empty() {
		t.Errorf("a second run planned %d steps", len(plan.Steps))
	}
}

// Scenario F, as a migration rather than as a mapping: changing tsrange to
// tstzrange is a change the engine sees, even though the Go type on both sides
// is orm.Range[time.Time].
func TestRoundTrip_rangeFamilyChangeIsVisible(t *testing.T) {
	conn := connect(t)
	before := rangeSchema()

	after := rangeSchema()
	for i := range after.Tables[0].Columns {
		switch after.Tables[0].Columns[i].Name {
		case "shift":
			after.Tables[0].Columns[i].Type = schema.Type{Name: "tstzrange"}
		case "slots":
			// Added and dropped columns are the ordinary operations, applied to
			// a multirange to prove nothing special is needed for one.
			after.Tables[0].Columns[i].Name = "windows"
		}
	}
	after.Tables[0].Columns = append(after.Tables[0].Columns, schema.Column{
		Name: "backlog", Type: schema.Type{Name: "int8multirange"}, Nullable: true,
	})
	after.Normalize()

	d, err := migrate.Compute(before, after, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if d.Empty() {
		t.Fatal("changing tsrange to tstzrange produced no operations; the two families collapsed")
	}
	// The change is visible, which is the contract. Applying it is a separate
	// question, and a sharper one than it looks: PostgreSQL has no cast from
	// tsrange to tstzrange at all, not even an explicit one, because converting
	// the bounds means choosing a time zone and only the project knows which.
	// So the conversion is written out, exactly as it would be for text -> int,
	// and the engine does not invent it — a cast it guessed would be a data
	// transformation nobody reviewed.
	change := migrationFor(t, "0002_change", before, after, "0001_ranges")
	var found bool
	for i, op := range change.Operations {
		alter, ok := op.(migrate.AlterColumn)
		if !ok || alter.Name != "shift" {
			continue
		}
		alter.Using = schema.Expr(
			"tstzrange(lower(shift) AT TIME ZONE 'UTC', upper(shift) AT TIME ZONE 'UTC')")
		change.Operations[i] = alter
		found = true
	}
	if !found {
		t.Fatal("no AlterColumn for shift, so the family change was not seen as a type change")
	}

	set := newSet(t,
		migrationFor(t, "0001_ranges", &schema.Schema{}, before),
		change)
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertEqual(t, after, introspect(t, conn))

	plan, err := m.Migrate(t.Context(), "")
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if !plan.Empty() {
		t.Errorf("a second run planned %d steps", len(plan.Steps))
	}
}

// The GiST index survives being dropped and recreated, which is the operation a
// range index is most likely to see.
func TestRoundTrip_gistIndexLifecycle(t *testing.T) {
	conn := connect(t)
	with := rangeSchema()

	without := rangeSchema()
	without.Tables[0].Indexes = nil
	without.Normalize()

	set := newSet(t,
		migrationFor(t, "0001_ranges", &schema.Schema{}, with),
		migrationFor(t, "0002_drop_index", with, without, "0001_ranges"),
		migrationFor(t, "0003_add_index", without, with, "0002_drop_index"))
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertEqual(t, with, introspect(t, conn))

	// PostgreSQL has the index and it is a GiST one. Which index the planner
	// chooses is its business; that this one exists is the contract.
	var method string
	if err := conn.QueryRow(t.Context(), `
		SELECT am.amname
		FROM pg_class i
		JOIN pg_am am ON am.oid = i.relam
		WHERE i.relname = 'bookings_period_gist'`).Scan(&method); err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	if method != "gist" {
		t.Errorf("bookings_period_gist uses %s, want gist", method)
	}
}
