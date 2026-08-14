package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 G2 Parts C and D: the destructive-safety gate.
//
// Everything here runs the real command. A planner helper called directly
// proves that a function refuses; it does not prove that the thing a user types
// refuses, and the gap between those two is where a safety property quietly
// stops being reachable.
//
// Every case asserts the same four things, because a refusal that wrote a
// migration or touched the database would be a refusal in name only:
//
//	the command failed
//	the migrations directory is byte-identical
//	the database is unchanged
//	the provenance table is unchanged

// matviewProject is a managed project whose materialized view this project
// created, with provenance, so that the refusals under test are about the
// transition rather than about adoption.
func matviewProject(t *testing.T) *project {
	t.Helper()
	p := newProject(t, matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, ""))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	return p
}

// matviewEntities builds the declaration set with a given definition body and
// optional extra declarations.
func matviewEntities(def, extra string) string {
	return `package domain

//orm:table public.users
type User struct {
	ID       int64 ` + "`orm:\"pk,identity\"`" + `
	Email    string
	Active   bool
	Verified bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`" + def + "`" + `
//orm:depends-on public.users
//orm:index totals_user_id_key (UserID) unique
type Total struct {
	UserID int64
	Email  string
}
` + extra
}

// snapshot is everything a refusal must leave alone.
type snapshot struct {
	migrations map[string]string
	relations  []string
	provenance []string
	history    []string
}

func take(t *testing.T, p *project) snapshot {
	t.Helper()
	s := snapshot{migrations: map[string]string{}}

	dir := filepath.Join(p.Dir, "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the migrations directory: %v", err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		s.migrations[e.Name()] = string(b)
	}

	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()

	s.relations = queryStrings(t, conn, `
		SELECT relname || ':' || relkind
		FROM pg_class WHERE relnamespace = 'public'::regnamespace
		  AND relkind IN ('r','v','m','i') ORDER BY relname`)
	s.provenance = queryStrings(t, conn, `
		SELECT schema_name || '.' || relation_name || ':' || kind || ':' || source_identity || ':' || canonical
		FROM public.orm_schema_views ORDER BY schema_name, relation_name`)
	s.history = queryStrings(t, conn, `
		SELECT migration_id FROM public.orm_schema_migrations ORDER BY migration_id`)
	return s
}

func queryStrings(t *testing.T, conn *pgx.Conn, sql string) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), sql)
	if err != nil {
		// A table that does not exist yet is an empty answer, not a failure:
		// some fixtures run before the first migration.
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// assertUnchanged is the whole contract of a refusal.
func assertUnchanged(t *testing.T, before, after snapshot) {
	t.Helper()
	if len(before.migrations) != len(after.migrations) {
		t.Errorf("the migrations directory changed: %d files before, %d after",
			len(before.migrations), len(after.migrations))
	}
	for name, body := range before.migrations {
		got, ok := after.migrations[name]
		if !ok {
			t.Errorf("migration %s disappeared", name)
			continue
		}
		if got != body {
			t.Errorf("migration %s was rewritten", name)
		}
	}
	for name := range after.migrations {
		if _, ok := before.migrations[name]; !ok {
			t.Errorf("a refusal wrote a migration: %s", name)
		}
	}
	assertSame(t, "relations", before.relations, after.relations)
	assertSame(t, "provenance", before.provenance, after.provenance)
	assertSame(t, "migration history", before.history, after.history)
}

func assertSame(t *testing.T, what string, before, after []string) {
	t.Helper()
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("%s changed during a refusal:\n before %v\n after  %v", what, before, after)
	}
}

// refuse runs makemigrations, requires it to fail, and requires the world to be
// exactly as it was.
func refuse(t *testing.T, p *project, wantIn ...string) string {
	t.Helper()
	before := take(t, p)
	code, stdout, stderr := p.Run("makemigrations", "--name", "attempt")
	out := stdout + stderr
	if code == exitClean {
		t.Fatalf("makemigrations succeeded:\n%s", out)
	}
	for _, want := range wantIn {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
	assertUnchanged(t, before, take(t, p))
	return out
}

// C1: an unmanaged view reading a managed materialized view blocks its drop,
// during planning, by name.
func TestMatViewSafety_externalViewDependentBlocksDrop(t *testing.T) {
	p := matviewProject(t)
	runSQL(t, p, `CREATE VIEW public.manual_b AS SELECT user_id FROM public.totals`)

	// The materialized view is no longer declared.
	p.Entities(matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, "")[:strings.Index(
		matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, ""), "//orm:materialized-view")])

	out := refuse(t, p, "manual_b")
	if !strings.Contains(out, "totals") {
		t.Errorf("the refusal does not name the relation being dropped:\n%s", out)
	}
	// The message explains that CASCADE would remove the dependents without
	// anybody listing them. That is a warning, not an offer — what must never
	// happen is a generated migration containing one, which
	// TestMatViewSafety_generatedMigrationsNeverCascade pins.
	if strings.Contains(out, "use CASCADE") || strings.Contains(out, "with CASCADE") {
		t.Errorf("the refusal offers CASCADE:\n%s", out)
	}
}

// C2: the same, for an unmanaged materialized view reading a managed one. The
// dependent check must not be ordinary-view-specific.
func TestMatViewSafety_externalMatViewDependentBlocksDrop(t *testing.T) {
	p := matviewProject(t)
	runSQL(t, p, `CREATE MATERIALIZED VIEW public.manual_m AS SELECT user_id FROM public.totals WITH DATA`)

	base := matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, "")
	p.Entities(base[:strings.Index(base, "//orm:materialized-view")])

	refuse(t, p, "manual_m")
}

// D1 to D4: every kind transition refuses, through the real command.
func TestMatViewSafety_kindTransitionsRefuse(t *testing.T) {
	users := `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}
