---
title: Testing
description: Real PostgreSQL, disposable databases, and no mocking of SQL.
---

## The position

Do not mock the database. A mock of a SQL driver tests that you can write a mock; the behaviours that break in production — constraints, types, NULL semantics, transaction isolation — are exactly the ones a mock has no opinion about.

Everything here is built to run against a real PostgreSQL and to make that cheap.

## Disposable databases

```go
import "github.com/AlexAli29/orm/ormtest"

func TestUsers(t *testing.T) {
    dsn := ormtest.Create(t, schemaSQL) // a fresh database, dropped on cleanup
    pool, _ := pgxpool.New(t.Context(), dsn)
    db := domain.New(pool)
    // ...
}
```

The DSN comes from `ORM_TEST_ADMIN_DSN`. Without it the helpers fail rather than skip, when the suite is one whose whole job is to run against a database — a suite that quietly runs nothing reports green on a machine with no PostgreSQL.

## Containers

```go
import ormpg "github.com/AlexAli29/orm/ormtest/postgres"

func TestMain(m *testing.M) {
    ormpg.Run(m, ormpg.WithImage("postgres:17"))
}
```

A module of its own, because Testcontainers is a heavy dependency and a project that has a PostgreSQL already should not pay for it.

## Transaction-per-test

Fast, and isolated without dropping anything:

```go
func TestSomething(t *testing.T) {
    err := orm.RunTx(t.Context(), pool, func(ex orm.Executor) error {
        db := domain.New(ex)
        // ... test ...
        return errors.New("rollback") // never commit
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
