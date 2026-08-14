package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// M16.5 G1: the artifact and provenance layers, attacked.

func artView(name, sql string) schema.View {
	return schema.View{
		Schema: "public", Name: name,
		Columns:    []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}},
		Definition: schema.Definition{SQL: sql},
		DependsOn:  []schema.RelationRef{{Schema: "public", Name: "users"}},
	}
}

// Every view operation survives being written to a file and read back.
func TestArtifact_viewOperationsRoundtrip(t *testing.T) {
	v := artView("active_users", "SELECT id FROM users WHERE active")
	for _, op := range []migrate.Operation{
		migrate.CreateView{View: v},
		migrate.ReplaceView{View: v},
		migrate.DropView{Schema: "public", Name: "active_users"},
	} {
		t.Run(op.Describe(), func(t *testing.T) {
			m := &migrate.Migration{ID: "0001_x", Operations: []migrate.Operation{op}}
			data, err := migrate.Render(m)
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			back, err := migrate.Parse(data)
			if err != nil {
				t.Fatalf("parsing: %v", err)
			}
			if len(back.Operations) != 1 {
				t.Fatalf("read back %d operations", len(back.Operations))
			}
			if got := back.Operations[0].Describe(); got != op.Describe() {
				t.Errorf("the operation changed: %s -> %s", op.Describe(), got)
			}
			// The SQL it would execute survives, which is what "nothing needed
			// for execution disappeared" means.
			wantSQL, err := op.SQL()
			if err != nil {
				t.Fatal(err)
			}
			gotSQL, err := back.Operations[0].SQL()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(gotSQL, "\n") != strings.Join(wantSQL, "\n") {
				t.Errorf("the statement changed:\n%s\n%s",
					strings.Join(wantSQL, "\n"), strings.Join(gotSQL, "\n"))
			}
			// And so does the checksum.
			wantSum, err := m.Checksum()
			if err != nil {
				t.Fatal(err)
			}
			gotSum, err := back.Checksum()
			if err != nil {
				t.Fatal(err)
			}
			if gotSum != wantSum {
				t.Errorf("the checksum changed across a roundtrip")
			}
		})
	}
}

// The definition's portable identity is part of the checksum, so editing a
// definition in a committed migration invalidates it.
func TestArtifact_definitionIdentityIsChecksummed(t *testing.T) {
	base := &migrate.Migration{ID: "0001_x", Operations: []migrate.Operation{
		migrate.CreateView{View: artView("v", "SELECT id FROM users WHERE active")},
	}}
	sum, err := base.Checksum()
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what string
		op   migrate.Operation
	}{
		{"a changed definition", migrate.CreateView{View: artView("v", "SELECT id FROM users WHERE NOT active")}},
		{"a reformatted definition", migrate.CreateView{View: artView("v", "SELECT id\nFROM users\nWHERE active")}},
		{"a changed relation name", migrate.CreateView{View: artView("w", "SELECT id FROM users WHERE active")}},
		{"a changed operation kind", migrate.ReplaceView{View: artView("v", "SELECT id FROM users WHERE active")}},
	} {
		t.Run(c.what, func(t *testing.T) {
			other := &migrate.Migration{ID: "0001_x", Operations: []migrate.Operation{c.op}}
			got, err := other.Checksum()
			if err != nil {
				t.Fatal(err)
			}
			if got == sum {
				t.Errorf("%s did not change the checksum, so editing a committed migration "+
					"would go unnoticed", c.what)
			}
		})
	}

	// Columns and dependencies count too: they are what the migration state is
	// built from, and a diff against a wrong state produces a wrong migration.
	v := artView("v", "SELECT id FROM users WHERE active")
	v.Columns = append(v.Columns, schema.Column{Name: "email", Type: schema.Type{Name: "text"}})
	changed := &migrate.Migration{ID: "0001_x", Operations: []migrate.Operation{migrate.CreateView{View: v}}}
	if got, _ := changed.Checksum(); got == sum {
		t.Error("an added output column did not change the checksum")
	}
}

// The artifact carries nothing server-specific.
func TestArtifact_carriesNothingFromAServer(t *testing.T) {
	v := artView("v", "SELECT id FROM users WHERE active")
	// A canonical text is present on the value and must not be written.
	v.Definition.Canonical = " SELECT users.id FROM users WHERE users.active;"
	m := &migrate.Migration{ID: "0001_x", Operations: []migrate.Operation{migrate.CreateView{View: v}}}
	data, err := migrate.Render(m)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"users.active", "Canonical", "/tmp/", "/home/", "server_version"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the migration artifact contains %q, which differs between servers or "+
				"machines:\n%s", forbidden, text)
		}
	}
	// The developer's own definition is there, because applying it needs that.
	if !strings.Contains(text, "SELECT id FROM users WHERE active") {
		t.Errorf("the definition the migration must apply is missing:\n%s", text)
	}
}

