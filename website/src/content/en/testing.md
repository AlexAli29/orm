---
title: Testing
description: Real PostgreSQL, disposable databases, and no mocking of SQL.
---

## The position

Do not mock the database. A mock of a SQL driver tests that you can write a mock; the behaviours that break in production — constraints, types, NULL semantics, transaction isolation — are exactly the ones a mock has no opinion about.

Everything here is built to run against a real PostgreSQL and to make that cheap.

## A database per test binary

`ormtest` does not create databases. It gives you the pieces that make a real
one usable: applying migrations, checking the schema is the one your config
describes, and running a test inside a transaction that is always rolled back.

```go
import "github.com/AlexAli29/orm/ormtest"

func TestMain(m *testing.M) {
    conn, _ := pgx.Connect(ctx, os.Getenv("TEST_DSN"))
    ormtest.Migrate(t, conn, "migrations")   // apply the committed artifacts
    os.Exit(m.Run())
}
```

`ormtest.CheckSchema(ctx, "orm.yaml")` is the assertion worth putting in CI: it
fails when the database is not the schema the declarations describe, which is the
failure that otherwise shows up as an unrelated test breaking oddly.

`RequireSchemaClean` is the same check as a hard requirement.

## Containers

```go
import ormpg "github.com/AlexAli29/orm/ormtest/postgres"

func TestMain(m *testing.M) {
    ormpg.Run(m, ormpg.WithImage("postgres:17"))
}
```

A module of its own, because Testcontainers is a heavy dependency and a project that has a PostgreSQL already should not pay for it.

## Emptying tables between tests

`Truncate` lives in `ormtest` rather than in the query API, because emptying a
table is a fixture operation rather than something an application does:

```go
import "github.com/AlexAli29/orm/ormtest"

ormtest.MustTruncate(t, pool, domain.Users, domain.Orders)

err := ormtest.TruncateWith(ctx, pool,
    []ormtest.TruncateOption{ormtest.RestartIdentity(), ormtest.Cascade()},
    domain.Users,
)
```

It takes the generated table handles directly. `RestartIdentity` resets the
identity sequences, which is usually what a fixture wants; `Cascade` follows
foreign keys and will empty tables you did not name, so it is opt-in.

Truncating several tables in one call is not a convenience — it is the only way
to empty tables that reference each other without `Cascade`.

## Transaction-per-test

Fast, and isolated without dropping anything:

```go
func TestSomething(t *testing.T) {
    ormtest.TxFunc(t, pool, func(ex orm.Executor) {
        db := domain.New(ex)
        // ... test ... the transaction is rolled back when it returns
    })
}
```

## Asserting on SQL without a database

```go
sql, args, err := q.SQL()
if !strings.Contains(sql, "LEFT JOIN") { /* ... */ }
```

Useful for the shape of a query. Not a substitute for running it: SQL that looks right and returns the wrong rows is the failure mode this whole project is arranged against.

## What CI should run

```bash
orm makemigrations --check   # a declaration with no migration
orm check --generated        # generated code that drifted
go test -race ./...
```

## Testing against every major

The project's own compatibility suite refuses to run against fewer than all five supported majors, and it is worth stealing the idea: a matrix that silently runs on whichever server happened to be up proves less than it claims.

```yaml
strategy:
  matrix:
    postgres: ['14', '15', '16', '17', '18']
```
