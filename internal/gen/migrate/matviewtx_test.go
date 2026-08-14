package migrate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Part B: a materialized view, its indexes and its provenance are one
// transaction.
//
// These run through Migrator.Migrate — the same path orm migrate uses — rather
// than calling the operations or the recorder directly. That distinction is the
// point: a helper that participates in a transaction it was handed proves
// nothing about whether the production path hands it one, and the failure this
// guards against is a migration that leaves a relation behind with no record of
// what it holds, or a record for a relation that does not exist.
//
// The database is the proof. Nothing here asserts on transaction identity.

// matviewFixture creates the base table these tests build views over.
func matviewFixture(t *testing.T, conn *pgx.Conn) *schema.Schema {
	t.Helper()
	base := &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "users",
		Columns: []schema.Column{
			{Name: "id", Type: schema.Type{Name: "int8"}},
			{Name: "email", Type: schema.Type{Name: "text"}},
		},
		PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
	}}}
	return base
}

// totals is the materialized view under test.
func totals(withData bool) schema.MaterializedView {
	return schema.MaterializedView{
		Schema: "public", Name: "totals", WithData: withData,
		Definition: schema.Definition{SQL: "SELECT id, email FROM users"},
		DependsOn:  []schema.RelationRef{{Schema: "public", Name: "users", Kind: schema.KindTable, KindKnown: true}},
	}
}

// totalsIndex is a unique index over a plain column: the shape REFRESH
// CONCURRENTLY needs, and the one whose rollback has to be proved.
func totalsIndex() schema.Index {
	return schema.Index{Name: "totals_id_key", Unique: true, Columns: []schema.IndexColumn{{Name: "id"}}}
}

// poison is an operation PostgreSQL always refuses, used to fail a migration
// after the operations under test have already reached the server.
func poison() migrate.Operation {
	return migrate.AddColumn{Schema: "public", Table: "users",
		Column: schema.Column{Name: "bad", Type: schema.Type{Name: "no_such_type"}, Nullable: true}}
}

// relationExists asks the catalog, on a connection outside any transaction.
func relationExists(t *testing.T, conn *pgx.Conn, qualified string) bool {
	t.Helper()
	var ok bool
	if err := conn.QueryRow(t.Context(),
		`SELECT to_regclass($1) IS NOT NULL`, qualified).Scan(&ok); err != nil {
		t.Fatalf("asking whether %s exists: %v", qualified, err)
	}
	return ok
}

func provenanceOf(t *testing.T, conn *pgx.Conn, qualified string) (migrate.ViewState, bool) {
	t.Helper()
	state, err := migrate.ReadViewState(t.Context(), conn)
	if err != nil {
		t.Fatalf("reading the view state: %v", err)
	}
	s, ok := state[qualified]
	return s, ok
}

func historyIDs(t *testing.T, m *migrate.Migrator) []string {
	t.Helper()
	applied, err := m.Applied(t.Context())
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	ids := make([]string, 0, len(applied))
	for _, a := range applied {
		ids = append(ids, a.ID)
	}
	return ids
}

// B1, B2, B3: a CREATE that fails afterwards leaves nothing — not the relation,
// not its index, not its provenance, not a history row.
func TestMatViewTx_createRollsBackEverything(t *testing.T) {
	for _, c := range []struct {
		what     string
		withData bool
		withIdx  bool
	}{
		{"with data", true, false},
		{"with data and an index", true, true},
		{"with no data", false, false},
	} {
		t.Run(c.what, func(t *testing.T) {
			conn := connect(t)
			base := matviewFixture(t, conn)

			ops := []migrate.Operation{migrate.CreateMaterializedView{View: totals(c.withData)}}
			if c.withIdx {
				ops = append(ops, migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()})
			}
			// The failure comes last, so everything above it has already
			// reached PostgreSQL when the transaction is abandoned.
			ops = append(ops, poison())

			set := newSet(t,
				migrationFor(t, "0001_initial", &schema.Schema{}, base),
				&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true, Operations: ops},
			)
			m := migrate.New(conn, set)
			if _, err := m.Migrate(t.Context(), ""); err == nil {
				t.Fatal("the migration succeeded")
			}

			if relationExists(t, conn, "public.totals") {
				t.Error("the materialized view survived a failed migration")
			}
			if c.withIdx && relationExists(t, conn, "public.totals_id_key") {
				t.Error("the index survived a failed migration")
			}
			if _, ok := provenanceOf(t, conn, "public.totals"); ok {
				t.Error("provenance survived a failed migration, so the recorder committed on its own")
			}
			if ids := historyIDs(t, m); len(ids) != 1 || ids[0] != "0001_initial" {
				t.Errorf("history = %v, want only the first migration", ids)
			}
		})
	}
}

