package migrate_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 G1: what the planner will and will not write.
//
// The rule under test is PostgreSQL's own: a replacement must keep every
// existing output column's position, name and type, and may append after them.
// Everything else is a refusal, and each refusal is a case where a
// plausible-looking statement would do real damage.

func vcol(name, typ string) schema.Column {
	return schema.Column{Name: name, Type: schema.Type{Name: typ}}
}

func vview(name string, sql string, cols ...schema.Column) schema.View {
	return schema.View{
		Schema: "public", Name: name, Columns: cols,
		Definition: schema.Definition{SQL: sql},
	}
}

func vplan(t *testing.T, state, desired *schema.Schema, in migrate.ViewPlanInput) (migrate.Diff, error) {
	t.Helper()
	return migrate.Compute(state, desired, migrate.Options{Views: in})
}

func vops(d migrate.Diff) []string {
	var out []string
	for _, o := range d.Operations {
		out = append(out, o.Describe())
	}
	return out
}

// A new view is created.
func TestPlan_createsANewView(t *testing.T) {
	desired := &schema.Schema{Views: []schema.View{
		vview("active_users", "SELECT id, email FROM users WHERE active",
			vcol("id", "int8"), vcol("email", "text")),
	}}
	d, err := vplan(t, &schema.Schema{}, desired, migrate.ViewPlanInput{})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if got := vops(d); len(got) != 1 || got[0] != "create view public.active_users" {
		t.Fatalf("operations = %v", got)
	}
	sql, err := d.Operations[0].SQL()
	if err != nil {
		t.Fatal(err)
	}
	stmt := strings.Join(sql, "\n")
	if !strings.Contains(stmt, `CREATE VIEW "public"."active_users" AS`) {
		t.Errorf("the statement is not a quoted, schema-qualified CREATE VIEW:\n%s", stmt)
	}
	if !strings.Contains(stmt, "SELECT id, email FROM users WHERE active") {
		t.Errorf("the body did not survive:\n%s", stmt)
	}
	// The body is SQL, not an identifier and not a literal.
	if strings.Contains(stmt, `'SELECT`) || strings.Contains(stmt, `"SELECT`) {
		t.Errorf("the definition body was quoted:\n%s", stmt)
	}
}

