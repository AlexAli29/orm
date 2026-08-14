package migrate_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// M16.5 adversarial audit: the fault points Part B left open.
//
// Part B labelled its cases B1 to B15 and proved B1 to B5, B7, B13 and B15. The
// rest were left, and the labels were never written down anywhere the repository
// can be asked — so what is closed here is the set of fault points rather than a
// reconstruction of somebody's numbering: a failure after one of several indexes,
// a failure writing provenance, and a failure writing history.
//
// The last two are the interesting ones, because they are the two writes that
// make a migration mean something. Part B could only fail a migration with a
// poisoned DDL operation, which fails before either write is reached. These fail
// the writes themselves, from the database, with a trigger that refuses the
// insert — so nothing in the production path is changed to make the fault
// injectable, and the transaction being tested is the real one.
//
// The invariant under attack is the same one throughout: a relation, the indexes
// built on it, the row saying what the server made of its definition and the row
// saying the migration ran are applied together or not at all. Every assertion
// asks PostgreSQL, on a connection outside the transaction.

// refuseInsertsOn puts a trigger on a table that makes every insert fail.
//
// This is fault injection from outside the process. The production code is
// unchanged and unaware, the statement it issues is the one it always issues,
// and what fails is the write — which is exactly the fault a full disk or a
// revoked grant would produce at that moment.
func refuseInsertsOn(t *testing.T, conn *pgx.Conn, table string) {
	t.Helper()
	fn := "orm_audit_refuse_" + strings.ReplaceAll(table, ".", "_")
	stmts := []string{
		`CREATE OR REPLACE FUNCTION ` + fn + `() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'orm audit: refusing the write to ` + table + `'; END $$`,
		`CREATE TRIGGER ` + fn + `_trg BEFORE INSERT ON ` + table +
			` FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(t.Context(), s); err != nil {
			t.Fatalf("installing the fault on %s: %v", table, err)
		}
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(t.Context(), `DROP TRIGGER IF EXISTS `+fn+`_trg ON `+table)
	})
}

// allowInsertsOn removes the fault, so a retry can be proven.
func allowInsertsOn(t *testing.T, conn *pgx.Conn, table string) {
	t.Helper()
	fn := "orm_audit_refuse_" + strings.ReplaceAll(table, ".", "_")
	if _, err := conn.Exec(t.Context(), `DROP TRIGGER IF EXISTS `+fn+`_trg ON `+table); err != nil {
		t.Fatalf("removing the fault on %s: %v", table, err)
	}
}

// indexOn returns an index over a named column, so a second index can be made
// to fail by naming a column that is not there.
func indexOn(name, column string) schema.Index {
	return schema.Index{Name: name, Columns: []schema.IndexColumn{{Name: column}}}
}

// auditBase is the base table the views are built over, plus a migrator whose
// bookkeeping tables already exist so a fault can be installed on them.
func auditBase(t *testing.T) (*pgx.Conn, *schema.Schema) {
	t.Helper()
	conn := connect(t)
	return conn, matviewFixture(t, conn)
}

// §3.2: the second of two indexes fails, and the first one goes with it.
//
// Part B proved a single index. Two is the case where a partial success exists
// to be kept: I1 is already built and committed to the transaction when I2 is
// refused, so an implementation that treated each index as its own unit would
// leave a relation carrying half the indexes it was declared with — and converge
// happily on the half, because the migration would not be recorded and the next
// plan would compare against the state the migrations describe.
func TestMatViewFault_secondIndexFailureRollsBackTheFirst(t *testing.T) {
	conn, base := auditBase(t)

	ops := []migrate.Operation{
		migrate.CreateMaterializedView{View: totals(true)},
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
		// A column the relation does not have: PostgreSQL refuses this index
		// after the one above it has already been built.
		migrate.CreateIndex{Schema: "public", Table: "totals", Index: indexOn("totals_bad_idx", "no_such_column")},
	}
	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true, Operations: ops},
	)
	m := migrate.New(conn, set)
	_, err := m.Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("a migration creating an index over a column that does not exist succeeded")
	}
	// The error names the operation that failed rather than the first one.
	if !strings.Contains(err.Error(), "totals_bad_idx") {
		t.Errorf("the error does not name the failing index: %v", err)
	}

	if relationExists(t, conn, "public.totals") {
		t.Error("the materialized view survived a failure in its second index")
	}
	if relationExists(t, conn, "public.totals_id_key") {
		t.Error("the first index survived a failure in the second, so each index was its own unit")
	}
	if relationExists(t, conn, "public.totals_bad_idx") {
		t.Error("the failing index exists")
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); ok {
		t.Error("provenance survived a failed migration")
	}
	if ids := historyIDs(t, m); len(ids) != 1 || ids[0] != "0001_initial" {
		t.Errorf("history = %v, want only the first migration", ids)
	}

	// And the same migration applies cleanly once the failing index is
	// corrected: a rollback nobody can recover from is not a rollback.
	fixed := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: indexOn("totals_email_idx", "email")},
			}},
	)
	if _, err := migrate.New(conn, fixed).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the corrected migration did not apply: %v", err)
	}
	for _, want := range []string{"public.totals", "public.totals_id_key", "public.totals_email_idx"} {
		if !relationExists(t, conn, want) {
			t.Errorf("%s is missing after the retry", want)
		}
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); !ok {
		t.Error("the retry left no provenance")
	}
	// Exactly once: a retry that applied the migration twice would double the
	// history, and the next plan would be computed from the wrong place.
	if ids := historyIDs(t, migrate.New(conn, fixed)); len(ids) != 2 {
		t.Errorf("history = %v, want both migrations exactly once", ids)
	}
}

// §3.4: the DDL succeeds and the provenance write fails.
//
// This is the shape Part B named as one of the two quiet ones: a relation left
// behind with no record of what it holds looks fine until a later check compares
// the database against a definition nothing recorded. The write is failed at the
// database, so the production path is the real one.
func TestMatViewFault_provenanceWriteFailureRollsBackTheDDL(t *testing.T) {
	conn, base := auditBase(t)

	// A first migration creates the base table and, with it, the bookkeeping
	// tables — the fault cannot be installed on a table that does not exist.
	first := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, base))
	m := migrate.New(conn, first)
	if err := m.EnsureHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the first migration failed: %v", err)
	}

	refuseInsertsOn(t, conn, migrate.HistorySchema+"."+migrate.ViewStateTable)

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	m2 := migrate.New(conn, set)
	_, err := m2.Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("the migration succeeded although recording provenance was refused")
	}
	if !strings.Contains(err.Error(), "orm audit: refusing the write") {
		t.Errorf("the failure the caller sees is not the provenance write: %v", err)
	}

	// Nothing may remain: a relation without provenance is the state this
	// transaction exists to make impossible.
	if relationExists(t, conn, "public.totals") {
		t.Error("the materialized view exists with no record of what it holds")
	}
	if relationExists(t, conn, "public.totals_id_key") {
		t.Error("the index survived a failed provenance write")
	}
	if ids := historyIDs(t, m2); len(ids) != 1 || ids[0] != "0001_initial" {
		t.Errorf("history = %v, want only the first migration", ids)
	}

	// And it applies once the write is possible again.
	allowInsertsOn(t, conn, migrate.HistorySchema+"."+migrate.ViewStateTable)
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if !relationExists(t, conn, "public.totals") {
		t.Error("the retry did not create the relation")
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); !ok {
		t.Error("the retry created the relation and recorded no provenance")
	}
}

// §3.5: everything succeeds and the history write fails.
//
// The mirror of the case above, and the more dangerous one. A database changed by
// a migration that is not recorded as applied will have that migration applied
// again — so if the DDL survived, the second attempt fails on a relation that
// already exists, and the project is stuck until somebody repairs it by hand.
func TestMatViewFault_historyWriteFailureRollsBackEverything(t *testing.T) {
	conn, base := auditBase(t)

	first := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, base))
	m := migrate.New(conn, first)
	if err := m.EnsureHistory(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the first migration failed: %v", err)
	}

	refuseInsertsOn(t, conn, migrate.HistorySchema+"."+migrate.HistoryTable)

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	m2 := migrate.New(conn, set)
	if _, err := m2.Migrate(t.Context(), ""); err == nil {
		t.Fatal("the migration succeeded although recording it was refused")
	}

	if relationExists(t, conn, "public.totals") {
		t.Error("the database changed and the migration is not recorded as applied, so " +
			"applying it again will fail on a relation that already exists")
	}
	if relationExists(t, conn, "public.totals_id_key") {
		t.Error("the index survived a failed history write")
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); ok {
		t.Error("provenance survived a failed history write")
	}

	allowInsertsOn(t, conn, migrate.HistorySchema+"."+migrate.HistoryTable)
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if ids := historyIDs(t, migrate.New(conn, set)); len(ids) != 2 {
		t.Errorf("history = %v, want both migrations exactly once", ids)
	}
}

// §3.3: a DROP whose provenance deletion is not the thing that fails, but whose
// history write is — destructive work has begun and must be undone.
//
// Part B proved a drop failed by a poisoned operation. This fails after every
// destructive statement has run and the record has been removed, which is the
// last moment at which a rollback still has everything to restore.
func TestMatViewFault_dropRollsBackWhenRecordingFails(t *testing.T) {
	conn, base := auditBase(t)

	withView := &schema.Schema{Tables: base.Tables,
		MaterializedViews: []schema.MaterializedView{totals(true)}}
	withView.MaterializedViews[0].Indexes = []schema.Index{totalsIndex()}

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	m := migrate.New(conn, set)
	if err := m.EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("setting up: %v", err)
	}
	before, ok := provenanceOf(t, conn, "public.totals")
	if !ok {
		t.Fatal("the setup recorded no provenance")
	}

	refuseInsertsOn(t, conn, migrate.HistorySchema+"."+migrate.HistoryTable)

	dropSet := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
		&migrate.Migration{ID: "0003_drop", DependsOn: []string{"0002_totals"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.DropMaterializedView{Schema: "public", Name: "totals"},
			}},
	)
	if _, err := migrate.New(conn, dropSet).Migrate(t.Context(), ""); err == nil {
		t.Fatal("the drop succeeded although recording it was refused")
	}

	// Everything the drop destroyed is back, including the record — and the
	// record is compared by value, because a restored row holding a different
	// definition would be worse than none: it would be believed.
	if !relationExists(t, conn, "public.totals") {
		t.Error("the materialized view was not restored")
	}
	if !relationExists(t, conn, "public.totals_id_key") {
		t.Error("the index was not restored")
	}
	after, ok := provenanceOf(t, conn, "public.totals")
	if !ok {
		t.Fatal("the provenance row was not restored")
	}
	if after != before {
		t.Errorf("the restored provenance is not the original row:\n before %+v\n after  %+v",
			before, after)
	}

	allowInsertsOn(t, conn, migrate.HistorySchema+"."+migrate.HistoryTable)
	if _, err := migrate.New(conn, dropSet).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the retried drop failed: %v", err)
	}
	if relationExists(t, conn, "public.totals") {
		t.Error("the retried drop left the relation")
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); ok {
		t.Error("the retried drop left the provenance row")
	}
}
