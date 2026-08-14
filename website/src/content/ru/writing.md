---
title: Запись данных
description: Insert, update, delete и COPY — всё явно.
---

## Insert

```go
user, err := db.Users.Insert(ctx, User{
    Email: "a@example.com",
    Active: true,
})
// user.ID заполнен из RETURNING
```

Много сразу:

```go
users, err := db.Users.InsertMany(ctx, []User{u1, u2, u3})
```

## Значения по умолчанию

Нулевое значение Go — это значение. Запрос значения по умолчанию отдельный и явный:

```go
db.Users.Insert(ctx, User{}, orm.Default(Users.Active, Users.CreatedAt))
```

Названные колонки полностью исключаются из `INSERT`, поэтому применяется `DEFAULT` колонки — или её последовательность, или NULL для nullable-колонки без умолчания.

## Update

```go
n, err := db.Users.Update(ctx).
    Set(Users.Active, false).
    Where(Users.CreatedAt.Lt(cutoff)).
    Exec(ctx)
```

Обновление без `WHERE` отвергается:

```go
_, err := db.Users.Update(ctx).Set(Users.Active, false).Exec(ctx)
// errors.Is(err, orm.ErrMissingWhere)
```

Если только вы не скажете, что имелись в виду все строки:

```go
db.Users.Update(ctx).Set(Users.Active, false).All().Exec(ctx)
```

Присвоение выражения вместо значения:

```go
db.Orders.Update(ctx).
    SetExpr(Orders.Total, Orders.Net.AddCol(Orders.Tax)).
    Where(Orders.ID.Eq(id))
```

## Delete

```go
n, err := db.Users.Delete(ctx).Where(Users.ID.Eq(id)).Exec(ctx)
```

То же правило `ErrMissingWhere` и по той же причине.

## Upsert

```go
db.Users.Insert(ctx, user,
    orm.OnConflict(Users.Email).DoUpdate(
        orm.Assign(Users.Active, true),
    ),
)

db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())
```

Цель конфликта — список колонок, по которому PostgreSQL его определяет; `DO UPDATE` видит конфликтную строку и `EXCLUDED`.

## COPY

Для массовой загрузки `COPY` на порядок быстрее `INSERT`:

```go
n, err := db.Events.CopyFrom(ctx, events)
```

Потоком, чтобы строки не существовали все сразу:

```go
n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event, error) bool) {
    for scanner.Scan() {
        ev, err := parse(scanner.Text())
        if !yield(ev, err) {
            return
        }
    }
})
```

Подмножество колонок:

```go
n, err := orm.CopyColumns(ctx, db.Events, events, Events.ID, Events.Kind)
```

Упавший `COPY` падает как один оператор — не применяется ничего. Если он должен быть успешным вместе с другой работой, оберните в транзакцию.

## Возврат проекции

```go
rows, err := db.Users.Insert(ctx, user, orm.Returning(Summaries))
```
