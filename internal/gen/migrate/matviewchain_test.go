package migrate_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G2 Part B: the whole chain, through the production artifact.
//
// Existing replay tests apply operations in memory. This one puts every step
// through marshal, checksum, persist and parse first, because that is what a
// migration actually is by the time anybody replays it — and it asserts the
// state after each step rather than only at the end. A defect that adds an
// index twice is invisible in a final count if something else removed one.

// throughArtifact renders operations to a migration file and reads them back,
// so what gets replayed is what a committed migration would contain.
func throughArtifact(t *testing.T, id string, ops ...migrate.Operation) []migrate.Operation {
	t.Helper()
	m := &migrate.Migration{ID: id, Operations: ops}
	sum, err := m.Checksum()
	if err != nil {
		t.Fatalf("checksumming: %v", err)
	}
	data, err := migrate.Render(m)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	back, err := migrate.Parse(data)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}
	backSum, err := back.Checksum()
	if err != nil {
		t.Fatal(err)
	}
	if backSum != sum {
		t.Errorf("the checksum changed across the artifact roundtrip")
	}
	if len(back.Operations) != len(ops) {
		t.Fatalf("read back %d operations, wrote %d", len(back.Operations), len(ops))
	}
	// Nothing a server or a machine contributed may be in the file.
	text := string(data)
	for _, forbidden := range []string{"Canonical", "server_version", "relfilenode", "/tmp/", "/home/", "populated"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("the artifact contains %q:\n%s", forbidden, text)
		}
	}
	return back.Operations
}

func idx(name, col string) schema.Index {
	return schema.Index{Name: name, Columns: []schema.IndexColumn{{Name: col}}}
}

// indexNames returns the state's index names for a materialized view, in order.
func indexNames(t *testing.T, s *schema.Schema, qualified string) []string {
	t.Helper()
	m, ok := findMat(s, qualified)
	if !ok {
		return nil
	}
	var out []string
	for _, i := range m.Indexes {
		out = append(out, i.Name)
	}
	return out
}

// countOf reports how many times a name appears, which is the assertion that
// matters: exactly once, never twice.
func countOf(names []string, want string) int {
	n := 0
	for _, s := range names {
		if s == want {
			n++
		}
	}
	return n
}

// The chain, asserted after every step.
func TestChain_replayThroughTheArtifact(t *testing.T) {
	m := schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id, email FROM users"},
		// Set on purpose: the relation operation must not carry these into
		// replay state, or every index would exist twice.
		Indexes:   []schema.Index{idx("carried_in", "id")},
		Populated: true,
	}
	i1 := schema.Index{Name: "i1", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}
	i2 := schema.Index{Name: "i2", Method: "gin", Columns: []schema.IndexColumn{{Name: "email"}}}
	i3 := schema.Index{
		Name: "i3", Columns: []schema.IndexColumn{{Name: "id"}},
		Include: []string{"email"}, Where: "id > 0",
	}

	s := &schema.Schema{}

	// After CreateMaterializedView: the relation, and no indexes at all.
	ops := throughArtifact(t, "0001", migrate.CreateMaterializedView{View: m})
	applyAll(t, s, ops...)
	got, ok := findMat(s, "public.totals")
	if !ok {
		t.Fatal("the materialized view is not in the state")
	}
	if !got.WithData {
		t.Error("the creation policy did not survive the artifact")
	}
	if got.Populated {
		t.Error("runtime population entered the migration state; it is not desired schema")
	}
	if len(got.Indexes) != 0 {
		t.Fatalf("CreateMaterializedView carried %d indexes into replay state. Every index the "+
			"migration also creates would then exist twice, and the project would never "+
			"converge: %+v", len(got.Indexes), got.Indexes)
	}

	// After each CreateIndex: exactly once, with its metadata.
	ops = throughArtifact(t, "0002",
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: i1})
	applyAll(t, s, ops...)
	if n := countOf(indexNames(t, s, "public.totals"), "i1"); n != 1 {
		t.Fatalf("i1 appears %d times, want once", n)
	}

	ops = throughArtifact(t, "0003",
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: i2})
	applyAll(t, s, ops...)
	names := indexNames(t, s, "public.totals")
	if countOf(names, "i1") != 1 || countOf(names, "i2") != 1 {
		t.Fatalf("after two creates the indexes are %v", names)
	}
	got, _ = findMat(s, "public.totals")
	for _, want := range got.Indexes {
		switch want.Name {
		case "i1":
			if !want.Unique {
				t.Error("i1 lost its uniqueness through the artifact")
			}
		case "i2":
			if want.Method != "gin" {
				t.Errorf("i2 has method %q, want gin", want.Method)
			}
		}
	}

	// After DropIndex: i1 gone, i2 still exactly once.
	ops = throughArtifact(t, "0004",
		migrate.DropIndex{Schema: "public", Table: "totals", Name: "i1"})
	applyAll(t, s, ops...)
	names = indexNames(t, s, "public.totals")
	if countOf(names, "i1") != 0 {
		t.Errorf("i1 survived its drop: %v", names)
	}
	if countOf(names, "i2") != 1 {
		t.Errorf("i2 appears %d times after an unrelated drop: %v", countOf(names, "i2"), names)
	}

	// After the third create: i2 and i3, each once.
	ops = throughArtifact(t, "0005",
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: i3})
	applyAll(t, s, ops...)
	names = indexNames(t, s, "public.totals")
	if countOf(names, "i2") != 1 || countOf(names, "i3") != 1 || len(names) != 2 {
		t.Fatalf("indexes = %v, want exactly i2 and i3", names)
	}
	got, _ = findMat(s, "public.totals")
	for _, have := range got.Indexes {
		if have.Name != "i3" {
			continue
		}
		if have.Where == "" || len(have.Include) != 1 {
			t.Errorf("i3 lost its predicate or INCLUDE through the artifact: %+v", have)
		}
	}

	// After DropMaterializedView: the relation and everything attached to it.
	ops = throughArtifact(t, "0006",
		migrate.DropMaterializedView{Schema: "public", Name: "totals"})
	applyAll(t, s, ops...)
	if _, ok := findMat(s, "public.totals"); ok {
		t.Error("the materialized view survived its drop")
	}
	if n := indexNames(t, s, "public.totals"); len(n) != 0 {
		t.Errorf("indexes outlived the relation they belonged to: %v", n)
	}
}