// The replacement matrix. The first is the useful case; the rest are refusals.
func TestPlan_replacementMatrix(t *testing.T) {
	old := vview("v", "SELECT id, email FROM users WHERE active",
		vcol("id", "int8"), vcol("email", "text"))
	state := &schema.Schema{Views: []schema.View{old}}
	recorded := map[string]string{"public.v": " SELECT id, email FROM users WHERE active;"}
	actual := &schema.Schema{Views: []schema.View{{
		Schema: "public", Name: "v",
		Definition: schema.Definition{Canonical: recorded["public.v"]},
		Columns:    old.Columns,
	}}}
	in := migrate.ViewPlanInput{Actual: actual, Recorded: recorded, Online: true}

	for _, c := range []struct {
		what    string
		desired schema.View
		wantOp  string
		refuse  string
	}{
		{
			"a changed predicate", // The main useful path.
			vview("v", "SELECT id, email FROM users WHERE NOT active",
				vcol("id", "int8"), vcol("email", "text")),
			"replace view public.v", "",
		},
		{
			"a join changed to an outer join",
			vview("v", "SELECT u.id, u.email FROM users u LEFT JOIN a ON a.id = u.id",
				vcol("id", "int8"), vcol("email", "text")),
			"replace view public.v", "",
		},
		{
			"a function swapped for another of the same type",
			vview("v", "SELECT id, lower(email) AS email FROM users",
				vcol("id", "int8"), vcol("email", "text")),
			"replace view public.v", "",
		},
		{
			"a column appended after the existing ones",
			vview("v", "SELECT id, email, created_at FROM users",
				vcol("id", "int8"), vcol("email", "text"), vcol("created_at", "timestamptz")),
			"replace view public.v", "",
		},
		{
			"a renamed output column",
			vview("v", "SELECT id, email AS email_address FROM users",
				vcol("id", "int8"), vcol("email_address", "text")),
			"", "keep their names",
		},
		{
			"a removed output column",
			vview("v", "SELECT id FROM users", vcol("id", "int8")),
			"", "lose 1 output column",
		},
		{
			"reordered output columns",
			vview("v", "SELECT email, id FROM users", vcol("email", "text"), vcol("id", "int8")),
			"", "keep their names",
		},
		{
			"a widened output type",
			vview("v", "SELECT id::int4, email FROM users", vcol("id", "int4"), vcol("email", "text")),
			"", "keep their exact type",
		},
		{
			"a column inserted in the middle",
			vview("v", "SELECT id, extra, email FROM users",
				vcol("id", "int8"), vcol("extra", "text"), vcol("email", "text")),
			"", "keep their names",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			d, err := vplan(t, state, &schema.Schema{Views: []schema.View{c.desired}}, in)
			if c.refuse != "" {
				if err == nil {
					t.Fatalf("%s was accepted; operations = %v", c.what, vops(d))
				}
				if !strings.Contains(err.Error(), c.refuse) {
					t.Errorf("the refusal does not say %q: %v", c.refuse, err)
				}
				// A refusal writes nothing at all.
				if len(d.Operations) != 0 {
					t.Errorf("a refused plan carried %v", vops(d))
				}
				return
			}
			if err != nil {
				t.Fatalf("%s was refused: %v", c.what, err)
			}
			if got := vops(d); len(got) != 1 || got[0] != c.wantOp {
				t.Errorf("operations = %v, want %s", got, c.wantOp)
			}
		})
	}
}

// Nullability is not part of the replacement proof: PostgreSQL records none on
// a view column and does not consult it, so refusing over it would refuse a
// legal migration on metadata the server never looks at.
func TestPlan_nullabilityIsNotPartOfReplacementLegality(t *testing.T) {
	old := vview("v", "SELECT id FROM users", vcol("id", "int8"))
	state := &schema.Schema{Views: []schema.View{old}}
	nullable := vcol("id", "int8")
	nullable.Nullable = true
	desired := vview("v", "SELECT id FROM users LEFT JOIN a ON true", nullable)

	// Online, because an unproved replacement is refused for a different reason
	// and would hide what this test is about.
	in := migrate.ViewPlanInput{
		Actual: &schema.Schema{Views: []schema.View{{Schema: "public", Name: "v",
			Definition: schema.Definition{Canonical: "c"}}}},
		Recorded: map[string]string{"public.v": "c"},
		Online:   true,
	}
	d, err := vplan(t, state, &schema.Schema{Views: []schema.View{desired}}, in)
	if err != nil {
		t.Fatalf("a change of ORM nullability metadata blocked a legal replacement: %v", err)
	}
	if got := vops(d); len(got) != 1 {
		t.Errorf("operations = %v", got)
	}
}

// An unchanged declaration plans nothing, which is what makes a second
// makemigrations empty.
func TestPlan_unchangedViewPlansNothing(t *testing.T) {
	v := vview("v", "SELECT id FROM users", vcol("id", "int8"))
	d, err := vplan(t, &schema.Schema{Views: []schema.View{v}},
		&schema.Schema{Views: []schema.View{v}}, migrate.ViewPlanInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Operations) != 0 {
		t.Errorf("an unchanged view planned %v", vops(d))
	}
}

