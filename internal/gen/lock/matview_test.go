package lock_test

import (
	"testing"

	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// The mapping fingerprint covers what generation reads.
//
// A materialized view's generated constructor carries the name of the unique
// index that lets REFRESH CONCURRENTLY run, so the runtime can refuse before
// sending a statement the server would reject. That name is read out of the
// schema at generation time, which makes it an input to generation.
//
// It was not covered by the fingerprint, and the consequence was a false
// statement about the database: adding a qualifying index through a migration
// left the generated code saying there was none, `orm check --generated`
// reported "Generated current" because the mapping had not moved, and
// Refresh(Concurrently) refused a refresh PostgreSQL accepted.

func matview(name string, uniques ...model.PGUnique) *model.Mapping {
	e := &model.GoEntity{Name: "Total", PkgPath: "example.com/p", Kind: model.RelMaterializedView}
	t := &model.PGTable{Schema: "public", Name: name, Kind: 'm', Uniques: uniques}
	return &model.Mapping{Entities: []*model.EntityMapping{{Entity: e, Table: t}}}
}

func plainUnique(name string) model.PGUnique {
	return model.PGUnique{Name: name, Cols: []*model.PGColumn{{Name: "user_id"}}}
}

// Gaining a qualifying index moves the fingerprint, so the generated code is
// reported stale and the developer is told to regenerate.
func TestFingerprint_movesWhenAQualifyingIndexAppears(t *testing.T) {
	none := lock.Fingerprint(matview("totals"))
	got := lock.Fingerprint(matview("totals", plainUnique("totals_user_id_key")))
	if none == got {
		t.Error("adding a qualifying unique index left the mapping fingerprint unchanged, " +
			"so nothing would report the generated code as stale")
	}
	// And losing it moves back.
	if again := lock.Fingerprint(matview("totals")); again != none {
		t.Error("removing the index did not restore the fingerprint")
	}
}

// Indexes that cannot satisfy PostgreSQL's rule are not what the generated code
// carries, so they must not move the fingerprint: churning the lock on a change
// that cannot affect generation would teach people to regenerate until the diff
// goes away.
func TestFingerprint_ignoresIndexesThatCannotQualify(t *testing.T) {
	base := lock.Fingerprint(matview("totals"))

	partial := plainUnique("totals_partial_key")
	partial.Partial = true
	if got := lock.Fingerprint(matview("totals", partial)); got != base {
		t.Error("a partial unique index moved the fingerprint, and it cannot be generated into anything")
	}

	expr := plainUnique("totals_expr_key")
	expr.Expression = true
	if got := lock.Fingerprint(matview("totals", expr)); got != base {
		t.Error("an expression unique index moved the fingerprint")
	}
}

// When several qualify, the lowest name wins, deterministically — the same rule
// the emitter applies, so the two cannot disagree about what was generated.
func TestFingerprint_picksTheSameIndexWhicheverOrderTheyArriveIn(t *testing.T) {
	a := lock.Fingerprint(matview("totals", plainUnique("aaa_key"), plainUnique("zzz_key")))
	b := lock.Fingerprint(matview("totals", plainUnique("zzz_key"), plainUnique("aaa_key")))
	if a != b {
		t.Error("the fingerprint depends on the order the catalog returned the indexes in")
	}
	// And it is the lower name that was chosen: dropping the higher one changes
	// nothing.
	if only := lock.Fingerprint(matview("totals", plainUnique("aaa_key"))); only != a {
		t.Error("the fingerprint did not settle on the lowest qualifying name")
	}
}

// Whether the view currently holds data is not part of what was generated.
//
// Population is runtime state the server owns: it changes every time anybody
// refreshes, and a refresh is not a schema change. A fingerprint carrying it
// would report generated code as stale after an ordinary REFRESH — and after a
// REFRESH WITH NO DATA the lock would disagree with itself depending on when it
// was written, so two developers generating from one commit against one schema
// would produce different locks.
func TestFingerprint_populationDoesNotMoveIt(t *testing.T) {
	populated := func(on bool) *model.Mapping {
		m := matview("totals", plainUnique("totals_user_id_key"))
		m.Entities[0].Table.Populated = on
		return m
	}
	if a, b := lock.Fingerprint(populated(true)), lock.Fingerprint(populated(false)); a != b {
		t.Error("whether the materialized view holds data moved the mapping fingerprint. " +
			"Refreshing would then report the generated code as stale, and the lock a " +
			"developer commits would depend on when they happened to generate it")
	}
}

// Nor is the relation's OID, nor what the server deparsed its definition to.
//
// Both are per-server facts. An OID differs between two databases built from one
// migration; the deparsed text differs between majors, because PostgreSQL 16
// stopped qualifying columns it does not need to. Either in the fingerprint makes
// one project produce different locks on different servers, which arrives as a
// diff nobody can explain.
func TestFingerprint_serverLocalFactsDoNotMoveIt(t *testing.T) {
	base := matview("totals", plainUnique("totals_user_id_key"))
	ref := lock.Fingerprint(base)

	withOID := matview("totals", plainUnique("totals_user_id_key"))
	withOID.Entities[0].Table.OID = 424242
	if got := lock.Fingerprint(withOID); got != ref {
		t.Error("the relation's OID moved the mapping fingerprint")
	}

	withCanonical := matview("totals", plainUnique("totals_user_id_key"))
	withCanonical.Entities[0].Table.Definition = " SELECT id,\n    email\n   FROM users;"
	if got := lock.Fingerprint(withCanonical); got != ref {
		t.Error("the server's deparsed definition moved the mapping fingerprint. It is not " +
			"portable: one unchanged view reads differently on PostgreSQL 15 and 16")
	}
}

// A table's generated code does not depend on its indexes, so its fingerprint
// must not move when they change. Otherwise every existing lock in every
// table-only project would change for no reason.
func TestFingerprint_tableIndexesDoNotMoveIt(t *testing.T) {
	table := func(uniques ...model.PGUnique) *model.Mapping {
		e := &model.GoEntity{Name: "User", PkgPath: "example.com/p", Kind: model.RelTable}
		tb := &model.PGTable{Schema: "public", Name: "users", Kind: 'r', Uniques: uniques}
		return &model.Mapping{Entities: []*model.EntityMapping{{Entity: e, Table: tb}}}
	}
	if a, b := lock.Fingerprint(table()), lock.Fingerprint(table(plainUnique("users_email_key"))); a != b {
		t.Error("a table's unique index moved the mapping fingerprint")
	}
}

// The digest is a function of the mapping, not of what produced it.
//
// A mapping carries the schema it was reconciled against, and that schema knows
// which search path was introspected — and is the obvious place for anything
// else somebody decides to record about the connection. None of it may reach the
// digest. The search path is a project setting rather than a server fact, so
// including it would already be wrong; a server fact stored beside it would make
// one project produce a different lock on every database it is generated
// against, and the difference would arrive as a diff nobody can explain.
func TestFingerprint_doesNotDependOnWhatItWasIntrospectedFrom(t *testing.T) {
	with := func(searchPath ...string) *model.Mapping {
		m := matview("totals", plainUnique("totals_user_id_key"))
		m.Schema = model.NewSchema(searchPath, []*model.PGTable{m.Entities[0].Table})
		return m
	}
	ref := lock.Fingerprint(with("public"))
	if got := lock.Fingerprint(with("public", "pg160004")); got != ref {
		t.Error("something recorded about the server the mapping was introspected from " +
			"moved the mapping fingerprint")
	}
	if got := lock.Fingerprint(with("public", "db:orm_test_a1b2c3")); got != ref {
		t.Error("the database the mapping was introspected from moved the mapping fingerprint")
	}
	// And a mapping with no schema at all agrees with both, so the digest never
	// depended on one being there.
	if got := lock.Fingerprint(matview("totals", plainUnique("totals_user_id_key"))); got != ref {
		t.Error("a mapping carrying no schema fingerprints differently from one that does")
	}
}
