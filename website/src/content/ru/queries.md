---
title: Запросы
description: Чтение сущностей — фильтры, сортировка, страницы и терминальные операции.
---

## Форма

```go
users, err := db.Users.Query().
    Where(Users.Active.Eq(true)).
    OrderBy(Users.CreatedAt.Desc()).
    Limit(50).
    All(ctx)
```

`Query` изменяем и одноразов. `Clone` ответвляет копию, когда нужна база:

```go
base := db.Users.Query().Where(Users.Active.Eq(true))
recent := base.Clone().Where(Users.CreatedAt.Gte(cutoff))
count, _ := base.Clone().Count(ctx)
```

## Терминалы

| Метод | Возвращает |
| --- | --- |
| `All(ctx)` | `[]E` |
| `One(ctx)` | `E` или `ErrNotFound` |
| `Count(ctx)` | `int64` |
| `Exists(ctx)` | `bool` |
| `Rows(ctx)` | `iter.Seq2[E, error]` — потоком |
| `SQL()` | запрос и аргументы, без выполнения |

Ошибки построения накапливаются и приходят вместе из терминала, поэтому запрос, который нельзя построить, никогда не доходит до PostgreSQL:

```go
_, err := db.Users.Query().Where(broken).OrderBy(alsoBroken).All(ctx)
// err сообщает про обе, а не только про первую
```

## Where

Несколько вызовов `Where` объединяются через AND. Это частый случай, и он оставляет динамическую фильтрацию читаемой:

```go
q := db.Users.Query()
if email != "" {
    q = q.Where(Users.Email.ILike("%" + email + "%"))
}
if onlyActive {
    q = q.Where(Users.Active.Eq(true))
}
users, err := q.All(ctx)
```

Для OR — явно:

```go
db.Users.Query().Where(orm.Or(
    Users.Email.Eq("a@example.com"),
    Users.Email.Eq("b@example.com"),
))
```

`orm.And()` по пустому срезу даёт запрос вообще без `WHERE`, а не `WHERE TRUE`.

## Сортировка и страницы

```go
db.Users.Query().
    OrderBy(Users.CreatedAt.Desc(), Users.ID.Asc()).
    Limit(20).
    Offset(40)
```

`Limit(0)` — законный запрос, возвращающий ничего. Ошибка — только отрицательное значение.

На больших таблицах keyset-пагинация лучше `OFFSET`, и типизированный API выражает её прямо:

```go
db.Users.Query().
    Where(orm.Or(
        Users.CreatedAt.Lt(lastSeenAt),
        orm.And(Users.CreatedAt.Eq(lastSeenAt), Users.ID.Lt(lastSeenID)),
    )).
    OrderBy(Users.CreatedAt.Desc(), Users.ID.Desc()).
    Limit(20)
```

## Потоковое чтение

`Rows` отдаёт строки по мере их прихода, поэтому большой результат никогда не существует целиком:

```go
for user, err := range db.Users.Query().Rows(ctx) {
    if err != nil {
        return err
    }
    if err := handle(user); err != nil {
        return err
    }
}
```

Связи требуют отдельного запроса, а для него нужно увидеть все строки — ровно то, чего потоковое чтение и избегает. Поэтому `Rows` отказывает `With`, а не буферизует молча.

## Блокировки

```go
db.Users.Query().Where(Users.ID.Eq(id)).ForUpdate()
db.Users.Query().Lock(orm.ForUpdateStrong, orm.SkipLocked())
db.Users.Query().Lock(orm.ForShare, orm.NoWait())
```

Блокировать nullable-сторону outer join PostgreSQL отказывается, поэтому при наличии джойнов блокировка явно называет корневую таблицу.

## Посмотреть SQL

```go
sql, args, err := db.Users.Query().Where(Users.Active.Eq(true)).SQL()
// SELECT "users"."id", ... FROM "public"."users" WHERE "users"."active" = $1
// args: [true]
```

Значений в SQL нет никогда. Каждое — параметр привязки, включая те, что внутри фрагментов `Expr`.
