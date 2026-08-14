package orm

import (
	"context"
	"errors"
	"time"

	"github.com/AlexAli29/orm/observe"
	"github.com/jackc/pgx/v5"
)

// Attaching a tracer.
//
// The tracer travels with the executor rather than with the repository, which
// settles three things at once. Generated code does not change, because it takes
// an executor and always has. A transaction inherits it, because a transaction
// is an executor built from one. And an application that traces some connections
// and not others can, because it decides which executor it hands over.
//
// This is ORM-level tracing: one event for one thing a caller asked for. pgx has
// its own tracer for the wire, and the two do not replace each other — a query
// that loads two relations is one ORM operation and three pgx queries, and both
// numbers are worth having. Nothing here touches pgx's ConnConfig.Tracer, so an
// application already using it keeps it.

// Traced returns an executor that reports ORM operations to the tracer.
//
//	pool, err := pgxpool.NewWithConfig(ctx, cfg)
//	if err != nil {
//	    return err
//	}
//	db := domain.New(orm.Traced(pool, myTracer))
//
// A nil tracer returns the executor unchanged, so a program can decide at
// startup without branching everywhere afterwards.
//
// It composes with pgx's own tracing rather than competing with it: pgx sees the
// statements, this sees the operations, and an ORM call that loads relations
// produces one of the second and several of the first.
func Traced(ex Executor, t observe.Tracer) Executor {
	if t == nil {
		return ex
	}
	return tracedExecutor{Executor: ex, tracer: t}
}

// tracedExecutor is an executor carrying a tracer.
//
// It embeds rather than wraps the Query method so that an executor which is also
// something else — a pgx.Tx, which the transaction helpers type-assert for — is
// still recognisable through it.
type tracedExecutor struct {
	Executor
	tracer observe.Tracer
}

// tracerOf returns the tracer an executor carries, if any.
func tracerOf(ex Executor) observe.Tracer {
	for {
		t, ok := ex.(tracedExecutor)
		if !ok {
			return nil
		}
		return t.tracer
	}
}

// Unwrap returns the executor underneath any tracing wrapper.
//
// Attaching a tracer is the ordinary production shape, and it must not stop
// anything asking what the executor really is. The COPY path needs the answer to
// find a connection that can COPY; an operational check needs it to read a
// pool's statistics. Without this, wrapping a pool in a tracer silently turns it
// into something neither can recognise.
//
// It unwraps tracing and nothing else: what comes back is whatever was passed to
// [Traced], with its own type intact.
func Unwrap(ex Executor) Executor { return unwrapExecutor(ex) }

// unwrapExecutor returns the executor underneath any tracing wrapper, which is
// what the transaction helpers need when they ask whether an executor is a pool
// or a transaction.
func unwrapExecutor(ex Executor) Executor {
	for {
		t, ok := ex.(tracedExecutor)
		if !ok {
			return ex
		}
		ex = t.Executor
	}
}

// inTransaction reports whether the executor is a transaction, which is
// knowable without inventing anything: pgx's own types say so.
func inTransaction(ex Executor) bool {
	_, ok := unwrapExecutor(ex).(pgx.Tx)
	return ok
}

// span is one traced operation in progress.
//
// The zero span traces nothing and costs a nil check, which is what an
// untraced program pays.
type span struct {
	tracer observe.Tracer
	ctx    context.Context
	op     observe.Op
	fp     string
	start  time.Time
}

// startSpan begins tracing an operation, or does nothing when there is no
// tracer.
//
// The statement is compiled by the caller and passed in rather than compiled
// here: an untraced program must not pay for building an event it will not send,
// and a traced one must see the same SQL that runs.
func startSpan(ctx context.Context, ex Executor, e observe.StartEvent) (context.Context, span) {
	t := tracerOf(ex)
	if t == nil {
		return ctx, span{}
	}
	e.StartedAt = time.Now()
	e.InTransaction = inTransaction(ex)
	if e.Fingerprint == "" && e.SQL != "" {
		e.Fingerprint = fingerprintOf(sqlKind(e.SQL), e.SQL).String()
	}
	ctx = t.Start(ctx, e)
	return ctx, span{tracer: t, ctx: ctx, op: e.Op, fp: e.Fingerprint, start: e.StartedAt}
}

// end reports the operation's outcome.
//
// rows is meaningful only when known is true: nothing here counts rows the
// operation did not already count.
func (s span) end(err error, rows int64, known bool) {
	if s.tracer == nil {
		return
	}
	s.tracer.End(s.ctx, observe.EndEvent{
		Op:          s.op,
		Fingerprint: s.fp,
		Duration:    time.Since(s.start),
		Rows:        rows,
		RowsKnown:   known,
		Err:         err,
		Cancelled:   errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded),
	})
}

// tracing reports whether an executor carries a tracer, so that a caller can
// skip building an event nobody will receive.
func tracing(ex Executor) bool { return tracerOf(ex) != nil }
