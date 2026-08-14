package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Execution.
//
// One connection does everything: it takes the advisory lock, reads the history,
// runs the DDL and records the result. That is not an optimisation — a lock held
// on one connection while another runs the migrations is a lock that protects
// nothing, because the second connection is not the one holding it.

// HistoryTable is where applied migrations are recorded.
const (
	HistorySchema = "public"
	HistoryTable  = "orm_schema_migrations"
)

// lockKey is the advisory lock every migrator on a database contends for.
//
// Advisory locks are already scoped to the database, so one constant is enough
// to mean "this database's migrations". It is derived from a fixed string
// rather than written as a number so that its provenance is legible.
var lockKey = int64(binary.BigEndian.Uint64(sha256.New().Sum([]byte("orm.schema_migrations"))[:8]))

// Conn is the single connection a migration run owns.
//
// *pgx.Conn and *pgxpool.Conn both satisfy it. A *pgxpool.Pool deliberately
// does not: a pool hands out a different connection per call, which would put
// the advisory lock somewhere other than the DDL.
type Conn interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// SQLRunner is what a data migration is handed.
//
// It is deliberately narrow, and deliberately not the generated ORM API. A
// migration written today has to keep working after the entity it was written
// against is renamed, moved or deleted — and one that referred to today's
// generated types would stop compiling the moment somebody changed them. SQL
// against the schema as it was at that point in history is the thing that stays
// valid.
//
// pgx.Tx and *pgx.Conn both satisfy it, so an atomic migration's callback runs
// on the transaction and a non-atomic one on the connection.
type SQLRunner interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Migrator applies migrations to one database.
type Migrator struct {
	conn   Conn
	set    *Set
	before func(Step)
	after  func(Step, error)
}

// New binds a migration set to a connection.
func New(conn Conn, set *Set) *Migrator { return &Migrator{conn: conn, set: set} }

// Watch installs callbacks around each step of a run.
//
// It exists because a migration run is the one operation whose progress a
// person genuinely needs to see as it happens: a plan computed beforehand
// cannot say which step the database is on when it stops responding. Either
// callback may be nil.
func (m *Migrator) Watch(before func(Step), after func(Step, error)) {
	m.before, m.after = before, after
}

