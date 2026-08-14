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
