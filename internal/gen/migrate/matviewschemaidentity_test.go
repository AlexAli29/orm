package migrate_test

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G2 #19: an index operation names its relation, and a name is not a
// schema.
//
// Every index operation carries a schema and a relation name, and the replay
// resolver has to use both. Using only the name is invisible in every fixture
// this project had, because every one of them declares a single relation per
// name — so a resolver that matched on the name alone would resolve every one of
// them correctly and no test would go red.
//
// The structure that exposes it is two relations that share a basename across
// schemas and carry an index of the same name. That is legal PostgreSQL and
// entirely ordinary: pg_class is namespaced, so public.same_name_mv and
// other.same_name_mv are different relations, and idx_common on one has nothing
// to do with idx_common on the other. Under a name-only resolver an operation
// against one of them silently edits whichever the state happened to list first,
// and the damage lands on a relation the migration never mentioned.
//
// The fixture is kept permanently because it is the only shape in the suite
// where owner identity is more than a formality.

// The two relations, and the index they both carry.
const (
	sameBasename   = "same_name_mv"
	sharedIdxName  = "idx_common"
	ownerSchemaOne = "public"
	ownerSchemaTwo = "other"
)

// sameNameMatViews builds the ambiguous state: one basename, two schemas, one
// index name on each.
//
// public is listed first on purpose. A name-only resolver returns the first
// match, so an operation against other.same_name_mv lands on public's — which
// makes the failure a wrong relation being edited rather than an operation that
// finds nothing and errors.
func sameNameMatViews() *schema.Schema {
	mv := func(schemaName string) schema.MaterializedView {
		return schema.MaterializedView{
			Schema: schemaName, Name: sameBasename, WithData: true,
			Definition: schema.Definition{SQL: "SELECT id FROM users"},
			Indexes: []schema.Index{{
				Name:    sharedIdxName,
				Unique:  true,
				Columns: []schema.IndexColumn{{Name: "id"}},
			}},
		}
	}
	return &schema.Schema{MaterializedViews: []schema.MaterializedView{
		mv(ownerSchemaOne), mv(ownerSchemaTwo),
	}}
}

// indexesOf returns one relation's index names, by qualified identity.
func indexesOf(t *testing.T, s *schema.Schema, schemaName, name string) []string {
	t.Helper()
	for _, m := range s.MaterializedViews {
		if m.Schema != schemaName || m.Name != name {
			continue
		}
		out := []string{}
		for _, i := range m.Indexes {
			out = append(out, i.Name)
		}
		return out
	}
	t.Fatalf("no materialized view %s.%s is in the state", schemaName, name)
	return nil
}

// TestMutationFixturePrecondition asserts that the fixtures the mutation
// campaign attacks actually contain the structure being attacked.
//
// A mutation whose fixture lacks the attacked feature cannot be caught and must
// not be recorded as a survivor: the test was never able to notice. That
// happened to #19, whose fixture declared two differently named relations and so
// contained no ambiguity at all. So the property is asserted here, on clean
// code, before any mutation is applied, and a failure is a fixture-precondition
// failure rather than a result about the product.
func TestMutationFixturePrecondition(t *testing.T) {
	// #6 attacks materialized-view resolution in DropIndex. The fixture is only
	// able to notice if the relation the operation names really is a
	// materialized view: a DropIndex against a table resolves through the table
	// branch, which the mutation leaves alone.
	t.Run("c6", func(t *testing.T) {
		s := sameNameMatViews()
		if len(s.Tables) != 0 {
			t.Fatalf("the fixture state holds %d table(s). If a table of the same name is "+
				"present the operation may resolve through the table branch, which is "+
				"not the branch under attack", len(s.Tables))
		}
		found := false
		for _, m := range s.MaterializedViews {
			if m.Schema != ownerSchemaOne || m.Name != sameBasename {
				continue
			}
			found = true
			if len(m.Indexes) == 0 {
				t.Fatalf("%s.%s carries no index, so there is nothing for DropIndex to "+
					"resolve", m.Schema, m.Name)
			}
			if m.Indexes[0].Name != sharedIdxName {
				t.Errorf("the index is named %q, want %q", m.Indexes[0].Name, sharedIdxName)
			}
		}
		if !found {
			t.Fatalf("no materialized view %s.%s owns the index the operation names",
				ownerSchemaOne, sameBasename)
		}
		// And the owner really is a materialized view rather than a table: the
		// resolver would otherwise never reach the branch being removed.
		if _, ok := findMat(s, ownerSchemaOne+"."+sameBasename); !ok {
			t.Fatalf("%s.%s is not a materialized view in the state",
				ownerSchemaOne, sameBasename)
		}
	})

	// #1 attacks CreateMaterializedView's payload. The operation must be shown a
	// payload that really carries indexes, or dropping them would be dropping
	// nothing and replay would look correct either way.
	t.Run("c1", func(t *testing.T) {
		m := schema.MaterializedView{
			Schema: "public", Name: "totals", WithData: true,
			Definition: schema.Definition{SQL: "SELECT id FROM users"},
			Indexes:    []schema.Index{idx("carried_in", "id")},
		}
		if len(m.Indexes) == 0 {
			t.Fatal("the operation's payload carries no indexes, so an operation that kept " +
				"them would keep nothing")
		}
		s := applyAll(t, &schema.Schema{}, migrate.CreateMaterializedView{View: m})
		got, ok := findMat(s, "public.totals")
		if !ok {
			t.Fatal("the materialized view is not in the replayed state")
		}
		// And the fixture is only meaningful because a CreateIndex follows: the
		// duplicate is what breaks, not the presence of one copy.
		applyAll(t, s, migrate.CreateIndex{
			Schema: "public", Table: "totals", Index: idx("carried_in", "id")})
		got, _ = findMat(s, "public.totals")
		if len(got.Indexes) == 0 {
			t.Fatal("no index reached the state at all")
		}
	})

	// #5 attacks materialized-view resolution in CreateIndex, as #6 does in
	// DropIndex. Same precondition: the owner must be a materialized view.
	t.Run("c5", func(t *testing.T) {
		s := sameNameMatViews()
		if len(s.Tables) != 0 {
			t.Fatalf("the fixture state holds %d table(s); the operation could resolve "+
				"through the table branch, which is not the branch under attack",
				len(s.Tables))
		}
		if _, ok := findMat(s, ownerSchemaOne+"."+sameBasename); !ok {
			t.Fatalf("%s.%s is not a materialized view in the state",
				ownerSchemaOne, sameBasename)
		}
	})

	t.Run("c19", func(t *testing.T) {
		s := sameNameMatViews()
		if len(s.MaterializedViews) != 2 {
			t.Fatalf("the fixture has %d relations, want exactly two",
				len(s.MaterializedViews))
		}
		a, b := s.MaterializedViews[0], s.MaterializedViews[1]

		if a.Schema == b.Schema {
			t.Errorf("both relations are in schema %q; without two schemas there is no "+
				"owner ambiguity to resolve", a.Schema)
		}
		if a.Name != b.Name {
			t.Errorf("the relations are named %q and %q. Differently named relations are "+
				"resolved correctly by a name-only resolver too, so such a fixture "+
				"proves nothing about schema identity", a.Name, b.Name)
		}
		if len(a.Indexes) != 1 || len(b.Indexes) != 1 {
			t.Fatalf("each relation must carry exactly one index; got %d and %d",
				len(a.Indexes), len(b.Indexes))
		}
		if a.Indexes[0].Name != b.Indexes[0].Name {
			t.Errorf("the indexes are named %q and %q; a shared index name is what makes "+
				"the wrong relation an available answer", a.Indexes[0].Name, b.Indexes[0].Name)
		}
		// Owner identity differs even though every component but the schema agrees.
		if a.Schema+"."+a.Name == b.Schema+"."+b.Name {
			t.Errorf("the two owners have the same identity %s.%s", a.Schema, a.Name)
		}
	})
}

