---
title: Сущности и теги
description: Директивы и теги структур, которые читает генератор.
---

## Директивы

Директива — это комментарий над типом. Она говорит, чем тип является.

```go
//orm:table public.users
type User struct { /* ... */ }

//orm:view public.active_users
//orm:definition `SELECT id, email FROM users WHERE active`
//orm:depends-on public.users
type ActiveUser struct { /* ... */ }

//orm:materialized-view public.user_summaries
//orm:definition `SELECT user_id, count(*) AS orders FROM user_orders GROUP BY user_id`
//orm:depends-on public.user_orders
//orm:index user_summaries_key (UserID) unique
type UserSummary struct { /* ... */ }
```

Имя отношения указывается со схемой. `public` не подразумевается: в проекте с двумя схемами у одного имени было бы два смысла.

## Грамматика тегов

Всё остальное — теги структуры под ключом `orm`:

| Директива | Значение |
| --- | --- |
| `pk` | Часть первичного ключа |
| `identity` | Значение генерирует PostgreSQL (`identity:always` — `GENERATED ALWAYS`) |
| `unique` | Уникальное ограничение по одной колонке |
| `column:name` | Имя колонки, если отличается от поля |
| `pgtype:uuid` | Тип PostgreSQL, если Go-тип его не задаёт |
| `type:name` | Ключ настроенного отображения типов |
| `default:expr` | `DEFAULT` колонки |
| `generated:expr` | Генерируемая колонка |
| `fk:user_id` | Колонка внешнего ключа для связи |
| `side:...` | Сторона связи |
| `-` | Полностью игнорировать поле |

```go
//orm:table public.orders
//orm:index orders_user_idx (UserID)
type Order struct {
    ID         uuid.UUID  `orm:"pk,pgtype:uuid"`
    UserID     uuid.UUID  `orm:"pgtype:uuid"`
    Label      string     `orm:"column:title"`
    Total      string     `orm:"pgtype:numeric"`
    CreatedAt  time.Time  `orm:"default:now()"`
    Internal   string     `orm:"-"`

    User orm.One[User] `orm:"fk:user_id"`
}
```

## Nullability

Указатель — это nullable-колонка. Больше знать нечего:

```go
Bio        *string     // bio text
OptionalID *uuid.UUID  // optional_id uuid
Tags       []string    // tags text[] NOT NULL
```

Пустой срез и NULL-массив — разные значения, и библиотека их различает. Если колонка-массив nullable, пишите `*[]string`.

## Нулевые значения — это значения

```go
db.Users.Insert(ctx, User{Active: false})               // сохранит FALSE
db.Users.Insert(ctx, User{}, orm.Default(Users.Active)) // сохранит DEFAULT колонки
```

Библиотека не отличит «false» от «поля, которого никто не касался», а угадывание — это способ получить строку со значением, которого никто не выбирал. Запрос значения по умолчанию — отдельное явное действие.

## Связи

`One` и `Many` объявляют связи и различают три состояния: не загружено, загружено и пусто, загружено и есть.

```go
type User struct {
    ID     int64 `orm:"pk,identity"`
    Orders orm.Many[Order]
}

type Order struct {
    ID     int64 `orm:"pk,identity"`
    UserID int64
    User   orm.One[User] `orm:"fk:user_id"`
}
```

Нулевое значение — «не загружено», поэтому литерал структуры без связи говорит «я этого не просил», а не «там пусто». Читается через `Get() ([]T, bool)` или `MustGet()`.

## Индексы

Объявляются на типе, потому что индекс принадлежит отношению, а не колонке:

```go
//orm:index users_email_key (Email) unique
//orm:index users_active_idx (Active, CreatedAt)
//orm:index users_lower_email_idx ("lower(email)")
//orm:index users_tags_gin_idx (Tags) using gin
//orm:index users_paid_idx (CreatedAt) where "paid_at IS NOT NULL"
```

Поля называются по Go-именам; строка в кавычках — выражение SQL.
