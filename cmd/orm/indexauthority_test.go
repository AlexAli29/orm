package main

import (
	"strings"
	"testing"
)

// M16.5: one index rule, reached from both commands a user runs.
//
// The unit tests prove the planner and the drift check ask the same rule. What
// they cannot prove is that the rule is reached from the outside on a real
// database — that a declared change of each kind produces a migration, that the
// same change made by hand produces a finding, and that neither disturbs the
// relation itself.
//
// So each axis is driven twice through the real commands, from one fixture:
// once by changing the declaration and requiring makemigrations to plan it, and
// once by changing the index in the database and requiring check to report it.
// The two directions are the two halves that used to have their own comparison,
// and an axis missing from either is precisely the failure that was invisible.
//
// The relation's OID is checked throughout. An index change must never recreate
// a materialized view: recreating discards every stored row, and doing it to add
// an operator class would be a data-loss bug wearing an index change's clothes.

// authorityEntities declares a materialized view whose single index is the
// fixture's parameter, over columns that support every axis under test.
func authorityEntities(index string) string {
	return `package domain

import "github.com/AlexAli29/orm"

//orm:table public.docs
type Doc struct {
	ID    int64        ` + "`orm:\"pk,identity\"`" + `
	Score int64
	Title string
	Tags  []string
	Doc   orm.TSVector ` + "`orm:\"pgtype:tsvector\"`" + `
}

//orm:materialized-view public.doc_rollup
//orm:definition ` + "`SELECT d.id, d.score, d.title, d.tags, d.doc FROM docs d`" + `
//orm:depends-on public.docs
` + index + `type DocRollupRow struct {
	ID    int64
	Score int64
	Title string
	Tags  []string
	Doc   orm.TSVector ` + "`orm:\"pgtype:tsvector\"`" + `
}
`
}

// indexAxis is one semantic difference, expressed three ways: as the starting
// declaration, as the changed declaration, and as the SQL that makes the same
// change by hand.
type indexAxis struct {
	axis   string
	from   string
	to     string
	byHand string
}

// indexAxisMatrix is the representative surface: one axis of each kind that a
// comparison could lose independently of the others.
func indexAxisMatrix() []indexAxis {
	return []indexAxis{
		{
			"method",
			"//orm:index doc_axis_idx (Doc) using gist\n",
			"//orm:index doc_axis_idx (Doc) using gin\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup USING gin (doc)`,
		},
		{
			"predicate",
			"//orm:index doc_axis_idx (Title) where \"score > 10\"\n",
			"//orm:index doc_axis_idx (Title) where \"score > 20\"\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup (title) WHERE score > 20`,
		},
		{
			"INCLUDE",
			"//orm:index doc_axis_idx (Title) include (Score)\n",
			"//orm:index doc_axis_idx (Title) include (ID)\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup (title) INCLUDE (id)`,
		},
		{
			"expression",
			"//orm:index doc_axis_idx (\"lower(title)\")\n",
			"//orm:index doc_axis_idx (\"upper(title)\")\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup (upper(title))`,
		},
		{
			"operator class",
			"//orm:index doc_axis_idx (Title text_pattern_ops)\n",
			"//orm:index doc_axis_idx (Title)\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup (title)`,
		},
		{
			"key order",
			"//orm:index doc_axis_idx (Title, Score)\n",
			"//orm:index doc_axis_idx (Score, Title)\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup (score, title)`,
		},
		{
			"uniqueness",
			"//orm:index doc_axis_idx (ID) unique\n",
			"//orm:index doc_axis_idx (ID)\n",
			`DROP INDEX doc_axis_idx; CREATE INDEX doc_axis_idx ON doc_rollup (id)`,
		},
	}
}

// A declared change of each kind is planned, and the relation survives it.
func TestIndexAuthority_plannerSeesEachDeclaredChange(t *testing.T) {
	for _, c := range indexAxisMatrix() {
		t.Run(c.axis, func(t *testing.T) {
			p := newProject(t, authorityEntities(c.from))
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("check")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Fatalf("the fixture did not converge before the change, so a later "+
					"migration could not be attributed to %s:\n%s", c.axis, out)
			}
			before := relationOID(t, p, "public.doc_rollup")
			if before == 0 {
				t.Fatal("the materialized view was not created")
			}

			p.Entities(authorityEntities(c.to))
			out := p.MustRun("makemigrations", "--name", "change")
			if strings.Contains(out, "No schema changes detected") {
				t.Fatalf("changing %s planned no migration, so the declaration and the "+
					"database disagree permanently and nothing says so:\n%s", c.axis, out)
			}
			if !strings.Contains(out, "doc_axis_idx") {
				t.Errorf("the plan does not name the index that changed:\n%s", out)
			}
			if strings.Contains(out, "materialized view") {
				t.Errorf("an index change touched the materialized view itself:\n%s", out)
			}
			p.MustRun("migrate")
			p.MustRun("check")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Errorf("the %s change did not converge:\n%s", c.axis, out)
			}
			if after := relationOID(t, p, "public.doc_rollup"); after != before {
				t.Errorf("changing %s recreated the materialized view: OID %d became %d. "+
					"Recreating discards every stored row", c.axis, before, after)
			}
		})
	}
}

// The same change made by hand is reported, and reported as an index.
func TestIndexAuthority_driftSeesEachManualChange(t *testing.T) {
	for _, c := range indexAxisMatrix() {
		t.Run(c.axis, func(t *testing.T) {
			p := newProject(t, authorityEntities(c.from))
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("check")
			before := relationOID(t, p, "public.doc_rollup")

			p.SQL(c.byHand)

			code, stdout, stderr := p.Run("check")
			out := stdout + stderr
			if code == exitClean {
				t.Fatalf("changing %s by hand left the check clean. The declaration and the "+
					"database now disagree and a user is never told:\n%s", c.axis, out)
			}
			if !strings.Contains(out, "doc_axis_idx") {
				t.Errorf("the finding does not name the index that changed:\n%s", out)
			}
			// It is an index finding and not a claim about the relation.
			for _, wrong := range []string{"not the one this project applied", "is a table",
				"provenance", "definition changed"} {
				if strings.Contains(strings.ToLower(out), strings.ToLower(wrong)) {
					t.Errorf("a %s change was also reported as %q:\n%s", c.axis, wrong, out)
				}
			}
			if after := relationOID(t, p, "public.doc_rollup"); after != before {
				t.Errorf("checking recreated the materialized view: OID %d became %d",
					before, after)
			}
		})
	}
}

// An index nobody touched is not a change, in either direction.
//
// Without this every case above would also be satisfied by a planner that
// rewrote the index on every run and a check that always complained.
func TestIndexAuthority_anUnchangedIndexIsSilent(t *testing.T) {
	for _, c := range indexAxisMatrix() {
		t.Run(c.axis, func(t *testing.T) {
			p := newProject(t, authorityEntities(c.from))
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("check")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Errorf("an unchanged %s index planned a migration:\n%s", c.axis, out)
			}
			// And again, so that nothing depends on it being the first run.
			p.MustRun("check")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Errorf("an unchanged %s index planned a migration on the second run:\n%s",
					c.axis, out)
			}
		})
	}
}