// DROP completes the provenance lifecycle, and rolls back with its relation.
func TestProvenance_dropLifecycle(t *testing.T) {
	c := conn(t, base+`CREATE VIEW v AS SELECT id FROM users;`)
	if err := migrate.New(c, nil).EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := migrate.RecordView(t.Context(), c, "public", "v", "view", "v1:a"); err != nil {
		t.Fatal(err)
	}
	present := func(q pgx.Tx) (view, prov bool) {
		t.Helper()
		var v, p bool
		if err := q.QueryRow(t.Context(),
			`SELECT to_regclass('public.v') IS NOT NULL,
			        EXISTS (SELECT 1 FROM public.orm_schema_views WHERE relation_name = 'v')`).
			Scan(&v, &p); err != nil {
			t.Fatal(err)
		}
		return v, p
	}

	// Rolled back: both survive. A drop that removed the provenance but not the
	// view — or the reverse — would leave the database and its record disagreeing.
	tx, err := c.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `DROP VIEW v`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.ForgetView(t.Context(), tx, "public", "v"); err != nil {
		t.Fatal(err)
	}
	if v, p := present(tx); v || p {
		t.Errorf("inside the transaction the drop did not take effect: view=%v provenance=%v", v, p)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	tx2, err := c.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx2.Rollback(context.Background()) }()
	if v, p := present(tx2); !v || !p {
		t.Errorf("after a rolled-back drop: view=%v provenance=%v, want both present. "+
			"A record that outlives its relation, or a relation whose record is gone, is a "+
			"database that disagrees with its own history", v, p)
	}
	_ = tx2.Rollback(t.Context())

	// Committed: both gone.
	tx3, err := c.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx3.Exec(t.Context(), `DROP VIEW v`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.ForgetView(t.Context(), tx3, "public", "v"); err != nil {
		t.Fatal(err)
	}
	if err := tx3.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	state, err := migrate.ReadViewState(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state["public.v"]; ok {
		t.Error("the provenance outlived the view it described")
	}
}

// unknownOp is an operation the artifact layer has never heard of.
type unknownOp struct{}

func (unknownOp) Describe() string           { return "unknown" }
func (unknownOp) Safety() migrate.Safety     { return migrate.Safe }
func (unknownOp) Transactional() bool        { return true }
func (unknownOp) SQL() ([]string, error)     { return []string{"SELECT 1"}, nil }
func (unknownOp) Apply(*schema.Schema) error { return nil }
func (unknownOp) Reverse(*schema.Schema) (migrate.Operation, error) {
	return nil, nil
}

// An operation with no case is an error, never a file that quietly lost it.
//
// This is the property that makes the marshal switch trustworthy: adding an
// operation and forgetting to serialise it produces a loud failure at the moment
// somebody tries, rather than a migration that applies less than it describes.
func TestArtifact_unknownOperationIsRefused(t *testing.T) {
	m := &migrate.Migration{ID: "0001_x", Operations: []migrate.Operation{unknownOp{}}}
	data, err := migrate.Render(m)
	if err == nil {
		t.Fatalf("an unrecognised operation was written to a migration file:\n%s", data)
	}
	if !strings.Contains(err.Error(), "cannot be written") {
		t.Errorf("the error does not say what happened: %v", err)
	}
}

// The provenance lifecycle through the execution path, which is what a
// migration actually takes — not the recording functions called directly.
func TestProvenance_lifecycleThroughExecution(t *testing.T) {
	c := conn(t, base)
	m := migrate.New(c, nil)
	if err := m.EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}

	v := artView("v", "SELECT id FROM users")
	recorded := func() (string, bool) {
		t.Helper()
		state, err := migrate.ReadViewState(t.Context(), c)
		if err != nil {
			t.Fatal(err)
		}
		r, ok := state["public.v"]
		return r.SourceIdentity, ok
	}

	// Create: the operation records.
	if err := migrate.ExecOperationForTest(t.Context(), c, migrate.CreateView{View: v}); err != nil {
		t.Fatalf("creating: %v", err)
	}
	id, ok := recorded()
	if !ok {
		t.Fatal("creating a view through the execution path recorded no provenance")
	}
	if id != string(v.Definition.Identity()) {
		t.Errorf("the recorded identity is %q, want the definition's", id)
	}

	// Replace: the operation updates it.
	v2 := artView("v", "SELECT id FROM users WHERE active")
	if err := migrate.ExecOperationForTest(t.Context(), c, migrate.ReplaceView{View: v2}); err != nil {
		t.Fatalf("replacing: %v", err)
	}
	id2, _ := recorded()
	if id2 == id {
		t.Error("replacing a view did not update its provenance, so a later check would " +
			"compare against the definition that is no longer there")
	}

	// Drop: the operation removes it.
	if err := migrate.ExecOperationForTest(t.Context(), c,
		migrate.DropView{Schema: "public", Name: "v"}); err != nil {
		t.Fatalf("dropping: %v", err)
	}
	if _, ok := recorded(); ok {
		t.Error("dropping a view left its provenance behind. A record of a relation that does " +
			"not exist is one a later adoption would trust")
	}
}