`
	for _, c := range []struct {
		what    string
		setup   string
		desired string
	}{
		{
			what:  "table to materialized view",
			setup: `CREATE TABLE public.foo (id bigint PRIMARY KEY, email text)`,
			desired: users + `
//orm:materialized-view public.foo
//orm:definition ` + "`SELECT id, email FROM users`" + `
//orm:depends-on public.users
type Foo struct {
	ID    int64
	Email string
}
`,
		},
		{
			what: "view to materialized view",
			setup: `CREATE VIEW public.foo AS SELECT id, email FROM public.users;
			         INSERT INTO public.orm_schema_views (schema_name, relation_name, kind, source_identity, canonical)
			         SELECT 'public','foo','view','v1:z', pg_get_viewdef('public.foo'::regclass, true)`,
			desired: users + `
//orm:materialized-view public.foo
//orm:definition ` + "`SELECT id, email FROM users`" + `
//orm:depends-on public.users
type Foo struct {
	ID    int64
	Email string
}
`,
		},
		{
			what: "materialized view to view",
			setup: `CREATE MATERIALIZED VIEW public.foo AS SELECT id, email FROM public.users WITH DATA;
			         INSERT INTO public.orm_schema_views (schema_name, relation_name, kind, source_identity, canonical)
			         SELECT 'public','foo','materialized view','v1:z', pg_get_viewdef('public.foo'::regclass, true)`,
			desired: users + `
//orm:view public.foo
//orm:definition ` + "`SELECT id, email FROM users`" + `
//orm:depends-on public.users
type Foo struct {
	ID    int64
	Email string
}
`,
		},
		{
			what: "materialized view to table",
			setup: `CREATE MATERIALIZED VIEW public.foo AS SELECT id, email FROM public.users WITH DATA;
			         INSERT INTO public.orm_schema_views (schema_name, relation_name, kind, source_identity, canonical)
			         SELECT 'public','foo','materialized view','v1:z', pg_get_viewdef('public.foo'::regclass, true)`,
			desired: users + `
//orm:table public.foo
type Foo struct {
	ID    int64 ` + "`orm:\"pk\"`" + `
	Email string
}
`,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := newProject(t, users)
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			runSQL(t, p, c.setup)

			p.Entities(c.desired)
			refuse(t, p)
		})
	}
}

