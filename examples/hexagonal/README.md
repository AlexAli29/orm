# The hexagonal example

The same kind of application as [`production/`](../production/), arranged as
ports and adapters — and, more to the point, with the arrangement **checked**.

```
core/domain/    the types and the rules. Standard library only.
core/port/      what the core needs from outside, in the core's vocabulary.
core/app/       the use cases. Decides what is atomic.
adapter/ormstore/  implements the ports with this ORM and PostgreSQL.
adapter/httpin/    turns HTTP into use-case calls.
cmd/server/     the composition root: the only file that names both sides.
```

## The interfaces are declared where they are used

`port.UserRepository` lives next to the code that calls it, not next to the code
that implements it. That is the direction the whole arrangement rests on: the
adapter depends on the core's idea of storage, and the core depends on nothing.
Declared the other way — beside its PostgreSQL implementation — the core would
have to import the database package just to name the type, which is the
dependency the hexagon exists to remove.

## The unit of work is a port

```go
type UnitOfWork interface {
    Do(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}
```

`Tx` is `any`. The core has to be able to say "these two things are one
operation" and hand the same unit to two repositories; it does not have to know
what a transaction is. There is no `Begin` for it to call, so there is no path
on which a transaction is opened and not finished.

Only `ormstore` unwraps the `Tx` back into an `orm.Executor`.

## The boundary is a test, not a promise

`boundary_test.go` reads the real import graph out of `go list -json` and states
the rules:

- the core reaches neither the ORM, nor pgx, nor `net/http`, nor the adapters —
  transitively, not just directly;
- `core/domain` imports nothing outside the standard library;
- exactly two packages import the ORM: the store adapter, and the composition
  root;
- the HTTP adapter cannot reach the store, so no handler can bypass a use case.

It is a test rather than a lint rule on purpose: a rule that lives in CI's YAML
fails after you push, and one that lives in a test fails in your editor.

`TestHexagonal_coreRunsWithoutADatabase` is the second, independent check on the
same boundary — it wires the use cases to repositories that are maps. If the
core had acquired a dependency on storage, it would not compile.

## Why the module is called `example.com/hexagonal`

Go's `internal/` rule is lexical, not modular. A module named
`github.com/AlexAli29/orm/examples/hexagonal` would have been allowed to import
`github.com/AlexAli29/orm/internal/...` — separate `go.mod` or not. Named as it
is, the compiler refuses.

## Running it

```bash
go test ./...    # starts PostgreSQL through Testcontainers; needs Docker

export HEXAGONAL_EXAMPLE_DSN='postgres://user:pass@localhost:5432/yourdb'
go run github.com/AlexAli29/orm/cmd/orm migrate --config orm.yaml
go run ./cmd/server
```
