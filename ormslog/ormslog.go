// Package ormslog logs the ORM's operations through log/slog.
//
// It is an adapter over the observability model rather than a second one: it
// implements [observe.Tracer], receives the events the ORM already emits, and
// writes them as structured log records. Installing it changes nothing about
// what a query does.
//
//	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
//	db := domain.New(orm.Traced(pool, ormslog.New(logger)))
//
// # It cannot log a bind value
//
// Not "does not" — cannot. The events it receives have no field for argument
// values, so there is nothing here to redact and nothing to accidentally
// forward. The SQL it can log is the statement with its placeholders still in
// it, which is what makes a log line useful without making it sensitive.
//
// The one exception is the one the ORM documents everywhere else: SQL written by
// a caller inside a raw statement may contain any literal the caller put there,
// and no amount of care in this package can find it without parsing SQL. Raw
// statements are marked as such on the event, so [WithRawSQL] can turn their SQL
// off separately if a project would rather not have it in the log at all.
//
// # It observes; it does not investigate
//
// A slow query is logged at a level the caller chose, with the numbers the event
// already carried. It does not run EXPLAIN to find out why — that would turn a
// logger into something that issues statements of its own, and for
// EXPLAIN ANALYZE it would execute the query a second time.
//
// # It adds no dependency
//
// log/slog is the standard library, so this package lives in the ORM's own
// module and costs a project nothing to have available.
package ormslog

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlexAli29/orm/observe"
)

// Logger is the part of *slog.Logger this package uses, so a caller can supply
// something else that logs.
type Logger interface {
	LogAttrs(ctx context.Context, level slog.Level, msg string, attrs ...slog.Attr)
}

// Option configures a [Tracer].
type Option func(*Tracer)

// WithLevel sets the level successful operations are logged at. The default is
// [slog.LevelDebug]: a query that worked is not news.
func WithLevel(l slog.Level) Option { return func(t *Tracer) { t.level = l } }

// WithErrorLevel sets the level failed operations are logged at. The default is
// [slog.LevelError].
func WithErrorLevel(l slog.Level) Option { return func(t *Tracer) { t.errorLevel = l } }

// WithSlowThreshold logs operations that took at least d at the slow level,
// which defaults to [slog.LevelWarn].
//
// There is no default threshold and there will not be one. What counts as slow
// is a property of an application — a report that takes four seconds may be
// fine and a lookup that takes forty milliseconds may not — and a number chosen
// here would be wrong for most projects while looking authoritative.
//
// It is observation, not diagnosis: crossing the threshold logs the operation
// and its duration. Nothing runs EXPLAIN.
func WithSlowThreshold(d time.Duration) Option {
	return func(t *Tracer) { t.slow = d }
}

// WithSlowLevel sets the level slow operations are logged at.
func WithSlowLevel(l slog.Level) Option { return func(t *Tracer) { t.slowLevel = l } }

// WithSQL controls whether the statement's SQL is logged. It is on by default:
// the SQL carries placeholders rather than values, and it is the most useful
// single field a query log can have.
func WithSQL(on bool) Option { return func(t *Tracer) { t.sql = on } }

// WithRawSQL controls whether the SQL of a *raw* statement is logged, separately
// from [WithSQL].
//
// It exists because raw SQL is the one statement text the ORM did not write. A
// project that logs SQL happily for generated statements may still want raw ones
// left out, because only their author knows whether a literal in one is a table
// name or a password.
func WithRawSQL(on bool) Option { return func(t *Tracer) { t.rawSQL = on } }

// Tracer writes the ORM's operations to a logger.
//
// It holds no mutable state after construction, so one may be shared by every
// goroutine in a program.
type Tracer struct {
	log        Logger
	level      slog.Level
	errorLevel slog.Level
	slowLevel  slog.Level
	slow       time.Duration
	sql        bool
	rawSQL     bool
}

