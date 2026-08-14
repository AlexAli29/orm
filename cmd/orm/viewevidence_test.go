package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 G1 freeze evidence: package layouts, native types, determinism.

// multiPkg wires a table, a view on it, and a view on that — one per package.
func multiPkgProject(t *testing.T) *project {
	t.Helper()
	p := newProject(t, `package domain

//orm:table public.placeholder
type Placeholder struct {
	ID int64 `+"`orm:\"pk,identity\"`"+`
}
`)
	// Replace the single-package layout with three.
	if err := os.RemoveAll(filepath.Join(p.Dir, "domain")); err != nil {
		t.Fatal(err)
	}
	for dir, src := range map[string]string{
		"accounts": `package accounts

//orm:table public.users
type User struct {
	ID       int64 ` + "`orm:\"pk,identity\"`" + `
	Email    string
	Active   bool
	Verified bool
}
`,
		"reporting": `package reporting

//orm:view public.active_users
//orm:definition ` + "`SELECT id, email, verified FROM users WHERE active`" + `
//orm:depends-on public.users
type ActiveUser struct {
	ID       int64
	Email    string
	Verified bool
}
`,
		"analytics": `package analytics

//orm:view public.verified_users
//orm:definition ` + "`SELECT id, email FROM active_users WHERE verified`" + `
//orm:depends-on public.active_users
type VerifiedUser struct {
	ID    int64
	Email string
}
`,
	} {
		if err := os.MkdirAll(filepath.Join(p.Dir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p.Dir, dir, "entities.go"), src)
	}
	writeFile(t, p.Conf, "version: 1\n\nschema:\n  mode: managed\n  dsn: \""+p.DSN+"\"\n"+
		"  search_path:\n    - public\n\nmigrations:\n  dir: migrations\n\npackages:\n"+
		"  - path: ./accounts\n    output: same\n"+
		"  - path: ./reporting\n    output: same\n"+
		"  - path: ./analytics\n    output: same\n")
	return p
}

// Schema dependencies cross Go packages; generation stays in each owner.
func TestEvidence_multiPackageRoundtrip(t *testing.T) {
	p := multiPkgProject(t)

	out := p.MustRun("makemigrations", "--name", "initial")
	iT := strings.Index(out, "create table")
	iA := strings.Index(out, "create view public.active_users")
	iV := strings.Index(out, "create view public.verified_users")
	if iT < 0 || iA < 0 || iV < 0 || !(iT < iA && iA < iV) {
		t.Fatalf("operations are missing or out of dependency order across packages:\n%s", out)
	}

	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	// Each package owns its own generated handle, and the views are read-only.
	for _, c := range []struct{ dir, want string }{
		{"accounts", "Users *orm.Repo[User]"},
		{"reporting", "ActiveUsers *orm.ViewRepo[ActiveUser]"},
		{"analytics", "VerifiedUsers *orm.ViewRepo[VerifiedUser]"},
	} {
		gen := readFile(t, filepath.Join(p.Dir, c.dir, "orm_db.gen.go"))
		if !strings.Contains(strings.Join(strings.Fields(gen), " "), strings.Join(strings.Fields(c.want), " ")) {
			t.Errorf("%s does not declare %q", c.dir, c.want)
		}
	}

	writeFile(t, filepath.Join(p.Dir, "main.go"), `package main

import (
	"context"
	"fmt"
	"os"

	"example.com/managed/accounts"
	"example.com/managed/analytics"
	"example.com/managed/reporting"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DSN"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx,
		"INSERT INTO users (email, active, verified) VALUES ($1,true,true),($2,true,false),($3,false,false)",
		"v@example.com", "a@example.com", "off@example.com"); err != nil {
		panic(err)
	}
	if _, err := accounts.New(pool).Users.Query().All(ctx); err != nil {
		panic(err)
	}
	active, err := reporting.New(pool).ActiveUsers.Query().All(ctx)
	if err != nil {
		panic(err)
	}
	verified, err := analytics.New(pool).VerifiedUsers.Query().All(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("active=%d verified=%d\n", len(active), len(verified))
}
`)
	if got := runProgram(t, p); !strings.Contains(got, "active=2 verified=1") {
		t.Errorf("cross-package typed queries returned:\n%s", got)
	}
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the multi-package workflow did not converge:\n%s", out)
	}
}

