package lock_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 Stage F.1: what a view contributes to the lock, and what it must not.
//
// The lock is the project's commitment, and it has to be byte-identical on
// PostgreSQL 14 and 18. Everything that would break that — a deparsed
// definition, a server version, an OID, a population state, a path — is a thing
// this file exists to keep out.

func demoSchema() *schema.Schema {
	return &schema.Schema{
		Tables: []schema.Table{{Schema: "public", Name: "users"}},
		Views: []schema.View{{
			Schema: "public", Name: "active_users",
			Columns: []schema.Column{
				{Name: "id", Type: schema.Type{Name: "int8"}},
				{Name: "email", Type: schema.Type{Name: "text"}, Nullable: true},
			},
			Definition: schema.Definition{
				SQL: "SELECT id, email FROM users WHERE active",
				// A canonical text is present and must not reach the lock.
				Canonical: " SELECT users.id,\n    users.email\n   FROM users\n  WHERE users.active;",
			},
			DependsOn: []schema.RelationRef{{Schema: "public", Name: "users"}},
		}},
		MaterializedViews: []schema.MaterializedView{{
			Schema: "public", Name: "totals",
			Columns:    []schema.Column{{Name: "user_id", Type: schema.Type{Name: "int8"}}},
			Definition: schema.Definition{SQL: "SELECT id AS user_id FROM users"},
			DependsOn:  []schema.RelationRef{{Schema: "public", Name: "users"}},
			WithData:   true,
			// Runtime state, deliberately set, and it must not appear.
			Populated: true,
			Indexes: []schema.Index{{
				Name: "totals_user_id_key", Unique: true,
				Columns: []schema.IndexColumn{{Name: "user_id"}},
			}},
		}},
	}
}

// The lock records the portable identity and never the server's text.
func TestLock_recordsPortableIdentityOnly(t *testing.T) {
	got := lock.FingerprintSchema(demoSchema())

	for _, want := range []string{
		"view public.active_users",
		"materialized-view public.totals",
		"definition raw v1:",
		"column id int8 not-null",
		"column email text null",
		"depends-on public.users",
		"create with-data",
		"index totals_user_id_key unique user_id",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the lock does not contain %q:\n%s", want, got)
		}
	}

	// The things that must never be there. Each of these would make the lock
	// depend on a server rather than on the project.
	for _, forbidden := range []struct{ what, text string }{
		{"the server's deparsed definition", "users.active"},
		{"the raw definition text", "SELECT id, email FROM users"},
		{"a population state", "populated"},
		{"an OID", "oid"},
		{"a server version", "server_version"},
		{"an absolute path", "/"},
	} {
		if strings.Contains(got, forbidden.text) {
			t.Errorf("the lock contains %s (%q):\n%s", forbidden.what, forbidden.text, got)
		}
	}
}

// A project with no views writes nothing, so an old lock stays valid.
func TestLock_viewsAreAdditive(t *testing.T) {
	if got := lock.FingerprintSchema(&schema.Schema{
		Tables: []schema.Table{{Schema: "public", Name: "users"}},
	}); got != "" {
		t.Errorf("a project with no views contributed %q to the lock", got)
	}
	if got := lock.FingerprintSchema(nil); got != "" {
		t.Errorf("a nil schema contributed %q", got)
	}
}

// The identity is the declaration's, so it cannot vary by server.
func TestLock_identityIsIndependentOfCanonicalText(t *testing.T) {
	base := demoSchema()
	first := lock.FingerprintSchema(base)

	// The same project as seen through a different PostgreSQL major: the
	// canonical text differs because the deparser changed in 16, and nothing
	// the project declared has moved.
	other := demoSchema()
	other.Views[0].Definition.Canonical = " SELECT id,\n    email\n   FROM users\n  WHERE active;"
	if second := lock.FingerprintSchema(other); second != first {
		t.Errorf("the deparser's spelling changed the lock:\n%s", diffLines(first, second))
	}

	// And a runtime refresh does not either.
	other = demoSchema()
	other.MaterializedViews[0].Populated = false
	if second := lock.FingerprintSchema(other); second != first {
		t.Errorf("emptying a materialized view changed the lock:\n%s", diffLines(first, second))
	}
}