// New returns a tracer writing to log.
func New(log Logger, opts ...Option) *Tracer {
	t := &Tracer{
		log:        log,
		level:      slog.LevelDebug,
		errorLevel: slog.LevelError,
		slowLevel:  slog.LevelWarn,
		sql:        true,
		rawSQL:     true,
	}
	for _, o := range opts {
		if o != nil {
			o(t)
		}
	}
	return t
}

// startKey carries the start event to the end of the operation.
type startKey struct{}

// Start records nothing and remembers the event.
//
// Logging at the start would double every line for no gain: the end event
// carries everything the start one did, plus the outcome.
func (t *Tracer) Start(ctx context.Context, e observe.StartEvent) context.Context {
	if t == nil || t.log == nil {
		return ctx
	}
	return context.WithValue(ctx, startKey{}, e)
}

// End writes the record.
func (t *Tracer) End(ctx context.Context, e observe.EndEvent) {
	if t == nil || t.log == nil {
		return
	}
	start, _ := ctx.Value(startKey{}).(observe.StartEvent)

	attrs := make([]slog.Attr, 0, 12)
	attrs = append(attrs,
		slog.String("op", string(e.Op)),
		slog.Duration("duration", e.Duration),
	)
	if e.Fingerprint != "" {
		attrs = append(attrs, slog.String("fingerprint", e.Fingerprint))
	}
	if start.Table != "" {
		attrs = append(attrs, slog.String("table", start.Table))
	}
	if start.Entity != "" {
		attrs = append(attrs, slog.String("entity", start.Entity))
	}
	if start.Relation != "" {
		attrs = append(attrs, slog.String("relation", start.Relation))
	}
	if len(start.Columns) > 0 {
		attrs = append(attrs, slog.Int("columns", len(start.Columns)))
	}
	// The count of arguments, never the arguments: there is no field on the
	// event that holds them.
	if start.Args > 0 {
		attrs = append(attrs, slog.Int("args", start.Args))
	}
	if start.InTransaction {
		attrs = append(attrs, slog.Bool("in_transaction", true))
	}
	if e.RowsKnown {
		attrs = append(attrs, slog.Int64("rows", e.Rows))
	}
	if sql := t.sqlFor(start); sql != "" {
		attrs = append(attrs, slog.String("sql", sql))
	}

	level, msg := t.level, "orm operation"
	switch {
	case e.Err != nil:
		level, msg = t.errorLevel, "orm operation failed"
		attrs = append(attrs, t.errorAttrs(e)...)
	case t.slow > 0 && e.Duration >= t.slow:
		level, msg = t.slowLevel, "orm operation was slow"
		attrs = append(attrs, slog.Duration("threshold", t.slow))
	}
	t.log.LogAttrs(ctx, level, msg, attrs...)
}

// sqlFor decides whether this operation's SQL is logged.
func (t *Tracer) sqlFor(start observe.StartEvent) string {
	switch {
	case start.SQL == "":
		return ""
	case start.Raw && !t.rawSQL:
		return ""
	case !start.Raw && !t.sql:
		return ""
	}
	return start.SQL
}

// errorAttrs describes a failure with the classification the ORM already made.
//
// The SQLSTATE and the constraint are structured, which is what a log is for:
// they are stable enough to alert on. The server's message is not included —
// PostgreSQL writes contextual detail into it, sometimes including the value
// that violated a constraint, and this package will not put that in a log
// without being asked.
func (t *Tracer) errorAttrs(e observe.EndEvent) []slog.Attr {
	// Classify turns the error into the structured facts a log can alert on,
	// without repeating the server's message.
	info := observe.Classify(e.Err)
	attrs := []slog.Attr{}
	if info.Kind != "" {
		attrs = append(attrs, slog.String("error_kind", info.Kind))
	}
	if info.SQLState != "" {
		attrs = append(attrs, slog.String("sqlstate", info.SQLState))
	}
	if info.Constraint != "" {
		attrs = append(attrs, slog.String("constraint", info.Constraint))
	}
	if info.Table != "" {
		attrs = append(attrs, slog.String("error_table", info.Table))
	}
	if e.Cancelled {
		attrs = append(attrs, slog.Bool("cancelled", true))
	}
	return attrs
}
