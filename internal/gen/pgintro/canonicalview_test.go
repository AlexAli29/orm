package pgintro_test

import (
	"context"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M16.5 F.2: the actual half of reconciliation, in the same value types the
// desired half is built in.

const canonicalViewDDL = `
CREATE SCHEMA archive;
CREATE TABLE users   (id bigint PRIMARY KEY, email text NOT NULL, active boolean NOT NULL, tags text[] NOT NULL DEFAULT '{}');
CREATE TABLE archive.users (id bigint PRIMARY KEY, email text NOT NULL);
CREATE TABLE accounts (id bigint PRIMARY KEY, user_id bigint NOT NULL REFERENCES users(id));

CREATE VIEW active_users AS SELECT id, email FROM users WHERE active;
CREATE VIEW recent AS SELECT id FROM active_users;
CREATE VIEW two_sources AS SELECT u.id, a.id AS account_id FROM users u JOIN accounts a ON a.user_id = u.id;
CREATE VIEW archived AS SELECT id FROM archive.users;
CREATE VIEW guarded AS SELECT id FROM users;
ALTER VIEW guarded SET (security_barrier = true);

CREATE MATERIALIZED VIEW totals AS SELECT id AS user_id, email, tags FROM users WITH DATA;
CREATE UNIQUE INDEX totals_user_id_key ON totals (user_id);
CREATE INDEX totals_email_lower ON totals (lower(email)) WHERE user_id > 0;
CREATE INDEX totals_tags_gin ON totals USING gin (tags);
CREATE INDEX totals_covering ON totals (user_id) INCLUDE (email);
CREATE INDEX totals_opclass ON totals (email text_pattern_ops);

CREATE MATERIALIZED VIEW from_view AS SELECT id FROM active_users WITH DATA;
`

func canonicalSchema(t *testing.T) *schema.Schema {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, canonicalViewDDL)
	c, err := pgintro.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	s, err := pgintro.Canonical(t.Context(), c, []string{"public", "archive"})
	if err != nil {
		t.Fatalf("reading the canonical schema: %v", err)
	}
	return s
}

func viewNamed(t *testing.T, s *schema.Schema, q string) schema.View {
	t.Helper()
	for _, v := range s.Views {
		if v.Qualified() == q {
			return v
		}
	}
	t.Fatalf("no view %s", q)
	return schema.View{}
}

func matNamed(t *testing.T, s *schema.Schema, q string) schema.MaterializedView {
	t.Helper()
	for _, m := range s.MaterializedViews {
		if m.Qualified() == q {
			return m
		}
	}
	t.Fatalf("no materialized view %s", q)
	return schema.MaterializedView{}
}

// Views and materialized views arrive as their own kinds, with columns.
func TestCanonical_readsViewsAndMatViews(t *testing.T) {
	s := canonicalSchema(t)
	if len(s.Views) != 5 {
		t.Errorf("views = %d, want 5", len(s.Views))
	}
	if len(s.MaterializedViews) != 2 {
		t.Errorf("materialized views = %d, want 2", len(s.MaterializedViews))
	}
	v := viewNamed(t, s, "public.active_users")
	if len(v.Columns) != 2 || v.Columns[0].Name != "id" || v.Columns[1].Name != "email" {
		t.Errorf("columns = %+v; order is the view's output order", v.Columns)
	}
	if v.Definition.Canonical == "" {
		t.Error("no canonical definition")
	}
	if v.Definition.SQL != "" {
		t.Error("the actual schema carries a source definition it cannot know")
	}
	// Every view column is nullable: PostgreSQL records no NOT NULL on one, and
	// the honest answer is that it does not know.
	for _, c := range v.Columns {
		if !c.Nullable {
			t.Errorf("%s is NOT NULL; PostgreSQL records no such thing on a view column", c.Name)
		}
	}
}

