package migrate_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Replaying a migration must rebuild the state it describes.
//
// This is the half that no planner test reaches. A plan is a list of
// operations; the state those operations produce is what the next migration is
// planned against, and it is rebuilt by applying them in order. G2 changed how
// CreateIndex and DropIndex find the relation they belong to — a table or a
// materialized view — and getting that wrong produces a planner that is correct
// and a project that cannot migrate twice.
//
// The first version of this code resolved tables only, and the failure it
// produced said "table public.user_totals is not in the migration state" about
// an index the same migration had just created.

// applyAll replays operations onto a state, as the migration engine does.
func applyAll(t *testing.T, s *schema.Schema, ops ...migrate.Operation) *schema.Schema {
	t.Helper()
	for i, op := range ops {
		if err := op.Apply(s); err != nil {
			t.Fatalf("operation %d (%s): %v", i, op.Describe(), err)
		}
	}
	return s
}

func TestReplay_indexesOnAMaterializedView(t *testing.T) {
	m := schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id FROM users"},
	}
	idx := schema.Index{Name: "totals_id_key", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}

	s := applyAll(t, &schema.Schema{},
		migrate.CreateMaterializedView{View: m},
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: idx},
	)

	got, ok := findMat(s, "public.totals")
	if !ok {
		t.Fatal("the materialized view is not in the replayed state")
	}
	if len(got.Indexes) != 1 || got.Indexes[0].Name != idx.Name {
		t.Fatalf("indexes = %+v, want the one the migration created", got.Indexes)
	}

	// And dropping it again leaves the relation with none.
	applyAll(t, s, migrate.DropIndex{Schema: "public", Table: "totals", Name: idx.Name})
	got, _ = findMat(s, "public.totals")
	if len(got.Indexes) != 0 {
		t.Errorf("after DropIndex the state still has %+v", got.Indexes)
	}
}

// The same resolver must still find tables. Removing that support is a mutation
// no materialized-view test would notice.
func TestReplay_indexesOnATableStillResolve(t *testing.T) {
	tbl := schema.Table{Schema: "public", Name: "users",
		Columns: []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}}}
	idx := schema.Index{Name: "users_id_idx", Columns: []schema.IndexColumn{{Name: "id"}}}

	s := applyAll(t, &schema.Schema{Tables: []schema.Table{tbl}},
		migrate.CreateIndex{Schema: "public", Table: "users", Index: idx})

	if len(s.Tables) != 1 || len(s.Tables[0].Indexes) != 1 {
		t.Fatalf("the table's indexes are %+v", s.Tables[0].Indexes)
	}
	applyAll(t, s, migrate.DropIndex{Schema: "public", Table: "users", Name: idx.Name})
	if len(s.Tables[0].Indexes) != 0 {
		t.Errorf("after DropIndex the table still has %+v", s.Tables[0].Indexes)
	}
}

// An index on a relation that is in neither list is an error naming both kinds,
// rather than a message about tables that sends the reader looking in the wrong
// place.
func TestReplay_indexOnAnAbsentRelation(t *testing.T) {
	err := migrate.CreateIndex{Schema: "public", Table: "nowhere",
		Index: schema.Index{Name: "x"}}.Apply(&schema.Schema{})
	if err == nil {
		t.Fatal("an index on an absent relation applied")
	}
	if !strings.Contains(err.Error(), "materialized view") {
		t.Errorf("the error mentions only tables: %v", err)
	}
}

// Dropping a materialized view takes its indexes with it, as PostgreSQL does.
func TestReplay_dropTakesTheIndexes(t *testing.T) {
	m := schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id FROM users"},
		Indexes:    []schema.Index{{Name: "totals_id_key", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}},
	}
	s := applyAll(t, &schema.Schema{}, migrate.CreateMaterializedView{View: m},
		migrate.DropMaterializedView{Schema: "public", Name: "totals"})
	if len(s.MaterializedViews) != 0 {
		t.Errorf("the state still holds %+v", s.MaterializedViews)
	}
}

// Creation policy survives a replay, and runtime population never enters it.
func TestReplay_creationPolicyIsStateAndPopulationIsNot(t *testing.T) {
	m := schema.MaterializedView{
		Schema: "public", Name: "empty", WithData: false,
		Definition: schema.Definition{SQL: "SELECT id FROM users"},
		// Whatever a server said about population is not schema, and must not
		// travel into the state a migration is planned against.
		Populated: true,
	}
	s := applyAll(t, &schema.Schema{}, migrate.CreateMaterializedView{View: m})
	got, _ := findMat(s, "public.empty")
	if got.WithData {
		t.Error("WITH NO DATA was lost in the replay")
	}
	if got.Populated {
		t.Error("runtime population entered the migration state")
	}
}

