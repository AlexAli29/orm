# Compatibility policy

What v1 promises, what counts as breaking it, and how the promise is enforced.

## The public API is a file, not a number

Earlier milestones tracked "the export count". That does not define an API: remove
one function, add another, and the count is unchanged while every caller breaks.

The contract is [`api/`](../api/) — a canonical structural manifest generated from
`go/types`, not from source text. It records packages, constants and their values,
variables, functions, named types, type parameters **and their constraints**,
struct fields and tags, interface method sets, methods with their receivers, and
the shape of any type an unexported package exposes through the public surface.

```
go run ./internal/tools/apimanifest -dir . -out api/orm.txt ./...
```

A test regenerates it and fails on any difference, so a change to the public
surface always appears in review as a diff to a committed file. Regenerating the
snapshot is how a change is *stated*; this document says which changes are
allowed.

## What is public

| Package | Promise |
|---|---|
| `github.com/AlexAli29/orm` | the ORM |
| `.../plan` | PostgreSQL's plan, typed |
| `.../diagnostics` | query diagnostics (`QS…`, `PL…`) |
| `.../observe` | tracing interface and events |
| `.../postgis` | PostGIS types and operators |
| `.../ormtest` | testing toolkit |
| `.../ormhealth` | health and operational introspection |
| `.../ormslog` | `log/slog` tracer |
| `.../gen/diag` | generator diagnostic codes and findings |
| `.../cmd/orm` | the CLI's behaviour and flags |

Everything under `internal/` is not public and never was — including the whole
generator pipeline, which lives in `internal/gen/`. The one exception is
`gen/diag`: diagnostic codes are machine-observable API, because projects
suppress and alert on them.

Separately versioned modules with their own manifests: `ormotel`,
`ormtest/postgres`.

**Not public, despite being in the repository:** `ormextdemo` and everything in
`examples/`. The examples are deliberately named `example.com/...` so that they
cannot be imported as libraries and cannot reach the ORM's internals.

## Breaking, under v1

A new major version is required for any of these:

- removing an exported symbol, or moving it to another package;
- changing a function or method signature — parameters, results, variadic status;
- tightening a generic constraint, or changing type-parameter order;
- changing an exported struct field's type, or removing one;
- **adding a method to an interface that consumers implement** — this compiles
  for us and breaks every external implementation;
- changing what a diagnostic code means;
- changing generated code so that previously generated code stops working with
  the new runtime;
- changing how existing migration files are interpreted;
- changing documented SQL semantics — what a builder emits for the same input.

## Additive, under a minor version

- a new function, type, method or package;
- a new field on a struct consumers do not construct with unkeyed literals;
- a new diagnostic code;
- a new PostgreSQL operator or type binding;
- a new value accepted by an existing configuration field — for example a future
  `packages[].output` other than `same`, which is reserved for exactly this;
- a new optional module.

Adding a method to an interface is **not** additive when consumers implement it.
Interfaces meant only to be consumed are documented as such.

## Error strings are not API

Error text is written for people and may be reworded in any release. Branch on:

- `errors.Is` against the documented sentinels;
- `errors.As` against the documented error types, and against
  `*pgconn.PgError` for anything PostgreSQL refused;
- a diagnostic code, for generator and schema findings;
- a SQLSTATE, for anything the server decided.

Two invariants hold everywhere and are tested: `*pgconn.PgError` stays reachable
through `errors.As` wherever PostgreSQL produced the failure, and
`context.Canceled` / `context.DeadlineExceeded` stay reachable through
`errors.Is` wherever a context ended the operation.

## Diagnostic codes

A published code may be **retired** but never **reassigned**. A project that
suppressed a code must never find itself silently suppressing a different
finding, and it has no way to notice. Retirement leaves the number unused
forever; M14 retired `PL003` this way rather than reusing it.

CI fails on a duplicate code, on a code emitted but not registered, and on a
registered code whose severity disagrees with its own name.

## Generated code

Generated code is part of the contract: projects commit it and regenerate on
upgrade.

**Regenerate when you upgrade.** The supported combination is generated code and
runtime from the same minor version. Older generated code against a newer runtime
is not promised to work across a minor bump, and mixing is not tested — so it is
not claimed. `orm.lock` records the mapping the code was generated from, and
`orm check --generated` reports when the two have drifted.

Generation is deterministic: the same declarations, config and schema produce
byte-identical output, independent of file order, package discovery order, and
the absolute path of the project.

## Deprecation

1. mark with Go's `// Deprecated:` convention, saying what to use instead;
2. keep it working for at least two minor releases;
3. remove only in the next major.

A deprecated symbol stays in the manifest until it is removed; removal is a
breaking change and is scheduled as one.

## Semantic import versioning

A future breaking major is published as a new module path ending in `/v2`, per
Go's semantic import versioning. No `/v2` exists today, and none is being
prepared. What matters now is that nothing in the release tooling assumes the
path is permanently `github.com/AlexAli29/orm`.
