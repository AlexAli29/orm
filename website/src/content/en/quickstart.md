---
title: Quickstart
description: From an empty directory to a typed query.
---

This walks the managed path, where the declarations own the schema. For database-first — an existing database you point the generator at — see the note at the end.

## 1. Declare an entity

```go
// internal/domain/entities.go
package domain

import "time"

//orm:table public.users
type User struct {
    ID        int64     `orm:"pk,identity"`
    Email     string    `orm:"unique"`
    Bio       *string
    Active    bool
    CreatedAt time.Time
}
```

A pointer means the column is nullable. `orm:"pk"` names the primary key; `identity` says PostgreSQL generates it.

## 2. Plan and apply the migration

```bash
orm makemigrations
orm migrate
```

`makemigrations` diffs the declarations against the schema the existing migrations describe and writes an artifact. `migrate` applies it in a transaction and records it.

Look at the plan before applying it:

```bash
orm makemigrations --dry-run --sql
```

## 3. Generate

```bash
orm generate
```

This introspects the database, reconciles it against the declarations, and writes the descriptors beside your entities. Nothing is generated for a field it could not prove.

## 4. Query

```go
package main

import (
    "context"
    "log"
    "os"

    "github.com/jackc/pgx/v5/pgxpool"

    "example.com/app/internal/domain"
)

func main() {
    ctx := context.Background()
    pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()

    db := domain.New(pool)

    users, err := db.Users.Query().
        Where(domain.Users.Active.Eq(true)).
        OrderBy(domain.Users.CreatedAt.Desc()).
        Limit(20).
        All(ctx)
    if err != nil {
        log.Fatal(err)
    }
    log.Printf("%d users", len(users))
}
```

## 5. Keep it honest in CI

Two commands belong in every pipeline:

```bash
orm makemigrations --check   # a declaration with no migration fails the build
orm check --generated        # the committed generated code is current
```

## Database-first instead

Omit `mode: managed` and point `dsn` at the existing database. Then you write declarations that describe what is already there, and `orm check` tells you where they disagree. No migrations are generated; the database is authoritative.

## Worked examples

### The same five steps, in a different shape

A subscription service rather than a user table, to show the steps are the shape
rather than the schema.

```go
// internal/domain/entities.go
package domain

import "time"

//orm:table public.plans
//orm:index plans_code_key (Code) unique
type Plan struct {
    ID    int64  `orm:"pk,identity"`
    Code  string
    Cents int32
}

//orm:table public.subscriptions
//orm:index subs_customer_idx (CustomerID, StartedAt)
type Subscription struct {
    ID         int64      `orm:"pk,identity"`
    CustomerID int64
    PlanID     int64
    StartedAt  time.Time  `orm:"default:now()"`
    CancelledAt *time.Time

    Plan orm.One[Plan] `orm:"fk:plan_id"`
}
```

```bash
orm makemigrations && orm migrate && orm generate
```

```go
// Active subscriptions with their plan, newest first.
subs, err := db.Subscriptions.Query().
    Where(Subscriptions.CancelledAt.IsNull()).
    With(Subscriptions.Plan).
    OrderBy(Subscriptions.StartedAt.Desc()).
    Limit(50).
    All(ctx)

// Revenue by plan code, one statement.
var revenue = orm.Project2(
    Plans.Code, orm.Count[Subscription](),
    func(code string, n int64) Row { return Row{code, n} },
)
```

`CancelledAt` is a pointer, so `IsNull` exists on it and reads as "not
cancelled". On `StartedAt`, which is `NOT NULL`, that method is not there to be
misused.
