package reconcile

import (
	"context"
	"fmt"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Reconciling views and materialized views.
//
// This is not a second checker beside reconciliation. It adds findings to the
// same report, through the same reconciler, in the same tiers — a view's
// columns are compared by the column tier that compares a table's, because a
// view column is a column and a second comparison would be a second place for
// a type to be read differently.
//
// What is genuinely different is two things: a relation has a kind, and a view
// has a body.
//
// # Three layers that must never collapse into each other
//
//	orm.lock             what the project declares, portable, committed
//	orm check            what this database holds, per server, never committed
//	orm_schema_views     what this server made of what was applied
//
// The lock answers "did the declaration change". A check answers "did the
// database change". They use different representations because they must: a
// lock has to be byte-identical on PostgreSQL 14 and 18, and a deparsed
// definition is not — 16 stopped qualifying columns it does not need to. A
// future refactor that made them share a representation would break one of the
// two, silently, and the one it breaks is whichever is tested less.

// kindAgrees reports whether the relation found is the kind the declaration
// asked for, and records a finding when it is not.
//
// A wrong kind is one finding rather than a missing relation and an unexpected
// one. Those two are what a diff sees; they are not what happened. What happened
// is that the name the project declared is occupied by something else, and
// telling somebody their view is missing while a table of that name sits in
// front of them is how a report loses their trust.
func (r *reconciler) kindAgrees(e *model.GoEntity, actual *model.PGTable) bool {
	want := e.Kind
	got := kindOf(actual)
	if want == got {
		return true
	}
	r.add(diag.Finding{
		Code: diag.E030,
		Message: fmt.Sprintf("%s is a %s, and %s declares it as a %s",
			actual.Qualified(), got, e.Display(), want),
		Reason: fmt.Sprintf("PostgreSQL has one namespace for tables, views and materialized views, "+
			"so %s is not missing — something else is standing where it should be. Nothing else about "+
			"this relation is compared, because comparing a %s against a %s produces findings that "+
			"are true of neither",
			actual.Qualified(), got, want),
		Fix: fmt.Sprintf("drop %s and let a migration create the %s, or change the directive on %s "+
			"to %s if the existing relation is the one you meant",
			actual.Qualified(), want, e.Display(), got.Directive()),
		Entity: e.Display(), Table: actual.Qualified(),
		Pos: e.Marker,
	})
	return false
}

// kindOf reads the catalog's relkind as a declaration kind.
func kindOf(t *model.PGTable) model.RelationKind {
	switch {
	case t == nil:
		return model.RelTable
	case t.Kind == 'm':
		return model.RelMaterializedView
	case t.Kind == 'v':
		return model.RelView
	default:
		return model.RelTable
	}
}

// DefinitionInput is what a definition check needs.
type DefinitionInput struct {
	// Entities are the declarations, including tables, which are skipped.
	Entities []*model.GoEntity
	// Schema is the introspected database.
	Schema *model.Schema
	// Reader reads the recorded definitions. It has no Exec: a check cannot
	// write through it, and that is a property of the type rather than a rule
	// somebody has to keep.
	Reader migrate.ViewReader
}

// CheckDefinitions compares what each managed view's body was when it was
// applied against what it is now, and adds its findings to the report.
//
// It is a separate entry point rather than a tier inside Run because it needs a
// connection and Run does not: Run reconciles an already-introspected schema and
// is used in places where there is nothing to query. Everything it produces
// goes into the same report.
//
// Nothing here compares a project's own SQL against the server's. Those two
// differ whenever PostgreSQL has reconstructed a definition, which is always,
// and treating them as comparable is the mistake the recording exists to avoid.
func CheckDefinitions(ctx context.Context, report *diag.Report, in DefinitionInput) error {
	var managed []*model.GoEntity
	for _, e := range in.Entities {
		if e.Kind != model.RelTable {
			managed = append(managed, e)
		}
	}
	if len(managed) == 0 {
		return nil
	}

	// One query for every recording, not one per view. A per-view lookup would
	// be a round trip per relation on a command somebody runs in CI.
	recorded, err := migrate.ReadViewState(ctx, in.Reader)
	if err != nil {
		return err
	}

	for _, e := range managed {
		actual, ok := in.Schema.Lookup(e.Table)
		if !ok {
			// Missing relations are the entity tier's finding; a stale
			// recording must not turn that into something else, and must not
			// make this one report a body that is not there.
			continue
		}
		if kindOf(actual) != e.Kind {
			// The kind tier already said so. Comparing bodies across kinds
			// would add a second, less accurate finding about the same fact.
			continue
		}
		checkOneDefinition(report, e, actual, recorded)
	}
	return nil
}

func checkOneDefinition(report *diag.Report, e *model.GoEntity, actual *model.PGTable, recorded map[string]migrate.ViewState) {
	rec, ok := recorded[actual.Qualified()]
	if !ok {
		// The relation is there and its shape may be right, and neither of
		// those says the body is the one this project declared. A view created
		// by hand, a database restored from elsewhere, a project upgraded into
		// view support — all reach here, and all of them are cases where
		// claiming the definition is clean would be claiming something nothing
		// checked.
		//
		// This is a warning rather than an error because the schema may be
		// perfectly correct; what is missing is the evidence, not the schema.
		report.Add(diag.Finding{
			Code: diag.W031,
			Message: fmt.Sprintf("%s exists, and there is no record of this project having applied its definition",
				actual.Qualified()),
			Reason: "definition drift is detected by comparing what PostgreSQL made of the definition " +
				"when a migration applied it against what it says now. Without the first of those there " +
				"is nothing to compare, so the body of this relation is unverified — it may be the " +
				"declared one, and nothing here can tell",
			Fix: fmt.Sprintf("apply %s through a migration so its definition is recorded. Until then "+
				"its body is not checked", actual.Qualified()),
			Entity: e.Display(), Table: actual.Qualified(),
			Pos: e.Marker,
		})
		return
	}
	if !schema.SameOnServer(rec.Canonical, actual.Definition) {
		report.Add(diag.Finding{
			Code: diag.E032,
			Message: fmt.Sprintf("the definition of %s is not the one this project applied",
				actual.Qualified()),
			Reason: "PostgreSQL's own reconstruction of the definition has changed since the migration " +
				"that applied it. Both readings come from this server, so formatting and comments are " +
				"already gone: something changed the body. A predicate, a projection or a join can " +
				"change without any column changing, which is why the shape of the relation still matches",
			Fix: fmt.Sprintf("restore %s from its declaration with a migration, or bring the "+
				"declaration in line with the database and apply that", actual.Qualified()),
			Entity: e.Display(), Table: actual.Qualified(),
			Pos: e.Marker,
		})
	}
}