// D6 to D15: every definition change refuses, whatever the output shape does.
// There is no safe-shape path for a materialized view, because the shape says
// nothing about the rows.
func TestMatViewSafety_definitionChangesRefuse(t *testing.T) {
	const original = `SELECT id AS user_id, email FROM users WHERE active`
	for _, c := range []struct {
		what string
		def  string
	}{
		{"a predicate change", `SELECT id AS user_id, email FROM users WHERE active AND verified`},
		{"a join change", `SELECT u.id AS user_id, u.email FROM users u JOIN users v ON v.id = u.id WHERE u.active`},
		{"a function change", `SELECT id AS user_id, lower(email) AS email FROM users WHERE active`},
		{"a calculation change", `SELECT id AS user_id, CASE WHEN active THEN email ELSE email END AS email FROM users WHERE active`},
		{"formatting only", "SELECT   id AS user_id,    email   FROM users   WHERE active"},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := newProject(t, matviewEntities(original, ""))
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")

			p.Entities(matviewEntities(c.def, ""))
			refuse(t, p, "definition changed")
		})
	}
}

// D16 and D17: creation policy applies at CREATE, so changing it on a relation
// that exists is refused rather than reinterpreted as a refresh.
func TestMatViewSafety_creationPolicyChangesRefuse(t *testing.T) {
	const def = `SELECT id AS user_id, email FROM users WHERE active`
	for _, c := range []struct {
		what  string
		first string
		then  string
	}{
		{"with data to with no data", "", "//orm:with-no-data\n"},
		{"with no data to with data", "//orm:with-no-data\n", ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := newProject(t, policyEntities(def, c.first))
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")

			p.Entities(policyEntities(def, c.then))
			out := refuse(t, p, "creation policy changed")
			if strings.Contains(strings.ToUpper(out), "REFRESH MATERIALIZED VIEW") {
				t.Errorf("the refusal offers to refresh:\n%s", out)
			}
		})
	}
}

// policyEntities is matviewEntities with a creation-policy directive.
func policyEntities(def, policy string) string {
	return `package domain

//orm:table public.users
type User struct {
	ID       int64 ` + "`orm:\"pk,identity\"`" + `
	Email    string
	Active   bool
	Verified bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`" + def + "`" + `
//orm:depends-on public.users
` + policy + `//orm:index totals_user_id_key (UserID) unique
type Total struct {
	UserID int64
	Email  string
}
`
}

// D.5: a materialized view with no provenance is never adopted.
func TestMatViewSafety_missingProvenanceRefuses(t *testing.T) {
	const def = `SELECT id AS user_id, email FROM users WHERE active`
	p := newProject(t, matviewEntities(def, ""))
	// Build the base table only, then create the materialized view by hand so
	// that nothing recorded what it holds.
	p.Entities(`package domain

//orm:table public.users
type User struct {
	ID       int64 ` + "`orm:\"pk,identity\"`" + `
	Email    string
	Active   bool
	Verified bool
}
`)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	runSQL(t, p, `CREATE MATERIALIZED VIEW public.totals AS SELECT id AS user_id, email FROM users WHERE active WITH DATA;
	              CREATE UNIQUE INDEX totals_user_id_key ON public.totals (user_id)`)

	p.Entities(matviewEntities(def, ""))
	refuse(t, p, "no migration of this project created it")
}

// D.4: a database changed by hand is never overwritten, even when the
// declaration moved on independently.
func TestMatViewSafety_manualDriftIsNeverOverwritten(t *testing.T) {
	p := matviewProject(t)

	// A DBA recreates it with a different body. The provenance row still
	// describes what this project applied.
	runSQL(t, p, `DROP MATERIALIZED VIEW public.totals;
	              CREATE MATERIALIZED VIEW public.totals AS
	                  SELECT id AS user_id, email FROM users WHERE active AND verified WITH DATA;
	              CREATE UNIQUE INDEX totals_user_id_key ON public.totals (user_id)`)

	// And the developer independently changes the declaration.
	p.Entities(matviewEntities(`SELECT id AS user_id, email FROM users WHERE verified`, ""))
	refuse(t, p)
}