// Declaration order does not matter; the set does.
func TestLock_orderIndependence(t *testing.T) {
	base := demoSchema()
	base.Views[0].DependsOn = []schema.RelationRef{
		{Schema: "public", Name: "users"}, {Schema: "archive", Name: "users"},
	}
	first := lock.FingerprintSchema(base)

	reordered := demoSchema()
	reordered.Views[0].DependsOn = []schema.RelationRef{
		{Schema: "archive", Name: "users"}, {Schema: "public", Name: "users"},
	}
	if second := lock.FingerprintSchema(reordered); second != first {
		t.Errorf("reordering //orm:depends-on changed the lock:\n%s", diffLines(first, second))
	}

	// Two views declared in either order render the same.
	a := demoSchema()
	a.Views = append(a.Views, schema.View{
		Schema: "public", Name: "aaa",
		Definition: schema.Definition{SQL: "SELECT 1"},
	})
	b := demoSchema()
	b.Views = append([]schema.View{{
		Schema: "public", Name: "aaa",
		Definition: schema.Definition{SQL: "SELECT 1"},
	}}, b.Views...)
	if lock.FingerprintSchema(a) != lock.FingerprintSchema(b) {
		t.Error("declaring two views in the other order changed the lock")
	}
}

// Semantic changes do change it.
func TestLock_semanticChangesMove(t *testing.T) {
	first := lock.FingerprintSchema(demoSchema())

	for _, c := range []struct {
		what   string
		mutate func(*schema.Schema)
	}{
		{"a changed predicate", func(s *schema.Schema) {
			s.Views[0].Definition.SQL = "SELECT id, email FROM users WHERE NOT active"
		}},
		{"a reformatted definition", func(s *schema.Schema) {
			s.Views[0].Definition.SQL = "SELECT id, email\nFROM users\nWHERE active"
		}},
		{"a renamed column", func(s *schema.Schema) { s.Views[0].Columns[1].Name = "user_email" }},
		{"a reordered column", func(s *schema.Schema) {
			s.Views[0].Columns[0], s.Views[0].Columns[1] = s.Views[0].Columns[1], s.Views[0].Columns[0]
		}},
		{"a changed type", func(s *schema.Schema) { s.Views[0].Columns[0].Type = schema.Type{Name: "int4"} }},
		{"a changed nullability", func(s *schema.Schema) { s.Views[0].Columns[0].Nullable = true }},
		{"an added dependency", func(s *schema.Schema) {
			s.Views[0].DependsOn = append(s.Views[0].DependsOn,
				schema.RelationRef{Schema: "public", Name: "orders"})
		}},
		{"a changed creation policy", func(s *schema.Schema) { s.MaterializedViews[0].WithData = false }},
		{"a dropped index", func(s *schema.Schema) { s.MaterializedViews[0].Indexes = nil }},
		{"an index that stopped being unique", func(s *schema.Schema) {
			s.MaterializedViews[0].Indexes[0].Unique = false
		}},
		{"a view becoming a materialized view", func(s *schema.Schema) {
			v := s.Views[0]
			s.Views = nil
			s.MaterializedViews = append(s.MaterializedViews, schema.MaterializedView{
				Schema: v.Schema, Name: v.Name, Columns: v.Columns,
				Definition: v.Definition, DependsOn: v.DependsOn, WithData: true,
			})
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := demoSchema()
			c.mutate(s)
			if lock.FingerprintSchema(s) == first {
				t.Errorf("%s did not change the lock", c.what)
			}
		})
	}
}

// Reformatting changing the lock is the documented contract, pinned so that a
// later maintainer cannot quietly "fix" it by adding whitespace normalization —
// which would need a tokenizer that knows a space inside a literal is data.
func TestLock_reformattingChangesIdentityByContract(t *testing.T) {
	a := schema.SourceIdentityOf("SELECT id FROM users WHERE active")
	b := schema.SourceIdentityOf("SELECT id\nFROM users\nWHERE active")
	if a == b {
		t.Error("raw formatting stopped affecting SourceIdentity. That is a change to a " +
			"documented contract, not a bug fix: making identity formatting-independent " +
			"requires a SQL tokenizer, and an approximate one calls two different " +
			"definitions equal. Update docs/view-identity.md and the lock contract with it")
	}
}

func diffLines(a, b string) string {
	al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for i := range max(len(al), len(bl)) {
		var x, y string
		if i < len(al) {
			x = al[i]
		}
		if i < len(bl) {
			y = bl[i]
		}
		if x != y {
			out = append(out, "-"+x, "+"+y)
		}
	}
	return strings.Join(out, "\n")
}
