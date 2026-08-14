---
title: Tracing and health
description: One tracer, attached once, and a health check that knows about migrations.
---

## The contract

The ORM defines an interface and two event types, and imports no telemetry library. An ORM that imported one would make every project using it depend on that library's version, its transitive tree and its opinions.

```go
type Tracer interface {
    Start(ctx context.Context, e observe.StartEvent) context.Context
    End(ctx context.Context, e observe.EndEvent)
}
```

## The rule worth stating first

**A start event never carries bound argument values.** Not by convention, not behind an option somebody can turn off — the field does not exist.

An ORM's tracing sees every query a program runs. A tracer that received the values would put every password, token and address the program handles into whatever the tracer writes to. The SQL is there with its placeholders, because `WHERE email = $1` is useful and says nothing about who.

The exception is SQL you wrote yourself: this package cannot redact a literal out of a raw statement without parsing SQL, and building a SQL parser would be building the thing the ORM exists not to have. `StartEvent.Raw` says which statements those are.

## Attaching

```go
db := domain.New(orm.Traced(pool, tracer))
```

One call, at startup, on the executor. Nothing below it — not the service, not the store, not the generated code — mentions telemetry, and a transaction started from this executor inherits it.

## Two destinations

An executor carries **one** tracer. Wrapping twice produces an executor whose tracer is the outer one and whose inner is never called — silently, with no error:

```go
ex := orm.Traced(orm.Traced(pool, logging), tracing) // WRONG
```

The way to reach two destinations is one tracer that forwards to both:

```go
type Multi []observe.Tracer

func (m Multi) Start(ctx context.Context, e observe.StartEvent) context.Context {
    for _, t := range m {
        if t != nil {
            ctx = t.Start(ctx, e) // threaded, so tracer two sees tracer one's context
        }
    }
    return ctx
}

func (m Multi) End(ctx context.Context, e observe.EndEvent) {
    for i := len(m) - 1; i >= 0; i-- { // reversed: nesting means closing inside-out
        if m[i] != nil {
            m[i].End(ctx, e)
        }
    }
}
```

## slog

```go
import "github.com/AlexAli29/orm/ormslog"

tracer := ormslog.New(log,
    ormslog.WithSQL(true),
    ormslog.WithSlowThreshold(200*time.Millisecond),
    ormslog.WithRawSQL(false), // the switch that would let literals reach the log
)
```

## OpenTelemetry

A module of its own, so a project that does not use it never compiles it:

```go
import "github.com/AlexAli29/orm/ormotel"

tracer := ormotel.New(otelTracer,
    ormotel.WithSQL(true),
    ormotel.WithRawSQL(false),
    ormotel.WithErrorMessages(false),
)
```

`WithErrorMessages(false)` is the default and worth keeping: PostgreSQL's message can quote a value from the row that broke a constraint, and a span goes somewhere with a different audience from the application log.

## Health

```go
import "github.com/AlexAli29/orm/ormhealth"

h := ormhealth.New(pool,
    ormhealth.WithMigrationState(migrationsDir),
)

http.Handle("/healthz", ormhealth.Handler(h))
```

`WithMigrationState` is the one that catches the deploy that half-happened: the pool is up, the queries work, and the schema is a version behind. Liveness says fine; this says otherwise.