// The rendered migrations never cascade, for either kind.
func TestMatViewSafety_generatedMigrationsNeverCascade(t *testing.T) {
	p := matviewProject(t)
	dir := filepath.Join(p.Dir, "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading migrations: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no migrations were generated, so this test checks nothing")
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToUpper(string(b)), "CASCADE") {
			t.Errorf("%s contains CASCADE", e.Name())
		}
	}
	// And the SQL the command would run says RESTRICT.
	sql := p.MustRun("sqlmigrate", strings.TrimSuffix(entries[0].Name(), ".json"))
	if strings.Contains(strings.ToUpper(sql), "CASCADE") {
		t.Errorf("rendered SQL contains CASCADE:\n%s", sql)
	}
}

// The blocking dependent is named deterministically when there is more than
// one, so a diagnostic does not depend on catalog or map order.
func TestMatViewSafety_dependencyDiagnosticIsDeterministic(t *testing.T) {
	p := matviewProject(t)
	runSQL(t, p, `CREATE VIEW public.zzz_b AS SELECT user_id FROM public.totals;
	              CREATE VIEW public.aaa_b AS SELECT user_id FROM public.totals;
	              CREATE VIEW public.mmm_b AS SELECT user_id FROM public.totals`)

	base := matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, "")
	p.Entities(base[:strings.Index(base, "//orm:materialized-view")])

	var first string
	for range 4 {
		code, stdout, stderr := p.Run("makemigrations", "--name", "attempt")
		if code == exitClean {
			t.Fatal("the drop was planned")
		}
		out := stdout + stderr
		names := namedDependents(out)
		if first == "" {
			first = names
			continue
		}
		if names != first {
			t.Errorf("the dependents are reported in a different order:\n %q\n %q", first, names)
		}
	}
	if !strings.Contains(first, "aaa_b") || !strings.Contains(first, "zzz_b") {
		t.Errorf("not every dependent was named: %q", first)
	}
}

// namedDependents extracts the dependent names a refusal listed, in order.
func namedDependents(out string) string {
	var found []string
	for _, name := range []string{"public.aaa_b", "public.mmm_b", "public.zzz_b"} {
		if i := strings.Index(out, name); i >= 0 {
			found = append(found, name)
		}
	}
	// Order as they appear in the message.
	sort.SliceStable(found, func(i, j int) bool {
		return strings.Index(out, found[i]) < strings.Index(out, found[j])
	})
	return strings.Join(found, ",")
}

// C4: when every managed relation in a dependency chain is removed, the drops
// are planned in reverse dependency order and nothing cascades.
//
// The order is the safety property. Dropping the materialized view first would
// leave the views that read it stranded, and PostgreSQL would refuse — at which
// point the only way out is the CASCADE this project will not write.
func TestMatViewSafety_managedDependentsDropInReverseOrder(t *testing.T) {
	const chain = `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`SELECT id AS user_id, email FROM users WHERE active`" + `
//orm:depends-on public.users
//orm:index totals_user_id_key (UserID) unique
type Total struct {
	UserID int64
	Email  string
}

//orm:view public.totals_b
//orm:definition ` + "`SELECT user_id FROM totals`" + `
//orm:depends-on public.totals
type TotalB struct {
	UserID int64
}

//orm:view public.totals_c
//orm:definition ` + "`SELECT user_id FROM totals_b`" + `
//orm:depends-on public.totals_b
type TotalC struct {
	UserID int64
}
`
	p := newProject(t, chain)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	// Every stored relation is removed at once.
	p.Entities(`package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}
