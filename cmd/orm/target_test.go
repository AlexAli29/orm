package main

import (
	"path/filepath"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Moving to a target, in both directions.
//
// A target behind the current state is a rollback, and the reverse of every
// step is computed before any of them runs — discovering halfway through a
// rollback that the next step cannot be undone would leave the database in a
// state nobody planned and no migration describes.

func TestMigrate_targetForwardAndBack(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")

	p.Entities(usersEntities("Email string `orm:\"unique\"`\n\tNickname *string"))
	p.MustRun("makemigrations", "--name", "nickname")

	// A target that is a unique prefix stops where it says.
	contains(t, p.MustRun("migrate", "0001"), "Applying 0001_initial ... OK", "the run")
	listing := p.MustRun("showmigrations")
	contains(t, listing, "[X] 0001_initial", "the listing")
	contains(t, listing, "[ ] 0002_nickname", "the listing")

	p.MustRun("migrate")
	if cols := p.Query(`SELECT column_name::text FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'nickname'`); len(cols) != 1 {
		t.Fatal("0002 did not add the column")
	}

	// Back again. The reverse of AddColumn is DropColumn, which the plan says
	// before it runs.
	plan := p.MustRun("migrate", "0001", "--plan")
	contains(t, plan, "1 migration(s) to run reverse", "the reverse plan")
	contains(t, plan, "- drop column nickname", "the reverse plan")
	contains(t, plan, migrate.WDropColumn, "the reverse plan warnings")

	contains(t, p.MustRun("migrate", "0001"), "Reversing 0002_nickname ... OK", "the rollback")
	if cols := p.Query(`SELECT column_name::text FROM information_schema.columns WHERE table_name = 'users' AND column_name = 'nickname'`); len(cols) != 0 {
		t.Error("the rollback left the column behind")
	}
	contains(t, p.MustRun("showmigrations"), "[ ] 0002_nickname", "the listing after the rollback")
}

// A rollback that would hit an operation with no inverse fails before it
// changes anything, rather than part way through.
func TestMigrate_refusesAnIrreversibleRollback(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")

	// Raw SQL with no reverse statement is the honest irreversible case: the
	// engine cannot read the SQL, so it cannot invent the undo.
	if _, err := migrate.NewStore(filepath.Join(p.Dir, "migrations")).Write(&migrate.Migration{
		ID: "0002_backfill", DependsOn: []string{"0001_initial"}, Atomic: true,
		Operations: []migrate.Operation{
			migrate.RawSQL{Up: "UPDATE users SET email = lower(email)", Atomic: true, Description: "normalise emails"},
			migrate.StateOnly{Op: migrate.AddColumn{Schema: "public", Table: "users", Column: schema.Column{
				Name: "normalised", Type: schema.Type{Name: "bool"}, Nullable: true,
			}}},
		},
	}); err != nil {
		t.Fatalf("writing the migration: %v", err)
	}

	p.MustRun("migrate")
	before := p.Query(`SELECT count(*)::text FROM public.orm_schema_migrations`)

	code, _, stderr := p.Run("migrate", "0001")
	if code != exitFailure {
		t.Fatalf("an irreversible rollback exited %d", code)
	}
	contains(t, stderr, "cannot be reversed", "the refusal")
	if after := p.Query(`SELECT count(*)::text FROM public.orm_schema_migrations`); after[0] != before[0] {
		t.Errorf("the refused rollback changed the history: %v -> %v", before, after)
	}
}

// sqlmigrate --reverse prints what a rollback would run, without a database.
func TestSqlmigrate_reverse(t *testing.T) {
	p := newProject(t, usersEntities("Email string `orm:\"unique\"`"))
	p.MustRun("makemigrations", "--name", "initial")
	p.Entities(usersEntities("Email string `orm:\"unique\"`\n\tNickname *string"))
	p.MustRun("makemigrations", "--name", "nickname")

	forward := p.MustRun("sqlmigrate", "0002_nickname")
	contains(t, forward, `ADD COLUMN "nickname" text`, "the forward SQL")

	back := p.MustRun("sqlmigrate", "0002_nickname", "--reverse")
	contains(t, back, "-- 0002_nickname (reverse)", "the reverse header")
	contains(t, back, `DROP COLUMN "nickname"`, "the reverse SQL")
}
