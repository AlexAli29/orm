// Package ormotel reports the ORM's operations as OpenTelemetry spans.
//
// It is an adapter over the ORM's observability model, not a replacement for
// it: it implements observe.Tracer, receives the events the ORM already emits,
// and turns each into a span. Installing it changes nothing about what a query
// does or what it returns.
//
//	db := domain.New(orm.Traced(pool, ormotel.New(otel.Tracer("myapp/db"))))
//
// # It is a module of its own
//
// OpenTelemetry is a large dependency and most projects using the ORM do not
// want it. This package is therefore a separate Go module: a project that never
// imports it never compiles OpenTelemetry and never has it in its dependency
// graph. That is the whole reason for the extra go.mod.
//
// # It cannot export a bind value
//
// Not "does not" — cannot. The events it receives have no field for argument
// values, so there is nothing here to forward. What goes on a span is the
// statement with its placeholders, the fingerprint, the table, and the counts.
//
// The exceptions are the ones the ORM documents everywhere: SQL written by a
// caller inside a raw statement may contain any literal its author put there,
// and PostgreSQL writes contextual detail into its own error messages. Raw
// statements are marked on the event so [WithRawSQL] can leave their SQL off,
// and errors are recorded by classification rather than by message unless asked.
//
// # It never attaches a plan
//
// An EXPLAIN produces a plan, and a plan contains the constants PostgreSQL
// planned with. Nothing here puts one on a span. An explain operation gets a
// span saying it happened; the plan goes back to the caller, which is the only
// place that knows whether it may be exported.
//
// # It coexists with pgx's own tracing
//
// This is an ORM-level span: one operation as the application asked for it,
// which may be several statements when relations are loaded. pgx's tracing is
// at the wire level and is configured on the connection. This package touches
// neither ConnConfig.Tracer nor anything else pgx owns, so a project already
// instrumenting pgx keeps both layers and can see which is which.
package ormotel

import (
	"context"

	"github.com/AlexAli29/orm/observe"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Attribute keys.
//
// The ones OpenTelemetry's semantic conventions define are spelled the way the
// conventions spell them, so that a backend already understanding database
// spans understands these. Everything the conventions have no name for lives
// under "orm.", which is a namespace this project can define without colliding
// with a future convention.
const (
	// The semantic conventions for a database client span.
	attrSystem     = "db.system"
	attrNamespace  = "db.namespace"
	attrOperation  = "db.operation.name"
	attrQueryText  = "db.query.text"
	attrCollection = "db.collection.name"

	// This project's own vocabulary.
	attrORMOp         = "orm.operation"
	attrFingerprint   = "orm.query.fingerprint"
	attrEntity        = "orm.entity"
	attrRelation      = "orm.relation.path"
	attrArgs          = "orm.query.args"
	attrRows          = "orm.rows"
	attrRaw           = "orm.raw"
	attrInTransaction = "orm.in_transaction"
	attrCopyColumns   = "orm.copy.columns"
	attrErrorKind     = "orm.error.kind"
	attrSQLState      = "orm.error.sqlstate"
	attrConstraint    = "orm.error.constraint"
)

// Option configures a [Tracer].
type Option func(*Tracer)

// WithSQL controls whether the statement's SQL becomes db.query.text. It is on
// by default: the SQL carries placeholders rather than values.
func WithSQL(on bool) Option { return func(t *Tracer) { t.sql = on } }

// WithRawSQL controls whether the SQL of a raw statement is exported, separately
// from [WithSQL].
//
// Raw SQL is the one statement text the ORM did not write, and only its author
// knows whether a literal in it is a table name or a secret.
func WithRawSQL(on bool) Option { return func(t *Tracer) { t.rawSQL = on } }

// WithErrorMessages records the server's error message on the span.
//
// It is off by default. PostgreSQL writes contextual detail into its messages —
// sometimes including the value that violated a constraint — so the message is
// a value-bearing surface. Without this, a failure is recorded by its
// classification: SQLSTATE, kind, and the constraint's name.
func WithErrorMessages(on bool) Option { return func(t *Tracer) { t.errorMessages = on } }

// Tracer turns the ORM's events into spans.
//
// It holds no mutable state after construction and may be shared by every
// goroutine in a program.
type Tracer struct {
	tracer        trace.Tracer
	sql           bool
	rawSQL        bool
	errorMessages bool
}

// New returns a tracer creating spans on tr.
//
// The tracer is the caller's: this package does not reach for a global provider,
// so a program that has not configured OpenTelemetry does not silently acquire
// a no-op one it did not ask for.
func New(tr trace.Tracer, opts ...Option) *Tracer {
	t := &Tracer{tracer: tr, sql: true, rawSQL: true}
	for _, o := range opts {
		if o != nil {
			o(t)
		}
	}
	return t
}

type spanKey struct{}

// Start opens a span and puts it in the context the operation will use.
//
// The returned context is what the ORM passes down, so a span opened here is
// the parent of anything the operation does — and is itself a child of whatever
// span the caller's context already carried. That is the whole of context
// propagation: no global state, no registry.
func (t *Tracer) Start(ctx context.Context, e observe.StartEvent) context.Context {
	if t == nil || t.tracer == nil {
		return ctx
	}

	attrs := []attribute.KeyValue{
		attribute.String(attrSystem, "postgresql"),
		attribute.String(attrORMOp, string(e.Op)),
	}
	if e.Fingerprint != "" {
		attrs = append(attrs, attribute.String(attrFingerprint, e.Fingerprint))
	}
	if e.Table != "" {
		attrs = append(attrs, attribute.String(attrCollection, e.Table))
	}
	if e.Entity != "" {
		attrs = append(attrs, attribute.String(attrEntity, e.Entity))
	}
	if e.Relation != "" {
		attrs = append(attrs, attribute.String(attrRelation, e.Relation))
	}
	if e.Args > 0 {
		attrs = append(attrs, attribute.Int(attrArgs, e.Args))
	}
	if e.Raw {
		attrs = append(attrs, attribute.Bool(attrRaw, true))
	}
	if e.InTransaction {
		attrs = append(attrs, attribute.Bool(attrInTransaction, true))
	}
	// The columns a COPY sends, as a count and as names — both are structure.
	// One COPY is one span whatever the number of rows; a span per row would
	// make a bulk load unusable in any backend.
	if len(e.Columns) > 0 {
		attrs = append(attrs,
			attribute.Int(attrCopyColumns, len(e.Columns)),
			attribute.StringSlice("orm.copy.column_names", e.Columns))
	}
	if sql := t.sqlFor(e); sql != "" {
		attrs = append(attrs,
			attribute.String(attrQueryText, sql),
			attribute.String(attrOperation, string(e.Op)))
	}

	ctx, span := t.tracer.Start(ctx, spanName(e),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...))
	return context.WithValue(ctx, spanKey{}, span)
}

