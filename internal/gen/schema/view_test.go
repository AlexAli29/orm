package schema

import "testing"

// M16.5: PostgreSQL's rules for REFRESH ... CONCURRENTLY, read off the schema.
//
// PostgreSQL requires a unique index that covers every row and is built from
// plain column names. Each of those words excludes something, and each
// exclusion is a case where offering a concurrent refresh would mean sending a
// statement the server was always going to reject.
//
// What is checked here is everything the schema can prove. The remaining
// requirement — that the view is populated — is runtime state, and the server
// keeps the last word on it.
func TestConcurrentRefreshIndex(t *testing.T) {
	plain := Index{Name: "mv_pkey", Unique: true, Columns: []IndexColumn{{Name: "id"}}}

	for _, c := range []struct {
		what    string
		indexes []Index
		want    string
	}{
		{"a plain unique index", []Index{plain}, "mv_pkey"},
		{"no indexes at all", nil, ""},
		{"only a non-unique index",
			[]Index{{Name: "mv_idx", Columns: []IndexColumn{{Name: "id"}}}}, ""},
		{"a partial unique index, which does not cover every row",
			[]Index{{Name: "mv_partial", Unique: true, Where: "id > 0",
				Columns: []IndexColumn{{Name: "id"}}}}, ""},
		{"an expression unique index, which is not plain column names",
			[]Index{{Name: "mv_expr", Unique: true,
				Columns: []IndexColumn{{Expression: "lower(email)"}}}}, ""},
		{"a unique index with an expression among its columns",
			[]Index{{Name: "mv_mixed", Unique: true,
				Columns: []IndexColumn{{Name: "id"}, {Expression: "lower(email)"}}}}, ""},
		{"a unique index over no columns",
			[]Index{{Name: "mv_empty", Unique: true}}, ""},
		{"several candidates, chosen deterministically",
			[]Index{plain, {Name: "aaa_key", Unique: true, Columns: []IndexColumn{{Name: "id"}}}},
			"aaa_key"},
		{"a qualifying index alongside disqualified ones",
			[]Index{{Name: "mv_partial", Unique: true, Where: "id > 0",
				Columns: []IndexColumn{{Name: "id"}}}, plain}, "mv_pkey"},
	} {
		t.Run(c.what, func(t *testing.T) {
			m := MaterializedView{Schema: "public", Name: "mv", Indexes: c.indexes}
			ix, ok := m.ConcurrentRefreshIndex()
			if c.want == "" {
				if ok {
					t.Errorf("%s was accepted as proof for CONCURRENTLY, and PostgreSQL "+
						"would have rejected the refresh: %s", c.what, ix.Name)
				}
				return
			}
			if !ok {
				t.Fatalf("%s was not accepted", c.what)
			}
			if ix.Name != c.want {
				t.Errorf("chose %s, want %s", ix.Name, c.want)
			}
		})
	}
}

// Definition identity: the server's reconstruction decides, and only when both
// sides have one.
func TestDefinitionSame(t *testing.T) {
	for _, c := range []struct {
		what string
		a, b Definition
		want bool
	}{
		{"two canonical texts that agree",
			Definition{SQL: "select 1", Canonical: " SELECT 1;"},
			Definition{SQL: "SELECT\n  1", Canonical: " SELECT 1;"}, true},
		{"two canonical texts that differ",
			Definition{Canonical: " SELECT 1;"}, Definition{Canonical: " SELECT 2;"}, false},
		{"the project's own text, reformatted, with no canonical form",
			Definition{SQL: "SELECT id\nFROM users"},
			Definition{SQL: "SELECT   id FROM users"}, true},
		{"the project's own text, genuinely changed",
			Definition{SQL: "SELECT id FROM users"},
			Definition{SQL: "SELECT id FROM people"}, false},
		{"canonical on one side only falls back to the written text",
			Definition{SQL: "SELECT 1", Canonical: " SELECT 1;"},
			Definition{SQL: "SELECT 1"}, true},
	} {
		t.Run(c.what, func(t *testing.T) {
			if got := c.a.Same(c.b); got != c.want {
				t.Errorf("Same = %v, want %v", got, c.want)
			}
			if got := c.b.Same(c.a); got != c.want {
				t.Errorf("Same is not symmetric: reversed = %v, want %v", got, c.want)
			}
		})
	}
}

// One name is one relation, and the schema answers which kind it is.
func TestSchemaRelation(t *testing.T) {
	s := &Schema{
		Tables:            []Table{{Schema: "public", Name: "users"}},
		Views:             []View{{Schema: "public", Name: "active_users"}},
		MaterializedViews: []MaterializedView{{Schema: "public", Name: "totals"}},
	}
	for _, c := range []struct {
		name   string
		want   RelationKind
		exists bool
	}{
		{"users", KindTable, true},
		{"active_users", KindView, true},
		{"totals", KindMaterializedView, true},
		{"nothing", KindTable, false},
	} {
		got, ok := s.Relation("public", c.name)
		if ok != c.exists {
			t.Errorf("%s: exists = %v, want %v", c.name, ok, c.exists)
		}
		if ok && got != c.want {
			t.Errorf("%s is %v, want %v", c.name, got, c.want)
		}
	}
}
