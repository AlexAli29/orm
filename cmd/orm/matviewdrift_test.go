package main

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 G2 Proof A: manual index drift on a materialized view.
//
// Somebody drops or redefines an index by hand. What must happen is that the
// check says so precisely — the index, not the definition, not the kind, not the
// population — and that whatever the planner then does matches the policy that
// already governs table indexes rather than a special one invented here.

func driftEntities(indexes string) string {
	return `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`SELECT id AS user_id, email FROM users WHERE active`" + `
//orm:depends-on public.users
` + indexes + `type Total struct {
	UserID int64
	Email  string
}
`
}

const driftIndexes = "//orm:index totals_user_id_key (UserID) unique\n" +
	"//orm:index totals_email_idx (Email) where \"email <> ''\"\n"

func driftProject(t *testing.T) *project {
	t.Helper()
	p := newProject(t, driftEntities(driftIndexes))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the project did not start clean:\n%s", out)
	}
	return p
}

func matviewOID(t *testing.T, p *project) uint32 {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var oid uint32
	if err := conn.QueryRow(t.Context(),
		`SELECT 'public.totals'::regclass::oid`).Scan(&oid); err != nil {
		t.Fatalf("reading the OID: %v", err)
	}
	return oid
}

func driftExec(t *testing.T, p *project, sql string) {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatalf("running %q: %v", sql, err)
	}
}

// An index dropped by hand is reported as a missing index, and as nothing else.
func TestMatViewDrift_missingIndex(t *testing.T) {
	p := driftProject(t)
	driftExec(t, p, `DROP INDEX totals_email_idx`)

	code, stdout, stderr := p.Run("check")
	out := stdout + stderr
	if code == exitClean {
		t.Fatalf("an index dropped by hand left the check clean:\n%s", out)
	}
	if !strings.Contains(out, "totals_email_idx") {
		t.Errorf("the finding does not name the index:\n%s", out)
	}
	// Nothing else about the relation changed, so nothing else may be claimed.
	for _, wrong := range []string{"definition", "not the one this project applied",
		"is a table", "materialized view is a", "provenance"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(wrong)) {
			t.Errorf("a missing index was also reported as %q:\n%s", wrong, out)
		}
	}
}

// An index redefined by hand is reported through the ordinary index differ.
func TestMatViewDrift_changedIndex(t *testing.T) {
	for _, c := range []struct{ what, sql, want string }{
		{
			"a changed predicate",
			`DROP INDEX totals_email_idx;
			 CREATE INDEX totals_email_idx ON totals (email) WHERE email = ''`,
			"totals_email_idx",
		},
		{
			// The predicate is kept. An earlier version of this case dropped it
			// while changing the method, so it moved two axes at once — and a
			// differ blind to the method still reported the predicate, which
			// means the case passed with method comparison entirely removed.
			// That is the same two-axis defect the unit-level cases were split
			// up to prevent, left behind here because this fixture is written in
			// SQL rather than built from a baseline.
			"a changed method",
			`DROP INDEX totals_email_idx;
			 CREATE INDEX totals_email_idx ON totals USING hash (email) WHERE email <> ''`,
			"totals_email_idx",
		},
		{
			// INCLUDE is part of an index's identity: a covering column changes
			// which queries the index can answer without a heap fetch, and an
			// index differ blind to it would call two different indexes equal.
			"a changed INCLUDE",
			`DROP INDEX totals_email_idx;
			 CREATE INDEX totals_email_idx ON totals (email) INCLUDE (user_id) WHERE email <> ''`,
			"totals_email_idx",
		},
		{
			"a lost uniqueness",
			`DROP INDEX totals_user_id_key;
			 CREATE INDEX totals_user_id_key ON totals (user_id)`,
			"totals_user_id_key",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := driftProject(t)
			driftExec(t, p, c.sql)

			code, stdout, stderr := p.Run("check")
			out := stdout + stderr
			if code == exitClean {
				t.Fatalf("%s left the check clean:\n%s", c.what, out)
			}
			if !strings.Contains(out, c.want) {
				t.Errorf("the finding does not name %s:\n%s", c.want, out)
			}
			if strings.Contains(out, "not the one this project applied") {
				t.Errorf("%s was reported as definition drift; the body did not change:\n%s",
					c.what, out)
			}
		})
	}
}

// What the planner does about manual index drift, recorded as the policy it
// actually is rather than the one a test might wish for.
//
// makemigrations plans nothing. That is not an index rule — it is the migration
// engine's founding one: a migration is computed against the state the
// migrations themselves describe, never against a live database. The state
// still contains the index, because migration 0001 created it, so there is
// nothing to plan. Diffing against the database instead would make the
// migration you get depend on which database you happened to point at, and
// would make a rename indistinguishable from a drop and an add.
//
// So manual index drift is detected and not repaired, and the two commands say
// different true things: check reports E036 against the database, and
// makemigrations reports that the declarations and the migrations agree.
func TestMatViewDrift_plannerLeavesManualDriftToCheck(t *testing.T) {
	p := driftProject(t)
	before := matviewOID(t, p)
	driftExec(t, p, `DROP INDEX totals_email_idx`)

	// Nothing to plan: the declarations and the migration state still agree.
	out := p.MustRun("makemigrations", "--name", "repair")
	if !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the planner wrote a migration from a difference it reads against the "+
			"database rather than against the migration state:\n%s", out)
	}

	// And the drift is still reported, so it is not lost — only not repaired.
	code, stdout, stderr := p.Run("check")
	if code == exitClean {
		t.Errorf("the drift stopped being reported:\n%s", stdout+stderr)
	}
	if !strings.Contains(stdout+stderr, "totals_email_idx") {
		t.Errorf("the check no longer names the missing index:\n%s", stdout+stderr)
	}

	// Nothing was touched: no migration file, no relation, no provenance.
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var indexes, prov int
	if err := conn.QueryRow(t.Context(),
		`SELECT (SELECT count(*) FROM pg_indexes WHERE tablename = 'totals'),
		        (SELECT count(*) FROM public.orm_schema_views WHERE relation_name = 'totals')`).
		Scan(&indexes, &prov); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Errorf("the index count changed: %d remain", indexes)
	}
	if prov != 1 {
		t.Errorf("the provenance was disturbed")
	}
	if after := matviewOID(t, p); after != before {
		t.Errorf("the relation was recreated: OID %d -> %d", before, after)
	}

	// The route back is to undo the drift, not to declare around it.
	//
	// Removing the index from the declaration produces a migration that drops
	// it — and PostgreSQL refuses, because it is already gone. That is the
	// frozen contract being consistent rather than failing: the migration
	// describes a transition from the state the migrations built, and somebody
	// has moved the database out from under it. Writing DROP INDEX IF EXISTS to
	// paper over that would hide every real drift behind a statement that
	// always succeeds.
	p.Entities(driftEntities("//orm:index totals_user_id_key (UserID) unique\n"))
	p.MustRun("makemigrations", "--name", "drop-index")
	code, stdout, stderr = p.Run("migrate")
	if code == exitClean {
		t.Fatalf("a migration dropping an index that is already gone succeeded, which means "+
			"something is written to tolerate a database it did not build:\n%s", stdout+stderr)
	}
	if !strings.Contains(stdout+stderr, "42704") {
		t.Errorf("PostgreSQL's own error did not reach the caller:\n%s", stdout+stderr)
	}

	// Restoring what was removed by hand is what lets it proceed.
	driftExec(t, p, `CREATE INDEX totals_email_idx ON totals (email) WHERE email <> ''`)
	p.MustRun("migrate")
	p.MustRun("check")
	if again := p.MustRun("makemigrations"); !strings.Contains(again, "No schema changes detected") {
		t.Errorf("after restoring the index the project did not converge:\n%s", again)
	}
	if after := matviewOID(t, p); after != before {
		t.Errorf("recovery recreated the relation: OID %d -> %d", before, after)
	}

}

// NULLS NOT DISTINCT is not silently dropped.
//
// It is a capability boundary rather than a grammar gap. schema.Unique can
// represent it, because a UNIQUE constraint on a table can carry it;
// schema.Index cannot. A materialized view owns indexes and cannot own table
// constraints, so there is nowhere for it to live — and an option accepted and
// ignored would produce an index PostgreSQL treats differently from the one the
// declaration describes, silently.
func TestMatViewIndex_nullsNotDistinctIsRefused(t *testing.T) {
	for _, decl := range []string{
		"//orm:index totals_key (UserID) unique nulls not distinct\n",
		"//orm:index totals_key (UserID) unique nulls_not_distinct\n",
	} {
		t.Run(strings.TrimSpace(decl), func(t *testing.T) {
			p := newProject(t, driftEntities(decl))
			code, stdout, stderr := p.Run("makemigrations", "--name", "initial")
			out := stdout + stderr
			if code == exitClean {
				t.Fatalf("an unsupported index option was accepted:\n%s", out)
			}
			if !strings.Contains(strings.ToLower(out), "nulls") {
				t.Errorf("the refusal does not name the option it rejected:\n%s", out)
			}
		})
	}
}