// End closes the span.
func (t *Tracer) End(ctx context.Context, e observe.EndEvent) {
	if t == nil || t.tracer == nil {
		return
	}
	span, _ := ctx.Value(spanKey{}).(trace.Span)
	if span == nil {
		return
	}
	defer span.End()

	if e.RowsKnown {
		span.SetAttributes(attribute.Int64(attrRows, e.Rows))
	}
	if e.Err == nil {
		span.SetStatus(codes.Ok, "")
		return
	}

	info := observe.Classify(e.Err)
	attrs := []attribute.KeyValue{}
	if info.Kind != "" {
		attrs = append(attrs, attribute.String(attrErrorKind, info.Kind))
	}
	if info.SQLState != "" {
		attrs = append(attrs, attribute.String(attrSQLState, info.SQLState))
	}
	if info.Constraint != "" {
		attrs = append(attrs, attribute.String(attrConstraint, info.Constraint))
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}

	// RecordError puts the error's message on the span as an event, which is
	// the value-bearing surface. Off by default; the classification above is
	// what a span carries instead.
	if t.errorMessages {
		span.RecordError(e.Err)
		span.SetStatus(codes.Error, e.Err.Error())
		return
	}
	span.SetStatus(codes.Error, info.Kind)
}

func (t *Tracer) sqlFor(e observe.StartEvent) string {
	switch {
	case e.SQL == "":
		return ""
	case e.Raw && !t.rawSQL:
		return ""
	case !e.Raw && !t.sql:
		return ""
	}
	return e.SQL
}

// spanName is what the span is called.
//
// It is the operation and the table rather than the SQL: a span name is a
// grouping key, and one that varied with the statement would give every distinct
// query its own name.
func spanName(e observe.StartEvent) string {
	name := "orm." + string(e.Op)
	if e.Table != "" {
		name += " " + e.Table
	}
	if e.Relation != "" {
		name += " (" + e.Relation + ")"
	}
	return name
}
