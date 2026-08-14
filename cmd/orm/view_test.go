package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 Stage B+C: what the CLI does with a managed view before the planner
// exists.
//
// The requirement is not that it works. It is that it refuses in a way nobody
// can mistake for success, and leaves nothing behind — no partial migration, no
// file half-written, and above all no view quietly reinterpreted as a table.

const viewProject = `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:view public.active_users
//orm:definition ` + "`SELECT id, email FROM users WHERE active`" + `
//orm:depends-on public.users
type ActiveUser struct {
	ID    int64
	Email string
}
`

// M16.5 G1: the complete raw managed view roundtrip, through the real commands.
//
// This is the question the milestone turns on: can a plan survive
// serialisation, be produced by the CLI, be applied with its provenance, and
// converge — so that running makemigrations again writes nothing?
func TestView_rawRoundtrip(t *testing.T) {
	p := newProject(t, viewProject)

	// Plan, apply, check.
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")

	// Converged: the second run has nothing to say.
	out := p.MustRun("makemigrations")
	if !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the second makemigrations was not empty:\n%s", out)
	}

	// The view is really there, and the migration created it in the right
	// order — it cannot exist unless the table it reads was created first.
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var kind string
	if err := conn.QueryRow(t.Context(),
		`SELECT relkind::text FROM pg_class WHERE relname = 'active_users'`).Scan(&kind); err != nil {
		t.Fatalf("the view was not created: %v", err)
	}
	if kind != "v" {
		t.Errorf("active_users has relkind %q, want v", kind)
	}

	// Provenance was recorded, by the migration, in the same transaction.
	var canonical string
	if err := conn.QueryRow(t.Context(),
		`SELECT canonical FROM public.orm_schema_views WHERE relation_name = 'active_users'`).
		Scan(&canonical); err != nil {
		t.Fatalf("no provenance was recorded: %v", err)
	}
	// It is the server's reconstruction, not the developer's text.
	if strings.Contains(canonical, "SELECT id, email FROM users WHERE active") {
		t.Errorf("the developer's own SQL was stored as the canonical definition:\n%s", canonical)
	}
	if !strings.Contains(canonical, "SELECT") {
		t.Errorf("the recorded definition does not look like a definition:\n%s", canonical)
	}
}

// A body-only change replaces the view in place, and converges again.
func TestView_bodyChangeRoundtrip(t *testing.T) {
	p := newProject(t, viewProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	p.Entities(strings.Replace(viewProject,
		"SELECT id, email FROM users WHERE active",
		"SELECT id, email FROM users WHERE active AND id > 0", 1))

	out := p.MustRun("makemigrations", "--name", "narrow")
	if !strings.Contains(out, "replace view") {
		t.Fatalf("a body-only change did not produce a replacement:\n%s", out)
	}
	p.MustRun("migrate")
	p.MustRun("check")

	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the roundtrip did not converge after a replacement:\n%s", out)
	}
}

// An unsafe change refuses and writes nothing.
func TestView_unsafeChangeRefuses(t *testing.T) {
	for _, c := range []struct{ what, from, to string }{
		{"a renamed output column", "Email string", "Email string `orm:\"column:email_address\"`"},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := newProject(t, viewProject)
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")

			p.Entities(strings.Replace(viewProject, c.from, c.to, 1))
			code, stdout, stderr := p.Run("makemigrations", "--name", "rename")
			out := stdout + stderr
			if code == exitClean {
				t.Fatalf("%s was accepted:\n%s", c.what, out)
			}
			before, _ := os.ReadDir(filepath.Join(p.Dir, "migrations"))
			if len(before) != 1 {
				t.Errorf("a refused run left %d migration files", len(before))
			}
		})
	}
}

// The manual-drift attack: a declaration change is not permission to overwrite
// somebody's manual edit.
func TestView_manualDriftIsNotOverwritten(t *testing.T) {
	p := newProject(t, viewProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	// Somebody changes the view in the database, by hand.
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(t.Context(),
		`CREATE OR REPLACE VIEW active_users AS SELECT id, email FROM users WHERE NOT active`); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(t.Context())

	// And the developer changes the declaration, independently.
	p.Entities(strings.Replace(viewProject,
		"SELECT id, email FROM users WHERE active",
		"SELECT id, email FROM users WHERE active AND id > 0", 1))

	code, stdout, stderr := p.Run("makemigrations", "--name", "narrow")
	out := stdout + stderr
	if code == exitClean {
		t.Fatalf("a source change silently overwrote a manual database edit:\n%s", out)
	}
	if !strings.Contains(out, "outside the migrations") {
		t.Errorf("the refusal does not explain what happened:\n%s", out)
	}
}

func TestView_projectsWithoutViewsStillMigrate(t *testing.T) {
	p := newProject(t, `package domain

//orm:table public.users
type User struct {
	ID    int64 `+"`orm:\"pk,identity\"`"+`
	Email string
}
`)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
}

// A view with no definition fails at check time, not at migration time: it is a
// declaration error and there is nothing to plan.
func TestView_missingDefinitionIsReportedEarly(t *testing.T) {
	p := newProject(t, `package domain

//orm:table public.users
type User struct {
	ID int64 `+"`orm:\"pk,identity\"`"+`
}

//orm:view public.active_users
type ActiveUser struct {
	ID int64
}
`)
	code, stdout, stderr := p.Run("makemigrations", "--name", "initial")
	out := stdout + stderr
	if code == exitClean {
		t.Fatal("a view with no definition was accepted")
	}
	if !strings.Contains(out, "E025") {
		t.Errorf("the refusal does not carry the registered code E025:\n%s", out)
	}
}
