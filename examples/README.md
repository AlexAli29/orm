# Examples

Four examples, in the order they are worth reading.

| Example | What it answers |
|---|---|
| [`managed/`](managed/) | What the declarations-own-the-schema workflow looks like end to end |
| [`blog/`](blog/) | What the database-first workflow looks like against an existing schema |
| [`production/`](production/) | What a real service looks like: transports, health, shutdown, observability |
| [`hexagonal/`](hexagonal/) | The same application arranged as ports and adapters, with the boundary enforced by a test |

`production/` and `hexagonal/` are **separate Go modules**, and their module
paths are `example.com/production` and `example.com/hexagonal` rather than
anything under `github.com/AlexAli29/orm/`. That is not cosmetic. Go's
`internal/` rule is lexical: a module named
`github.com/AlexAli29/orm/examples/production` would have been *permitted* to
import `github.com/AlexAli29/orm/internal/...`, separate `go.mod` or not. Named
as they are, the compiler refuses — so these examples are external consumers in
the only sense that cannot be arranged by agreement.

Each has a `replace` directive pointing at the checkout, so they build against
the working tree rather than a published version.

## Running them

```bash
# The production example's tests start their own PostgreSQL through
# Testcontainers, so this needs Docker and nothing else.
cd examples/production && go test ./...
cd examples/hexagonal  && go test ./...
```

To run the server:

```bash
cd examples/production
export PRODUCTION_EXAMPLE_DSN='postgres://user:pass@localhost:5432/yourdb'
go run ./cmd/orm migrate --config orm.yaml   # from the repository root
go run ./cmd/server
```

## Which architecture

Both are real, and the choice is not about which is better. See
[`docs/production.md`](../docs/production.md#choosing-between-the-two-examples).

- `production/` is the **pragmatic** arrangement: a domain package, an adapter
  that knows the ORM, a service that owns transactions, and transports. Four
  layers, no interfaces where there is one implementation.
- `hexagonal/` is **ports and adapters**: the core declares what it needs, the
  adapters implement it, and the dependency direction is checked by a test that
  reads the actual import graph.
