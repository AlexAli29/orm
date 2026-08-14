package desired_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/desired"
	"github.com/AlexAli29/orm/internal/gen/goscan"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 Stage B+C: managed view declarations reaching the desired schema.
//
// Everything here is static. Nothing runs the declaring package, nothing reads
// a database, and nothing parses the SQL — the definition is carried as the
// developer wrote it and the dependencies are carried as the developer declared
// them.

const usersTable = `
//orm:table public.users
type User struct {
	ID     int64  ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}
`

const activeView = `
//orm:view public.active_users
//orm:definition "SELECT id, email FROM users WHERE active"
//orm:depends-on public.users
type ActiveUser struct {
	ID    int64
	Email string
}
`

const totalsMatView = `
//orm:materialized-view public.user_totals
//orm:definition "SELECT id AS user_id, count(*) AS orders FROM users GROUP BY id"
//orm:depends-on public.users
//orm:index user_totals_user_id_key (UserID) unique
type UserTotal struct {
	UserID int64
	Orders int64
}
`

func pkg(body string) string { return "package domain\n" + body }

// A view and a materialized view become distinct desired relations.
func TestBuild_managedViewsEnterTheDesiredSchema(t *testing.T) {
	s := mustBuild(t, map[string]string{"domain": pkg(usersTable + activeView + totalsMatView)})

	if len(s.Tables) != 1 || s.Tables[0].Qualified() != "public.users" {
		t.Fatalf("tables = %v", tableNames(s))
	}
	if len(s.Views) != 1 {
		t.Fatalf("views = %d, want 1", len(s.Views))
	}
	v := s.Views[0]
	if v.Qualified() != "public.active_users" {
		t.Errorf("view is %s", v.Qualified())
	}
	if !strings.Contains(v.Definition.SQL, "WHERE active") {
		t.Errorf("the definition did not survive: %q", v.Definition.SQL)
	}
	if v.Definition.Canonical != "" {
		t.Error("a desired schema that has never seen a server carries a canonical definition")
	}
	if len(v.Columns) != 2 || v.Columns[0].Name != "id" || v.Columns[1].Name != "email" {
		t.Errorf("columns = %+v; order is the view's output order", v.Columns)
	}
	if len(v.DependsOn) != 1 || v.DependsOn[0].Qualified() != "public.users" {
		t.Errorf("dependencies = %+v", v.DependsOn)
	}

	if len(s.MaterializedViews) != 1 {
		t.Fatalf("materialized views = %d, want 1", len(s.MaterializedViews))
	}
	m := s.MaterializedViews[0]
	if m.Qualified() != "public.user_totals" {
		t.Errorf("materialized view is %s", m.Qualified())
	}
	// WITH DATA is the default and is creation policy, not runtime state.
	if !m.WithData {
		t.Error("the default creation policy is not WITH DATA")
	}
	if m.Populated {
		t.Error("a desired schema claims a runtime population state")
	}
	if len(m.Indexes) != 1 || !m.Indexes[0].Unique {
		t.Fatalf("indexes = %+v; a materialized view's index is an ordinary index", m.Indexes)
	}
	if _, ok := m.ConcurrentRefreshIndex(); !ok {
		t.Error("the declared unique index does not qualify for REFRESH CONCURRENTLY")
	}

	// And the relation kinds are distinct, by name, in one namespace.
	for _, c := range []struct {
		name string
		want schema.RelationKind
	}{
		{"users", schema.KindTable},
		{"active_users", schema.KindView},
		{"user_totals", schema.KindMaterializedView},
	} {
		got, ok := s.Relation("public", c.name)
		if !ok {
			t.Errorf("%s is not in the schema", c.name)
			continue
		}
		if got != c.want {
			t.Errorf("%s is a %v, want %v", c.name, got, c.want)
		}
	}
}

// WITH NO DATA is opt-in and named.
func TestBuild_withNoDataIsCreationPolicy(t *testing.T) {
	s := mustBuild(t, map[string]string{"domain": pkg(usersTable + `
//orm:materialized-view public.empty_totals
//orm:definition "SELECT id FROM users"
//orm:with-no-data
type EmptyTotal struct {
	ID int64
}
`)})
	if len(s.MaterializedViews) != 1 {
		t.Fatalf("materialized views = %d", len(s.MaterializedViews))
	}
	if s.MaterializedViews[0].WithData {
		t.Error("//orm:with-no-data did not reach the desired schema")
	}
	if s.MaterializedViews[0].Populated {
		t.Error("a creation policy was recorded as a runtime population state")
	}
}

// A definition in a file, read at scan time, with the path kept relative.
func TestBuild_definitionFromAFile(t *testing.T) {
	s := mustBuildFiles(t,
		map[string]string{"domain": pkg(usersTable + `
//orm:view public.active_users
//orm:definition sql/active_users.sql
//orm:depends-on public.users
type ActiveUser struct {
	ID    int64
	Email string
}
`)},
		map[string]string{
			filepath.Join("domain", "sql", "active_users.sql"): "SELECT id, email\nFROM users\nWHERE active\n",
		})

	if len(s.Views) != 1 {
		t.Fatalf("views = %d", len(s.Views))
	}
	if !strings.Contains(s.Views[0].Definition.SQL, "WHERE active") {
		t.Errorf("the file's SQL did not reach the schema: %q", s.Views[0].Definition.SQL)
	}
	// Nothing machine-specific: a definition path must not put a checkout's
	// location into anything that gets committed.
	for _, bad := range []string{"/tmp", "/home", "/Users", t.TempDir()} {
		if strings.Contains(s.Views[0].Definition.SQL, bad) {
			t.Errorf("the definition carries an absolute path")
		}
	}
}

