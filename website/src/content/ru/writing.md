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
    orm.OnConflict(Users.Email).DoUpdateSet(
        Users.Active.Set(true),
    ),
)

db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())
```

Цель конфликта — список колонок, по которому PostgreSQL его определяет; `DO UPDATE` видит конфликтную строку и `EXCLUDED`.

## Upsert подробно

`OnConflict` называет колонки, по которым PostgreSQL определяет конфликт, —
обычно это колонки уникального ограничения.

```go
// Ничего не делать, если запись уже есть.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())

// Взять значения пришедшей строки для названных колонок.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.Seen))

// Или задать их самому.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdateSet(
    Users.Seen.Set(time.Now()),
    Users.Hits.SetExpr(Users.Hits.Add(1)),
))
```

`DoUpdate` — частый случай, читается как «эти колонки берут новые значения».
`DoUpdateSet` — когда новое значение вычисляется: инкремент счётчика, выбор
большего из двух чисел, добавление в массив.

Частичному индексу нужен тот же предикат в конструкции конфликта:

```go
orm.OnConflict(Users.Email).Where(Users.Active.Eq(true)).DoNothing()
```

## RETURNING в update и delete

Вставка возвращает строки сама. Обновление и удаление — нет, поэтому просите:

```go
updated, err := orm.UpdateReturningEntity(
    db.Users.Update(ctx).Set(Users.Active, false).Where(Users.ID.Eq(id)),
).All(ctx)
// []User — строки в том виде, в каком они после обновления

deleted, err := orm.DeleteReturningEntity(
    db.Users.Delete(ctx).Where(Users.ID.Eq(id)),
).One(ctx)
```

Или вернуть проекцию вместо целой сущности:

```go
rows, err := orm.UpdateReturning(
    db.Users.Update(ctx).Set(Users.Active, false).Where(cond),
    Summaries,
).All(ctx)
```

Так вы узнаёте, что на самом деле сделала условная запись: счётчик строк говорит
сколько, а `RETURNING` — какие именно.

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