// The four refusals that are about the database rather than the shape.
func TestPlan_refusesUnprovableTransitions(t *testing.T) {
	v := vview("v", "SELECT id FROM users", vcol("id", "int8"))
	changed := vview("v", "SELECT id FROM users WHERE active", vcol("id", "int8"))

	t.Run("a name a table already holds", func(t *testing.T) {
		state := &schema.Schema{Tables: []schema.Table{{Schema: "public", Name: "v"}}}
		_, err := vplan(t, state, &schema.Schema{Views: []schema.View{v}}, migrate.ViewPlanInput{})
		if err == nil || !strings.Contains(err.Error(), "already has a table") {
			t.Fatalf("creating a view over a table's name: %v", err)
		}
	})

	t.Run("an existing view this project never applied", func(t *testing.T) {
		actual := &schema.Schema{Views: []schema.View{{Schema: "public", Name: "v"}}}
		_, err := vplan(t, &schema.Schema{}, &schema.Schema{Views: []schema.View{v}},
			migrate.ViewPlanInput{Actual: actual, Online: true})
		if err == nil || !strings.Contains(err.Error(), "no migration of this project created it") {
			t.Fatalf("adopting an unknown view: %v", err)
		}
	})

	t.Run("a replacement with no recorded provenance", func(t *testing.T) {
		state := &schema.Schema{Views: []schema.View{v}}
		actual := &schema.Schema{Views: []schema.View{{Schema: "public", Name: "v"}}}
		_, err := vplan(t, state, &schema.Schema{Views: []schema.View{changed}},
			migrate.ViewPlanInput{Actual: actual, Online: true})
		if err == nil || !strings.Contains(err.Error(), "nothing to compare") {
			t.Fatalf("replacing without provenance: %v", err)
		}
	})

	t.Run("a database that drifted since it was applied", func(t *testing.T) {
		state := &schema.Schema{Views: []schema.View{v}}
		actual := &schema.Schema{Views: []schema.View{{
			Schema: "public", Name: "v",
			Definition: schema.Definition{Canonical: " SELECT id FROM users WHERE something_else;"},
		}}}
		_, err := vplan(t, state, &schema.Schema{Views: []schema.View{changed}},
			migrate.ViewPlanInput{
				Actual:   actual,
				Recorded: map[string]string{"public.v": " SELECT id FROM users;"},
				Online:   true,
			})
		if err == nil || !strings.Contains(err.Error(), "outside the migrations") {
			t.Fatalf("a source change overwrote unknown manual drift: %v", err)
		}
	})
}

// Creation order follows dependencies; drops reverse it; neither uses CASCADE.
func TestPlan_dependencyOrdering(t *testing.T) {
	dep := func(q string) schema.RelationRef {
		i := strings.Index(q, ".")
		return schema.RelationRef{Schema: q[:i], Name: q[i+1:]}
	}
	a := vview("a", "SELECT id FROM users", vcol("id", "int8"))
	b := vview("b", "SELECT id FROM a", vcol("id", "int8"))
	b.DependsOn = []schema.RelationRef{dep("public.a")}
	c := vview("c", "SELECT id FROM b", vcol("id", "int8"))
	c.DependsOn = []schema.RelationRef{dep("public.b")}

	// Declared in the wrong order on purpose.
	desired := &schema.Schema{Views: []schema.View{c, a, b}}
	d, err := vplan(t, &schema.Schema{}, desired, migrate.ViewPlanInput{})
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	want := []string{"create view public.a", "create view public.b", "create view public.c"}
	got := vops(d)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("creation order = %v, want %v", got, want)
	}

	// Dropping all three reverses it: a dependent goes before what it reads, so
	// nothing needs CASCADE.
	d, err = vplan(t, desired, &schema.Schema{}, migrate.ViewPlanInput{})
	if err != nil {
		t.Fatalf("planning drops: %v", err)
	}
	got = vops(d)
	wantDrops := []string{"drop view public.c", "drop view public.b", "drop view public.a"}
	for i := range wantDrops {
		if got[i] != wantDrops[i] {
			t.Fatalf("drop order = %v, want %v", got, wantDrops)
		}
	}
	for _, o := range d.Operations {
		sql, err := o.SQL()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range sql {
			if strings.Contains(strings.ToUpper(s), "CASCADE") {
				t.Errorf("a drop used CASCADE: %s", s)
			}
			if !strings.Contains(strings.ToUpper(s), "RESTRICT") {
				t.Errorf("a drop does not say RESTRICT, so a reader cannot see CASCADE "+
					"was not chosen: %s", s)
			}
		}
	}
}