// B7, B15: the failed migration left no history row, so the same migration
// applies cleanly once the failure is removed.
func TestMatViewTx_createRetriesAfterRollback(t *testing.T) {
	conn := connect(t)
	base := matviewFixture(t, conn)

	failing := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
				poison(),
			}},
	)
	if _, err := migrate.New(conn, failing).Migrate(t.Context(), ""); err == nil {
		t.Fatal("the migration succeeded")
	}

	// The same migration, without the poison.
	good := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	m := migrate.New(conn, good)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("retrying after a rolled-back failure: %v", err)
	}
	if !relationExists(t, conn, "public.totals") {
		t.Error("the retry did not create the materialized view")
	}
	if !relationExists(t, conn, "public.totals_id_key") {
		t.Error("the retry did not create the index")
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); !ok {
		t.Error("the retry recorded no provenance")
	}
	if ids := historyIDs(t, m); len(ids) != 2 {
		t.Errorf("history = %v, want both migrations", ids)
	}
}

// B4: a successful drop removes the relation, its indexes and its record, and
// leaves no stale metadata behind.
func TestMatViewTx_dropLifecycle(t *testing.T) {
	conn, m := createdMatView(t)

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, matviewFixture(t, conn)),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
		&migrate.Migration{ID: "0003_drop", DependsOn: []string{"0002_totals"}, Atomic: true,
			Operations: []migrate.Operation{migrate.DropMaterializedView{Schema: "public", Name: "totals"}}},
	)
	m = migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("dropping: %v", err)
	}
	if relationExists(t, conn, "public.totals") {
		t.Error("the materialized view survived its drop")
	}
	if relationExists(t, conn, "public.totals_id_key") {
		t.Error("the index survived the relation it was built on")
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); ok {
		t.Error("provenance survived the drop, leaving a record of a relation that is gone")
	}
	if ids := historyIDs(t, m); len(ids) != 3 {
		t.Errorf("history = %v, want three migrations", ids)
	}
}

// B5: a drop that fails afterwards restores everything — the relation, its
// index, and the exact provenance that was there before.
func TestMatViewTx_dropRollsBackToTheOriginalState(t *testing.T) {
	conn, _ := createdMatView(t)

	before, ok := provenanceOf(t, conn, "public.totals")
	if !ok {
		t.Fatal("the fixture recorded no provenance")
	}
	if before.Canonical == "" {
		t.Fatal("the fixture's provenance has no canonical definition to compare")
	}

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, matviewFixture(t, conn)),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
		&migrate.Migration{ID: "0003_drop", DependsOn: []string{"0002_totals"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.DropMaterializedView{Schema: "public", Name: "totals"},
				poison(),
			}},
	)
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err == nil {
		t.Fatal("the drop migration succeeded")
	}

	if !relationExists(t, conn, "public.totals") {
		t.Fatal("the materialized view was not restored by the rollback")
	}
	if !relationExists(t, conn, "public.totals_id_key") {
		t.Error("the index was not restored by the rollback")
	}
	after, ok := provenanceOf(t, conn, "public.totals")
	if !ok {
		t.Fatal("provenance was not restored by the rollback")
	}
	// Not merely "a row exists": the original one, byte for byte. A row with a
	// different canonical definition would mean a later check comparing the
	// database against something it never applied.
	if after.Canonical != before.Canonical {
		t.Errorf("the restored canonical definition differs:\n before %q\n after  %q",
			before.Canonical, after.Canonical)
	}
	if after.SourceIdentity != before.SourceIdentity {
		t.Errorf("the restored source identity differs: %q vs %q", before.SourceIdentity, after.SourceIdentity)
	}
	if after.Kind != before.Kind {
		t.Errorf("the restored kind differs: %q vs %q", before.Kind, after.Kind)
	}
	if ids := historyIDs(t, m); len(ids) != 2 {
		t.Errorf("history = %v, want the drop to be absent", ids)
	}
}

