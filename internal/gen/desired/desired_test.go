package desired_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/desired"
	"github.com/AlexAli29/orm/internal/gen/goscan"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Building a desired schema from Go declarations.
//
// The fixture project has never been generated into: it has entities, schema
// declarations and nothing else. That is the property under test — a project
// has to be able to describe the schema it wants before the code that reads
// that schema exists.

// project writes the fixture into a module of its own.
//
// It is built the way a real project is — its own go.mod, the ORM required and
// replaced with this checkout — because what is being tested is that a project
// can describe its schema before anything has been generated into it, and a
// fixture compiled as part of this repository would not be that.
func project(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	dir := t.TempDir()

	ownMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	mod := strings.Replace(string(ownMod), "module github.com/AlexAli29/orm", "module example.com/managed", 1) +
		"\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => " + root + "\n"
	write(t, filepath.Join(dir, "go.mod"), mod)

	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	write(t, filepath.Join(dir, "go.sum"), string(sum))

	entities, err := os.ReadFile(filepath.Join("testdata", "managed", "internal", "domain", "entities.go"))
	if err != nil {
		t.Fatalf("reading the fixture entities: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "domain"), 0o755); err != nil {
		t.Fatalf("creating the entity package: %v", err)
	}
	write(t, filepath.Join(dir, "domain", "entities.go"), string(entities))

	// Nothing is generated into it. That is the point.
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// build runs the pipeline over the fixture project.
func build(t *testing.T) *schema.Schema {
	t.Helper()
	root := project(t)
	scanned, err := goscan.Scan(t.Context(), root, []goscan.Target{{
		Dir: filepath.Join(root, "domain"),
	}})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(scanned.TagErrors) > 0 {
		t.Fatalf("tag errors: %+v", scanned.TagErrors)
	}
	s, err := desired.Build(desired.Input{
		Config:   &config.Config{Schema: config.Schema{Mode: config.ModeManaged, SearchPath: []string{"public"}}},
		Entities: scanned.Entities,
		Decls:    scanned.Decls,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return s
}

func table(t *testing.T, s *schema.Schema, name string) schema.Table {
	t.Helper()
	tbl, ok := s.Table("public", name)
	if !ok {
		t.Fatalf("no table %s in the desired schema", name)
	}
	return tbl
}

// The whole point: a desired schema is built with no database and no generated
// code in sight.
func TestBuild_freshProject(t *testing.T) {
	s := build(t)

	if len(s.Tables) != 5 {
		t.Fatalf("built %d tables, want five", len(s.Tables))
	}
	users := table(t, s, "users")

	// Column order is field order, which is what the scanner and the select
	// list agree on.
	var names []string
	for _, c := range users.Columns {
		names = append(names, c.Name)
	}
	want := []string{"id", "email", "name", "nickname", "active", "created_at"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("columns = %v, want %v", names, want)
	}
}

// Nullability comes from the Go type, by the same rule reconciliation uses.
func TestBuild_nullability(t *testing.T) {
	users := table(t, build(t), "users")

	for _, tt := range []struct {
		column   string
		nullable bool
	}{
		{"email", false},
		{"name", false},
		{"nickname", true},
		{"active", false},
	} {
		c, ok := users.Column(tt.column)
		if !ok {
			t.Fatalf("no column %s", tt.column)
		}
		if c.Nullable != tt.nullable {
			t.Errorf("%s nullable = %t, want %t", tt.column, c.Nullable, tt.nullable)
		}
	}
}

// A default is SQL, and a Go zero value is not a default. The bool column has
// one because a tag says so, and the string column has none because nothing
// does.
func TestBuild_defaults(t *testing.T) {
	users := table(t, build(t), "users")

	active, _ := users.Column("active")
	if active.Default != "true" {
		t.Errorf("active default = %q, want the declared SQL", active.Default)
	}
	created, _ := users.Column("created_at")
	if created.Default != "now()" {
		t.Errorf("created_at default = %q", created.Default)
	}
	name, _ := users.Column("name")
	if !name.Default.Empty() {
		t.Errorf("name default = %q, want none; a Go zero value is not a default", name.Default)
	}

	id, _ := users.Column("id")
	if id.Identity != schema.IdentityByDefault {
		t.Errorf("id identity = %v, want by default", id.Identity)
	}
	if !id.Default.Empty() || id.Nullable {
		t.Errorf("id = %+v, want an identity column with no default and no NULLs", id)
	}
}

func TestBuild_types(t *testing.T) {
	s := build(t)
	posts := table(t, s, "posts")

	for _, tt := range []struct {
		column string
		want   schema.Type
	}{
		{"id", schema.Type{Name: "int8"}},
		{"title", schema.Type{Name: "text"}},
		{"created_at", schema.Type{Name: "timestamptz"}},
		{"status", schema.Type{Schema: "public", Name: "post_status"}},
	} {
		c, ok := posts.Column(tt.column)
		if !ok {
			t.Fatalf("no column %s", tt.column)
		}
		if c.Type != tt.want {
			t.Errorf("%s type = %s, want %s", tt.column, c.Type, tt.want)
		}
	}

	// The enum itself is a schema object, declared once and in label order.
	if len(s.Enums) != 1 {
		t.Fatalf("enums = %+v, want one", s.Enums)
	}
	e := s.Enums[0]
	if e.Qualified() != "public.post_status" {
		t.Errorf("enum = %s", e.Qualified())
	}
	if strings.Join(e.Labels, ",") != "draft,published,archived" {
		t.Errorf("labels = %v, want them in declaration order", e.Labels)
	}
}

// The index declaration is where the canonical model's capabilities have to
// survive being written down.
func TestBuild_index(t *testing.T) {
	posts := table(t, build(t), "posts")

	var idx schema.Index
	for _, i := range posts.Indexes {
		if i.Name == "posts_feed_idx" {
			idx = i
		}
	}
	if idx.Name == "" {
		t.Fatalf("indexes = %+v, want posts_feed_idx", posts.Indexes)
	}
	if len(idx.Columns) != 2 {
		t.Fatalf("keys = %+v, want two", idx.Columns)
	}
	// Key order is meaning, and so is the direction.
	if idx.Columns[0].Name != "author_id" || idx.Columns[1].Name != "created_at" {
		t.Errorf("keys = %+v, want (author_id, created_at)", idx.Columns)
	}
	if idx.Columns[1].Direction != schema.Desc {
		t.Error("the second key is not descending")
	}
	if len(idx.Include) != 1 || idx.Include[0] != "title" {
		t.Errorf("include = %v, want (title)", idx.Include)
	}
	if idx.Where != "status = 'published'" {
		t.Errorf("predicate = %q", idx.Where)
	}
	// A partial index is not a uniqueness proof, and this one is not unique
	// either, so it must not have become one.
	for _, u := range posts.Uniques {
		if u.Name == "posts_feed_idx" {
			t.Error("a non-unique partial index was recorded as a uniqueness object")
		}
	}
}

// A relation and a foreign key are one fact. The relation declares it and the
// canonical schema holds exactly one constraint for it.
func TestBuild_foreignKeys(t *testing.T) {
	s := build(t)
	posts := table(t, s, "posts")

	if len(posts.ForeignKeys) != 1 {
		t.Fatalf("posts foreign keys = %+v, want one", posts.ForeignKeys)
	}
	fk := posts.ForeignKeys[0]
	if fk.Name != "posts_author_id_fkey" {
		t.Errorf("name = %s, want PostgreSQL's own convention", fk.Name)
	}
	if strings.Join(fk.Columns, ",") != "author_id" || fk.RefTable != "users" ||
		strings.Join(fk.RefColumns, ",") != "id" {
		t.Errorf("constraint = %+v", fk)
	}

	// The other side of the same relation does not declare it again.
	users := table(t, s, "users")
	if len(users.ForeignKeys) != 0 {
		t.Errorf("users foreign keys = %+v, want none; the other side owns them", users.ForeignKeys)
	}

	// A comment carries two, one per relation, in declaration order.
	comments := table(t, s, "comments")
	if len(comments.ForeignKeys) != 2 {
		t.Fatalf("comment foreign keys = %+v, want two", comments.ForeignKeys)
	}

	// A relation with no action tag asks for nothing, which is PostgreSQL's
	// NO ACTION. Saying so explicitly would be a change to the constraint.
	if fk.OnDelete != "" || fk.OnUpdate != "" {
		t.Errorf("posts_author_id_fkey = %+v, want no referential actions", fk)
	}
}

// The referential actions a relation declares reach the constraint.
//
// Without them managed mode could not express ON DELETE CASCADE at all, and
// worse: a database that already had one drifted against a declaration with no
// way to say it, so makemigrations planned to replace the cascade with NO
// ACTION. The schema type always carried the fields and the SQL writer always
// emitted them — the only thing missing was a way for an author to ask.
func TestBuild_referentialActions(t *testing.T) {
	s := build(t)
	comments := table(t, s, "comments")

	byName := map[string]schema.ForeignKey{}
	for _, fk := range comments.ForeignKeys {
		byName[fk.Name] = fk
	}

	post, ok := byName["comments_post_id_fkey"]
	if !ok {
		t.Fatalf("constraints = %+v, want one on post_id", comments.ForeignKeys)
	}
	if post.OnDelete != schema.Cascade {
		t.Errorf("on delete = %q, want %q", post.OnDelete, schema.Cascade)
	}
	if post.OnUpdate != "" {
		t.Errorf("on update = %q, want nothing; only ondelete was declared", post.OnUpdate)
	}

	author, ok := byName["comments_author_id_fkey"]
	if !ok {
		t.Fatalf("constraints = %+v, want one on author_id", comments.ForeignKeys)
	}
	if author.OnDelete != schema.Restrict || author.OnUpdate != schema.Cascade {
		t.Errorf("author actions = %q/%q, want RESTRICT/CASCADE", author.OnDelete, author.OnUpdate)
	}

	// And they reach the rendered schema, which is the only part a database sees.
	var buf strings.Builder
	if err := schema.Text(&buf, s); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	if !strings.Contains(buf.String(), "ON DELETE CASCADE") {
		t.Errorf("the rendered schema has no ON DELETE CASCADE:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "ON UPDATE CASCADE") {
		t.Errorf("the rendered schema has no ON UPDATE CASCADE:\n%s", buf.String())
	}
}

// A unique tag and a unique declaration both produce constraints, named the way
// PostgreSQL names them.
func TestBuild_uniques(t *testing.T) {
	s := build(t)

	users := table(t, s, "users")
	if len(users.Uniques) != 1 || users.Uniques[0].Name != "users_email_key" {
		t.Fatalf("users uniques = %+v", users.Uniques)
	}
	if !users.Uniques[0].Constraint {
		t.Error("a unique tag produced an index rather than a constraint")
	}

	comments := table(t, s, "comments")
	var multi schema.Unique
	for _, u := range comments.Uniques {
		if u.Name == "comments_post_author_key" {
			multi = u
		}
	}
	if strings.Join(multi.Columns, ",") != "post_id,author_id" {
		t.Errorf("unique columns = %v, want them in declaration order", multi.Columns)
	}
}

func TestBuild_checks(t *testing.T) {
	users := table(t, build(t), "users")
	if len(users.Checks) != 1 {
		t.Fatalf("checks = %+v, want one", users.Checks)
	}
	if users.Checks[0].Name != "users_email_not_blank" || users.Checks[0].Expression != "email <> ''" {
		t.Errorf("check = %+v", users.Checks[0])
	}
}

// The same declarations produce the same schema every time. A pipeline that
// depended on map order would produce a different migration on alternate runs.
func TestBuild_isDeterministic(t *testing.T) {
	first := build(t)
	for range 20 {
		again := build(t)
		if diffs := schema.Diff(first, again); len(diffs) > 0 {
			t.Fatalf("the desired schema differs between runs:\n    %s", strings.Join(diffs, "\n    "))
		}
	}
}

// The desired schema is a canonical schema like any other, so the migration
// engine can diff it without knowing where it came from.
func TestBuild_feedsTheMigrationEngine(t *testing.T) {
	s := build(t)

	d, err := migrate.Compute(&schema.Schema{}, s, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if d.Empty() {
		t.Fatal("no operations")
	}

	// Applying it reaches the schema it was computed from, which is the
	// invariant everything downstream depends on.
	state := &schema.Schema{}
	for _, op := range d.Operations {
		if err := op.Apply(state); err != nil {
			t.Fatalf("applying %s: %v", op.Describe(), err)
		}
	}
	if diffs := schema.Diff(state, s); len(diffs) > 0 {
		t.Errorf("applying the diff did not reach the desired schema:\n    %s", strings.Join(diffs, "\n    "))
	}
}

// A unique index is an index and nothing else.
//
// It was once recorded as a uniqueness object as well, which produced a schema
// naming one object twice — and a CREATE TABLE that built it twice, the second
// statement failing on a name the first had taken. That made the first
// migration of any project declaring a unique index unrunnable.
func TestBuild_uniqueIndexIsNotAlsoAConstraint(t *testing.T) {
	root := project(t)
	write(t, filepath.Join(root, "domain", "extra.go"), `package domain

//orm:table widgets
//orm:index widgets_code_key (Code) unique
//orm:index widgets_slug_key (Slug) unique where "code <> ''"
type Widget struct {
	ID   int64  `+"`orm:\"pk,identity\"`"+`
	Code string
	Slug string
}
`)
	scanned, err := goscan.Scan(t.Context(), root, []goscan.Target{{Dir: filepath.Join(root, "domain")}})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	s, err := desired.Build(desired.Input{
		Config:   &config.Config{Schema: config.Schema{Mode: config.ModeManaged, SearchPath: []string{"public"}}},
		Entities: scanned.Entities,
		Decls:    scanned.Decls,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	widgets := table(t, s, "widgets")

	if len(widgets.Uniques) != 0 {
		t.Errorf("a declared unique index became a uniqueness constraint too: %+v", widgets.Uniques)
	}
	var total, partial bool
	for _, i := range widgets.Indexes {
		switch i.Name {
		case "widgets_code_key":
			total = i.Unique
		case "widgets_slug_key":
			partial = i.Unique && !i.Where.Empty()
		}
	}
	if !total || !partial {
		t.Fatalf("indexes = %+v", widgets.Indexes)
	}

	// And the migration that builds it names it once.
	d, err := migrate.Compute(&schema.Schema{}, s, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	var statements []string
	for _, op := range d.Operations {
		got, err := op.SQL()
		if err != nil {
			t.Fatalf("%s: %v", op.Describe(), err)
		}
		statements = append(statements, got...)
	}
	var created int
	for _, st := range statements {
		if strings.Contains(st, `"widgets_code_key"`) {
			created++
		}
	}
	if created != 1 {
		t.Errorf("widgets_code_key is created %d times:\n%s", created, strings.Join(statements, "\n"))
	}
}

// M12.2: a managed schema creates the right range family for each field.
//
// The three that share time.Time are the point. Nothing infers a family from a
// Go type: the two that are not the default say so with a tag, and the one that
// is gets the zoned family for the same reason a bare time.Time does.
func TestBuild_ranges(t *testing.T) {
	s := build(t)
	b := table(t, s, "bookings")

	for _, tt := range []struct{ column, want string }{
		{"period", "tstzrange"},
		{"stay", "daterange"},
		{"shift", "tsrange"},
		{"quota", "int4range"},
		{"span", "int8range"},
		{"revised", "tstzrange"},
		{"lease", "interval"},
		{"grace", "interval"},
		{"holds", "tstzmultirange"},
		{"slots", "int4multirange"},
	} {
		col, ok := b.Column(tt.column)
		if !ok {
			t.Errorf("no column %s", tt.column)
			continue
		}
		if got := col.Type.String(); got != tt.want {
			t.Errorf("bookings.%s is %s, want %s", tt.column, got, tt.want)
		}
	}

	// Nullability still comes from the Go type and from nowhere else.
	for _, tt := range []struct {
		column   string
		nullable bool
	}{
		{"period", false}, {"revised", true}, {"lease", false}, {"grace", true},
		{"holds", false}, {"slots", true},
	} {
		col, ok := b.Column(tt.column)
		if !ok {
			continue
		}
		if col.Nullable != tt.nullable {
			t.Errorf("bookings.%s nullable = %v, want %v", tt.column, col.Nullable, tt.nullable)
		}
	}

	// A GiST index over a range column is an ordinary index declaration: the
	// access method is what makes it useful, and nothing here is range-aware.
	var found bool
	for _, idx := range b.Indexes {
		if idx.Name != "bookings_period_gist" {
			continue
		}
		found = true
		if idx.Method != "gist" {
			t.Errorf("the index method is %q, want gist", idx.Method)
		}
		if len(idx.Columns) != 1 || idx.Columns[0].Name != "period" {
			t.Errorf("the index covers %+v, want period", idx.Columns)
		}
	}
	if !found {
		t.Errorf("no bookings_period_gist index in %+v", b.Indexes)
	}
}

// Release-critical: one table is described by one entity.
//
// Two entities naming the same table used to each contribute a Table to the
// desired schema. The differ then compared the database against whichever of
// the two it reached, so a second, partial view of a table was read as an
// instruction to drop every column it did not mention — and `orm makemigrations`
// wrote that migration and exited zero. Reconciliation had always reported the
// conflict as E017, but reconciliation compares Go against the database and
// does not run on this path: in managed mode the declarations are the schema.
//
// Both layouts are tested because the second is the one that makes the mistake
// easy: two packages cannot see each other's //orm:table directives, so nothing
// but this check stands between a duplicated table name and a destructive
// migration.
func TestBuild_refusesTwoEntitiesOverOneTable(t *testing.T) {
	const wide = `package %s

//orm:table products
type Product struct {
	ID   int64  ` + "`orm:\"pk,identity\"`" + `
	SKU  string ` + "`orm:\"unique\"`" + `
	Name string
}
`
	const narrow = `package %s

//orm:table products
type ProductView struct {
	ID  int64 ` + "`orm:\"pk,identity\"`" + `
	SKU string
}
`

	for _, c := range []struct {
		what  string
		files map[string]string // package dir -> source
	}{
		{
			what: "one package",
			files: map[string]string{
				"domain": fmt.Sprintf(wide, "domain") + "\n" +
					strings.TrimPrefix(fmt.Sprintf(narrow, "domain"), "package domain\n"),
			},
		},
		{
			what: "two packages",
			files: map[string]string{
				"catalog": fmt.Sprintf(wide, "catalog"),
				"sales":   fmt.Sprintf(narrow, "sales"),
			},
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			root := module(t, c.files)

			targets := make([]goscan.Target, 0, len(c.files))
			for dir := range c.files {
				targets = append(targets, goscan.Target{Dir: filepath.Join(root, dir)})
			}
			slices.SortFunc(targets, func(a, b goscan.Target) int { return strings.Compare(a.Dir, b.Dir) })

			scanned, err := goscan.Scan(t.Context(), root, targets)
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			_, err = desired.Build(desired.Input{
				Config:   &config.Config{Schema: config.Schema{Mode: config.ModeManaged, SearchPath: []string{"public"}}},
				Entities: scanned.Entities,
				Decls:    scanned.Decls,
			})
			if err == nil {
				t.Fatal("two entities over one table built a desired schema; the next step would have been a migration that drops columns")
			}
			msg := err.Error()
			// The message has to name both sides. "duplicate table" leaves the
			// reader grepping for which two.
			for _, want := range []string{"Product", "ProductView", "public.products"} {
				if !strings.Contains(msg, want) {
					t.Errorf("the error does not mention %q: %s", want, msg)
				}
			}
		})
	}
}

// module writes a throwaway module containing the given packages.
func module(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	dir := t.TempDir()

	ownMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	write(t, filepath.Join(dir, "go.mod"),
		strings.Replace(string(ownMod), "module github.com/AlexAli29/orm", "module example.com/multi", 1)+
			"\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => "+root+"\n")

	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	write(t, filepath.Join(dir, "go.sum"), string(sum))

	for pkg, src := range files {
		if err := os.MkdirAll(filepath.Join(dir, pkg), 0o755); err != nil {
			t.Fatalf("creating %s: %v", pkg, err)
		}
		write(t, filepath.Join(dir, pkg, "entities.go"), src)
	}
	return dir
}

// Entities in several packages, with no relation between them, build one
// schema. This is the shape the check above must not break.
func TestBuild_spansPackages(t *testing.T) {
	root := module(t, map[string]string{
		"catalog": `package catalog

//orm:table products
type Product struct {
	ID  int64  ` + "`orm:\"pk,identity\"`" + `
	SKU string ` + "`orm:\"unique\"`" + `
}
`,
		"sales": `package sales

//orm:table orders
type Order struct {
	ID        int64 ` + "`orm:\"pk,identity\"`" + `
	ProductID int64
}
`,
	})
	scanned, err := goscan.Scan(t.Context(), root, []goscan.Target{
		{Dir: filepath.Join(root, "catalog")},
		{Dir: filepath.Join(root, "sales")},
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	s, err := desired.Build(desired.Input{
		Config:   &config.Config{Schema: config.Schema{Mode: config.ModeManaged, SearchPath: []string{"public"}}},
		Entities: scanned.Entities,
		Decls:    scanned.Decls,
	})
	if err != nil {
		t.Fatalf("Build across two packages: %v", err)
	}
	if len(s.Tables) != 2 {
		t.Fatalf("the desired schema has %d tables, want 2", len(s.Tables))
	}
	table(t, s, "products")
	table(t, s, "orders")
}