// A diamond is ordered legally and deterministically.
func TestPlan_diamond(t *testing.T) {
	dep := func(qs ...string) []schema.RelationRef {
		var out []schema.RelationRef
		for _, q := range qs {
			i := strings.Index(q, ".")
			out = append(out, schema.RelationRef{Schema: q[:i], Name: q[i+1:]})
		}
		return out
	}
	a := vview("a", "SELECT id FROM users", vcol("id", "int8"))
	b := vview("b", "SELECT id FROM a", vcol("id", "int8"))
	b.DependsOn = dep("public.a")
	c := vview("c", "SELECT id FROM a", vcol("id", "int8"))
	c.DependsOn = dep("public.a")
	dv := vview("d", "SELECT id FROM b", vcol("id", "int8"))
	dv.DependsOn = dep("public.b", "public.c")

	var first []string
	for _, order := range [][]schema.View{{a, b, c, dv}, {dv, c, b, a}, {c, dv, a, b}} {
		got, err := vplan(t, &schema.Schema{}, &schema.Schema{Views: order}, migrate.ViewPlanInput{})
		if err != nil {
			t.Fatalf("planning: %v", err)
		}
		names := vops(got)
		if first == nil {
			first = names
		} else if strings.Join(names, ",") != strings.Join(first, ",") {
			t.Fatalf("declaration order changed the migration:\n%v\n%v", first, names)
		}
		// And it is topologically legal whatever it is.
		pos := map[string]int{}
		for i, n := range names {
			pos[strings.TrimPrefix(n, "create view ")] = i
		}
		for _, e := range [][2]string{{"public.a", "public.b"}, {"public.a", "public.c"},
			{"public.b", "public.d"}, {"public.c", "public.d"}} {
			if pos[e[0]] > pos[e[1]] {
				t.Errorf("%s was created after %s: %v", e[0], e[1], names)
			}
		}
	}
}

// A cycle writes nothing.
func TestPlan_cycleWritesNothing(t *testing.T) {
	a := vview("a", "SELECT 1", vcol("id", "int8"))
	a.DependsOn = []schema.RelationRef{{Schema: "public", Name: "b"}}
	b := vview("b", "SELECT 1", vcol("id", "int8"))
	b.DependsOn = []schema.RelationRef{{Schema: "public", Name: "a"}}

	d, err := vplan(t, &schema.Schema{}, &schema.Schema{Views: []schema.View{a, b}},
		migrate.ViewPlanInput{})
	if err == nil {
		t.Fatal("a cycle produced a migration")
	}
	if !strings.Contains(err.Error(), "depend on each other") {
		t.Errorf("the error does not name the cycle: %v", err)
	}
	if len(d.Operations) != 0 {
		t.Errorf("a refused plan carried %v", vops(d))
	}
}

// A drop refused because something outside the project reads it.
func TestPlan_refusesToDropSomethingStillRead(t *testing.T) {
	v := vview("v", "SELECT id FROM users", vcol("id", "int8"))
	actual := &schema.Schema{Views: []schema.View{
		{Schema: "public", Name: "v"},
		{Schema: "public", Name: "unmanaged",
			DependsOn: []schema.RelationRef{{Schema: "public", Name: "v"}}},
	}}
	_, err := vplan(t, &schema.Schema{Views: []schema.View{v}}, &schema.Schema{},
		migrate.ViewPlanInput{Actual: actual, Online: true})
	if err == nil {
		t.Fatal("a view was dropped while something still reads it")
	}
	if !strings.Contains(err.Error(), "public.unmanaged") {
		t.Errorf("the refusal does not name the dependent: %v", err)
	}
	if strings.Contains(err.Error(), "CASCADE would") == false {
		t.Errorf("the refusal does not explain why CASCADE is not the answer: %v", err)
	}
}

