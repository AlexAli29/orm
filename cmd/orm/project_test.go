package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// A managed project, built the way a real one is.
//
// The scenarios below are end-to-end on purpose. Every part of the managed
// workflow is unit-tested somewhere, and none of that would catch the failure
// that actually matters: that the commands do not compose — that the migration
// makemigrations wrote is not one migrate can run, or that the schema migrate
// produced is not one generate can prove a mapping against.
//
// So the fixture is a module of its own, with its own go.mod, its own database,
// and nothing generated into it. The commands are then run in-process against
// it, in the order a person would run them.

// project is a throwaway managed project.
type project struct {
	t    *testing.T
	Dir  string
	DSN  string
	Conf string
}

// newProject writes a module with an entity package and a database of its own.
func newProject(t *testing.T, entities string) *project {
	t.Helper()
	testdb.AdminDSN(t)

	root, err := filepath.Abs(filepath.Join("..", ".."))
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
	writeFile(t, filepath.Join(dir, "go.mod"), mod)

	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	writeFile(t, filepath.Join(dir, "go.sum"), string(sum))

	p := &project{t: t, Dir: dir, DSN: testdb.Create(t, ""), Conf: filepath.Join(dir, "orm.yaml")}
	p.Entities(entities)
	p.Config("managed")
	return p
}

// Config writes an orm.yaml for the given schema mode.
func (p *project) Config(mode string) {
	p.t.Helper()
	// The DSN is written literally rather than through an environment
	// reference: the point of the fixture is the workflow, and an expansion
	// failure would look like a workflow failure.
	writeFile(p.t, p.Conf, "version: 1\n\nschema:\n  mode: "+mode+"\n  dsn: \""+p.DSN+"\"\n"+
		"  search_path:\n    - public\n\nmigrations:\n  dir: migrations\n\n"+
		"packages:\n  - path: ./domain\n    output: same\n")
}

// Entities replaces the entity package's source.
func (p *project) Entities(src string) {
	p.t.Helper()
	if err := os.MkdirAll(filepath.Join(p.Dir, "domain"), 0o755); err != nil {
		p.t.Fatalf("creating the entity package: %v", err)
	}
	writeFile(p.t, filepath.Join(p.Dir, "domain", "entities.go"), src)
}

// Run invokes a command, with nothing on standard input.
func (p *project) Run(args ...string) (int, string, string) {
	p.t.Helper()
	return p.RunInput("", args...)
}

// RunInput invokes a command with input to answer its questions.
func (p *project) RunInput(stdin string, args ...string) (int, string, string) {
	p.t.Helper()
	full := append(append([]string{}, args...), "--config", p.Conf)
	var stdout, stderr bytes.Buffer
	code := runIO(p.t.Context(), full, strings.NewReader(stdin), &stdout, &stderr)
	p.t.Logf("orm %s -> %d\n%s%s", strings.Join(args, " "), code, stdout.String(), stderr.String())
	return code, stdout.String(), stderr.String()
}

// MustRun invokes a command and fails the test unless it succeeded.
func (p *project) MustRun(args ...string) string {
	p.t.Helper()
	code, stdout, stderr := p.Run(args...)
	if code != exitClean {
		p.t.Fatalf("orm %s: exit %d\n%s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

// SQL runs statements against the project's database.
func (p *project) SQL(statements string) {
	p.t.Helper()
	conn := p.Conn()
	defer func() { _ = conn.Close(context.WithoutCancel(p.t.Context())) }()
	if _, err := conn.PgConn().Exec(p.t.Context(), statements).ReadAll(); err != nil {
		p.t.Fatalf("running SQL: %v", err)
	}
}

// Query runs one query and returns its first column, as text.
func (p *project) Query(sql string) []string {
	p.t.Helper()
	conn := p.Conn()
	defer func() { _ = conn.Close(context.WithoutCancel(p.t.Context())) }()
	rows, err := conn.Query(p.t.Context(), sql)
	if err != nil {
		p.t.Fatalf("querying: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v *string
		if err := rows.Scan(&v); err != nil {
			p.t.Fatalf("scanning: %v", err)
		}
		if v == nil {
			out = append(out, "<null>")
			continue
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		p.t.Fatalf("querying: %v", err)
	}
	return out
}

func (p *project) Conn() *pgx.Conn {
	p.t.Helper()
	conn, err := pgx.Connect(p.t.Context(), p.DSN)
	if err != nil {
		p.t.Fatalf("connecting: %v", err)
	}
	return conn
}

// Migrations lists the migration files present.
func (p *project) Migrations() []string {
	p.t.Helper()
	entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		p.t.Fatalf("reading the migrations directory: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func contains(t *testing.T, got, want, what string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s does not contain %q:\n%s", what, want, got)
	}
}

func absent(t *testing.T, got, unwanted, what string) {
	t.Helper()
	if strings.Contains(got, unwanted) {
		t.Errorf("%s contains %q and should not:\n%s", what, unwanted, got)
	}
}

// blogEntities is the model scenario A starts from.
//
// It is deliberately the whole vocabulary: an identity primary key, a foreign
// key, a unique constraint, a default, a nullable column, an enum, a check, a
// multi-column index and a partial covering index. A scenario that exercised
// only columns would prove that columns work.
const blogEntities = `package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

//orm:enum public.post_status (draft, published, archived)
type PostStatus string

const (
	PostDraft     PostStatus = "draft"
	PostPublished PostStatus = "published"
	PostArchived  PostStatus = "archived"
)

//orm:table users
//orm:check users_email_not_blank "email <> ''"
//orm:index users_active_created_idx (Active, CreatedAt desc)
type User struct {
	ID        int64  ` + "`orm:\"pk,identity\"`" + `
	Email     string ` + "`orm:\"unique\"`" + `
	Name      string
	Nickname  *string
	Active    bool      ` + "`orm:\"default:true\"`" + `
	CreatedAt time.Time ` + "`orm:\"default:now()\"`" + `

	Profile  orm.One[Profile]
	Posts    orm.Many[Post]
	Comments orm.Many[Comment]
}

//orm:table profiles
type Profile struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	UserID int64 ` + "`orm:\"unique\"`" + `
	Bio    *string

	User orm.One[User] ` + "`orm:\"side:local\"`" + `
}

//orm:table posts
//orm:index posts_feed_idx (AuthorID, CreatedAt desc) include (Title) where "status = 'published'"
//orm:check posts_title_not_blank "title <> ''"
type Post struct {
	ID        int64 ` + "`orm:\"pk,identity\"`" + `
	AuthorID  int64
	Title     string
	Body      string
	Status    PostStatus ` + "`orm:\"default:'draft'\"`" + `
	CreatedAt time.Time  ` + "`orm:\"default:now()\"`" + `

	Author   orm.One[User] ` + "`orm:\"side:local\"`" + `
	Comments orm.Many[Comment]
}

//orm:table comments
//orm:unique comments_post_author_key (PostID, AuthorID)
type Comment struct {
	ID        int64 ` + "`orm:\"pk,identity\"`" + `
	PostID    int64
	AuthorID  int64
	Body      string
	CreatedAt time.Time ` + "`orm:\"default:now()\"`" + `

	Post   orm.One[Post] ` + "`orm:\"side:local\"`" + `
	Author orm.One[User] ` + "`orm:\"side:local\"`" + `
}
`