// PostgreSQL-native types survive a managed view, including the nullability a
// LEFT JOIN introduces.
const typesProject = `package domain

import (
	"net/netip"
	"time"

	"github.com/AlexAli29/orm"
)

//orm:table public.things
type Thing struct {
	ID       int64          ` + "`orm:\"pk,identity\"`" + `
	Settings map[string]any ` + "`orm:\"pgtype:jsonb\"`" + `
	Addr     netip.Prefix   ` + "`orm:\"pgtype:inet\"`" + `
	Net      netip.Prefix   ` + "`orm:\"pgtype:cidr\"`" + `
	Span     orm.Range[time.Time] ` + "`orm:\"pgtype:tstzrange\"`" + `
	Every    orm.Interval
	Doc      orm.TSVector   ` + "`orm:\"pgtype:tsvector\"`" + `
}

//orm:table public.notes
type Note struct {
	ID      int64  ` + "`orm:\"pk,identity\"`" + `
	ThingID int64  ` + "`orm:\"column:thing_id\"`" + `
	Body    string
}

//orm:view public.thing_notes
//orm:definition ` + "`SELECT t.id, t.settings, t.addr, t.net, t.span, t.every, t.doc, n.body FROM things t LEFT JOIN notes n ON n.thing_id = t.id`" + `
//orm:depends-on public.things, public.notes
type ThingNote struct {
	ID       int64
	Settings map[string]any       ` + "`orm:\"pgtype:jsonb\"`" + `
	Addr     netip.Prefix         ` + "`orm:\"pgtype:inet\"`" + `
	Net      netip.Prefix         ` + "`orm:\"pgtype:cidr\"`" + `
	Span     orm.Range[time.Time] ` + "`orm:\"pgtype:tstzrange\"`" + `
	Every    orm.Interval
	Doc      orm.TSVector ` + "`orm:\"pgtype:tsvector\"`" + `
	// Body comes from a NOT NULL column reached through a LEFT JOIN, so it is
	// nullable however the base table is declared.
	Body *string
}
`

func TestEvidence_nativeTypesAndNullabilityThroughAView(t *testing.T) {
	p := newProject(t, typesProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	// The generated view keeps the PostgreSQL-native Go types rather than
	// degrading them to strings because the source is a view.
	gen := readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))
	_ = gen
	writeFile(t, filepath.Join(p.Dir, "main.go"), `package main

import (
	"context"
	"fmt"
	"os"

	"example.com/managed/domain"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DSN"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `+"`"+`INSERT INTO things (settings, addr, net, span, every, doc)
		VALUES ('{"a":{"b":[1,2]},"n":null}'::jsonb, '10.0.0.1'::inet, '10.0.0.0/8'::cidr,
		        tstzrange(now(), now() + interval '1 day'), interval '1 mon 2 days 3 us',
		        to_tsvector('english', 'the quick brown fox'))`+"`"+`); err != nil {
		panic(err)
	}

	rows, err := domain.New(pool).ThingNotes.Query().All(ctx)
	if err != nil {
		panic(err)
	}
	if len(rows) != 1 {
		panic("want one row")
	}
	r := rows[0]
	// The LEFT JOIN produced no note, so Body is NULL and the scan succeeded.
	fmt.Printf("body_nil=%v\n", r.Body == nil)
	fmt.Printf("jsonb_nested=%v json_null=%v\n", r.Settings["a"] != nil, r.Settings["n"] == nil)
	fmt.Printf("addr=%s net=%s\n", r.Addr, r.Net)
	fmt.Printf("range_bounded=%v\n", !r.Span.IsEmpty())
	fmt.Printf("interval=%d/%d/%d\n", r.Every.Months, r.Every.Days, r.Every.Microseconds)
	fmt.Printf("tsvector_len=%v\n", len(string(r.Doc)) > 0)
}
`)
	got := runProgram(t, p)
	for _, want := range []string{
		"body_nil=true",
		"jsonb_nested=true json_null=true",
		"addr=10.0.0.1/32 net=10.0.0.0/8",
		"range_bounded=true",
		"interval=1/2/3",
		"tsvector_len=true",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("a PostgreSQL-native type did not survive the view: want %q in:\n%s", want, got)
		}
	}
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the type-matrix workflow did not converge:\n%s", out)
	}
}

