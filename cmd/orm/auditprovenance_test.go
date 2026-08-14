package main

import (
	"strings"
	"testing"
)

// M16.5 adversarial audit: provenance that no longer describes reality.
//
// Provenance is the row saying what this project applied to this database. Every
// safety decision about a stored relation rests on it: whether the thing in the
// catalog is the thing the migrations created, and therefore whether a plan may
// touch it. So the attacks are on the gap between the row and the world.
//
// The invariant throughout is conservative refusal. When identity cannot be
// proven the planner must decline rather than infer — because the inference it
// would have to make is "this is mine, I may drop and recreate it", and a
// materialized view holds rows that were computed rather than written. Being
// wrong about ownership is not a failed migration, it is deleted data.
//
// Nothing here asserts on error text alone. Each case checks what happened to the
// database as well, because a refusal that had already written an artifact or
// changed a row would be a refusal in name only.

// auditProvenanceProject is a managed project with one materialized view whose
// provenance this project recorded.
func auditProvenanceProject(t *testing.T) *project {
	t.Helper()
	p := newProject(t, auditMatView("SELECT id AS user_id, email FROM users WHERE active"))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	if got := p.Query(`SELECT count(*)::text FROM public.orm_schema_views
	                    WHERE schema_name = 'public' AND relation_name = 'totals'`); got[0] != "1" {
		t.Fatalf("the setup recorded %v provenance rows, want one", got)
	}
	return p
}