`)
	out := p.MustRun("makemigrations", "--name", "drops")

	// C must go before B, and B before the materialized view they read.
	iC := strings.Index(out, "totals_c")
	iB := strings.Index(out, "totals_b")
	iA := strings.LastIndex(out, "drop materialized view")
	if iC < 0 || iB < 0 || iA < 0 {
		t.Fatalf("not every relation was dropped:\n%s", out)
	}
	if !(iC < iB && iB < iA) {
		t.Errorf("the drops are not in reverse dependency order (c=%d b=%d matview=%d):\n%s", iC, iB, iA, out)
	}
	if strings.Contains(strings.ToUpper(out), "CASCADE") {
		t.Errorf("a generated drop cascades:\n%s", out)
	}

	// It applies, which is the other half: an order PostgreSQL refuses is not
	// an order.
	p.MustRun("migrate")
	if out := p.MustRun("makemigrations", "--name", "again"); !strings.Contains(out, "No schema changes") {
		t.Errorf("the project did not converge:\n%s", out)
	}
}

// C6: the rendered SQL of a real drop migration says RESTRICT and never
// cascades — checked on a migration that actually contains a drop.
func TestMatViewSafety_dropMigrationSQLIsRestrict(t *testing.T) {
	p := matviewProject(t)
	base := matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, "")
	p.Entities(base[:strings.Index(base, "//orm:materialized-view")])
	p.MustRun("makemigrations", "--name", "drops")

	entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		sql := p.MustRun("sqlmigrate", strings.TrimSuffix(e.Name(), ".json"))
		if !strings.Contains(strings.ToUpper(sql), "DROP MATERIALIZED VIEW") {
			continue
		}
		found = true
		if !strings.Contains(strings.ToUpper(sql), "RESTRICT") {
			t.Errorf("the drop does not say RESTRICT:\n%s", sql)
		}
		if strings.Contains(strings.ToUpper(sql), "CASCADE") {
			t.Errorf("the drop cascades:\n%s", sql)
		}
	}
	if !found {
		t.Fatal("no migration dropped the materialized view, so this test checked nothing")
	}
}

// D.4, isolated: the database drifted and the declaration did not.
//
// This is the case only the drift check can refuse. When the declaration has
// also moved, the definition-change refusal fires first and would hide a drift
// check that had stopped working — so the declaration is left exactly as it was
// and the database is changed underneath it.
func TestMatViewSafety_manualDriftAloneRefuses(t *testing.T) {
	p := matviewProject(t)

	// A DBA recreates it with a different body. Nothing in the project moved.
	runSQL(t, p, `DROP MATERIALIZED VIEW public.totals;
	              CREATE MATERIALIZED VIEW public.totals AS
	                  SELECT id AS user_id, email FROM users WHERE verified WITH DATA;
	              CREATE UNIQUE INDEX totals_user_id_key ON public.totals (user_id)`)

	out := refuse(t, p, "no longer holds the definition this project applied")
	if strings.Contains(out, "definition changed") {
		t.Errorf("the drift was reported as a declaration change:\n%s", out)
	}
}

// E2, E3, F10, F11, F13: the index lifecycle on a materialized view, and the
// eligibility that is derived from it.
//
// The relation must not be recreated to manage its indexes — proven by its OID,
// which PostgreSQL changes when a relation is replaced and keeps when it is
// not. And the generated code must not be able to disagree with the database
// about which indexes exist: that disagreement is what made Refresh report
// "this materialized view has none" about a view that had one.
func TestMatViewIndex_lifecycleKeepsTheRelationAndTracksEligibility(t *testing.T) {
	noIndex := `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`SELECT id AS user_id, email FROM users WHERE active`" + `