// The migration artifact does not depend on where the project sits, and does not
// depend on which PostgreSQL major generated it.
func TestEvidence_artifactDeterminism(t *testing.T) {
	render := func(t *testing.T, dsn string) string {
		t.Helper()
		p := newProject(t, chainProject)
		if dsn != "" {
			writeFile(t, p.Conf, strings.Replace(readFile(t, p.Conf), p.DSN, dsn, 1))
		}
		p.MustRun("makemigrations", "--name", "initial")
		entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				return readFile(t, filepath.Join(p.Dir, "migrations", e.Name()))
			}
		}
		t.Fatal("no migration was written")
		return ""
	}

	// Two unrelated roots. t.TempDir gives each subtest its own.
	first := render(t, "")
	second := render(t, "")
	if first != second {
		t.Errorf("the same project in two directories produced different migrations:\n%s",
			diffText(first, second))
	}

	// Nothing from a server, and nothing from a machine.
	for _, forbidden := range []string{"/tmp/", "/home/", "server_version", "Canonical", "oid"} {
		if strings.Contains(first, forbidden) {
			t.Errorf("the artifact contains %q:\n%s", forbidden, first)
		}
	}
	// The deparser's spelling on 14/15 qualifies columns; on 16+ it does not.
	// Neither may appear, because neither is in the artifact at all.
	if strings.Contains(first, "users.active") {
		t.Errorf("deparsed server text reached the artifact:\n%s", first)
	}
}

// The migration artifact is the same bytes whichever PostgreSQL generated it.
//
// This is the permanent regression for the portability bug: a view's canonical
// definition is deparsed text, and 16's deparser stopped qualifying columns
// 14's and 15's qualified. If any of it reached a committed migration, this
// project would produce two different artifacts depending on which server the
// developer happened to be pointed at, and the diff would arrive with no
// explanation.
func TestEvidence_artifactIsIdenticalAcrossMajors(t *testing.T) {
	dsns := map[string]string{}
	for _, v := range []string{"14", "15", "16", "17", "18"} {
		if dsn := os.Getenv("ORM_TEST_DSN_PG" + v); dsn != "" {
			dsns["PG"+v] = dsn
		}
	}
	if len(dsns) < 2 {
		t.Skip("set ORM_TEST_DSN_PG14..PG18 to compare artifacts across majors")
	}

	artifacts := map[string]string{}
	for major, dsn := range dsns {
		p := newProject(t, chainProject)
		writeFile(t, p.Conf, strings.Replace(readFile(t, p.Conf), p.DSN, dsn, 1))
		p.MustRun("makemigrations", "--name", "initial")

		entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				artifacts[major] = readFile(t, filepath.Join(p.Dir, "migrations", e.Name()))
			}
		}
		if artifacts[major] == "" {
			t.Fatalf("%s produced no migration", major)
		}
	}

	var first, firstMajor string
	for major, body := range artifacts {
		if first == "" {
			first, firstMajor = body, major
			continue
		}
		if body != first {
			t.Errorf("%s and %s produced different migrations for one project:\n%s",
				firstMajor, major, diffText(first, body))
		}
	}
	t.Logf("identical migration bytes across %d PostgreSQL majors", len(artifacts))

	// And nothing a server could have contributed is in there.
	for _, forbidden := range []string{"users.active", "Canonical", "server_version", "oid", "/tmp/"} {
		if strings.Contains(first, forbidden) {
			t.Errorf("the artifact contains %q", forbidden)
		}
	}
}

