// Package observability wires the ORM's tracing to logs and to traces at the
// same time.
//
// The ORM defines an interface and two event types and imports no telemetry
// library. What it does not define is how to send the same events to more than
// one place, because that is an application's decision — so this package shows
// the decision being made: a tracer that fans out, and a note about why
// wrapping twice does not.
//
// # Attaching two tracers is not nesting two executors
//
//	ex := orm.Traced(orm.Traced(pool, logging), tracing) // WRONG
//
// An executor carries one tracer. Wrapping twice produces an executor whose
// tracer is the outer one, and the inner is never called — silently, with no
// error and no missing type. The way to reach two destinations is one tracer
// that forwards to both, which is [Multi].
//
// # What is in an event, and what is not
//
// The adapters differ in what they emit but not in what they are given: an
// event carries the SQL with its placeholders intact, the number of arguments,
// a fingerprint, the entity and table, and the outcome. It never carries a bind
// value. That is a property of the core, and the sweep in this package's test
// checks it end to end — a password, a token and an email address are written
// through every kind of statement, and every log line and every span attribute
// is searched for them.
//
// Turning on [ormslog.WithRawSQL] or [ormotel.WithRawSQL] widens that: SQL you
// wrote yourself inside orm.Raw may contain literals the ORM cannot redact
// without parsing SQL. Both are off here, and the test proves what turning them
// on would cost.
package observability

import (
	"context"
	"log/slog"
	"time"

	"github.com/AlexAli29/orm/observe"
	"github.com/AlexAli29/orm/ormotel"
	"github.com/AlexAli29/orm/ormslog"
	"go.opentelemetry.io/otel/trace"
)

// Multi forwards every event to each tracer in turn.
//
// Start is the awkward one: each tracer may return a context carrying its own
// state, and there is one context to pass on. They are threaded, so tracer two
// sees what tracer one added — which is what makes an OTel span visible to a
// log adapter that wants to print the trace id.
//
// A nil tracer in the list is skipped, so an application can build the list
// from configuration without branching.
type Multi []observe.Tracer

// Start begins the operation on every tracer, threading the context through.
func (m Multi) Start(ctx context.Context, e observe.StartEvent) context.Context {
	for _, t := range m {
		if t == nil {
			continue
		}
		ctx = t.Start(ctx, e)
	}
	return ctx
}

// End reports the outcome to every tracer.
//
// They are ended in reverse order, so that a tracer which opened a span inside
// another's closes first. Nothing in the interface requires that; it is what
// nesting means, and the cost of getting it right is a reversed loop.
func (m Multi) End(ctx context.Context, e observe.EndEvent) {
	for i := len(m) - 1; i >= 0; i-- {
		if m[i] == nil {
			continue
		}
		m[i].End(ctx, e)
	}
}

// Config is what an application chooses about its own observability.
type Config struct {
	// LogSQL prints the statement, with placeholders, on every operation. It is
	// off by default because the volume is a production decision.
	LogSQL bool
	// SlowThreshold is the duration above which an operation is logged at a
	// higher level. Zero means the adapter's own default.
	SlowThreshold time.Duration
	// Traces enables OpenTelemetry spans. The tracer provider is the
	// application's; this package does not create or install one, because a
	// library that installs a global provider takes a decision away from the
	// program that owns it.
	Traces trace.Tracer
}

// New builds the tracer to attach to the executor.
//
//	ex := orm.Traced(pool, observability.New(log, cfg))
//	svc := service.New(ex)
//
// One call, at startup, on the executor. Nothing below it — not the service,
// not the store, not the generated code — mentions telemetry, and a transaction
// started from this executor inherits it.
func New(log *slog.Logger, cfg Config) observe.Tracer {
	tracers := Multi{
		ormslog.New(log,
			ormslog.WithSQL(cfg.LogSQL),
			ormslog.WithSlowThreshold(cfg.SlowThreshold),
			// Off, and stated rather than omitted: this is the switch that lets
			// literals written inside orm.Raw reach the log.
			ormslog.WithRawSQL(false),
		),
	}
	if cfg.Traces != nil {
		tracers = append(tracers, ormotel.New(cfg.Traces,
			ormotel.WithSQL(cfg.LogSQL),
			ormotel.WithRawSQL(false),
			// The server's message can quote a value from the row that broke a
			// constraint, and a span is exported to somewhere with a different
			// audience from the application log.
			ormotel.WithErrorMessages(false),
		))
	}
	return tracers
}