// The refusals.
func TestBuild_refusesBadViewDeclarations(t *testing.T) {
	for _, c := range []struct {
		what string
		src  string
		want string
	}{
		{
			"a view with no definition",
			usersTable + `
//orm:view public.active_users
type ActiveUser struct {
	ID int64
}
`, "E025"},
		{
			"a materialized view with no definition",
			usersTable + `
//orm:materialized-view public.totals
type Total struct {
	ID int64
}
`, "E025"},
		{
			"one name declared as a table and a view",
			usersTable + `
//orm:view public.users
//orm:definition "SELECT id FROM other"
type UserView struct {
	ID int64
}
`, "E026"},
		{
			"one name declared as a view and a materialized view",
			usersTable + `
//orm:view public.thing
//orm:definition "SELECT id FROM users"
type Thing struct {
	ID int64
}

//orm:materialized-view public.thing
//orm:definition "SELECT id FROM users"
type ThingM struct {
	ID int64
}
`, "E026"},
		{
			"two views over one name",
			usersTable + `
//orm:view public.thing
//orm:definition "SELECT id FROM users"
type Thing struct {
	ID int64
}

//orm:view public.thing
//orm:definition "SELECT id FROM users"
type Thing2 struct {
	ID int64
}
`, "both describe"},
		{
			"a dependency on a relation nobody declares",
			usersTable + `
//orm:view public.active_users
//orm:definition "SELECT id FROM users"
//orm:depends-on public.nothing
type ActiveUser struct {
	ID int64
}
`, "E027"},
		{
			"a view that depends on itself",
			usersTable + `
//orm:view public.active_users
//orm:definition "SELECT id FROM users"
//orm:depends-on public.active_users
type ActiveUser struct {
	ID int64
}
`, "E028"},
		{
			"a two-node cycle",
			usersTable + `
//orm:view public.a
//orm:definition "SELECT id FROM users"
//orm:depends-on public.b
type A struct {
	ID int64
}

//orm:view public.b
//orm:definition "SELECT id FROM users"
//orm:depends-on public.a
type B struct {
	ID int64
}
`, "E029"},
		{
			"a three-node cycle",
			usersTable + `
//orm:view public.a
//orm:definition "SELECT id FROM users"
//orm:depends-on public.b
type A struct {
	ID int64
}

//orm:view public.b
//orm:definition "SELECT id FROM users"
//orm:depends-on public.c
type B struct {
	ID int64
}

//orm:view public.c
//orm:definition "SELECT id FROM users"
//orm:depends-on public.a
type C struct {
	ID int64
}
`, "E029"},
		{
			"an index on an ordinary view",
			usersTable + `
//orm:view public.active_users
//orm:definition "SELECT id FROM users"
//orm:index active_id (ID)
type ActiveUser struct {
	ID int64
}
`, "PostgreSQL allows none on an ordinary view"},
		{
			"a primary key on a materialized view",
			usersTable + `
//orm:materialized-view public.totals
//orm:definition "SELECT id FROM users"
type Total struct {
	ID int64 ` + "`orm:\"pk\"`" + `
}
`, "primary key"},
	} {
		t.Run(c.what, func(t *testing.T) {
			_, err := buildErr(t, map[string]string{"domain": pkg(c.src)})
			if err == nil {
				t.Fatalf("%s was accepted", c.what)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not mention %q: %v", c.want, err)
			}
		})
	}
}

// A diamond is not a cycle.
func TestBuild_diamondIsNotACycle(t *testing.T) {
	mustBuild(t, map[string]string{"domain": pkg(usersTable + `
//orm:view public.a
//orm:definition "SELECT id FROM users"
//orm:depends-on public.users
type A struct {
	ID int64
}

//orm:view public.b
//orm:definition "SELECT id FROM a"
//orm:depends-on public.a
type B struct {
	ID int64
}

//orm:view public.c
//orm:definition "SELECT id FROM a"
//orm:depends-on public.a
type C struct {
	ID int64
}

//orm:view public.d
//orm:definition "SELECT id FROM b"
//orm:depends-on public.b, public.c
type D struct {
	ID int64
}
`)})
}