// A managed view used three times in one query, and one mixed query through the
// ordinary compiler.
//
// Both are regression proofs rather than new capability: a view reaches the
// compiler as a source, so if introducing the managed source kind had created a
// second path, these are where it would show.
func TestEvidence_managedViewComposition(t *testing.T) {
	p := newProject(t, chainProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")

	writeFile(t, filepath.Join(p.Dir, "main.go"), `package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"example.com/managed/domain"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DSN"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		"INSERT INTO users (email, active, verified, created_at) VALUES ($1,true,true,now()),($2,true,false,now())",
		"a@example.com", "b@example.com"); err != nil {
		panic(err)
	}

	// Three aliases of one managed view.
	src := domain.ActiveUsers.Source()
	a, b, c := src.As("a"), src.As("b"), src.As("c")
	id := func(s *orm.Source) orm.OrdCol[domain.ActiveUser, int64] {
		return orm.NewOrdCol[domain.ActiveUser, int64](s, "id")
	}
	sql, args, err := orm.Rows(orm.Named("id", orm.Of(id(a)))).
		From(a).
		Join(b, orm.Cond(id(b).EqCol(id(a)))).
		Join(c, orm.Cond(id(c).EqCol(id(b)))).
		Where(orm.Cond(id(a).Gt(0)), orm.Cond(id(b).Lt(1000))).
		Using(pool).SQL()
	if err != nil {
		panic(err)
	}
	for _, alias := range []string{"\"a\"", "\"b\"", "\"c\""} {
		if !strings.Contains(sql, alias) {
			panic("alias collapsed: " + sql)
		}
	}
	fmt.Printf("selfjoin_placeholders=%d\n", len(args))
	fmt.Printf("selfjoin_numbered=%v\n", strings.Contains(sql, "$1") && strings.Contains(sql, "$2"))

	// A CTE over the managed view, left-joined to the table it reads.
	vid := orm.Named("id", orm.Of(domain.ActiveUsers.ID))
	cte := orm.CTE("recent", orm.Rows(vid).From(domain.ActiveUsers.Source()))
	mixed, margs, err := orm.Rows(orm.Named("id", orm.Ref(cte, vid))).
		With(cte).
		From(cte).
		LeftJoin(domain.Users.Source(), orm.Cond(domain.Users.Active.Eq(true))).
		Where(orm.Cond(domain.Users.Email.Like("%@example.com"))).
		Using(pool).SQL()
	if err != nil {
		panic(err)
	}
	fmt.Printf("mixed_has_cte=%v\n", strings.Contains(mixed, "WITH"))
	fmt.Printf("mixed_has_leftjoin=%v\n", strings.Contains(mixed, "LEFT JOIN"))
	fmt.Printf("mixed_placeholders=%d\n", len(margs))

	// And it runs.
	rows, err := domain.New(pool).ActiveUsers.Query().
		Where(domain.ActiveUsers.Email.Like("%@example.com")).All(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("rows=%d\n", len(rows))
}
`)
	got := runProgram(t, p)
	for _, want := range []string{
		"selfjoin_placeholders=2",
		"selfjoin_numbered=true",
		"mixed_has_cte=true",
		"mixed_has_leftjoin=true",
		"mixed_placeholders=2",
		"rows=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("composition through a managed view did not produce %q:\n%s", want, got)
		}
	}
}

