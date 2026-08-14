---
title: Быстрый старт
description: От пустого каталога до типизированного запроса.
---

Здесь описан режим managed, где схемой владеют декларации. Про database-first — существующую базу, на которую вы наводите генератор, — в конце.

## 1. Опишите сущность

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

Указатель означает nullable-колонку. `orm:"pk"` задаёт первичный ключ, `identity` говорит, что значение генерирует PostgreSQL.

## 2. Спланируйте и примените миграцию

```bash
orm makemigrations
orm migrate
```

`makemigrations` сравнивает декларации со схемой, которую описывают существующие миграции, и пишет артефакт. `migrate` применяет его в транзакции и записывает в историю.

Посмотрите план до применения:

```bash
orm makemigrations --dry-run --sql
```

## 3. Сгенерируйте

```bash
orm generate
```

Команда читает базу, сверяет её с декларациями и пишет дескрипторы рядом с сущностями. Для поля, которое не удалось доказать, не генерируется ничего.

## 4. Запрос

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

## 5. Держите это честным в CI

Две команды должны быть в каждом пайплайне:

```bash
orm makemigrations --check   # декларация без миграции роняет сборку
orm check --generated        # закоммиченный сгенерированный код актуален
```

## Вместо этого — database-first

Уберите `mode: managed` и наведите `dsn` на существующую базу. Тогда вы пишете декларации, описывающие то, что уже есть, а `orm check` показывает расхождения. Миграции не генерируются: авторитетна база.

## Разобранные примеры

### Те же пять шагов в другой форме

Сервис подписок вместо таблицы пользователей — чтобы показать, что шаги
определяются формой работы, а не схемой.

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
// Активные подписки вместе с тарифом, сначала свежие.
subs, err := db.Subscriptions.Query().
    Where(Subscriptions.CancelledAt.IsNull()).
    With(Subscriptions.Plan).
    OrderBy(Subscriptions.StartedAt.Desc()).
    Limit(50).
    All(ctx)

// Выручка по кодам тарифов, один запрос.
var revenue = orm.Project2(
    Plans.Code, orm.Count[Subscription](),
    func(code string, n int64) Row { return Row{code, n} },
)
```

`CancelledAt` — указатель, поэтому у него есть `IsNull`, и он читается как «не
отменена». У `StartedAt`, объявленной `NOT NULL`, этого метода просто нет, и
ошибиться нечем.
