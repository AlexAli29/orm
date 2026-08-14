package reconcile_test

import (
	"context"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/reconcile"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// M16.5 Stage F: reconciling views.
//
// The findings go into the same report the table tiers produce, and the tests
// assert codes rather than prose so they keep working when a message improves.

const viewBase = `
CREATE TABLE users (
	id     bigint PRIMARY KEY,
	email  text NOT NULL,
	active boolean NOT NULL
);
`

// fixture builds a database, applies a view the way a migration would — create,
// then record what the server made of it — and returns the connection.
func fixture(t *testing.T, ddl string, views ...[3]string) *pgx.Conn {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, viewBase+ddl)
	c, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })

	if len(views) > 0 {
		if err := migrate.New(c, nil).EnsureViewState(t.Context()); err != nil {
			t.Fatalf("creating the view state table: %v", err)
		}
	}
	for _, v := range views {
		name, kind, sql := v[0], v[1], v[2]
		if err := migrate.RecordView(t.Context(), c, "public", name, kind,
			string(schema.SourceIdentityOf(sql))); err != nil {
			t.Fatalf("recording %s: %v", name, err)
		}
	}
	return c
}

// entity stands in for a scanned declaration.
func entity(name, relation string, kind model.RelationKind) *model.GoEntity {
	return &model.GoEntity{
		Name: name, PkgPath: "example.com/p", PkgName: "p",
		Table: model.TableRef{Schema: "public", Name: relation},
		Kind:  kind,
	}
}

func introspect(t *testing.T, c *pgx.Conn) *model.Schema {
	t.Helper()
	s, err := pgintro.Introspect(t.Context(), c, []string{"public"})
	if err != nil {
		t.Fatalf("introspecting: %v", err)
	}
	return s
}

func viewCodes(r *diag.Report) []string {
	var out []string
	for _, f := range r.Findings() {
		out = append(out, string(f.Code))
	}
	return out
}

func hasCode(r *diag.Report, code diag.Code) bool {
	for _, f := range r.Findings() {
		if f.Code == code {
			return true
		}
	}
	return false
}

func check(t *testing.T, c *pgx.Conn, entities ...*model.GoEntity) *diag.Report {
	t.Helper()
	report := &diag.Report{}
	err := reconcile.CheckDefinitions(t.Context(), report, reconcile.DefinitionInput{
		Entities: entities, Schema: introspect(t, c), Reader: c,
	})
	if err != nil {
		t.Fatalf("checking definitions: %v", err)
	}
	return report
}

// A view applied through a migration reconciles clean.
func TestView_appliedDefinitionIsClean(t *testing.T) {
	const sql = "SELECT id, email FROM users WHERE active"
	c := fixture(t, `CREATE VIEW active_users AS `+sql+`;`,
		[3]string{"active_users", "view", sql})

	if r := check(t, c, entity("ActiveUser", "active_users", model.RelView)); len(r.Findings()) != 0 {
		t.Errorf("a view nobody touched reported %v", viewCodes(r))
	}
}

// The drift matrix. Every one of these keeps the relation's shape identical, so
// nothing about its columns can see them.
func TestView_manualBodyEditsAreDetected(t *testing.T) {
	for _, c := range []struct{ what, before, after string }{
		{
			"a predicate change",
			"SELECT id, email FROM users WHERE active",
			"SELECT id, email FROM users WHERE NOT active",
		},
		{
			"a projection expression change with the same type",
			"SELECT id, email FROM users",
			"SELECT id, lower(email) AS email FROM users",
		},
		{
			"a join change",
			"SELECT u.id, u.email FROM users u JOIN users v ON v.id = u.id",
			"SELECT u.id, u.email FROM users u LEFT JOIN users v ON v.id = u.id",
		},
		{
			"a function change",
			"SELECT id, upper(email) AS email FROM users",
			"SELECT id, lower(email) AS email FROM users",
		},
		{
			"an added filter",
			"SELECT id, email FROM users",
			"SELECT id, email FROM users WHERE id > 0",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			conn := fixture(t, `CREATE VIEW v AS `+c.before+`;`,
				[3]string{"v", "view", c.before})
			e := entity("V", "v", model.RelView)

			if r := check(t, conn, e); len(r.Findings()) != 0 {
				t.Fatalf("clean state already reports %v", viewCodes(r))
			}
			if _, err := conn.Exec(t.Context(),
				`CREATE OR REPLACE VIEW v AS `+c.after); err != nil {
				t.Fatalf("editing the view: %v", err)
			}
			r := check(t, conn, e)
			if !hasCode(r, diag.E032) {
				t.Errorf("%s was not detected; findings were %v. The columns are unchanged, "+
					"so nothing about the relation's shape could have seen it", c.what, viewCodes(r))
			}
		})
	}
}

// Reformatting the definition in the database is not drift: both readings are
// deparsed by one server, so formatting is already gone.
func TestView_reformattingIsNotDrift(t *testing.T) {
	const sql = "SELECT id, email FROM users WHERE active"
	c := fixture(t, `CREATE VIEW v AS `+sql+`;`, [3]string{"v", "view", sql})

	if _, err := c.Exec(t.Context(), "CREATE OR REPLACE VIEW v AS\n"+
		"  -- only the active ones\n  select id,\n     email\n  from users\n  where active"); err != nil {
		t.Fatalf("reformatting: %v", err)
	}
	if r := check(t, c, entity("V", "v", model.RelView)); len(r.Findings()) != 0 {
		t.Errorf("reformatting was reported as drift: %v", viewCodes(r))
	}
}