// Materialized views are still refused, and the message says why.
func TestPlan_materializedViewIsCreatedWithItsIndexesAfterIt(t *testing.T) {
	desired := &schema.Schema{MaterializedViews: []schema.MaterializedView{{
		Schema: "public", Name: "m", WithData: true,
		Definition: schema.Definition{SQL: "SELECT 1 AS one"},
		Indexes:    []schema.Index{{Name: "m_one_key", Unique: true, Columns: []schema.IndexColumn{{Name: "one"}}}},
	}}}
	d, err := vplan(t, &schema.Schema{}, desired, migrate.ViewPlanInput{})
	if err != nil {
		t.Fatalf("planning a new materialized view: %v", err)
	}
	ops := vops(d)
	if len(ops) != 2 {
		t.Fatalf("operations = %v, want a create and an index", ops)
	}
	if _, ok := d.Operations[0].(migrate.CreateMaterializedView); !ok {
		t.Errorf("first operation is %T, want CreateMaterializedView", d.Operations[0])
	}
	// There is no order in which an index precedes the relation it is built on.
	if _, ok := d.Operations[1].(migrate.CreateIndex); !ok {
		t.Errorf("second operation is %T, want CreateIndex", d.Operations[1])
	}
}

// The central G2 invariant: a definition that moved is refused, and nothing is
// written. PostgreSQL has no in-place replacement, so the only automatic
// alternative would be a drop and a create that discards the stored rows.
func TestPlan_materializedViewDefinitionChangeIsRefused(t *testing.T) {
	have := schema.MaterializedView{
		Schema: "public", Name: "m", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id FROM t WHERE active"},
	}
	want := have
	want.Definition = schema.Definition{SQL: "SELECT id FROM t WHERE active AND verified"}

	state := &schema.Schema{MaterializedViews: []schema.MaterializedView{have}}
	desired := &schema.Schema{MaterializedViews: []schema.MaterializedView{want}}

	d, err := vplan(t, state, desired, migrate.ViewPlanInput{
		Online:   true,
		Recorded: map[string]string{"public.m": "SELECT id FROM t WHERE active"},
		Actual: &schema.Schema{MaterializedViews: []schema.MaterializedView{{
			Schema: "public", Name: "m",
			Definition: schema.Definition{Canonical: "SELECT id FROM t WHERE active"},
		}}},
	})
	if err == nil {
		t.Fatal("a changed materialized-view definition was planned")
	}
	for _, want := range []string{"definition changed", "CREATE OR REPLACE MATERIALIZED VIEW"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if len(d.Operations) != 0 {
		t.Errorf("a refused plan carried %v", vops(d))
	}
}

// An index change on an unchanged materialized view is planned as index
// operations and nothing else. The relation is never recreated to change one.
func TestPlan_materializedViewIndexChangeDoesNotRecreateIt(t *testing.T) {
	def := schema.Definition{SQL: "SELECT id FROM t"}
	have := schema.MaterializedView{Schema: "public", Name: "m", WithData: true, Definition: def}
	want := have
	want.Indexes = []schema.Index{{Name: "m_id_key", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}}

	d, err := vplan(t,
		&schema.Schema{MaterializedViews: []schema.MaterializedView{have}},
		&schema.Schema{MaterializedViews: []schema.MaterializedView{want}},
		migrate.ViewPlanInput{
			Online:   true,
			Recorded: map[string]string{"public.m": "SELECT id FROM t"},
			Actual: &schema.Schema{MaterializedViews: []schema.MaterializedView{{
				Schema: "public", Name: "m", Definition: schema.Definition{Canonical: "SELECT id FROM t"},
			}}},
		})
	if err != nil {
		t.Fatalf("planning an index change: %v", err)
	}
	if len(d.Operations) != 1 {
		t.Fatalf("operations = %v, want one CreateIndex", vops(d))
	}
	if _, ok := d.Operations[0].(migrate.CreateIndex); !ok {
		t.Errorf("operation is %T, want CreateIndex", d.Operations[0])
	}
	for _, op := range d.Operations {
		switch op.(type) {
		case migrate.CreateMaterializedView, migrate.DropMaterializedView:
			t.Errorf("changing an index recreated the materialized view: %T", op)
		}
	}
}

// Creation policy applies at CREATE and nowhere else, so changing it on a
// relation that already exists is refused rather than read as a REFRESH.
func TestPlan_materializedViewCreationPolicyChangeIsRefused(t *testing.T) {
	def := schema.Definition{SQL: "SELECT id FROM t"}
	have := schema.MaterializedView{Schema: "public", Name: "m", WithData: true, Definition: def}
	want := have
	want.WithData = false

	_, err := vplan(t,
		&schema.Schema{MaterializedViews: []schema.MaterializedView{have}},
		&schema.Schema{MaterializedViews: []schema.MaterializedView{want}},
		migrate.ViewPlanInput{
			Online:   true,
			Recorded: map[string]string{"public.m": "SELECT id FROM t"},
			Actual: &schema.Schema{MaterializedViews: []schema.MaterializedView{{
				Schema: "public", Name: "m", Definition: schema.Definition{Canonical: "SELECT id FROM t"},
			}}},
		})
	if err == nil {
		t.Fatal("a creation-policy change was planned")
	}
	if !strings.Contains(err.Error(), "creation policy changed") {
		t.Errorf("the refusal changed: %v", err)
	}
}

// A materialized view of this name that no migration of this project created is
// not adopted: its rows were computed from a body nothing here has read.
func TestPlan_materializedViewWithoutProvenanceIsRefused(t *testing.T) {
	want := schema.MaterializedView{
		Schema: "public", Name: "m", WithData: true,
		Definition: schema.Definition{SQL: "SELECT id FROM t"},
	}
	_, err := vplan(t, &schema.Schema{},
		&schema.Schema{MaterializedViews: []schema.MaterializedView{want}},
		migrate.ViewPlanInput{
			Online: true,
			Actual: &schema.Schema{MaterializedViews: []schema.MaterializedView{{
				Schema: "public", Name: "m",
				Definition: schema.Definition{Canonical: "SELECT id FROM t"},
			}}},
		})
	if err == nil {
		t.Fatal("an unmanaged materialized view was adopted")
	}
	if !strings.Contains(err.Error(), "no migration of this project created it") {
		t.Errorf("the refusal changed: %v", err)
	}
}

// An offline plan refuses replacement rather than allowing it.
//
// Both proofs a replacement needs — that the database holds what this project
// applied, and that nothing has changed it since — require a database. Without
// one, neither was made, and this planner's rule is that what it cannot prove it
// refuses. Creation is unaffected: an absent relation has nothing to overwrite.
func TestPlan_offlineReplacementIsRefused(t *testing.T) {
	old := vview("v", "SELECT id FROM users", vcol("id", "int8"))
	changed := vview("v", "SELECT id FROM users WHERE active", vcol("id", "int8"))

	_, err := vplan(t, &schema.Schema{Views: []schema.View{old}},
		&schema.Schema{Views: []schema.View{changed}}, migrate.ViewPlanInput{})
	if err == nil {
		t.Fatal("an offline plan replaced a view without checking what it was replacing")
	}
	if !strings.Contains(err.Error(), "no database was consulted") {
		t.Errorf("the refusal does not say why: %v", err)
	}

	// Creation still works offline.
	if _, err := vplan(t, &schema.Schema{},
		&schema.Schema{Views: []schema.View{old}}, migrate.ViewPlanInput{}); err != nil {
		t.Errorf("an offline plan refused to create a new view: %v", err)
	}
}
