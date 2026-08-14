package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Scenario C — a relation and the index that makes it useful.
//
// A foreign key with an index over (fk, time DESC), partial and covering, is
// the shape almost every feed query wants and the shape a portable schema
// abstraction cannot express. So it is the one worth proving survives the whole
// round trip: declaration, migration, SQL, PostgreSQL, and back.
func TestScenarioC_relationIndex(t *testing.T) {
	p := newProject(t, feedEntities)

	p.MustRun("makemigrations", "--name", "initial")
	sql := p.MustRun("sqlmigrate", "0001_initial")
	contains(t, sql, `"author_id" int8 NOT NULL`, "the SQL")
	contains(t, sql, `FOREIGN KEY ("author_id") REFERENCES "public"."users"("id")`, "the SQL")
	contains(t, sql, `CREATE INDEX "posts_feed_idx" ON "public"."posts" ("author_id", "created_at" DESC)`, "the SQL")
	contains(t, sql, `INCLUDE ("title")`, "the SQL")
	contains(t, sql, `WHERE published = true`, "the SQL")

	p.MustRun("migrate")

	// Semantic equality: what PostgreSQL has is what the migration said, read
	// back through the catalog rather than through what we hoped we wrote.
	contains(t, p.MustRun("check"), "Database     fully migrated, no drift", "the check")

	def := p.Query(`SELECT indexdef FROM pg_indexes WHERE indexname = 'posts_feed_idx'`)
	if len(def) != 1 {
		t.Fatalf("posts_feed_idx = %v", def)
	}
	for _, want := range []string{"author_id, created_at DESC", "INCLUDE (title)", "WHERE (published"} {
		contains(t, def[0], want, "the index PostgreSQL built")
	}
}

// Scenario D — CONCURRENTLY, and the consequences of it.
//
// An index built concurrently cannot run inside a transaction, and that one
// fact decides how the whole migration executes. The chain has to hold end to
// end: the declaration says concurrently, the artifact says non-atomic, the
// plan says so, the SQL says so, and the executor does not open a transaction.
func TestScenarioD_concurrently(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	p.Entities(`package domain

//orm:table users
//orm:index users_email_lookup_idx (Email) concurrently
type User struct {
	ID    int64  ` + "`orm:\"pk,identity\"`" + `
	Email string ` + "`orm:\"unique\"`" + `
}
`)
	out := p.MustRun("makemigrations", "--name", "feed_index")
	contains(t, out, "Migration 0002_feed_index [non-atomic]", "the summary")
	contains(t, out, migrate.WNonAtomic, "the warnings")

	plan := p.MustRun("migrate", "--plan")
	contains(t, plan, "0002_feed_index [non-atomic]", "the plan")
	contains(t, plan, "runs outside a transaction", "the plan warnings")

	sql := p.MustRun("sqlmigrate", "0002_feed_index")
	contains(t, sql, "CREATE INDEX CONCURRENTLY", "the SQL")
	contains(t, sql, "runs outside a transaction", "the SQL header")

	// It runs, and only then is it recorded.
	contains(t, p.MustRun("migrate"), "Applying 0002_feed_index ... OK", "the run")
	contains(t, p.MustRun("showmigrations"), "[X] 0002_feed_index  (non-atomic)", "the listing")
	if got := p.Query(`SELECT indexdef FROM pg_indexes WHERE indexname = 'users_email_lookup_idx'`); len(got) != 1 {
		t.Fatalf("the index was not built: %v", got)
	}
}

