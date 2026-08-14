package main

import (
	"strings"
	"testing"
)

// M16.5 G2 #13/#14: an index's access method is part of what it is.
//
// The per-axis comparison test proves that two Index values differing only in
// their method are not equal. That is necessary and not sufficient: it says
// nothing about whether a method a developer declared ever reaches that field.
// If the scanner dropped `using gist`, or introspection never read pg_am, the
// comparison would be perfectly correct about values nothing ever produced —
// and changing a real GiST index to a real GIN index would plan no migration.
//
// So the method axis is also proven from the outside, on a real index that
// PostgreSQL really built with that access method: change the declared method
// and a migration must be planned. Both directions of the fixture matter. The
// initial `check` being clean proves the declared method survived generation,
// creation and introspection intact — a lost method would already show as drift
// there. The planned migration proves the method is compared.
//
// GiST and BRIN each get their own case because a comparison can be made to
// ignore one access method without ignoring the others.

// methodEntities declares a table and a materialized view carrying one index
// whose access method is the fixture's parameter.
//
// The column types are chosen so that both methods under test are legal for the
// same column, which is what makes the transition an isolated method change
// rather than a change of key as well.
func methodEntities(index string) string {
	return `package domain

import "github.com/AlexAli29/orm"

//orm:table public.docs
type Doc struct {
	ID    int64        ` + "`orm:\"pk,identity\"`" + `
	Score int64
	Tags  []string
	Doc   orm.TSVector ` + "`orm:\"pgtype:tsvector\"`" + `
}

//orm:materialized-view public.doc_rollup
//orm:definition ` + "`SELECT d.id, d.score, d.tags, d.doc FROM docs d`" + `
//orm:depends-on public.docs
` + index + `type DocRollupRow struct {
	ID    int64
	Score int64
	Tags  []string
	Doc   orm.TSVector ` + "`orm:\"pgtype:tsvector\"`" + `
}
`
}

// methodChangeIsPlanned drives one method transition end to end.
//
// The assertion is convergence in both directions: nothing to do before the
// change, and something to do after it. A comparison that ignored the method
// would report "No schema changes detected" for a genuine change of access
// method, leaving the database holding an index the declarations do not
// describe, permanently and silently.
func methodChangeIsPlanned(t *testing.T, from, to string) {
	t.Helper()
	p := newProject(t, methodEntities(from))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	// A clean check here is what proves the declared method survived the whole
	// round trip: generation, CREATE INDEX, and introspection back out of
	// pg_am. A dropped method would already be drift at this point.
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the fixture does not converge before the method changes, so a later "+
			"migration could not be attributed to the method:\n%s", out)
	}

	p.Entities(methodEntities(to))
	out := p.MustRun("makemigrations")
	if strings.Contains(out, "No schema changes detected") {
		t.Fatalf("changing the declared access method planned no migration. The method is "+
			"not compared, so the database keeps an index the declarations do not "+
			"describe:\n%s", out)
	}
	// And the plan really is about the index rather than about anything else the
	// transition might have disturbed.
	if !strings.Contains(out, "doc_method_idx") {
		t.Errorf("the planned migration does not mention the index whose method changed:\n%s", out)
	}
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the method change did not converge:\n%s", out)
	}
}

// #13: a real GiST index, changed to a real GIN index.
//
// tsvector supports both, so the key stays the same and the method is the only
// axis that moves.
func TestMatViewIndexMethod_gistChangeIsPlanned(t *testing.T) {
	methodChangeIsPlanned(t,
		"//orm:index doc_method_idx (Doc) using gist\n",
		"//orm:index doc_method_idx (Doc) using gin\n")
}

// And the other direction, so the case does not depend on which side of the
// comparison the GiST index is on.
func TestMatViewIndexMethod_gistIsReachedFromEitherSide(t *testing.T) {
	methodChangeIsPlanned(t,
		"//orm:index doc_method_idx (Doc) using gin\n",
		"//orm:index doc_method_idx (Doc) using gist\n")
}

// #14: a real BRIN index, changed to a btree.
//
// A plain bigint column supports both, and btree is the method a declaration
// gets when it names none — so this also proves the default is not applied so
// widely that it swallows an explicit method.
func TestMatViewIndexMethod_brinChangeIsPlanned(t *testing.T) {
	methodChangeIsPlanned(t,
		"//orm:index doc_method_idx (Score) using brin\n",
		"//orm:index doc_method_idx (Score)\n")
}

// And BRIN reached from the other side.
func TestMatViewIndexMethod_brinIsReachedFromEitherSide(t *testing.T) {
	methodChangeIsPlanned(t,
		"//orm:index doc_method_idx (Score)\n",
		"//orm:index doc_method_idx (Score) using brin\n")
}

// The same axis, on the other authority.
//
// Planning a migration and reporting drift are different questions asked on
// different occasions, and each has its own index comparison. Removing an access
// method from one of them leaves the other perfectly correct, so a killer that
// only plans a migration proves nothing about the check a user runs to be told
// what somebody changed by hand. Both halves therefore have a method killer.
//
// The drift direction is the quieter failure of the two: makemigrations goes on
// working, and the only symptom is that `orm check` stops mentioning an index
// that no longer matches its declaration.
func methodDriftIsReported(t *testing.T, declared, byHand string) {
	t.Helper()
	p := newProject(t, methodEntities(declared))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")

	p.SQL(byHand)

	code, stdout, stderr := p.Run("check")
	out := stdout + stderr
	if code == exitClean {
		t.Fatalf("redefining the index with a different access method left the check clean. "+
			"A hand-edited index is never reported, and the declaration and the database "+
			"disagree permanently:\n%s", out)
	}
	if !strings.Contains(out, "doc_method_idx") {
		t.Errorf("the finding does not name the index whose method changed:\n%s", out)
	}
	if strings.Contains(out, "not the one this project applied") {
		t.Errorf("the method change was reported as definition drift; the body did not "+
			"change:\n%s", out)
	}
}

// #13 on the drift authority: a real GiST index rebuilt by hand as GIN.
func TestMatViewIndexMethod_gistDriftIsReported(t *testing.T) {
	methodDriftIsReported(t,
		"//orm:index doc_method_idx (Doc) using gist\n",
		`DROP INDEX doc_method_idx;
		 CREATE INDEX doc_method_idx ON doc_rollup USING gin (doc)`)
}

// And with the GiST index on the database's side rather than the declaration's.
func TestMatViewIndexMethod_gistDriftIsReportedFromEitherSide(t *testing.T) {
	methodDriftIsReported(t,
		"//orm:index doc_method_idx (Doc) using gin\n",
		`DROP INDEX doc_method_idx;
		 CREATE INDEX doc_method_idx ON doc_rollup USING gist (doc)`)
}

// #14 on the drift authority: a real BRIN index rebuilt by hand as a btree.
func TestMatViewIndexMethod_brinDriftIsReported(t *testing.T) {
	methodDriftIsReported(t,
		"//orm:index doc_method_idx (Score) using brin\n",
		`DROP INDEX doc_method_idx;
		 CREATE INDEX doc_method_idx ON doc_rollup USING btree (score)`)
}

// And BRIN on the database's side.
func TestMatViewIndexMethod_brinDriftIsReportedFromEitherSide(t *testing.T) {
	methodDriftIsReported(t,
		"//orm:index doc_method_idx (Score)\n",
		`DROP INDEX doc_method_idx;
		 CREATE INDEX doc_method_idx ON doc_rollup USING brin (score)`)
}