//orm:depends-on public.users
type Total struct {
	UserID int64
	Email  string
}
`
	withIndex := strings.Replace(noIndex,
		"//orm:depends-on public.users\ntype Total",
		"//orm:depends-on public.users\n//orm:index totals_user_id_key (UserID) unique\ntype Total", 1)

	p := newProject(t, noIndex)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")

	before := relationOID(t, p, "public.totals")
	if before == 0 {
		t.Fatal("the materialized view was not created")
	}
	// With no qualifying index the generated code says so.
	if got := generatedConcurrentIndex(t, p); got != "" {
		t.Errorf("the generated concurrent index is %q, want empty", got)
	}

	// Adding an index is an index migration and nothing else.
	p.Entities(withIndex)
	out := p.MustRun("makemigrations", "--name", "addindex")
	if strings.Contains(out, "materialized view") {
		t.Errorf("adding an index touched the materialized view:\n%s", out)
	}
	p.MustRun("migrate")

	if after := relationOID(t, p, "public.totals"); after != before {
		t.Errorf("the materialized view was recreated to add an index: OID %d became %d", before, after)
	}

	// Release-critical: the generated code is now stale, and the tool says so.
	// Before this was fixed the mapping fingerprint ignored index facts, so
	// check reported "Generated current" while Refresh(Concurrently) refused a
	// refresh PostgreSQL would have accepted.
	code, stdout, stderr := p.Run("check", "--generated")
	if code == exitClean {
		t.Errorf("check reported the generated code as current after an index migration:\n%s", stdout+stderr)
	}
	p.MustRun("generate")
	if got := generatedConcurrentIndex(t, p); got != "totals_user_id_key" {
		t.Errorf("after regenerating the concurrent index is %q", got)
	}
	p.MustRun("check", "--generated")

	// And removing it goes back the other way, still without recreating.
	p.Entities(noIndex)
	p.MustRun("makemigrations", "--name", "dropindex")
	p.MustRun("migrate")
	if after := relationOID(t, p, "public.totals"); after != before {
		t.Errorf("the materialized view was recreated to remove an index: OID %d became %d", before, after)
	}
	if code, stdout, stderr := p.Run("check", "--generated"); code == exitClean {
		t.Errorf("check reported current after the index was removed:\n%s", stdout+stderr)
	}
	p.MustRun("generate")
	if got := generatedConcurrentIndex(t, p); got != "" {
		t.Errorf("after removing the index the generated code still names %q", got)
	}
	if out := p.MustRun("makemigrations", "--name", "again"); !strings.Contains(out, "No schema changes") {
		t.Errorf("the project did not converge:\n%s", out)
	}
}

// relationOID reads a relation's OID, which changes only when PostgreSQL
// replaces the relation itself.
func relationOID(t *testing.T, p *project, qualified string) uint32 {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var oid uint32
	if err := conn.QueryRow(t.Context(), `SELECT COALESCE(to_regclass($1)::oid, 0)`, qualified).Scan(&oid); err != nil {
		t.Fatalf("reading the OID of %s: %v", qualified, err)
	}
	return oid
}

// generatedConcurrentIndex reads the index name the generated constructor
// carries, which is what the runtime decides eligibility from.
func generatedConcurrentIndex(t *testing.T, p *project) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
	if err != nil {
		t.Fatalf("reading the generated handle: %v", err)
	}
	const marker = "NewMaterializedViewRepo(ex, &totalMeta, "
	i := strings.Index(string(b), marker)
	if i < 0 {
		t.Fatalf("the generated handle has no materialized view constructor:\n%s", b)
	}
	rest := string(b)[i+len(marker):]
	j := strings.Index(rest, ")")
	arg := strings.TrimSpace(rest[:j])
	return strings.Trim(arg, `"`)
}

// A materialized view declared with its indexes converges on the first run.
//
// The plan creates the relation and then each index. If the create operation
// also carried the indexes, replaying the migration would count each one twice
// and the very next makemigrations would write a migration to drop the
// duplicate — one that never converges, because it re-plans the same drop and
// create every time. The database is correct throughout, which is what made it
// worth a test that asks the planner rather than the catalog.
func TestMatViewIndex_declaredWithIndexesConvergesImmediately(t *testing.T) {
	p := newProject(t, matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, ""))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	if out := p.MustRun("makemigrations", "--name", "again"); !strings.Contains(out, "No schema changes") {
		t.Fatalf("a materialized view declared with an index did not converge:\n%s", out)
	}
	// And it still converges after the index changes, which is where the
	// duplicate actually shows. A duplicate is invisible to a diff that looks
	// indexes up by name — both copies answer to it — until one of them
	// differs from the declaration. Then the lookup finds the stale copy, plans
	// a drop and a create, and finds it again on the next run.
	p.Entities(strings.Replace(matviewEntities(`SELECT id AS user_id, email FROM users WHERE active`, ""),
		"//orm:index totals_user_id_key (UserID) unique",
		"//orm:index totals_user_id_key (UserID) unique include (Email)", 1))
	p.MustRun("makemigrations", "--name", "idxchange")
	p.MustRun("migrate")

	if out := p.MustRun("makemigrations", "--name", "again2"); !strings.Contains(out, "No schema changes") {
		t.Errorf("the project did not converge after an index change:\n%s", out)
	}
	p.MustRun("check")
}

