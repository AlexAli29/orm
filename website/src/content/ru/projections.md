---
title: Проекции
description: Чтение выбранной формы результата вместо целой сущности.
---

## Зачем они

Выборка сущности читает все отображённые колонки. Когда нужны две колонки и счётчик, проекция говорит это прямо — и читает результат в названный вами тип, без рефлексии на пути строки.

```go
type Summary struct {
    Email  string
    Orders int64
}

var Summaries = orm.Project2(
    Users.Email, orm.Count[User](),
    func(email string, n int64) Summary { return Summary{Email: email, Orders: n} },
)

rows, err := orm.Select(db.Users, Summaries).
    GroupBy(Users.Email).
    OrderBy(Users.Email.Asc()).
    All(ctx)
```

## Арность выписана

От `Project1` до `Project8`. Go не умеет сказать «список выражений, у которых все типы разные и все запомнены»: у вариадика один тип, а пакета параметров типа не существует. Каждая библиотека, которая делает вид, что умеет, делает это через `[]any` и приведения в рантайме — то есть переносит ошибку с компилятора на пользователя.

Что даёт выписанная арность: горячий путь строки не делает рефлексии, не держит карту и ничего не приводит. Сканирование — это N типизированных локальных переменных, один `Scan` и один вызов.

`Projection` — это значение. Постройте её один раз и используйте из многих запросов: она неизменяема и безопасна для совместного использования.

## Агрегаты

```go
orm.Count[User]()                 // count(*)          -> int64
orm.CountOf(Users.Bio)            // count(bio)        -> int64
orm.Max(Orders.Total)             // max(total)        -> *T
orm.Min(Orders.Placed)            // min(placed)       -> *T
orm.SumInt32(Orders.Qty)          // sum(qty)          -> *int64
orm.AvgInt64(Orders.Qty)          // avg(qty)          -> *N
orm.SumNumeric[Order, Decimal](Orders.Total)
```

Большинство агрегатов возвращают указатель, и это не осторожность: `max` по пустому множеству — это NULL, а результату без указателя его некуда положить. Исключение — `count`: он ноль.

## Группировка и HAVING

```go
orm.Select(db.Orders, byUser).
    Where(Orders.Placed.Gte(cutoff)).
    GroupBy(Orders.UserID).
    Having(orm.Count[Order]().Gt(3)).
    OrderBy(Orders.UserID.Asc()).
    All(ctx)
```

Порядок группировки ваш и никогда не сортируется — он определяет группировку, которую выполнит PostgreSQL.

## DISTINCT

```go
orm.Select(db.Orders, shape).Distinct()
orm.Select(db.Orders, shape).DistinctOn(Orders.UserID)
```

`DISTINCT ON` оставляет первую строку каждой группы равных значений — это другая конструкция, чем `DISTINCT`, и одновременно их задать нельзя: строитель скажет об этом сам, а не выдаст SQL, который отвергнет сервер.

## Имена результатов

Когда проекция становится производной таблицей или CTE, её колонкам нужны имена. Это `Named`, и имя обязательно, а не выводится: у `count(*)` его нет, а придумывание имени по отрисованному выражению сделало бы колонку производной таблицы зависящей от того, как компилятор её записал.

```go
userID := orm.Named("user_id", orm.Of(Orders.UserID))
total  := orm.Named("total", orm.Count[orm.Composed]())
```