// A relation with no recording is not reported clean.
func TestView_missingProvenanceIsNotClean(t *testing.T) {
	// Created by hand, never recorded: the shape is exactly what the
	// declaration asks for, and nothing has verified the body.
	c := fixture(t, `CREATE VIEW v AS SELECT id, email FROM users;`)

	r := check(t, c, entity("V", "v", model.RelView))
	if !hasCode(r, diag.W031) {
		t.Errorf("a view with no recorded provenance was not reported: %v", viewCodes(r))
	}
	if hasCode(r, diag.E032) {
		t.Errorf("a view with no recording was reported as drifted, which claims to know "+
			"something nothing checked: %v", viewCodes(r))
	}
	// It is a warning: the schema may be entirely correct, and what is missing
	// is the evidence rather than the schema.
	for _, f := range r.Findings() {
		if f.Code == diag.W031 && f.Severity != diag.SeverityWarning {
			t.Errorf("W031 is %v, want a warning", f.Severity)
		}
	}
}

// A recording for a relation that is gone must not hide the relation being gone.
func TestView_staleRecordingDoesNotHideAMissingRelation(t *testing.T) {
	const sql = "SELECT id FROM users"
	c := fixture(t, `CREATE VIEW v AS `+sql+`;`, [3]string{"v", "view", sql})
	if _, err := c.Exec(t.Context(), `DROP VIEW v`); err != nil {
		t.Fatalf("dropping: %v", err)
	}

	r := check(t, c, entity("V", "v", model.RelView))
	// The definition check says nothing: the relation's absence is the entity
	// tier's finding, and reporting a body that is not there would be a second,
	// wrong answer to the same question.
	if hasCode(r, diag.E032) || hasCode(r, diag.W031) {
		t.Errorf("a stale recording produced a body finding about a relation that is gone: %v", viewCodes(r))
	}
}

// A wrong kind is one finding, and it stops body comparison.
func TestView_wrongKindDominates(t *testing.T) {
	for _, c := range []struct {
		what     string
		ddl      string
		declared model.RelationKind
	}{
		{"a table where a view is declared", `CREATE TABLE foo (id bigint);`, model.RelView},
		{"a table where a materialized view is declared", `CREATE TABLE foo (id bigint);`, model.RelMaterializedView},
		{"a view where a table is declared", `CREATE VIEW foo AS SELECT id FROM users;`, model.RelTable},
		{"a view where a materialized view is declared", `CREATE VIEW foo AS SELECT id FROM users;`, model.RelMaterializedView},
		{"a materialized view where a view is declared", `CREATE MATERIALIZED VIEW foo AS SELECT id FROM users;`, model.RelView},
		{"a materialized view where a table is declared", `CREATE MATERIALIZED VIEW foo AS SELECT id FROM users;`, model.RelTable},
	} {
		t.Run(c.what, func(t *testing.T) {
			conn := fixture(t, c.ddl)
			e := entity("Foo", "foo", c.declared)

			_, report := reconcile.Run(reconcile.Input{
				Config:   testConfig(),
				Entities: []*model.GoEntity{e},
				Schema:   introspect(t, conn),
			})
			if !hasCode(report, diag.E030) {
				t.Fatalf("no wrong-kind finding; got %v", viewCodes(report))
			}
			// And not reported as a missing relation, which is what a naive
			// diff would say while something of that name sits in front of you.
			if hasCode(report, diag.E016) {
				t.Errorf("the relation was also reported missing: %v", viewCodes(report))
			}
			// Nothing about columns, keys or indexes: those would be findings
			// about a relation of a kind nobody asked for.
			for _, f := range report.Findings() {
				if f.Code != diag.E030 {
					t.Errorf("a wrong kind produced a second finding %s: %s", f.Code, f.Message)
				}
			}
			// The message names both kinds.
			for _, f := range report.Findings() {
				if f.Code == diag.E030 {
					if !strings.Contains(f.Message, "public.foo") {
						t.Errorf("the finding does not name the relation: %s", f.Message)
					}
				}
			}

			// The body is not compared across kinds either.
			if r := check(t, conn, e); hasCode(r, diag.W031) || hasCode(r, diag.E032) {
				t.Errorf("a wrong kind produced a body finding: %v", viewCodes(r))
			}
		})
	}
}

// A materialized view refreshed WITH NO DATA is not schema drift: population is
// runtime state, and a schema that reported it would report drift every time
// anybody refreshed.
func TestMatView_populationIsNotDrift(t *testing.T) {
	const sql = "SELECT id FROM users"
	c := fixture(t, `CREATE MATERIALIZED VIEW m AS `+sql+` WITH DATA;`,
		[3]string{"m", "materialized view", sql})
	e := entity("M", "m", model.RelMaterializedView)

	if r := check(t, c, e); len(r.Findings()) != 0 {
		t.Fatalf("clean state reports %v", viewCodes(r))
	}
	if _, err := c.Exec(t.Context(), `REFRESH MATERIALIZED VIEW m WITH NO DATA`); err != nil {
		t.Fatalf("refreshing with no data: %v", err)
	}
	if r := check(t, c, e); len(r.Findings()) != 0 {
		t.Errorf("emptying a materialized view was reported as schema drift: %v", viewCodes(r))
	}
}

// Database-first relations nobody declares produce no provenance finding: there
// is no declaration to have applied, so there is nothing unverified.
func TestView_databaseFirstNeedsNoProvenance(t *testing.T) {
	c := fixture(t, `CREATE VIEW v AS SELECT id FROM users;`)
	if r := check(t, c); len(r.Findings()) != 0 {
		t.Errorf("a view nobody declares produced %v", viewCodes(r))
	}
}

// testConfig is the minimum a reconciliation needs.
func testConfig() *config.Config {
	return &config.Config{Schema: config.Schema{
		Mode: config.ModeManaged, SearchPath: []string{"public"},
	}}
}
