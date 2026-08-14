package orm

import (
	"errors"

	"github.com/AlexAli29/orm/internal/expr"
)

// Row locking.
//
// A locking clause is only meaningful inside a transaction: the lock is held
// until the transaction ends, so a statement that locks rows and then commits
// immediately has locked them for no time at all. Run these through
// [RunTx] or the generated Tx, on the repository bound to that transaction.
//
// Three decisions, and they are separate on purpose:
//
//	how strong        FOR KEY SHARE .. FOR UPDATE
//	what to do about a row somebody else holds   wait, fail, or skip
//	which sources     all of them, or the ones you name
//
// None of them is a general synchronisation primitive. SKIP LOCKED is how a
// worker claims rows nobody else is working on; NOWAIT is how a request gives
// up rather than queueing. Neither is a mutex, and using them as one gets the
// surprises that come with a database that is allowed to say no.

// LockStrength is how strongly [Lock] holds the rows a statement returns.
type LockStrength = expr.LockStrength

// The locking strengths, weakest first. Each blocks strictly more than the one
// before it, so the right one is the weakest that answers the question:
//
//	ForKeyShare      blocks deletes and key updates
//	ForShare         blocks every update
//	ForNoKeyUpdate   blocks other writers, allows concurrent foreign-key checks
//	ForUpdateStrong  blocks everything
const (
	ForKeyShare     = expr.LockKeyShare
	ForShare        = expr.LockShare
	ForNoKeyUpdate  = expr.LockNoKeyUpdate
	ForUpdateStrong = expr.LockUpdate
)

// LockOption configures a locking clause.
//
// It is an option rather than a chain of methods so that two that cannot both
// apply — NOWAIT and SKIP LOCKED — are refused rather than silently ordered.
type LockOption interface{ applyLock(*expr.Lock) error }

type lockWaitOpt expr.LockWait

func (o lockWaitOpt) applyLock(l *expr.Lock) error {
	if l.Wait != expr.LockWaitBlock && l.Wait != expr.LockWait(o) {
		return errors.New("NOWAIT and SKIP LOCKED are different answers to the same question; a statement takes one of them")
	}
	l.Wait = expr.LockWait(o)
	return nil
}

// NoWait makes the statement fail rather than wait for a row somebody else has
// locked. PostgreSQL reports it as a lock_not_available error, which arrives as
// a *pgconn.PgError.
var NoWait LockOption = lockWaitOpt(expr.LockNoWait)

// SkipLocked leaves out the rows somebody else has locked rather than waiting
// for them.
//
// It is what a queue worker wants: several of them can run the same statement
// and each gets rows the others did not. What it is not is a guarantee about
// how many rows come back — a worker asking for ten may get three, because
// seven were taken.
var SkipLocked LockOption = lockWaitOpt(expr.LockSkipLocked)

// LockOf names the sources to lock, for a statement that reads several.
//
// Without it PostgreSQL locks every source it can, which for a join means rows
// of tables the caller only wanted to read. Naming them is also how a statement
// with an outer join stays legal: PostgreSQL refuses to lock the nullable side,
// and locking only the side that cannot be absent is the way to ask.
func LockOf(sources ...*Source) LockOption { return lockOfOpt(sources) }

type lockOfOpt []*Source

func (o lockOfOpt) applyLock(l *expr.Lock) error {
	for _, s := range o {
		if s == nil {
			return errors.New("LockOf was given no source")
		}
		if !s.IsTable() {
			return errors.New("only a table can be locked; a derived table or a CTE has no rows of its own to lock")
		}
	}
	l.Of = append(l.Of, o...)
	return nil
}

// buildLock folds the options into a clause.
func buildLock(strength LockStrength, opts []LockOption) (expr.Lock, error) {
	l := expr.Lock{Strength: strength}
	for _, o := range opts {
		if o == nil {
			return l, errors.New("a lock option is missing")
		}
		if err := o.applyLock(&l); err != nil {
			return l, err
		}
	}
	return l, nil
}

// Lock locks the rows the query returns.
//
//	db.Jobs.Query().
//	    Where(Jobs.Status.Eq(Pending)).
//	    OrderBy(Jobs.ID.Asc()).
//	    Limit(10).
//	    Lock(orm.ForUpdateStrong, orm.SkipLocked)
//
// [Query.ForUpdate] remains what it was — FOR UPDATE, waiting, on the root
// table — and this is the form that says which strength and which policy.
func (q *Query[E]) Lock(strength LockStrength, opts ...LockOption) *Query[E] {
	l, err := buildLock(strength, opts)
	if err != nil {
		q.fail(err)
		return q
	}
	q.lock = l
	return q
}

// Lock locks the rows the projection returns.
func (q *SelectQuery[E, R]) Lock(strength LockStrength, opts ...LockOption) *SelectQuery[E, R] {
	l, err := buildLock(strength, opts)
	if err != nil {
		q.fail(err)
		return q
	}
	q.lock = l
	return q
}

// Lock locks the rows the composed query returns.
//
// A statement reading several sources locks all of them unless [LockOf] names
// which — and a derived table or a CTE cannot be locked at all, because the
// rows it produces are not rows of a table.
func (q *ComposedQuery[R]) Lock(strength LockStrength, opts ...LockOption) *ComposedQuery[R] {
	l, err := buildLock(strength, opts)
	if err != nil {
		q.fail(err)
		return q
	}
	q.lock = l
	return q
}
