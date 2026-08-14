package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// M16.5 G1 acceptance: a managed view, from declaration to typed query.
//
// This is the criterion the milestone turns on. A user writes a view
// declaration, runs four commands, and gets a typed read-only source — and the
// writes are compile errors rather than runtime ones.

const chainProject = `package domain

import "time"

//orm:table public.users
type User struct {
	ID        int64     ` + "`orm:\"pk,identity\"`" + `
	Email     string
	Active    bool
	Verified  bool
	CreatedAt time.Time ` + "`orm:\"column:created_at\"`" + `
}

//orm:view public.active_users
//orm:definition ` + "`SELECT id, email, created_at FROM users WHERE active`" + `
//orm:depends-on public.users
type ActiveUser struct {
	ID        int64
	Email     string
	CreatedAt time.Time ` + "`orm:\"column:created_at\"`" + `
}

//orm:view public.verified_users
//orm:definition ` + "`SELECT id, email FROM active_users`" + `
//orm:depends-on public.active_users
type VerifiedUser struct {
	ID    int64
	Email string
}
`

// The whole workflow, then a program that uses what it produced.
func TestG1_managedViewChainRoundtrip(t *testing.T) {
	p := newProject(t, chainProject)

	out := p.MustRun("makemigrations", "--name", "initial")
	// Dependency order is visible in the migration itself: the table, then the
	// view that reads it, then the view that reads that.
	iTable := strings.Index(out, "create table")
	iActive := strings.Index(out, "create view public.active_users")
	iVerified := strings.Index(out, "create view public.verified_users")
	if iTable < 0 || iActive < 0 || iVerified < 0 {
		t.Fatalf("the migration is missing an operation:\n%s", out)
	}
	if !(iTable < iActive && iActive < iVerified) {
		t.Errorf("operations are out of dependency order:\n%s", out)
	}

	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	// The generated handle gives a view a read-only repository.
	gen := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
	for _, want := range []string{
		"ActiveUsers *orm.ViewRepo[ActiveUser]",
		"VerifiedUsers *orm.ViewRepo[VerifiedUser]",
		"Users *orm.Repo[User]",
	} {
		if !strings.Contains(strings.Join(strings.Fields(gen), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("the generated handle does not declare %q:\n%s", want, gen)
		}
	}

	// A real program, compiled and run against the real database.
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
	db := domain.New(pool)

	if _, err := pool.Exec(ctx,
		"INSERT INTO users (email, active, verified, created_at) VALUES ($1, true, true, now()), ($2, false, false, now())",
		"a@example.com", "gone@example.com"); err != nil {
		panic(err)
	}

	rows, err := db.ActiveUsers.Query().
		Where(domain.ActiveUsers.Email.Like("%@example.com")).
		OrderBy(domain.ActiveUsers.Email.Asc()).
		All(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("active=%d\n", len(rows))
	for _, r := range rows {
		fmt.Printf("row=%s\n", r.Email)
	}

	v, err := db.VerifiedUsers.Query().All(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Printf("verified=%d\n", len(v))
}
`)
	got := runProgram(t, p)
	for _, want := range []string{"active=1", "row=a@example.com", "verified=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("the typed query did not produce %q:\n%s", want, got)
		}
	}

	// Converged.
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the workflow did not converge:\n%s", out)
	}
}

// A body-only change replaces the view and leaves the generated API alone.
func TestG1_bodyChangeDoesNotChurnGeneratedCode(t *testing.T) {
	p := newProject(t, chainProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")
	before := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
	beforeMeta := readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))

	p.Entities(strings.Replace(chainProject,
		"SELECT id, email, created_at FROM users WHERE active",
		"SELECT id, email, created_at FROM users WHERE active AND verified", 1))

	out := p.MustRun("makemigrations", "--name", "verified")
	if !strings.Contains(out, "replace view public.active_users") {
		t.Fatalf("a body-only change did not produce a replacement:\n%s", out)
	}
	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	// The output shape did not move, so neither did the generated API. A body
	// change that churned generated code would make every predicate edit a
	// diff across the whole package.
	if got := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")); got != before {
		t.Errorf("a body-only change rewrote the generated handle:\n%s", diffText(before, got))
	}
	if got := readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go")); got != beforeMeta {
		t.Errorf("a body-only change rewrote the generated metadata")
	}
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the replacement did not converge:\n%s", out)
	}
}

// The acceptance criterion's other half: the writes do not compile.
func TestG1_generatedViewIsCompileTimeReadOnly(t *testing.T) {
	p := newProject(t, chainProject)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")

	for _, c := range []struct {
		what string
		body string
	}{
		{"Insert", "_, _ = db.ActiveUsers.Insert(ctx, domain.ActiveUser{})"},
		{"InsertMany", "_, _ = db.ActiveUsers.InsertMany(ctx, nil)"},
		{"Update", "_ = db.ActiveUsers.Update()"},
		{"Delete", "_ = db.ActiveUsers.Delete()"},
		{"CopyFrom", "_, _ = db.ActiveUsers.CopyFrom(ctx, nil)"},
		{"Refresh", "_ = db.ActiveUsers.Refresh(ctx)"},
	} {
		t.Run("no "+c.what, func(t *testing.T) {
			if out := compileAgainst(t, p, c.body); out == "" {
				t.Errorf("db.ActiveUsers.%s compiled. A write on a read-only source is a "+
					"runtime failure waiting on the least-tested path there is", c.what)
			} else if !strings.Contains(out, "undefined") && !strings.Contains(out, "has no field or method") {
				t.Errorf("%s failed to compile for the wrong reason:\n%s", c.what, out)
			}
		})
	}

	// And the reads do compile, so the absence is not a type with no methods.
	for _, c := range []struct{ what, body string }{
		{"Query().All", "_, _ = db.ActiveUsers.Query().All(ctx)"},
		{"Query().Where", "_, _ = db.ActiveUsers.Query().Where(domain.ActiveUsers.ID.Gt(0)).All(ctx)"},
		{"Query().OrderBy", "_, _ = db.ActiveUsers.Query().OrderBy(domain.ActiveUsers.ID.Asc()).All(ctx)"},
		{"Query().Limit", "_, _ = db.ActiveUsers.Query().Limit(1).All(ctx)"},
		{"QueryFrom", "_, _ = db.ActiveUsers.QueryFrom(domain.ActiveUsers.Source()).All(ctx)"},
		{"Query().SQL", "_, _, _ = db.ActiveUsers.Query().SQL()"},
		{"Query().Rows", "for range db.ActiveUsers.Query().Rows(ctx) { }"},
		{"Query().Count", "_, _ = db.ActiveUsers.Query().Count(ctx)"},
	} {
		t.Run("has "+c.what, func(t *testing.T) {
			if out := compileAgainst(t, p, c.body); out != "" {
				t.Errorf("%s does not compile on a view:\n%s", c.what, out)
			}
		})
	}

	// The table keeps everything, so this did not narrow the API generally.
	for _, c := range []struct{ what, body string }{
		{"Insert", "_, _ = db.Users.Insert(ctx, domain.User{})"},
		{"Update", "_ = db.Users.Update()"},
		{"Delete", "_ = db.Users.Delete()"},
		{"CopyFrom", "_, _ = db.Users.CopyFrom(ctx, nil)"},
	} {
		t.Run("table keeps "+c.what, func(t *testing.T) {
			if out := compileAgainst(t, p, c.body); out != "" {
				t.Errorf("a table lost %s:\n%s", c.what, out)
			}
		})
	}
}

// compileAgainst builds a program using the generated package, returning the
// compiler's output and empty when it built.
func compileAgainst(t *testing.T, p *project, body string) string {
	t.Helper()
	dir := filepath.Join(p.Dir, "probe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "main.go"), `package main

import (
	"context"

	"example.com/managed/domain"
)

var (
	ctx = context.Background()
	db  = domain.New(nil)
)

func main() {
	`+body+`
}
`)
	cmd := exec.Command("go", "build", "-o", os.DevNull, "./probe/")
	cmd.Dir = p.Dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return ""
	}
	return string(out)
}

// runProgram builds and runs the fixture's main package against its database.
func runProgram(t *testing.T, p *project) string {
	t.Helper()
	cmd := exec.Command("go", "run", ".")
	cmd.Dir = p.Dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "DSN="+p.DSN)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running the generated consumer: %v\n%s", err, out)
	}
	return string(out)
}

func diffText(a, b string) string {
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