// eligibilityProject builds a managed materialized view whose index set is
// given, and returns the project after a full migrate + generate.
func eligibilityProject(t *testing.T, indexes string) *project {
	t.Helper()
	src := `package domain

//orm:table public.users
type User struct {
	ID       int64 ` + "`orm:\"pk,identity\"`" + `
	Email    string
	Active   bool
	Verified bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`SELECT id AS user_id, email, active FROM users`" + `
//orm:depends-on public.users
` + indexes + `type Total struct {
	UserID int64
	Email  string
	Active bool
}
`
	p := newProject(t, src)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")
	p.MustRun("check", "--generated")
	return p
}

// F6 to F12: which index shapes make a concurrent refresh possible.
//
// Every one of these goes through the managed path — declaration, migration,
// generation — rather than a hand-written descriptor, because what is being
// tested is the whole lifecycle's answer and not just the rule in isolation.
//
// The negative cases are what matter. PostgreSQL would reject each of them, and
// the generated code is what lets the runtime say so without a round trip; a
// rule that were too generous would send a statement the server was always
// going to refuse.
// eligibilityCase is one index set and the qualifying index it must produce.
type eligibilityCase struct {
	what    string
	indexes string
	want    string
}

// eligibilityCases is the matrix, shared with the fixture precondition so that
// what is asserted to contain each disqualifying shape is what the killers run.
func eligibilityCases() []eligibilityCase {
	return []eligibilityCase{
		{"a plain unique index", "//orm:index totals_uid_key (UserID) unique\n", "totals_uid_key"},
		{"a unique index with INCLUDE",
			"//orm:index totals_uid_key (UserID) unique include (Email)\n", "totals_uid_key"},
		{"a non-unique index", "//orm:index totals_uid_idx (UserID)\n", ""},
		{"a partial unique index",
			"//orm:index totals_uid_key (UserID) unique where \"active\"\n", ""},
		{"a unique index over an expression",
			"//orm:index totals_lower_key (\"lower(email)\") unique\n", ""},
		{"a unique index mixing a column and an expression",
			"//orm:index totals_mixed_key (UserID, \"lower(email)\") unique\n", ""},
		{"several indexes, none qualifying",
			"//orm:index totals_uid_idx (UserID)\n" +
				"//orm:index totals_part_key (UserID) unique where \"active\"\n" +
				"//orm:index totals_lower_key (\"lower(email)\") unique\n", ""},
		{"several indexes, exactly one qualifying",
			"//orm:index totals_uid_idx (UserID)\n" +
				"//orm:index totals_part_key (UserID) unique where \"active\"\n" +
				"//orm:index totals_good_key (Email) unique\n", "totals_good_key"},
	}
}

func TestMatViewEligibility_onlyPlainUniqueIndexesQualify(t *testing.T) {
	for _, c := range eligibilityCases() {
		t.Run(c.what, func(t *testing.T) {
			p := eligibilityProject(t, c.indexes)
			if got := generatedConcurrentIndex(t, p); got != c.want {
				t.Errorf("the generated concurrent index is %q, want %q", got, c.want)
			}
		})
	}
}

// F12 and item 13: when several indexes qualify, the generated code names the
// same one whatever order the declarations arrive in.
//
// This is load-bearing rather than cosmetic. The generated artifact stores a
// name, and the mapping fingerprint covers it, so a selection that depended on
// declaration or catalog order would produce different generated bytes and a
// different fingerprint for the same schema — and every regeneration would
// churn the lock.
func TestMatViewEligibility_selectionIsDeterministicAmongEquals(t *testing.T) {
	const a = "//orm:index totals_aaa_key (UserID) unique\n"
	const b = "//orm:index totals_zzz_key (Email) unique\n"

	first := eligibilityProject(t, a+b)
	second := eligibilityProject(t, b+a)

	got1, got2 := generatedConcurrentIndex(t, first), generatedConcurrentIndex(t, second)
	if got1 != got2 {
		t.Errorf("the chosen index depends on declaration order: %q then %q", got1, got2)
	}
	// The lowest name, so the choice is stable and explicable rather than
	// merely repeatable.
	if got1 != "totals_aaa_key" {
		t.Errorf("the chosen index is %q, want the lowest qualifying name", got1)
	}
	// And the fingerprint agrees, which is what makes regeneration quiet.
	if l1, l2 := projectFile(t, first, "orm.lock"), projectFile(t, second, "orm.lock"); l1 != l2 {
		t.Error("the same schema produced two different locks depending on declaration order")
	}
}

func projectFile(t *testing.T, p *project, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.Dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

// F14 and F16: the two directions of the staleness contract.
//
// Eligibility is generated metadata, so a migration that changes it leaves the
// generated code behind. What must never happen is that nothing says so — that
// was the defect: check reported "Generated current" while the runtime refused
// a refresh PostgreSQL accepted.
func TestMatViewEligibility_staleGeneratedCodeIsAlwaysReported(t *testing.T) {
	for _, c := range []struct {
		what  string
		start string
		then  string
		was   string
		now   string
	}{
		{
			what:  "a qualifying index is added",
			start: "//orm:index totals_uid_idx (UserID)\n",
			then:  "//orm:index totals_uid_idx (UserID)\n//orm:index totals_uid_key (UserID) unique\n",
			was:   "", now: "totals_uid_key",
		},
		{
			what:  "a qualifying index is removed",
			start: "//orm:index totals_uid_key (UserID) unique\n",
			then:  "//orm:index totals_uid_idx (UserID)\n",
			was:   "totals_uid_key", now: "",
		},
		{
			what:  "a qualifying index becomes partial",
			start: "//orm:index totals_uid_key (UserID) unique\n",
			then:  "//orm:index totals_uid_key (UserID) unique where \"active\"\n",
			was:   "totals_uid_key", now: "",
		},
		{
			what:  "a qualifying index becomes an expression",
			start: "//orm:index totals_uid_key (Email) unique\n",
			then:  "//orm:index totals_uid_key (\"lower(email)\") unique\n",
			was:   "totals_uid_key", now: "",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := eligibilityProject(t, c.start)
			if got := generatedConcurrentIndex(t, p); got != c.was {
				t.Fatalf("the starting state is %q, want %q", got, c.was)
			}
			oid := relationOID(t, p, "public.totals")

			p.Entities(strings.Replace(projectFile(t, p, "domain/entities.go"), c.start, c.then, 1))
			p.MustRun("makemigrations", "--name", "idxchange")
			p.MustRun("migrate")

			// The relation was never recreated to change its indexes.
			if after := relationOID(t, p, "public.totals"); after != oid {
				t.Errorf("the materialized view was recreated: OID %d became %d", oid, after)
			}
			// Release-critical: before regenerating, the tool says the
			// generated code is stale.
			if code, stdout, stderr := p.Run("check", "--generated"); code == exitClean {
				t.Errorf("check called the generated code current after eligibility changed:\n%s", stdout+stderr)
			}
			p.MustRun("generate")
			if got := generatedConcurrentIndex(t, p); got != c.now {
				t.Errorf("after regenerating the concurrent index is %q, want %q", got, c.now)
			}
			p.MustRun("check", "--generated")
			if out := p.MustRun("makemigrations", "--name", "again"); !strings.Contains(out, "No schema changes") {
				t.Errorf("the project did not converge:\n%s", out)
			}
		})
	}
}