// EnsureHistory creates the history table if it is not there.
//
// It runs under the advisory lock, so two migrators starting at once cannot
// both create it and cannot see a half-created one. IF NOT EXISTS alone would
// not be enough: two sessions racing on it can deadlock, which is exactly the
// failure this is meant to prevent.
func (m *Migrator) EnsureHistory(ctx context.Context) error {
	_, err := m.conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+historyTable()+` (
		migration_id text PRIMARY KEY,
		checksum     text NOT NULL,
		applied_at   timestamptz NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return fmt.Errorf("creating the migration history table: %w", err)
	}
	return nil
}

// Applied reads the migration history, in application order.
//
// A database with no history table has applied nothing, which is a different
// statement from "this failed": every read-only command — a plan, a listing, a
// check — has to work on a database nothing has migrated yet, and creating a
// table to discover that would make those commands write.
func (m *Migrator) Applied(ctx context.Context) ([]Applied, error) {
	var present *string
	if err := m.conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, HistorySchema+"."+HistoryTable).Scan(&present); err != nil {
		return nil, fmt.Errorf("looking for the migration history table: %w", err)
	}
	if present == nil {
		return nil, nil
	}

	rows, err := m.conn.Query(ctx,
		`SELECT migration_id, checksum FROM `+historyTable()+` ORDER BY applied_at, migration_id`)
	if err != nil {
		return nil, fmt.Errorf("reading the migration history: %w", err)
	}
	defer rows.Close()

	var out []Applied
	for rows.Next() {
		var a Applied
		if err := rows.Scan(&a.ID, &a.Checksum); err != nil {
			return nil, fmt.Errorf("reading the migration history: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the migration history: %w", err)
	}
	return out, nil
}

// Plan computes what a run to target would do, without changing anything.
//
// It is read-only down to the history table: a plan is what somebody runs to
// find out whether to run anything, and a command that answered by creating a
// table would be a command nobody could run against production to look.
func (m *Migrator) Plan(ctx context.Context, target string) (Plan, error) {
	return m.withLock(ctx, func(ctx context.Context) (Plan, error) {
		applied, err := m.Applied(ctx)
		if err != nil {
			return Plan{}, err
		}
		return PlanTarget(m.set, applied, target)
	})
}

// Migrate moves the database to target, which is the latest migration when
// empty.
//
// The whole plan is computed and validated before the first statement runs, so
// a target that cannot be reached — because a migration was edited after it was
// applied, or because reaching it would mean reversing something irreversible —
// fails without having changed anything.
func (m *Migrator) Migrate(ctx context.Context, target string) (Plan, error) {
	return m.withLock(ctx, func(ctx context.Context) (Plan, error) {
		if err := m.EnsureViewState(ctx); err != nil {
			return Plan{}, err
		}
		if err := m.EnsureHistory(ctx); err != nil {
			return Plan{}, err
		}
		applied, err := m.Applied(ctx)
		if err != nil {
			return Plan{}, err
		}
		plan, err := PlanTarget(m.set, applied, target)
		if err != nil {
			return Plan{}, err
		}
		for _, step := range plan.Steps {
			if err := m.run(ctx, step); err != nil {
				return plan, err
			}
		}
		return plan, nil
	})
}

// MarkApplied records migrations as applied without running them.
//
// It is how a database that already has the schema enters the managed
// workflow: the baseline migration describes what is already there, so running
// it would try to create tables that exist. The history still gets a real row
// with a real checksum, so every later check behaves exactly as it would have
// if the migration had run.
//
// Nothing calls this on its own. Recording a migration as done when it was not
// is a claim about a database that only the person making it can justify, so it
// is always something somebody asked for.
func (m *Migrator) MarkApplied(ctx context.Context, ids ...string) (Plan, error) {
	return m.withLock(ctx, func(ctx context.Context) (Plan, error) {
		if err := m.EnsureHistory(ctx); err != nil {
			return Plan{}, err
		}
		applied, err := m.Applied(ctx)
		if err != nil {
			return Plan{}, err
		}
		// The history still has to make sense afterwards, so it is validated
		// the way a real run would validate it.
		if err := validateHistory(m.set, applied); err != nil {
			return Plan{}, err
		}
		known := make(map[string]bool, len(applied))
		for _, a := range applied {
			known[a.ID] = true
		}

		var plan Plan
		for _, id := range ids {
			mig, ok := m.set.Get(id)
			if !ok {
				return Plan{}, &ErrUnknownTarget{Target: id}
			}
			if known[id] {
				continue
			}
			sum, _ := m.set.Checksum(id)
			step := Step{Migration: mig, Direction: Forward, Atomic: true, Checksum: sum}
			if err := m.record(ctx, m.conn, step); err != nil {
				return plan, err
			}
			plan.Steps = append(plan.Steps, step)
		}
		return plan, nil
	})
}

// withLock runs fn while holding the migration advisory lock.
//
// The lock is released on the way out whatever happened, and would be released
// by the server anyway if the process died — which is the property that makes
// an advisory lock the right tool here and a row in a table the wrong one.
func (m *Migrator) withLock(ctx context.Context, fn func(context.Context) (Plan, error)) (Plan, error) {
	// Waiting for the lock is the one place a migration blocks on somebody
	// else. A caller that gives up while waiting gets its context's error back
	// — and, because pgx cannot leave a cancelled query half-read on a shared
	// connection, that connection is closed with it. Reconnect before trying
	// again; the command-line tool opens one per run, so this only concerns a
	// program that keeps a connection of its own.
	if _, err := m.conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		// The usual cause is another migrator holding it, not a database that
		// is down — and reporting it as the latter would send somebody to check
		// the wrong thing.
		if ctx.Err() != nil {
			return Plan{}, fmt.Errorf("the migration lock could not be acquired before the deadline;"+
				" another migration is probably still running: %w", err)
		}
		return Plan{}, fmt.Errorf("acquiring the migration lock: %w", err)
	}
	defer func() {
		// The unlock runs on a context of its own: the usual reason fn failed
		// is that the caller's context was cancelled, and an unlock on a
		// cancelled context never reaches the server.
		release, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		_, _ = m.conn.Exec(release, "SELECT pg_advisory_unlock($1)", lockKey)
	}()
	return fn(ctx)
}

// unlockTimeout bounds the release that follows a failed run.
const unlockTimeout = 5_000_000_000 // 5s, in nanoseconds

// run executes one step and records it.
func (m *Migrator) run(ctx context.Context, step Step) error {
	if m.before != nil {
		m.before(step)
	}
	var err error
	if step.Atomic {
		err = m.runAtomic(ctx, step)
	} else {
		err = m.runNonAtomic(ctx, step)
	}
	if m.after != nil {
		m.after(step, err)
	}
	return err
}

// runAtomic executes a step and records it in one transaction.
//
// Either the whole migration and its history row are there, or neither is.
// Recording the history inside the same transaction is what makes that true:
// a separate write could succeed after the DDL rolled back, leaving history
// claiming something that did not happen.
func (m *Migrator) runAtomic(ctx context.Context, step Step) (err error) {
	tx, err := m.conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migration %s: beginning a transaction: %w", step.Migration.ID, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollback, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
		defer cancel()
		if rerr := tx.Rollback(rollback); rerr != nil && !errors.Is(rerr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("migration %s: rolling back: %w", step.Migration.ID, rerr))
		}
	}()

	for i, op := range step.Operations {
		if err := execOperation(ctx, tx, op); err != nil {
			return &ErrExecution{ID: step.Migration.ID, Index: i, Operation: op.Describe(), Atomic: true, Err: err}
		}
	}
	if err := m.record(ctx, tx, step); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migration %s: committing: %w", step.Migration.ID, err)
	}
	committed = true
	return nil
}

// runNonAtomic executes a step outside a transaction.
//
// This is what CREATE INDEX CONCURRENTLY needs, and it comes with the honest
// consequence: an operation that fails after earlier ones succeeded leaves
// those applied. Nothing here pretends otherwise — the error says which
// operation failed and how many ran before it, and the migration is not
// recorded, so a rerun starts from the beginning of it.
func (m *Migrator) runNonAtomic(ctx context.Context, step Step) error {
	for i, op := range step.Operations {
		if err := execOperation(ctx, m.conn, op); err != nil {
			return &ErrExecution{
				ID: step.Migration.ID, Index: i, Operation: op.Describe(),
				Atomic: false, Completed: i, Err: err,
			}
		}
	}
	return m.record(ctx, m.conn, step)
}

// record writes or removes the history row for a step.
func (m *Migrator) record(ctx context.Context, ex SQLRunner, step Step) error {
	if step.Direction == Reverse {
		_, err := ex.Exec(ctx, `DELETE FROM `+historyTable()+` WHERE migration_id = $1`, step.Migration.ID)
		if err != nil {
			return fmt.Errorf("migration %s: removing its history row: %w", step.Migration.ID, err)
		}
		return nil
	}
	_, err := ex.Exec(ctx,
		`INSERT INTO `+historyTable()+` (migration_id, checksum) VALUES ($1, $2)`,
		step.Migration.ID, step.Checksum)
	if err != nil {
		return fmt.Errorf("migration %s: recording it as applied: %w", step.Migration.ID, err)
	}
	return nil
}

// execOperation runs one operation's statements, or its Go function.
func execOperation(ctx context.Context, ex SQLRunner, op Operation) error {
	if run, ok := op.(RunFunc); ok {
		if run.Up == nil {
			return fmt.Errorf("data migration %q has no function", run.Name)
		}
		return run.Up(ctx, ex)
	}
	statements, err := op.SQL()
	if err != nil {
		return err
	}
	for _, stmt := range statements {
		if _, err := ex.Exec(ctx, stmt); err != nil {
			return err
		}
	}
	return recordViewProvenance(ctx, ex, op)
}

// recordViewProvenance records what the server made of a view this operation
// just created, replaced or dropped.
//
// It runs on the same executor the DDL ran on, which for an atomic migration is
// the transaction. That is the whole point: the relation and the record of what
// it was are applied or abandoned together, so a migration that fails later
// cannot leave provenance for a view that does not exist — or, worse, leave a
// view whose recorded body says something else.
func recordViewProvenance(ctx context.Context, ex SQLRunner, op Operation) error {
	w, ok := ex.(ViewWriter)
	if !ok {
		// The runner cannot read rows back, which no production path produces.
		// Recording nothing is the safe answer: a later check reports the body
		// as unverified rather than as matching something never read.
		return nil
	}
	switch o := op.(type) {
	case CreateView:
		return RecordView(ctx, w, o.View.Schema, o.View.Name, "view",
			string(o.View.Definition.Identity()))
	case ReplaceView:
		return RecordView(ctx, w, o.View.Schema, o.View.Name, "view",
			string(o.View.Definition.Identity()))
	case DropView:
		return ForgetView(ctx, w, o.Schema, o.Name)
	case CreateMaterializedView:
		return RecordView(ctx, w, o.View.Schema, o.View.Name, "materialized view",
			string(o.View.Definition.Identity()))
	case DropMaterializedView:
		return ForgetView(ctx, w, o.Schema, o.Name)
	}
	return nil
}

// historyTable is the quoted, qualified name of the history table.
func historyTable() string { return qualified(HistorySchema, HistoryTable) }

// ErrExecution reports a migration that failed while running.
//
// It carries what ran and what did not, because the answer differs by mode:
// an atomic migration left nothing behind, and a non-atomic one left everything
// before the failure.
type ErrExecution struct {
	ID        string
	Index     int
	Operation string
	Atomic    bool
	// Completed is how many operations succeeded before the failure, and is
	// meaningful only for a non-atomic migration.
	Completed int
	Err       error
}

func (e *ErrExecution) Error() string {
	if e.Atomic {
		return fmt.Sprintf("migration %s failed at operation %d (%s); the transaction was rolled back and the migration is not recorded: %v",
			e.ID, e.Index+1, e.Operation, e.Err)
	}
	return fmt.Sprintf("migration %s failed at operation %d (%s) and runs outside a transaction:"+
		" the %d operation(s) before it are still applied and the migration is not recorded, so the database is in a state no migration describes: %v",
		e.ID, e.Index+1, e.Operation, e.Completed, e.Err)
}

// Unwrap keeps PostgreSQL's own error reachable, so a caller can still match a
// *pgconn.PgError through everything this type adds.
func (e *ErrExecution) Unwrap() error { return e.Err }