// Declarations span packages: a table here, a view there, a materialized view
// somewhere else, all in one desired schema.
func TestBuild_declarationsSpanPackages(t *testing.T) {
	s := mustBuild(t, map[string]string{
		"accounts":  "package accounts\n" + usersTable,
		"reporting": "package reporting\n" + activeView,
		"analytics": "package analytics\n" + totalsMatView,
	})
	if len(s.Tables) != 1 || len(s.Views) != 1 || len(s.MaterializedViews) != 1 {
		t.Fatalf("the desired schema saw %d tables, %d views, %d materialized views across three packages",
			len(s.Tables), len(s.Views), len(s.MaterializedViews))
	}
	// And a dependency declared in one package on a relation declared in
	// another resolves: schema dependency is not Go package dependency.
	if len(s.Views[0].DependsOn) != 1 || s.Views[0].DependsOn[0].Qualified() != "public.users" {
		t.Errorf("the cross-package dependency did not resolve: %+v", s.Views[0].DependsOn)
	}
}

// Multi-line SQL belongs in a file: a // comment ends at a newline, so the
// inline form cannot hold it and does not pretend to.
func TestBuild_multiLineDefinitionsLiveInFiles(t *testing.T) {
	sql := "SELECT id -- the identifier\nFROM users\nWHERE active /* still here */\n"
	s := mustBuildFiles(t,
		map[string]string{"domain": pkg(usersTable + `
//orm:view public.v
//orm:definition sql/v.sql
//orm:depends-on public.users
type V struct {
	ID int64
}
`)},
		map[string]string{filepath.Join("domain", "sql", "v.sql"): sql})

	if got := s.Views[0].Definition.SQL; got != strings.TrimSpace(sql) {
		t.Errorf("the file's SQL was altered:\n got: %q\nwant: %q", got, strings.TrimSpace(sql))
	}
}

// Hostile SQL is carried, not parsed. Nothing here splits on a semicolon, reads
// a comment or unwraps a quote: the definition is developer-authored schema SQL
// and PostgreSQL is the only thing entitled to interpret it.
func TestBuild_definitionIsCarriedNotParsed(t *testing.T) {
	for _, c := range []struct{ what, sql string }{
		{"a semicolon inside a literal", `SELECT id, 'a;b' AS s FROM users`},
		{"a comment marker inside a literal", `SELECT id, 'a--b' AS s FROM users`},
		{"a block comment marker inside a literal", `SELECT id, 'a/*b*/c' AS s FROM users`},
		{"a dollar-quoted string", `SELECT id, $q$ hi $1 there $q$ AS s FROM users`},
		{"a quoted identifier", `SELECT id, "weird name" FROM users`},

		{"a CTE", `WITH a AS (SELECT id FROM users) SELECT id FROM a`},
		{"a nested subquery", `SELECT id FROM (SELECT id FROM users) t`},
		{"a CASE", `SELECT id, CASE WHEN active THEN 'y' ELSE 'n' END AS s FROM users`},
		{"a JSONB expression", `SELECT id, settings -> 'a' ->> 'b' AS s FROM users`},
		{"a function call", `SELECT id, ST_Centroid(geom) AS center FROM users`},
		{"a backslash", `SELECT id, 'a\b' AS s FROM users`},
		{"a unicode literal", `SELECT id, 'héllo ☃' AS s FROM users`},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := mustBuild(t, map[string]string{"domain": pkg(usersTable + `
//orm:view public.v
//orm:definition ` + "`" + c.sql + "`" + `
//orm:depends-on public.users
type V struct {
	ID int64
}
`)})
			if len(s.Views) != 1 {
				t.Fatalf("views = %d", len(s.Views))
			}
			if got := s.Views[0].Definition.SQL; got != c.sql {
				t.Errorf("the definition was altered:\n got: %q\nwant: %q", got, c.sql)
			}
		})
	}
}

func tableNames(s *schema.Schema) []string {
	var out []string
	for _, t := range s.Tables {
		out = append(out, t.Qualified())
	}
	return out
}

// buildErr scans a module and builds its desired schema, returning the error.
func buildErr(t *testing.T, files map[string]string) (*schema.Schema, error) {
	t.Helper()
	return buildFilesErr(t, files, nil)
}

func buildFilesErr(t *testing.T, files, extra map[string]string) (*schema.Schema, error) {
	t.Helper()
	root := module(t, files)
	for path, body := range extra {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, full, body)
	}
	targets := make([]goscan.Target, 0, len(files))
	for dir := range files {
		targets = append(targets, goscan.Target{Dir: filepath.Join(root, dir)})
	}
	slices.SortFunc(targets, func(a, b goscan.Target) int { return strings.Compare(a.Dir, b.Dir) })

	scanned, err := goscan.Scan(t.Context(), root, targets)
	if err != nil {
		return nil, err
	}
	return desired.Build(desired.Input{
		Config:   &config.Config{Schema: config.Schema{Mode: config.ModeManaged, SearchPath: []string{"public"}}},
		Entities: scanned.Entities,
		Decls:    scanned.Decls,
	})
}

func mustBuild(t *testing.T, files map[string]string) *schema.Schema {
	t.Helper()
	s, err := buildErr(t, files)
	if err != nil {
		t.Fatalf("building the desired schema: %v", err)
	}
	return s
}

func mustBuildFiles(t *testing.T, files, extra map[string]string) *schema.Schema {
	t.Helper()
	s, err := buildFilesErr(t, files, extra)
	if err != nil {
		t.Fatalf("building the desired schema: %v", err)
	}
	return s
}