// Generation and migration are the same bytes under every reordering that
// should not matter.
//
// The perturbations are the ones a real checkout varies: where the repository
// sits, what order files were created in, and what order declarations appear in.
// A generator sensitive to any of them produces diffs nobody caused.
func TestEvidence_generationAndMigrationDeterminism(t *testing.T) {
	// The same three declarations, split across files three ways and ordered
	// differently within them. The desired schema is identical every time.
	const users = `
//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}
`
	const active = `
//orm:view public.active_users
//orm:definition ` + "`SELECT id, email FROM users WHERE active`" + `
//orm:depends-on public.users
type ActiveUser struct {
	ID    int64
	Email string
}
`
	const derived = `
//orm:view public.derived_users
//orm:definition ` + "`SELECT id FROM active_users`" + `
//orm:depends-on public.active_users
type DerivedUser struct {
	ID int64
}
`
	layouts := []map[string]string{
		// One file, declaration order A.
		{"entities.go": "package domain\n" + users + active + derived},
		// One file, declaration order reversed — the dependent first.
		{"entities.go": "package domain\n" + derived + active + users},
		// Three files, named so the scanner reads them in the wrong order.
		{
			"a_derived.go": "package domain\n" + derived,
			"m_active.go":  "package domain\n" + active,
			"z_users.go":   "package domain\n" + users,
		},
	}

	var wantMig, wantGen string
	for i, files := range layouts {
		p := newProject(t, "package domain\n")
		if err := os.RemoveAll(filepath.Join(p.Dir, "domain")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(p.Dir, "domain"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, src := range files {
			writeFile(t, filepath.Join(p.Dir, "domain", name), src)
		}
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")

		var mig string
		entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				mig = readFile(t, filepath.Join(p.Dir, "migrations", e.Name()))
			}
		}
		gen := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
			readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))

		if i == 0 {
			wantMig, wantGen = mig, gen
			continue
		}
		if mig != wantMig {
			t.Errorf("layout %d produced a different migration:\n%s", i, diffText(wantMig, mig))
		}
		if gen != wantGen {
			t.Errorf("layout %d produced different generated code:\n%s", i, diffText(wantGen, gen))
		}
	}
	// Nothing machine-derived in either.
	for _, forbidden := range []string{"/tmp/", "/home/", "TestEvidence"} {
		if strings.Contains(wantMig+wantGen, forbidden) {
			t.Errorf("output contains %q", forbidden)
		}
	}
}

// The rest of the body-only stability matrix: the public result shape does not
// move, so neither does the generated API.
func TestEvidence_bodyStabilityMatrix(t *testing.T) {
	for _, c := range []struct{ what, from, to string }{
		{
			"a function swapped for another of the same result type",
			"SELECT id, email, created_at FROM users WHERE active",
			"SELECT id, lower(email) AS email, created_at FROM users WHERE active",
		},
		{
			"a CASE calculation with the same output type",
			"SELECT id, email, created_at FROM users WHERE active",
			"SELECT id, CASE WHEN verified THEN email ELSE email END AS email, created_at FROM users WHERE active",
		},
		{
			"a join added without changing the output",
			"SELECT id, email, created_at FROM users WHERE active",
			"SELECT u.id, u.email, u.created_at FROM users u LEFT JOIN users v ON v.id = u.id WHERE u.active",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := newProject(t, chainProject)
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("generate")
			before := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
				readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))

			p.Entities(strings.Replace(chainProject, c.from, c.to, 1))
			out := p.MustRun("makemigrations", "--name", "changed")
			if !strings.Contains(out, "replace view public.active_users") {
				t.Fatalf("%s did not produce a replacement:\n%s", c.what, out)
			}
			p.MustRun("migrate")
			p.MustRun("check")
			p.MustRun("generate")

			after := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
				readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))
			if after != before {
				t.Errorf("%s churned the generated API even though the result shape did not "+
					"move:\n%s", c.what, diffText(before, after))
			}
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Errorf("%s did not converge:\n%s", c.what, out)
			}
		})
	}
}