// B13: the forced failure is still the error a caller sees. A rollback must not
// replace the cause with a report about the rollback.
func TestMatViewTx_theOriginalFailureSurvives(t *testing.T) {
	conn := connect(t)
	base := matviewFixture(t, conn)
	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				poison(),
			}},
	)
	_, err := migrate.New(conn, set).Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("the migration succeeded")
	}
	var exec *migrate.ErrExecution
	if !errors.As(err, &exec) {
		t.Fatalf("error = %v, want an execution error", err)
	}
	if exec.Index != 1 {
		t.Errorf("the failure is reported at operation %d, want the poison at 1", exec.Index)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error = %v, want PostgreSQL's own error to be reachable", err)
	}
	if pgErr.Code == "" {
		t.Error("the SQLSTATE was lost")
	}
}

// createdMatView builds a database with the materialized view applied through
// the production path, and returns the connection and migrator.
func createdMatView(t *testing.T) (*pgx.Conn, *migrate.Migrator) {
	t.Helper()
	conn := connect(t)
	base := matviewFixture(t, conn)
	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("building the fixture: %v", err)
	}
	if !relationExists(t, conn, "public.totals") {
		t.Fatal("the fixture did not create the materialized view")
	}
	return conn, m
}

var _ = context.Background

// The rendered SQL states the creation policy, always.
//
// PostgreSQL's default is WITH DATA, so a renderer that dropped the clause
// would still produce a valid statement — and would silently populate a
// relation the declaration asked to leave empty. That is a migration doing
// work nobody asked for, on a view whose whole point may be that computing it
// is expensive.
func TestMatViewSQL_statesTheCreationPolicy(t *testing.T) {
	for _, c := range []struct {
		what     string
		withData bool
		want     string
		notWant  string
	}{
		{"with data", true, "WITH DATA", "WITH NO DATA"},
		{"with no data", false, "WITH NO DATA", ""},
	} {
		t.Run(c.what, func(t *testing.T) {
			stmts, err := migrate.CreateMaterializedView{View: totals(c.withData)}.SQL()
			if err != nil {
				t.Fatalf("rendering: %v", err)
			}
			if len(stmts) != 1 {
				t.Fatalf("rendered %d statements, want one", len(stmts))
			}
			sql := stmts[0]
			if !strings.Contains(sql, "CREATE MATERIALIZED VIEW") {
				t.Errorf("not a materialized view statement: %s", sql)
			}
			if !strings.Contains(sql, c.want) {
				t.Errorf("the statement does not say %s:\n%s", c.want, sql)
			}
			if c.notWant != "" && strings.Contains(sql, c.notWant) {
				t.Errorf("the statement says %s when it should say %s:\n%s", c.notWant, c.want, sql)
			}
			// The body is the developer's SQL, not a quoted string.
			if !strings.Contains(sql, "SELECT id, email FROM users") {
				t.Errorf("the definition was not written through verbatim:\n%s", sql)
			}
		})
	}
}

// A drop never cascades. RESTRICT is PostgreSQL's default and is written out
// anyway, so a reader can see that CASCADE was not chosen.
func TestMatViewSQL_dropIsRestrictAndNeverCascades(t *testing.T) {
	stmts, err := migrate.DropMaterializedView{Schema: "public", Name: "totals"}.SQL()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	sql := strings.Join(stmts, "\n")
	if !strings.Contains(sql, "RESTRICT") {
		t.Errorf("the drop does not say RESTRICT:\n%s", sql)
	}
	if strings.Contains(sql, "CASCADE") {
		t.Errorf("the drop cascades:\n%s", sql)
	}
}