// Scenario E — what a failure leaves behind, told honestly.
//
// The two modes fail differently and the difference matters more than either
// mode does: an atomic migration leaves nothing, a non-atomic one leaves
// everything before the failure. A tool that reported both the same way would
// send somebody looking for a clean database that is not there.
func TestScenarioE_atomicFailureRollsBack(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	writeMigration(t, p, &migrate.Migration{
		ID: "0002_broken", DependsOn: []string{"0001_initial"}, Atomic: true,
		Operations: []migrate.Operation{
			migrate.RawSQL{Up: "CREATE TABLE landed (id int)", Down: "DROP TABLE landed", Atomic: true, Description: "first"},
			migrate.RawSQL{Up: "THIS IS NOT SQL", Atomic: true, Description: "second"},
		},
	})

	code, out, stderr := p.Run("migrate")
	if code != exitFailure {
		t.Fatalf("a failing migration exited %d", code)
	}
	contains(t, out, "Applying 0002_broken ... FAILED", "the run")
	contains(t, stderr, "the transaction was rolled back and the migration is not recorded", "the error")
	contains(t, stderr, "syntax error", "the error keeps PostgreSQL's own diagnostic")

	if tables := p.Query(`SELECT to_regclass('public.landed')::text`); tables[0] != "<null>" {
		t.Error("the first operation survived a rolled-back atomic migration")
	}
	contains(t, p.MustRun("showmigrations"), "[ ] 0002_broken", "the listing")
}

func TestScenarioE_nonAtomicFailureIsReportedHonestly(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	writeMigration(t, p, &migrate.Migration{
		ID: "0002_broken", DependsOn: []string{"0001_initial"}, Atomic: false,
		Operations: []migrate.Operation{
			migrate.RawSQL{Up: "CREATE TABLE landed (id int)", Down: "DROP TABLE landed", Description: "first"},
			migrate.RawSQL{Up: "THIS IS NOT SQL", Description: "second"},
		},
	})

	code, out, stderr := p.Run("migrate")
	if code != exitFailure {
		t.Fatalf("a failing migration exited %d", code)
	}
	contains(t, out, "Applying 0002_broken ... FAILED", "the run")
	contains(t, stderr, "the 1 operation(s) before it are still applied", "the error")
	contains(t, stderr, "the database is in a state no migration describes", "the error")

	if tables := p.Query(`SELECT to_regclass('public.landed')::text`); tables[0] == "<null>" {
		t.Error("the first operation was rolled back, which a non-atomic migration cannot do")
	}
	contains(t, p.MustRun("showmigrations"), "[ ] 0002_broken", "the listing")
}

// Scenario F — adopting a database that already exists.
//
// The property under test is that adoption changes nothing. A baseline is a
// description of what is already there, so the migration is written, recorded,
// and never run — and the schema afterwards has to be byte-for-byte the schema
// before, or the adoption would have been a migration in disguise.
func TestScenarioF_baseline(t *testing.T) {
	p := newProject(t, adoptedEntities)
	p.SQL(adoptedDDL)

	// It starts as a database-first project, which is the whole point: this is
	// how one stops being one.
	p.Config("database")
	before := p.MustRun("inspect")

	out := p.MustRun("makemigrations", "--baseline")
	contains(t, out, "Migration 0001_initial", "the baseline")
	contains(t, out, "Set schema.mode: managed", "the baseline")
	contains(t, out, "orm migrate --fake 0001_initial", "the baseline")

	// Now the decision about who owns the schema has been made.
	p.Config("managed")

	// The baseline describes the database, so there is nothing left to migrate.
	contains(t, p.MustRun("makemigrations"), "No schema changes detected.", "after the baseline")

	faked := p.MustRun("migrate", "--fake", "0001_initial")
	contains(t, faked, "Faking 0001_initial ... recorded, not run", "the fake")
	contains(t, faked, "No schema SQL ran.", "the fake")

	if after := p.MustRun("inspect"); after != before {
		t.Errorf("adoption changed the schema:\n--- before\n%s\n--- after\n%s", before, after)
	}
	contains(t, p.MustRun("showmigrations"), "[X] 0001_initial", "the listing")
	contains(t, p.MustRun("migrate"), "The database is up to date.", "a migrate after adoption")

	// From here it is an ordinary managed project: one model change, one
	// migration, one increment.
	p.Entities(adoptedEntities + `
//orm:table notes
type Note struct {
	ID   int64  ` + "`orm:\"pk,identity\"`" + `
	Body string
}
`)
	out = p.MustRun("makemigrations", "--name", "notes")
	contains(t, out, "Migration 0002_notes", "the increment")
	absent(t, out, "users", "the increment")
	p.MustRun("migrate")
	contains(t, p.MustRun("check"), "Database     fully migrated, no drift", "the check")
}