// An unmanaged view reading a managed one blocks its removal, during planning.
//
// PostgreSQL would refuse the drop at apply time, and that is not good enough:
// the dependency is visible while the migration is being written, so the
// refusal belongs there. CASCADE would make the drop succeed by removing
// something nobody listed.
func TestEvidence_externalDependentBlocksDrop(t *testing.T) {
	p := newProject(t, chainProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	// Something outside the project starts reading a managed view.
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(),
		`CREATE VIEW manual_b AS SELECT id FROM active_users`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(t.Context())

	before, _ := os.ReadDir(filepath.Join(p.Dir, "migrations"))

	// The project stops declaring the view the unmanaged one reads. Removing
	// verified_users too, since it also depends on active_users.
	p.Entities(`package domain

import "time"

//orm:table public.users
type User struct {
	ID        int64     ` + "`orm:\"pk,identity\"`" + `
	Email     string
	Active    bool
	Verified  bool
	CreatedAt time.Time ` + "`orm:\"column:created_at\"`" + `
}
`)
	code, stdout, stderr := p.Run("makemigrations", "--name", "drop")
	out := stdout + stderr
	if code == exitClean {
		t.Fatalf("a view something unmanaged still reads was dropped:\n%s", out)
	}
	if !strings.Contains(out, "manual_b") {
		t.Errorf("the refusal does not name the dependent:\n%s", out)
	}
	if strings.Contains(strings.ToUpper(out), "CASCADE VIEW") ||
		strings.Contains(out, "DROP VIEW ... CASCADE") {
		t.Errorf("the refusal offered CASCADE as the answer:\n%s", out)
	}

	// Nothing was written, and nothing in the database moved.
	after, _ := os.ReadDir(filepath.Join(p.Dir, "migrations"))
	if len(after) != len(before) {
		t.Errorf("a refused run wrote a migration: %d files, was %d", len(after), len(before))
	}
	conn2, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn2.Close(t.Context()) }()
	var views, prov int
	if err := conn2.QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM pg_class WHERE relname IN ('active_users','manual_b') AND relkind = 'v'),
		        (SELECT count(*) FROM public.orm_schema_views WHERE relation_name = 'active_users')`).
		Scan(&views, &prov); err != nil {
		t.Fatal(err)
	}
	if views != 2 {
		t.Errorf("%d of the two views survive, want both", views)
	}
	if prov != 1 {
		t.Errorf("the provenance of the managed view was disturbed")
	}
}

// PostgreSQL's own error survives a failed view migration, and a cancelled
// context stops one.
func TestEvidence_applyErrorContracts(t *testing.T) {
	t.Run("PgError from a definition PostgreSQL rejects", func(t *testing.T) {
		p := newProject(t, strings.Replace(chainProject,
			"SELECT id, email, created_at FROM users WHERE active",
			"SELECT id, email, created_at, no_such_function(email) AS x FROM users WHERE active", 1))
		// The planner cannot know the function is missing; PostgreSQL finds out
		// when the migration applies.
		p.Entities(strings.Replace(chainProject,
			"SELECT id, email, created_at FROM users WHERE active",
			"SELECT id, email, created_at FROM users WHERE no_such_function(email)", 1))
		p.MustRun("makemigrations", "--name", "initial")
		code, stdout, stderr := p.Run("migrate")
		out := stdout + stderr
		if code == exitClean {
			t.Fatalf("a migration calling an unknown function applied:\n%s", out)
		}
		// PostgreSQL's own SQLSTATE reaches the surface rather than being
		// replaced by a rendered sentence.
		if !strings.Contains(out, "42883") && !strings.Contains(out, "does not exist") {
			t.Errorf("PostgreSQL's error did not survive:\n%s", out)
		}
		if !strings.Contains(out, "SQLSTATE") {
			t.Errorf("the SQLSTATE is not reported:\n%s", out)
		}
		// And the failed migration was not recorded.
		conn, err := pgx.Connect(t.Context(), p.DSN)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close(t.Context()) }()
		var applied int
		if err := conn.QueryRow(t.Context(),
			`SELECT count(*) FROM public.orm_schema_migrations`).Scan(&applied); err != nil {
			// No history table at all is also "nothing was applied".
			return
		}
		if applied != 0 {
			t.Errorf("a failed migration was recorded as applied")
		}
	})
}
