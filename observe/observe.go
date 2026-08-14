// Package observe is the ORM's tracing contract.
//
// It is types and one interface, and it depends on nothing but the standard
// library. That is deliberate: an ORM that imported a telemetry library would
// make every project that uses it depend on that library's version, its
// transitive tree and its opinions. Adapters for OpenTelemetry, Prometheus and
// the rest belong in packages somebody can choose.
//
// The one rule worth stating before anything else: a start event never carries
// bound argument values. Not by convention, not by an option somebody can turn
// off — the field does not exist. An ORM's tracing sees every query a program
// runs, so a tracer that received the values would put every password, token
// and address a program handles into whatever the tracer writes to. The SQL is
// there with its placeholders, because WHERE email = $1 is useful and says
// nothing about who.
//
// The exception is SQL the caller wrote. This package cannot redact a literal
// out of a raw statement without parsing SQL, and building a SQL parser to do
// it would be building the thing the ORM exists not to have. [StartEvent.Raw]
// says which statements those are, so a tracer that needs to be careful can be.
package observe

import (
	"context"
	"time"
)

// Tracer receives an event when an ORM operation starts and when it ends.
//
// The context returned by Start is passed to whatever the operation does and
// back to End, so a tracer can carry a span through it. Returning ctx unchanged
// is a complete implementation.
//
// A tracer's methods are called from whatever goroutine ran the query, and one
// tracer may be shared by many. Making that safe is the tracer's job; this
// package introduces no serialisation of its own, because a mutex around every
// query in a program that does not need one is a cost nobody asked for.
//
// If a tracer panics, the panic propagates. It is not recovered and turned into
// an error: a panic in a callback is a bug in the callback, and hiding it would
// make it a bug nobody finds.
type Tracer interface {
	Start(ctx context.Context, e StartEvent) context.Context
	End(ctx context.Context, e EndEvent)
}

// Op is what the ORM was asked to do.
//
// It is the ORM's own vocabulary rather than SQL's: one Op can be several
// statements, because loading a query's relations is one thing a caller asked
// for and three statements the server sees.
type Op string

// The operations this package reports.
const (
	// OpQuery is a read that buffers: All, One, Count, Exists.
	OpQuery Op = "query"
	// OpStream is a read that streams, which ends when the caller stops
	// reading rather than when the server stops sending.
	OpStream Op = "stream"
	// OpInsert, OpUpdate and OpDelete are the writes.
	OpInsert Op = "insert"
	OpUpdate Op = "update"
	OpDelete Op = "delete"
	// OpCopy is a COPY.
	OpCopy Op = "copy"
	// OpRelation is one relation-loading statement issued on behalf of a
	// query that asked for it.
	OpRelation Op = "relation"
	// OpRefresh is REFRESH MATERIALIZED VIEW. It is one operation whatever the
	// view costs to rebuild: the statement names a relation, carries no bind
	// values, and produces no rows, so there is nothing per-row to instrument
	// and nothing in it that could carry a value into a log.
	OpRefresh Op = "refresh"
	// OpExplain is EXPLAIN; OpExplainAnalyze is the one that executes.
	OpExplain        Op = "explain"
	OpExplainAnalyze Op = "explain_analyze"
)

// StartEvent describes an operation about to run.
//
// Everything in it is structure. There is no field for argument values, and
// adding one would defeat the point of the rest.
type StartEvent struct {
	// Op is what the ORM was asked to do.
	Op Op

	// SQL is the statement with its placeholders, as the server will see it.
	//
	// It is here because it is the most useful single thing a tracer can have
	// and it carries no values — every value this package sends is a bind
	// parameter. For a raw statement it is the caller's own SQL, which may
	// contain anything the caller wrote; see Raw.
	SQL string

	// Args is how many arguments the statement binds. The values are not here
	// and there is no field for them.
	Args int

	// Fingerprint identifies the statement's structure, as "v1:" and a hex
	// digest. It is the identity to group by.
	Fingerprint string

	// Entity is the Go type the operation reads or writes, when there is one.
	Entity string
	// Table is the relation it is rooted at, schema-qualified.
	Table string

	// Relation is the path of the relation being loaded, for OpRelation:
	// "Posts", or "Posts.Comments" for one nested inside another. It is empty
	// for everything else.
	Relation string

	// Columns are the columns a COPY sends, in order. It is empty for
	// everything else.
	Columns []string

	// Raw reports that the SQL was written by the caller rather than built by
	// the ORM.
	//
	// It matters for exactly one reason: this package guarantees that no bound
	// argument reaches a tracer, and it cannot guarantee anything about
	// literals inside SQL somebody else wrote. A tracer that exports SQL should
	// treat a raw statement as text of unknown sensitivity.
	Raw bool

	// InTransaction reports that the operation runs inside a transaction this
	// package started, when that is known from the executor. It is not a
	// transaction identifier: there is no honest one to give, and a pointer
	// address is not an identifier.
	InTransaction bool

	// StartedAt is when the operation began.
	StartedAt time.Time
}

// EndEvent describes an operation that has finished.
type EndEvent struct {
	// Op is what it was, repeated so that a tracer that keys on nothing else
	// still has it.
	Op Op
	// Fingerprint identifies the statement, repeated for the same reason.
	Fingerprint string

	// Duration is how long the operation took.
	//
	// What that measures depends on the operation, and the difference is worth
	// knowing. For a buffered read it is the whole thing: sending the
	// statement, waiting, and scanning every row into its destination. For a
	// stream it is the lifetime of the iteration — which includes however long
	// the caller spent between one row and the next, because the rows arrive as
	// they are asked for. A slow stream is not necessarily a slow query.
	Duration time.Duration

	// Rows is how many rows the operation read or affected, and RowsKnown says
	// whether that number means anything.
	//
	// Nothing here counts rows the operation did not already count. A COPY
	// reports what the driver returned; a stream reports how many the caller
	// took; an EXPLAIN reports nothing, because the rows it returned were a
	// plan.
	Rows      int64
	RowsKnown bool

	// Err is what went wrong, or nil.
	//
	// It is the error the caller receives, unwrapped by nothing. A PostgreSQL
	// error carries the server's message, and the server's message can quote a
	// value from the row that caused it — a unique violation names the key. A
	// tracer that exports errors verbatim is exporting that; [Classify] is the
	// summary that does not.
	Err error

	// Cancelled reports that the operation ended because its context did.
	Cancelled bool
}

// ErrorInfo is what an error can be reported as without repeating what the
// server said.
//
// PostgreSQL's messages are helpful precisely because they quote the data —
// "Key (email)=(someone@example.com) already exists" — so a tracer that wants to
// record failures without recording data wants this rather than the message.
type ErrorInfo struct {
	// Failed reports that there was an error at all.
	Failed bool
	// SQLState is PostgreSQL's five-character code, when the error came from
	// the server.
	SQLState string
	// Kind is a short class: "unique_violation", "foreign_key_violation",
	// "cancelled", "other".
	Kind string
	// Table and Constraint name what the server was working on, when it said.
	// They are identifiers rather than data.
	Table      string
	Constraint string
	Column     string
}
