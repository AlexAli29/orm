package orm

import (
	"errors"

	"github.com/AlexAli29/orm/internal/expr"
)

// The sentinels a caller branches on.
//
// They are wrapped, never replaced, so errors.Is keeps working through the
// context each layer adds. Nobody should have to match on an error string to
// tell "no such user" from "the database is down".
var (
	// ErrNotFound reports that a query expecting one row found none.
	ErrNotFound = errors.New("orm: not found")

	// ErrMultipleRows reports that a query expecting one row found more.
	//
	// Returning the first row instead would be the friendlier-looking choice
	// and the wrong one: a query that matches two rows is a query whose author
	// believed something about the data that is not true, and silently picking
	// one hides that until it matters.
	ErrMultipleRows = errors.New("orm: multiple rows")

	// ErrRawPlaceholder reports a raw fragment whose placeholders and
	// arguments do not agree.
	ErrRawPlaceholder = expr.ErrRawPlaceholder

	// ErrMissingWhere reports an update or delete that named no rows.
	//
	// The absence of a WHERE clause is also what a forgotten one looks like,
	// and a full-table write is among the few mistakes that cannot be undone.
	// Call All to say every row was meant.
	ErrMissingWhere = errors.New("orm: missing WHERE clause; call All() to write every row deliberately")

	// ErrMissingSet reports an update that assigns nothing.
	ErrMissingSet = errors.New("orm: update assigns nothing; call Set()")

	// ErrDuplicateAssignment reports one column assigned more than once.
	ErrDuplicateAssignment = errors.New("orm: column assigned more than once")

	// ErrInvalidDefault reports a Default for a column PostgreSQL cannot
	// supply a value for.
	ErrInvalidDefault = errors.New("orm: column has no database default")

	// ErrConflictIgnored reports an insert that PostgreSQL discarded because
	// it conflicted and the conflict clause said to do nothing.
	//
	// No row comes back from such an insert, so there is no entity to return.
	// Fetching the row that was already there would be a query nobody asked
	// for, and returning a zero entity with no error would look like a row
	// that was written.
	ErrConflictIgnored = errors.New("orm: insert ignored because the row conflicted")

	// ErrStreamingRelation reports a relation that cannot be streamed.
	//
	// A to-many relation is loaded in a statement of its own, which needs every
	// root row before it can run. Answering it during a stream would mean
	// buffering the whole result, which is the one thing streaming exists to
	// avoid, so it is refused instead of quietly becoming All.
	ErrStreamingRelation = errors.New("orm: relation cannot be streamed")

	// ErrNoTransaction reports an executor that cannot begin a transaction.
	//
	// A *pgxpool.Pool, a *pgx.Conn and a pgx.Tx all can. Anything else — a stub
	// in a test, a wrapper that forwards only Query — cannot, and saying so
	// beats running the callback outside the transaction it asked for.
	ErrNoTransaction = errors.New("orm: executor cannot begin a transaction")

	// ErrNestedTxOptions reports transaction characteristics requested inside a
	// transaction.
	//
	// PostgreSQL sets isolation level and access mode for a transaction, not for
	// a savepoint within one. A nested request for them cannot be honoured, and
	// honouring it in appearance only — running the callback under the outer
	// transaction's isolation while reporting success — is the failure mode this
	// error exists to prevent.
	ErrNestedTxOptions = errors.New("orm: transaction options cannot be set on a nested transaction")

	// ErrRawColumns reports a raw statement that does not return the columns the
	// entity needs.
	//
	// [Raw] scans by position into the generated destinations, so the statement
	// has to select every mapped column of the entity in the order the entity
	// declares them. The count is checked before any row is read.
	ErrRawColumns = errors.New("orm: raw statement returns the wrong number of columns")
)

// ScopeError reports a predicate or ordering term referring to a table the
// query does not select from.
//
// It is an alias for the compiler's own type so that errors.As reaches the same
// value the compiler produced, with the sources it saw.
type ScopeError = expr.ScopeError

// AliasCollisionError reports two occurrences of a table claiming one alias in
// a single query.
type AliasCollisionError = expr.AliasCollisionError
