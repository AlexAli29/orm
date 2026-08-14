package orm

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlexAli29/orm/observe"
	"time"

	"github.com/jackc/pgx/v5"
)

// Transactions.
//
// There is no hidden transaction anywhere in this package. A query runs on the
// executor it was given, and the only way to get several of them onto one
// transaction is to ask for one. What that buys is that reading a function tells
// you its transaction boundaries: nothing widens them from underneath, and
// nothing retries a callback that failed.
//
// The mechanics belong to pgx. This package decides when to commit and when to
// roll back, and leaves savepoints, isolation levels and the wire protocol to
// the driver that owns them.

// Beginner starts a transaction. *pgxpool.Pool, *pgx.Conn and pgx.Tx all
// satisfy it; the last of those begins a savepoint rather than a transaction,
// which is what makes nesting work.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// TxBeginner starts a transaction with explicit characteristics.
//
// It is deliberately a second interface rather than a method on the first,
// because pgx.Tx satisfies Beginner and cannot satisfy this one: a savepoint
// inside a transaction cannot have its own isolation level. Collapsing the two
// would mean either pretending it can or refusing nesting altogether.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// rollbackTimeout bounds the cleanup rollback that follows a failed callback.
//
// The callback often failed because its context was cancelled, and a rollback
// on a cancelled context never reaches PostgreSQL — leaving the transaction
// open until the connection is reused or dropped. Cleanup therefore runs on a
// context that outlives the caller's, but not on one that outlives anything: an
// unbounded rollback would trade a leaked transaction for a stuck goroutine.
const rollbackTimeout = 5 * time.Second

// RunTx runs fn inside a transaction begun on ex.
//
// The callback receives the transaction as an executor. Everything it runs
// through that executor is in the transaction, and everything it runs through
// any other is not — there is no ambient state making the difference invisible.
//
// fn returning nil commits; fn returning an error rolls back and the error is
// returned unchanged, so a caller can still match their own sentinel through
// it. fn panicking rolls back and re-panics with the original value, because a
// panic is a bug and turning it into an error would hide where it came from.
//
// Nothing is retried. A serialization failure is returned like any other error,
// for the caller to decide about: whether retrying is safe depends on what the
// callback did, which is knowledge this package does not have.
//
// Generated code calls this from DB.Tx.
func RunTx(ctx context.Context, ex Executor, fn func(Executor) error) error {
	b, ok := unwrapExecutor(ex).(Beginner)
	if !ok {
		return fmt.Errorf("%w: %T", ErrNoTransaction, ex)
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	return runTx(ctx, tx, tracerOf(ex), fn)
}

// RunTxOptions runs fn inside a transaction begun with the given
// characteristics.
//
// Zero options mean the same thing as [RunTx], including inside another
// transaction: asking for nothing in particular is not a request the driver has
// to refuse. Non-zero options inside a transaction are refused with
// [ErrNestedTxOptions], because PostgreSQL sets isolation and access mode for a
// transaction and not for a savepoint within one — accepting them would report
// success for a change that did not happen.
//
// Generated code calls this from DB.TxOptions.
func RunTxOptions(ctx context.Context, ex Executor, opts pgx.TxOptions, fn func(Executor) error) error {
	if opts == (pgx.TxOptions{}) {
		return RunTx(ctx, ex, fn)
	}
	b, ok := unwrapExecutor(ex).(TxBeginner)
	if !ok {
		if _, nested := unwrapExecutor(ex).(Beginner); nested {
			return fmt.Errorf("%w: %T is already in a transaction, and PostgreSQL sets isolation and access mode"+
				" for the transaction rather than for a savepoint inside it", ErrNestedTxOptions, ex)
		}
		return fmt.Errorf("%w: %T", ErrNoTransaction, ex)
	}
	tx, err := b.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	return runTx(ctx, tx, tracerOf(ex), fn)
}

// runTx drives one transaction to exactly one of its two ends.
// tracer is the tracer the transaction inherits from the executor it was begun
// on, so that a query inside a transaction is traced exactly as one outside it
// would have been.
func runTx(ctx context.Context, tx pgx.Tx, tracer observe.Tracer, fn func(Executor) error) (err error) {
	settled := false
	defer func() {
		if settled {
			return
		}
		// Reached either because fn failed or because it panicked. Both end the
		// transaction, and the rollback happens before the panic resumes so
		// that a panicking callback does not leave one open.
		rerr := rollback(ctx, tx)
		if p := recover(); p != nil {
			panic(p)
		}
		// A rollback that itself failed says something about the connection,
		// which the caller is better off seeing than not. It joins the
		// callback's error rather than replacing it: the callback's is why the
		// transaction ended.
		if rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()

	if err = fn(Traced(tx, tracer)); err != nil {
		return err
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return fmt.Errorf("committing the transaction: %w", cerr)
	}
	settled = true
	return nil
}

// rollback undoes a transaction, on a context of its own.
func rollback(ctx context.Context, tx pgx.Tx) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	// A commit that failed already ended the transaction, so the rollback that
	// follows it finds nothing to undo. That is the expected path rather than a
	// second failure to report.
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("rolling back the transaction: %w", err)
	}
	return nil
}