func findMat(s *schema.Schema, qualified string) (schema.MaterializedView, bool) {
	for _, m := range s.MaterializedViews {
		if m.Qualified() == qualified {
			return m, true
		}
	}
	return schema.MaterializedView{}, false
}

// A created materialized view's indexes come from the CreateIndex operations
// that follow it, and from nowhere else.
//
// The plan emits the relation and then one operation per index. If the create
// operation also carried them, replaying the migration would add each index
// twice: once from the relation it describes and once from the CreateIndex.
// The duplicate is invisible in the database — which only ever saw one CREATE
// INDEX — and visible in the migration state, where the next plan finds an
// index the declarations do not have and writes a migration to drop it. That
// migration never converges; it re-plans the same drop and create every run.
//
// The database looked right the whole time, which is why this needed a test
// about state rather than about SQL.
func TestReplay_createdIndexesAreNotCountedTwice(t *testing.T) {
	idx := schema.Index{Name: "totals_id_key", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}
	m := schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id FROM users"},
		Indexes:    []schema.Index{idx},
	}

	// The plan as the planner writes it: the relation without its indexes,
	// then the indexes.
	bare := m.Clone()
	bare.Indexes = nil
	s := applyAll(t, &schema.Schema{},
		migrate.CreateMaterializedView{View: bare},
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: idx},
	)

	got, ok := findMat(s, "public.totals")
	if !ok {
		t.Fatal("the materialized view is not in the replayed state")
	}
	if len(got.Indexes) != 1 {
		t.Fatalf("the replayed state has %d indexes, want one: %+v", len(got.Indexes), got.Indexes)
	}

	// And the state now matches the desired relation, which is what makes the
	// next plan empty.
	if len(m.Indexes) != len(got.Indexes) || m.Indexes[0].Name != got.Indexes[0].Name {
		t.Errorf("the replayed state does not match the declaration:\n state %+v\n want  %+v",
			got.Indexes, m.Indexes)
	}
}

// M16.5 G2 #1: the relation operation contributes no indexes to replay state.
//
// CreateMaterializedView renders CREATE MATERIALIZED VIEW and nothing else, so
// every index comes from a CreateIndex beside it. An artifact whose payload
// carries indexes therefore describes a database that was never built that way,
// and replaying it adds each index twice — once from here and once from the
// operation that really created it.
//
// The planner strips them before writing, so no end-to-end fixture can reach
// this: what it defends against is an artifact the planner did not write, one
// committed before that was fixed or edited by hand. That makes replay the only
// level the property is observable at, and this is that observation, made
// directly rather than as a step in a longer chain.
func TestReplay_theRelationOperationContributesNoIndexes(t *testing.T) {
	m := schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id FROM users"},
		Indexes: []schema.Index{{
			Name: "carried_in", Columns: []schema.IndexColumn{{Name: "id"}},
		}},
	}
	if len(m.Indexes) == 0 {
		t.Fatal("the payload carries no indexes, so dropping them would drop nothing")
	}

	s := applyAll(t, &schema.Schema{}, migrate.CreateMaterializedView{View: m})
	got, ok := findMat(s, "public.totals")
	if !ok {
		t.Fatal("the materialized view is not in the replayed state")
	}
	if len(got.Indexes) != 0 {
		t.Fatalf("the relation operation put %d index(es) into replay state: %+v. The "+
			"CreateIndex that follows adds them again, and the duplicate is invisible in "+
			"the database — which only ever saw one CREATE INDEX — and fatal in the state, "+
			"where the next plan finds an index the declarations do not have",
			len(got.Indexes), got.Indexes)
	}

	// And the index the migration really creates arrives exactly once.
	applyAll(t, s, migrate.CreateIndex{Schema: "public", Table: "totals",
		Index: schema.Index{Name: "carried_in", Columns: []schema.IndexColumn{{Name: "id"}}}})
	got, _ = findMat(s, "public.totals")
	if len(got.Indexes) != 1 {
		t.Errorf("after the CreateIndex the state holds %d copies: %+v", len(got.Indexes), got.Indexes)
	}
}
