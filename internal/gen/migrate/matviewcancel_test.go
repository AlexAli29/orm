package migrate_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// cancelFixture builds one database the whole case shares.
//
// connect() makes a fresh database per call, and a cancellation has to be
// observed from a second connection to the same one — the connection the
// migration was cancelled on is not necessarily reusable.
func cancelFixture(t *testing.T) (string, *schema.Schema) {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "")
	return dsn, &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "users",
		Columns: []schema.Column{
			{Name: "id", Type: schema.Type{Name: "int8"}},
			{Name: "email", Type: schema.Type{Name: "text"}},
		},
		PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
	}}}
}

// openTo opens a connection to a database this test already made.
func openTo(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

// M16.5 adversarial audit: cancellation during a materialized-view migration.
//
// G1 recorded this as a known limitation, and the reason it stayed open is that
// most of the interesting boundaries are not reachable from outside the process.
// What can be injected deterministically is a statement that blocks: put
// pg_sleep where the boundary is and cancel during it, and the cancellation lands
// exactly there every time.
//
// The invariant is the one every other fault shares, stated for interruption
// rather than failure: either the work never started, or the transaction rolled
// back completely. A partially committed materialized view — the relation without
// its indexes, either without provenance, any of it without a history row — is
// the state that must be impossible, because nothing afterwards would notice.
//
// The caller's error must still be the cancellation. A rollback that replaced
// context.Canceled with a message of its own would make a deadline look like a
// database fault, and the retry logic a caller writes would be wrong.

// sleeper is an operation that blocks inside the transaction, so a cancellation
// can be aimed at a boundary rather than at whatever happened to be slow.
func sleeper(seconds string, description string) migrate.Operation {
	return migrate.RawSQL{
		Up: "SELECT pg_sleep(" + seconds + ")", Atomic: true,
		Description: description,
	}
}

// cancelDuring runs a migration and cancels it while the sleeping operation is
// in flight, returning the error the caller saw.
func cancelDuring(t *testing.T, dsn string, set *migrate.Set, after time.Duration) error {
	t.Helper()
	m := migrate.New(openTo(t, dsn), set)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(after)
		cancel()
	}()
	_, err := m.Migrate(ctx, "")
	cancel()
	return err
}

// requireCancellation checks the caller kept the cancellation rather than a
// rewrite of it.
//
// pgx reports a query interrupted by a cancelled context either as the context
// error or as PostgreSQL's own 57014 query_canceled, depending on how far the
// statement got. Both are the truth; a message that is neither is not.
func requireCancellation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the migration completed although its context was cancelled")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "57014" {
		return
	}
	t.Errorf("the caller's error is neither the cancellation nor PostgreSQL's own "+
		"query_canceled: %v", err)
}

// nothingRemains asserts no part of the materialized view was committed.
func nothingRemains(t *testing.T, dsn string, set *migrate.Set) {
	t.Helper()
	// A fresh connection, because the cancelled one may be unusable.
	conn := openTo(t, dsn)
	for _, rel := range []string{"public.totals", "public.totals_id_key", "public.totals_email_idx"} {
		if relationExists(t, conn, rel) {
			t.Errorf("%s was committed by a cancelled migration", rel)
		}
	}
	if _, ok := provenanceOf(t, conn, "public.totals"); ok {
		t.Error("provenance was committed by a cancelled migration")
	}
	ids := historyIDs(t, migrate.New(conn, set))
	for _, id := range ids {
		if id == "0002_totals" {
			t.Error("a cancelled migration was recorded as applied")
		}
	}
}

// The boundaries. Each places the sleep where the cancellation should land.
func TestMatViewCancel_everyReachableBoundaryLeavesNothing(t *testing.T) {
	for _, c := range []struct {
		boundary string
		ops      func() []migrate.Operation
	}{
		{
			// Before any DDL: the sleep is the first operation, so the
			// cancellation arrives before the relation is created.
			"before the first DDL",
			func() []migrate.Operation {
				return []migrate.Operation{
					sleeper("30", "block before any DDL"),
					migrate.CreateMaterializedView{View: totals(true)},
					migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
				}
			},
		},
		{
			// Between the relation and its first index.
			"between the relation and its index",
			func() []migrate.Operation {
				return []migrate.Operation{
					migrate.CreateMaterializedView{View: totals(true)},
					sleeper("30", "block between the relation and its index"),
					migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
				}
			},
		},
		{
			// Between two indexes: one is built, one is not.
			"between two indexes",
			func() []migrate.Operation {
				return []migrate.Operation{
					migrate.CreateMaterializedView{View: totals(true)},
					migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
					sleeper("30", "block between two indexes"),
					migrate.CreateIndex{Schema: "public", Table: "totals", Index: indexOn("totals_email_idx", "email")},
				}
			},
		},
		{
			// After every operation, so provenance has been written and the
			// history row has not: the last boundary at which a rollback still
			// has everything to undo.
			"after the last operation and before the history row",
			func() []migrate.Operation {
				return []migrate.Operation{
					migrate.CreateMaterializedView{View: totals(true)},
					migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
					sleeper("30", "block before the history row is written"),
				}
			},
		},
	} {
		t.Run(c.boundary, func(t *testing.T) {
			dsn, b := cancelFixture(t)
			set := newSet(t,
				migrationFor(t, "0001_initial", &schema.Schema{}, b),
				&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"},
					Atomic: true, Operations: c.ops()},
			)
			requireCancellation(t, cancelDuring(t, dsn, set, 700*time.Millisecond))
			nothingRemains(t, dsn, set)
		})
	}
}

// A deadline is the same story told by the clock.
func TestMatViewCancel_aDeadlineIsAlsoCompletelyRolledBack(t *testing.T) {
	dsn, b := cancelFixture(t)
	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, b),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				sleeper("30", "outlive the deadline"),
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	_, err := migrate.New(openTo(t, dsn), set).Migrate(ctx, "")
	requireCancellation(t, err)
	nothingRemains(t, dsn, set)
}

// And a cancelled migration is still applicable afterwards.
//
// A rollback nobody can recover from is not a rollback, and a cancellation is the
// most ordinary way for a migration to stop: somebody pressed ^C, or a deploy
// timed out.
func TestMatViewCancel_theMigrationStillAppliesAfterwards(t *testing.T) {
	dsn, b := cancelFixture(t)
	cancelled := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, b),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				sleeper("30", "be interrupted"),
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	requireCancellation(t, cancelDuring(t, dsn, cancelled, 700*time.Millisecond))

	// The same migration without the blocking operation: a fresh connection,
	// because the cancelled one is not necessarily reusable.
	fresh := openTo(t, dsn)
	good := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, b),
		&migrate.Migration{ID: "0002_totals", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.CreateMaterializedView{View: totals(true)},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
			}},
	)
	if _, err := migrate.New(fresh, good).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("the migration did not apply after being cancelled: %v", err)
	}
	if !relationExists(t, fresh, "public.totals") {
		t.Error("the retry did not create the relation")
	}
	if _, ok := provenanceOf(t, fresh, "public.totals"); !ok {
		t.Error("the retry left no provenance")
	}
	ids := historyIDs(t, migrate.New(fresh, good))
	if len(ids) != 2 {
		t.Errorf("history = %v, want both migrations exactly once", ids)
	}
	for _, id := range ids {
		if strings.Count(strings.Join(ids, " "), id) != 1 {
			t.Errorf("%s appears more than once in %v", id, ids)
		}
	}
}