// Direct dependencies only, with the kind, exact by schema.
func TestCanonical_dependencies(t *testing.T) {
	s := canonicalSchema(t)

	deps := func(q string) []string {
		var out []string
		for _, d := range viewNamed(t, s, q).DependsOn {
			out = append(out, d.Qualified())
		}
		return out
	}
	if got := deps("public.active_users"); len(got) != 1 || got[0] != "public.users" {
		t.Errorf("active_users depends on %v", got)
	}
	// A view on a view: the edge is direct, not flattened to the base table.
	// Comparing transitive closures would make this read as depending on users.
	if got := deps("public.recent"); len(got) != 1 || got[0] != "public.active_users" {
		t.Errorf("recent depends on %v, want only the view it reads", got)
	}
	if got := deps("public.two_sources"); len(got) != 2 {
		t.Errorf("two_sources depends on %v, want both", got)
	}
	// Cross-schema: archive.users is not public.users.
	if got := deps("public.archived"); len(got) != 1 || got[0] != "archive.users" {
		t.Errorf("archived depends on %v; an unqualified comparison would have "+
			"collapsed this into public.users", got)
	}
	// The kind travels with the edge.
	for _, d := range viewNamed(t, s, "public.recent").DependsOn {
		if !d.KindKnown || d.Kind != schema.KindView {
			t.Errorf("the edge to %s has kind %v (known=%v), want a view",
				d.Name, d.Kind, d.KindKnown)
		}
	}
	// A materialized view reading a view.
	m := matNamed(t, s, "public.from_view")
	if len(m.DependsOn) != 1 || m.DependsOn[0].Qualified() != "public.active_users" {
		t.Errorf("from_view depends on %+v", m.DependsOn)
	}
}

// Indexes come through the reader tables use, with everything it reads.
func TestCanonical_materializedViewIndexes(t *testing.T) {
	m := matNamed(t, canonicalSchema(t), "public.totals")

	byName := map[string]schema.Index{}
	for _, ix := range m.Indexes {
		byName[ix.Name] = ix
	}
	if len(byName) != 5 {
		t.Fatalf("indexes = %d, want 5: %+v", len(byName), m.Indexes)
	}

	if ix := byName["totals_user_id_key"]; !ix.Unique || len(ix.Columns) != 1 || ix.Columns[0].Name != "user_id" {
		t.Errorf("the unique index read as %+v", ix)
	}
	// Partial and expression, both of which disqualify it for CONCURRENTLY.
	if ix := byName["totals_email_lower"]; ix.Where == "" || ix.Columns[0].Expression == "" {
		t.Errorf("the partial expression index read as %+v", ix)
	}
	if ix := byName["totals_tags_gin"]; ix.Method != "gin" {
		t.Errorf("the GIN index has method %q", ix.Method)
	}
	if ix := byName["totals_covering"]; len(ix.Include) != 1 || ix.Include[0] != "email" {
		t.Errorf("INCLUDE read as %+v", ix.Include)
	}
	if ix := byName["totals_opclass"]; ix.Columns[0].OpClass != "text_pattern_ops" {
		t.Errorf("the operator class read as %q", ix.Columns[0].OpClass)
	}

	// Eligibility is derived from these, not stored: only the plain unique one
	// qualifies, and the partial expression index does not.
	got, ok := m.ConcurrentRefreshIndex()
	if !ok || got.Name != "totals_user_id_key" {
		t.Errorf("concurrent-refresh index = %v (%v), want totals_user_id_key", got.Name, ok)
	}
}

// Options are read for ordinary views, and materialized views have none.
func TestCanonical_viewOptions(t *testing.T) {
	s := canonicalSchema(t)
	var found bool
	for _, o := range viewNamed(t, s, "public.guarded").Options {
		if o.Name == "security_barrier" {
			found = true
		}
	}
	if !found {
		t.Error("security_barrier was dropped; reading a view and writing it back " +
			"without it silently changes a security boundary")
	}
	// A materialized view has no Options field at all: PostgreSQL exposes
	// security_barrier, security_invoker and the check option on ordinary views
	// only, so inventing symmetry would mean a field that can never be set.
	_ = matNamed(t, s, "public.totals")
}

// Population is read and is runtime state, not schema identity.
func TestCanonical_populationIsRuntimeState(t *testing.T) {
	if !matNamed(t, canonicalSchema(t), "public.totals").Populated {
		t.Error("a materialized view created WITH DATA reports itself unpopulated")
	}
}