func auditMatView(definition string) string {
	return `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`" + definition + "`" + `
//orm:depends-on public.users
//orm:index totals_user_id_key (UserID) unique
type Total struct {
	UserID int64
	Email  string
}
`
}

// snapshotWorld is everything a refusal must leave alone.
type snapshotWorld struct {
	relations  string
	provenance string
	migrations int
}

func worldOf(t *testing.T, p *project) snapshotWorld {
	t.Helper()
	return snapshotWorld{
		relations: strings.Join(p.Query(`SELECT n.nspname||'.'||c.relname||' '||c.relkind::text
		                                   FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		                                  WHERE n.nspname='public' AND c.relkind IN ('r','v','m')
		                                  ORDER BY 1`), "\n"),
		provenance: strings.Join(p.Query(`SELECT schema_name||'.'||relation_name||' '||kind||' '||source_identity
		                                    FROM public.orm_schema_views ORDER BY 1`), "\n"),
		migrations: len(p.Migrations()),
	}
}

func requireUnchanged(t *testing.T, what string, before, after snapshotWorld) {
	t.Helper()
	if before.relations != after.relations {
		t.Errorf("%s changed the relations:\n before %s\n after  %s", what, before.relations, after.relations)
	}
	if before.provenance != after.provenance {
		t.Errorf("%s changed the provenance:\n before %s\n after  %s", what, before.provenance, after.provenance)
	}
	if before.migrations != after.migrations {
		t.Errorf("%s wrote %d migration file(s)", what, after.migrations-before.migrations)
	}
}

// §5: provenance exists and the world has moved on underneath it.
func TestAuditProvenance_staleAgainstReality(t *testing.T) {
	for _, c := range []struct {
		what   string
		damage string
		// expectCheckDirty says the check must report something.
		expectCheckDirty bool
	}{
		{
			"the relation was deleted by hand",
			`DROP MATERIALIZED VIEW public.totals`,
			true,
		},
		{
			"the relation was recreated by hand with a different body",
			`DROP MATERIALIZED VIEW public.totals;
			 CREATE MATERIALIZED VIEW public.totals AS SELECT id AS user_id, email FROM users`,
			true,
		},
		{
			"the relation was replaced by hand with an ordinary view",
			`DROP MATERIALIZED VIEW public.totals;
			 CREATE VIEW public.totals AS SELECT id AS user_id, email FROM users WHERE active`,
			true,
		},
		{
			"the relation was replaced by hand with a table of the same name",
			`DROP MATERIALIZED VIEW public.totals;
			 CREATE TABLE public.totals (user_id bigint, email text)`,
			true,
		},
		{
			"its indexes were changed by hand",
			`DROP INDEX public.totals_user_id_key;
			 CREATE INDEX totals_user_id_key ON public.totals (user_id)`,
			true,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := auditProvenanceProject(t)
			p.SQL(c.damage)
			before := worldOf(t, p)

			code, stdout, stderr := p.Run("check")
			out := stdout + stderr
			if c.expectCheckDirty && code == exitClean {
				t.Errorf("check is clean after %s:\n%s", c.what, out)
			}
			// check is read-only: it may not repair, and may not touch the row
			// it is reading.
			requireUnchanged(t, "check after "+c.what, before, worldOf(t, p))

			// And planning must not decide on its own to drop and recreate
			// something whose identity it cannot prove.
			_, mo, me := p.Run("makemigrations", "--name", "repair")
			plan := mo + me
			if strings.Contains(strings.ToLower(plan), "drop materialized view") &&
				strings.Contains(strings.ToLower(plan), "create materialized view") {
				t.Errorf("planning after %s produced a drop and a recreate, which discards "+
					"every stored row:\n%s", c.what, plan)
			}
			requireUnchanged(t, "makemigrations after "+c.what, before, worldOf(t, p))
		})
	}
}

// §6: the provenance row itself is wrong.
//
// A row that does not describe what was applied is worse than no row, because it
// is believed. Each case corrupts one field and requires the tool not to act on
// it destructively.
func TestAuditProvenance_corruptedRowIsNotActedOn(t *testing.T) {
	for _, c := range []struct{ what, damage string }{
		{
			"the recorded source identity is another declaration's",
			`UPDATE public.orm_schema_views SET source_identity = 'v1:deadbeefdeadbeefdeadbeefdeadbeef'
			  WHERE relation_name = 'totals'`,
		},
		{
			"the recorded canonical text is not what this server would say",
			`UPDATE public.orm_schema_views SET canonical = 'SELECT 1'
			  WHERE relation_name = 'totals'`,
		},
		{
			"the recorded kind is wrong",
			`UPDATE public.orm_schema_views SET kind = 'view' WHERE relation_name = 'totals'`,
		},
		{
			"the row names a relation that does not exist",
			`UPDATE public.orm_schema_views SET relation_name = 'totals_ghost'
			  WHERE relation_name = 'totals'`,
		},
		{
			"the row names another schema",
			`UPDATE public.orm_schema_views SET schema_name = 'pg_temp' WHERE relation_name = 'totals'`,
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := auditProvenanceProject(t)
			p.SQL(c.damage)
			before := worldOf(t, p)

			// Whatever it decides, it may not destroy the relation.
			p.Run("check")
			requireUnchanged(t, "check with "+c.what, before, worldOf(t, p))

			_, mo, me := p.Run("makemigrations", "--name", "after-corruption")
			plan := mo + me
			if strings.Contains(strings.ToLower(plan), "drop materialized view") {
				t.Errorf("planning with %s produced a drop:\n%s", c.what, plan)
			}
			requireUnchanged(t, "makemigrations with "+c.what, before, worldOf(t, p))

			// The relation is still there, holding its rows.
			if got := p.Query(`SELECT count(*)::text FROM pg_class
			                    WHERE relname = 'totals' AND relkind = 'm'`); got[0] != "1" {
				t.Errorf("the materialized view is gone after %s", c.what)
			}
		})
	}
}

// §7: a relation with the same name that this project did not create.
//
// The adversary is ownership. Somebody drops the managed relation and creates an
// unrelated one with the same schema and name — a different query, holding
// somebody else's rows. The provenance row still says this project applied
// something here. Nothing may treat the name as proof of ownership.
func TestAuditProvenance_sameNameIsNotOwnership(t *testing.T) {
	p := auditProvenanceProject(t)

	// The replacement is a different query over a different base, so nothing
	// about it matches what was recorded except its name.
	p.SQL(`DROP MATERIALIZED VIEW public.totals;
	       CREATE TABLE public.other_source (user_id bigint, email text);
	       INSERT INTO public.other_source VALUES (99, 'someone@else.example');
	       CREATE MATERIALIZED VIEW public.totals AS SELECT user_id, email FROM public.other_source`)
	before := worldOf(t, p)
	rowsBefore := p.Query(`SELECT count(*)::text FROM public.totals`)

	code, stdout, stderr := p.Run("check")
	out := stdout + stderr
	if code == exitClean {
		t.Errorf("check is clean about a relation this project did not create:\n%s", out)
	}
	requireUnchanged(t, "check against a foreign relation", before, worldOf(t, p))

	_, mo, me := p.Run("makemigrations", "--name", "adopt")
	plan := mo + me
	if strings.Contains(strings.ToLower(plan), "drop materialized view") {
		t.Errorf("planning offered to drop a relation this project did not create, which "+
			"deletes somebody else's rows:\n%s", plan)
	}
	requireUnchanged(t, "makemigrations against a foreign relation", before, worldOf(t, p))

	// The rows are still there.
	if got := p.Query(`SELECT count(*)::text FROM public.totals`); got[0] != rowsBefore[0] {
		t.Errorf("the foreign relation's rows changed: %v -> %v", rowsBefore, got)
	}
	if got := p.Query(`SELECT email FROM public.totals`); len(got) != 1 || got[0] != "someone@else.example" {
		t.Errorf("the foreign relation's contents changed: %v", got)
	}
}

// The same question for an ordinary view, since G1 and G2 share the record.
func TestAuditProvenance_sameNameIsNotOwnershipForAView(t *testing.T) {
	src := `package domain

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
	p := newProject(t, src)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")

	p.SQL(`DROP VIEW public.active_users;
	       CREATE VIEW public.active_users AS SELECT id, email FROM users WHERE NOT active`)
	before := worldOf(t, p)

	if code, so, se := p.Run("check"); code == exitClean {
		t.Errorf("check is clean about a view whose body somebody replaced:\n%s", so+se)
	}
	requireUnchanged(t, "check against a replaced view", before, worldOf(t, p))
}