// Faking is refused when the claim it would record is not true.
func TestMigrate_fakeVerifies(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")

	code, _, stderr := p.Run("migrate", "--fake", "0001_initial")
	if code != exitFailure {
		t.Fatalf("faking an unbuilt schema exited %d", code)
	}
	contains(t, stderr, "the database is not in the state 0001_initial describes", "the refusal")
	contains(t, stderr, "pass --force", "the refusal")
	contains(t, p.MustRun("showmigrations"), "[ ] 0001_initial", "the listing")
}

// Scenario G — somebody changed the database.
//
// The distinction this proves is the one the whole three-state design exists
// for: a database that lost a column is drift, not a missing migration, and the
// fix for the two is opposite. Reporting drift as a missing migration would
// have somebody write a migration that drops the column on every other
// database too.
func TestScenarioG_drift(t *testing.T) {
	p := newProject(t, blogEntities)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")
	contains(t, p.MustRun("check"), "Database     fully migrated, no drift", "the clean check")

	// The mandatory case: somebody ran ALTER TABLE by hand.
	p.SQL(`ALTER TABLE users DROP COLUMN created_at`)

	code, out, _ := p.Run("check")
	if code != exitFindings {
		t.Errorf("check exit = %d, want %d with drift", code, exitFindings)
	}
	contains(t, out, "Database     drift:", "the check")
	contains(t, out, "Database drift detected.", "the check")
	contains(t, out, "column created_at is only in the first schema", "the check")
	contains(t, out, "Models       match the latest migration", "the check")
	absent(t, out, "Models have schema changes not represented by migrations", "the check")

	// Nothing was rewritten to match: makemigrations still sees no model change,
	// because the models and the migrations still agree with each other.
	contains(t, p.MustRun("makemigrations"), "No schema changes detected.", "makemigrations under drift")

	// Generation refuses rather than baking the drifted schema into the runtime.
	code, _, stderr := p.Run("generate")
	if code != exitFindings {
		t.Errorf("generate exit = %d, want %d under drift", code, exitFindings)
	}
	contains(t, stderr, "differs from what its applied migrations describe", "the refusal")

	// Repairing the database clears it, and nothing had to be faked.
	// Repairing by hand means repairing everything the change took with it —
	// dropping the column dropped the index over it too.
	p.SQL(`ALTER TABLE users ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
	       CREATE INDEX users_active_created_idx ON users (active, created_at DESC)`)
	contains(t, p.MustRun("check"), "Database     fully migrated, no drift", "the check after repair")
}

// Database mode is untouched: a project with no migrations at all must not
// start failing because the migration engine exists.
func TestDatabaseMode_isUnaffected(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	p.Config("database")
	out := p.MustRun("check")
	contains(t, out, "reconciliation clean", "the database-mode check")
	absent(t, out, "Migrations", "the database-mode check")
	absent(t, out, "Pending", "the database-mode check")

	for _, args := range [][]string{
		{"makemigrations"}, {"migrate"}, {"showmigrations"}, {"sqlmigrate", "0001_initial"},
	} {
		code, _, stderr := p.Run(args...)
		if code != exitFailure {
			t.Errorf("orm %s in database mode exited %d, want %d", strings.Join(args, " "), code, exitFailure)
		}
		contains(t, stderr, "needs schema.mode: managed", "the refusal")
	}

	// inspect is read-only and belongs to both modes.
	contains(t, p.MustRun("inspect"), "table public.users", "inspect in database mode")
}