// An index operation edits the relation it names, and only that one.
func TestSchemaIdentity_indexOperationsResolveByQualifiedOwner(t *testing.T) {
	// Dropping from the second schema leaves the first alone.
	s := sameNameMatViews()
	applyAll(t, s, migrate.DropIndex{
		Schema: ownerSchemaTwo, Table: sameBasename, Name: sharedIdxName,
	})
	if got := indexesOf(t, s, ownerSchemaTwo, sameBasename); len(got) != 0 {
		t.Errorf("%s.%s kept %v after its index was dropped",
			ownerSchemaTwo, sameBasename, got)
	}
	if got := indexesOf(t, s, ownerSchemaOne, sameBasename); len(got) != 1 ||
		got[0] != sharedIdxName {
		t.Errorf("dropping %s.%s.%s changed %s.%s, whose indexes are now %v. The resolver "+
			"matched a relation name without its schema and edited a relation the "+
			"migration never named",
			ownerSchemaTwo, sameBasename, sharedIdxName, ownerSchemaOne, sameBasename, got)
	}

	// And the mirror image, so the result does not depend on which one is listed
	// first: dropping from the first schema leaves the second alone.
	s = sameNameMatViews()
	applyAll(t, s, migrate.DropIndex{
		Schema: ownerSchemaOne, Table: sameBasename, Name: sharedIdxName,
	})
	if got := indexesOf(t, s, ownerSchemaOne, sameBasename); len(got) != 0 {
		t.Errorf("%s.%s kept %v after its index was dropped",
			ownerSchemaOne, sameBasename, got)
	}
	if got := indexesOf(t, s, ownerSchemaTwo, sameBasename); len(got) != 1 {
		t.Errorf("dropping %s.%s.%s changed %s.%s, whose indexes are now %v",
			ownerSchemaOne, sameBasename, sharedIdxName, ownerSchemaTwo, sameBasename, got)
	}

	// Creating is the same question in the other direction: a new index must
	// land on the named relation rather than on its namesake.
	s = sameNameMatViews()
	applyAll(t, s, migrate.CreateIndex{
		Schema: ownerSchemaTwo, Table: sameBasename,
		Index: schema.Index{Name: "idx_second", Columns: []schema.IndexColumn{{Name: "id"}}},
	})
	if got := indexesOf(t, s, ownerSchemaTwo, sameBasename); len(got) != 2 {
		t.Errorf("%s.%s has %v; the new index did not land on the relation named",
			ownerSchemaTwo, sameBasename, got)
	}
	if got := indexesOf(t, s, ownerSchemaOne, sameBasename); len(got) != 1 {
		t.Errorf("creating an index on %s.%s added one to %s.%s, whose indexes are now %v",
			ownerSchemaTwo, sameBasename, ownerSchemaOne, sameBasename, got)
	}
}
