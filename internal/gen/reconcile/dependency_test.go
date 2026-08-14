package reconcile_test

import (
	"context"
	"math/rand"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/reconcile"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M16.5 F.2.1: dependency reconciliation.
//
// The contract is direct edges on both sides, compared by schema and name. The
// actual side comes from a real database so the filtering is the real filtering.

const depDDL = `
CREATE SCHEMA archive;
CREATE TABLE users          (id bigint PRIMARY KEY, active boolean NOT NULL);
CREATE TABLE accounts       (id bigint PRIMARY KEY, user_id bigint NOT NULL);
CREATE TABLE archive.users  (id bigint PRIMARY KEY);

CREATE VIEW one_source  AS SELECT id FROM users;
CREATE VIEW two_sources AS SELECT u.id, a.id AS aid FROM users u JOIN accounts a ON a.user_id = u.id;
CREATE VIEW on_view     AS SELECT id FROM one_source;
CREATE VIEW archived    AS SELECT id FROM archive.users;
CREATE MATERIALIZED VIEW m_on_view AS SELECT id FROM one_source WITH DATA;
CREATE MATERIALIZED VIEW m_base    AS SELECT id FROM users WITH DATA;
CREATE VIEW on_matview  AS SELECT user_id FROM (SELECT id AS user_id FROM m_base) q;
`

func actualSchema(t *testing.T) *schema.Schema {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, depDDL)
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

func ref(q string) schema.RelationRef {
	i := strings.Index(q, ".")
	return schema.RelationRef{Schema: q[:i], Name: q[i+1:]}
}

func want(name string, deps ...string) schema.View {
	v := schema.View{Schema: "public", Name: name}
	for _, d := range deps {
		v.DependsOn = append(v.DependsOn, ref(d))
	}
	return v
}

func report(desired, actual *schema.Schema) *diag.Report {
	r := &diag.Report{}
	reconcile.CheckDependencies(r, desired, actual)
	return r
}

// A declared set matching the real one is clean, whatever order it is written in.
func TestDeps_matchingSetIsClean(t *testing.T) {
	actual := actualSchema(t)
	for _, order := range [][]string{
		{"public.users", "public.accounts"},
		{"public.accounts", "public.users"},
	} {
		desired := &schema.Schema{Views: []schema.View{want("two_sources", order...)}}
		if r := report(desired, actual); len(r.Findings()) != 0 {
			t.Errorf("order %v reported %v", order, viewCodes(r))
		}
	}
}

// A relation reading something nobody declared.
func TestDeps_undeclaredDependencyIsAnError(t *testing.T) {
	desired := &schema.Schema{Views: []schema.View{want("two_sources", "public.users")}}
	r := report(desired, actualSchema(t))
	if !hasCode(r, diag.E033) {
		t.Fatalf("reading an undeclared relation was accepted: %v", viewCodes(r))
	}
	var msg string
	for _, f := range r.Findings() {
		if f.Code == diag.E033 {
			msg = f.Message
		}
	}
	if !strings.Contains(msg, "public.accounts") {
		t.Errorf("the finding does not name what is read: %s", msg)
	}
}

// A declared edge the relation does not have.
func TestDeps_staleDeclaredDependencyIsAnError(t *testing.T) {
	desired := &schema.Schema{Views: []schema.View{
		want("one_source", "public.users", "public.accounts"),
	}}
	r := report(desired, actualSchema(t))
	if !hasCode(r, diag.E034) {
		t.Fatalf("a declared dependency the view does not have was accepted: %v", viewCodes(r))
	}
}

// Cross-schema names do not collapse.
func TestDeps_crossSchemaIsExact(t *testing.T) {
	// archived reads archive.users; declaring public.users is two findings, not
	// none: one edge missing and one that is not there.
	desired := &schema.Schema{Views: []schema.View{want("archived", "public.users")}}
	r := report(desired, actualSchema(t))
	if !hasCode(r, diag.E033) || !hasCode(r, diag.E034) {
		t.Errorf("an unqualified comparison would have called these equal: %v", viewCodes(r))
	}
}

// Edges are direct, never transitive: a view on a view depends on the view.
func TestDeps_edgesAreDirectNotTransitive(t *testing.T) {
	actual := actualSchema(t)

	// Correct: the view it actually reads.
	if r := report(&schema.Schema{Views: []schema.View{
		want("on_view", "public.one_source"),
	}}, actual); len(r.Findings()) != 0 {
		t.Errorf("declaring the direct edge reported %v", viewCodes(r))
	}
	// Flattened: declaring the base table is wrong in both directions.
	r := report(&schema.Schema{Views: []schema.View{want("on_view", "public.users")}}, actual)
	if !hasCode(r, diag.E033) || !hasCode(r, diag.E034) {
		t.Errorf("a transitive comparison would have accepted the flattened edge: %v", viewCodes(r))
	}
}

// Every relation kind can be depended on and can depend.
func TestDeps_everyKindPair(t *testing.T) {
	actual := actualSchema(t)
	for _, c := range []struct {
		what     string
		desired  *schema.Schema
		wantSize int
	}{
		{"view on a table", &schema.Schema{Views: []schema.View{want("one_source", "public.users")}}, 0},
		{"view on a view", &schema.Schema{Views: []schema.View{want("on_view", "public.one_source")}}, 0},
		{"view on a materialized view", &schema.Schema{Views: []schema.View{want("on_matview", "public.m_base")}}, 0},
		{"materialized view on a view", &schema.Schema{MaterializedViews: []schema.MaterializedView{
			{Schema: "public", Name: "m_on_view", DependsOn: []schema.RelationRef{ref("public.one_source")}},
		}}, 0},
		{"materialized view on a table", &schema.Schema{MaterializedViews: []schema.MaterializedView{
			{Schema: "public", Name: "m_base", DependsOn: []schema.RelationRef{ref("public.users")}},
		}}, 0},
	} {
		t.Run(c.what, func(t *testing.T) {
			if r := report(c.desired, actual); len(r.Findings()) != c.wantSize {
				t.Errorf("%s reported %v", c.what, viewCodes(r))
			}
		})
	}
}

// A relation the desired schema names and the database does not have produces
// no dependency finding: its absence belongs to the entity tier.
func TestDeps_absentRelationIsNotADependencyFinding(t *testing.T) {
	desired := &schema.Schema{Views: []schema.View{want("nowhere", "public.users")}}
	if r := report(desired, actualSchema(t)); len(r.Findings()) != 0 {
		t.Errorf("a relation that does not exist produced %v", viewCodes(r))
	}
}

// Findings are the same, in the same order, whatever order the catalog and the
// declarations arrive in.
func TestDeps_orderIsDeterministic(t *testing.T) {
	actual := actualSchema(t)
	base := &schema.Schema{Views: []schema.View{
		want("two_sources", "public.users"),
		want("one_source", "public.users", "public.accounts"),
		want("archived", "public.users"),
	}}
	first := renderFindings(report(base, actual))

	rng := rand.New(rand.NewSource(1))
	for range 20 {
		shuffled := &schema.Schema{Views: append([]schema.View(nil), base.Views...)}
		rng.Shuffle(len(shuffled.Views), func(i, j int) {
			shuffled.Views[i], shuffled.Views[j] = shuffled.Views[j], shuffled.Views[i]
		})
		// And the catalog's own row order.
		shuffledActual := *actual
		shuffledActual.Views = append([]schema.View(nil), actual.Views...)
		rng.Shuffle(len(shuffledActual.Views), func(i, j int) {
			shuffledActual.Views[i], shuffledActual.Views[j] = shuffledActual.Views[j], shuffledActual.Views[i]
		})
		if got := renderFindings(report(shuffled, &shuffledActual)); got != first {
			t.Fatalf("reordering the input changed the report:\n--- first ---\n%s\n--- now ---\n%s", first, got)
		}
	}
}

func renderFindings(r *diag.Report) string {
	var b strings.Builder
	for _, f := range r.Findings() {
		b.WriteString(string(f.Code) + " " + f.Message + "\n")
	}
	return b.String()
}