// The one thing history must never do is change under an applied migration.
func TestShowmigrations_reportsAnEditedMigration(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")

	// Edit the artifact after it was applied, the way a well-meaning fix does.
	path := filepath.Join(p.Dir, "migrations", "0001_initial.json")
	edited := strings.Replace(readFile(t, path), `"Name": "email"`, `"Name": "email_address"`, 1)
	writeFile(t, path, edited)

	code, out, stderr := p.Run("showmigrations")
	if code != exitFindings {
		t.Errorf("showmigrations exit = %d, want %d", code, exitFindings)
	}
	contains(t, out, "[!] 0001_initial", "the listing")
	contains(t, stderr, "was modified after it was applied", "the diagnosis")
	contains(t, stderr, "history cannot be rewritten", "the diagnosis")

	code, _, stderr = p.Run("migrate")
	if code != exitFailure {
		t.Errorf("migrate exit = %d over an edited history, want %d", code, exitFailure)
	}
	contains(t, stderr, "was modified after it was applied", "the refusal")
}

// A migration is data, and reading it back has to produce the same migration.
func TestMigrationArtifact_roundTrips(t *testing.T) {
	p := newProject(t, blogEntities)
	p.MustRun("makemigrations", "--name", "initial")

	set, err := migrate.NewStore(filepath.Join(p.Dir, "migrations")).Load()
	if err != nil {
		t.Fatalf("loading the migrations: %v", err)
	}
	state, err := set.State()
	if err != nil {
		t.Fatalf("reconstructing the state: %v", err)
	}
	// The state the artifact rebuilds is the schema the models asked for, which
	// is the property that makes a migration file a record of anything.
	if len(state.Tables) != 4 || len(state.Enums) != 1 {
		t.Fatalf("the reconstructed state has %d tables and %d enums", len(state.Tables), len(state.Enums))
	}
	posts, ok := state.Table("public", "posts")
	if !ok {
		t.Fatal("no posts table in the reconstructed state")
	}
	var idx schema.Index
	for _, i := range posts.Indexes {
		if i.Name == "posts_feed_idx" {
			idx = i
		}
	}
	if len(idx.Include) != 1 || idx.Where.Empty() || idx.Columns[1].Direction != schema.Desc {
		t.Errorf("the index did not survive the file: %+v", idx)
	}
}

func writeMigration(t *testing.T, p *project, m *migrate.Migration) {
	t.Helper()
	if _, err := migrate.NewStore(filepath.Join(p.Dir, "migrations")).Write(m); err != nil {
		t.Fatalf("writing %s: %v", m.ID, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}

// feedEntities is scenario C: one relation, and the index that serves it.
const feedEntities = `package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

//orm:table users
type User struct {
	ID    int64  ` + "`orm:\"pk,identity\"`" + `
	Email string ` + "`orm:\"unique\"`" + `

	Posts orm.Many[Post]
}

//orm:table posts
//orm:index posts_feed_idx (AuthorID, CreatedAt desc) include (Title) where "published = true"
type Post struct {
	ID        int64 ` + "`orm:\"pk,identity\"`" + `
	AuthorID  int64
	Title     string
	Published bool      ` + "`orm:\"default:false\"`" + `
	CreatedAt time.Time ` + "`orm:\"default:now()\"`" + `

	Author orm.One[User] ` + "`orm:\"side:local\"`" + `
}
`

// adoptedEntities describes a schema somebody else created.
const adoptedEntities = `package domain

import "time"

//orm:enum public.account_state (active, suspended)
type AccountState string

const (
	AccountActive    AccountState = "active"
	AccountSuspended AccountState = "suspended"
)

//orm:table users
//orm:check users_email_not_blank "email <> ''"
type User struct {
	ID        int64  ` + "`orm:\"pk,identity\"`" + `
	Email     string ` + "`orm:\"unique\"`" + `
	State     AccountState ` + "`orm:\"default:'active'\"`" + `
	CreatedAt time.Time    ` + "`orm:\"default:now()\"`" + `
}
`

// adoptedDDL is the schema as somebody else's tool built it.
const adoptedDDL = `
CREATE TYPE public.account_state AS ENUM ('active', 'suspended');

CREATE TABLE public.users (
    id         bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
    email      text NOT NULL,
    state      public.account_state NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT users_email_key UNIQUE (email),
    CONSTRAINT users_email_not_blank CHECK (email <> '')
);
`
