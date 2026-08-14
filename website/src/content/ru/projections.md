---
title: Проекции
description: Выбор нужных колонок в тип, который выбрали вы.
---

## Что такое проекция

Проекция — это две вещи, связанные вместе:

1. **какие выражения выбрать** и
2. **функция, которая превращает эти значения в ваш тип результата**.

Вот и вся идея. Всё дальше — это она же, но с большим числом колонок.

## Начнём с одной колонки

Запрос по сущности возвращает целые сущности:

```go
users, err := db.Users.Query().All(ctx)
// []User — SELECT id, email, bio, active, created_at FROM users
```

Допустим, нужны только адреса. Скажите это прямо:

```go
var Emails = orm.Project1(
    Users.Email,                        // выбрать вот это
    func(email string) string {         // и вернуть вот так
        return email
    },
)

emails, err := orm.Select(db.Users, Emails).All(ctx)
// []string — SELECT email FROM users
```

`[]string`, а не `[]User`. База прислала одну колонку вместо пяти, а тип результата — тот, который вернула ваша функция.

## Две колонки, в структуру

Тип результата ваш. Обычно это объявленная вами структура:

```go
type Summary struct {
    ID    int64
    Email string
}

var Summaries = orm.Project2(
    Users.ID,       // 1-е выражение
    Users.Email,    // 2-е выражение
    func(id int64, email string) Summary {
        //   ↑ 1-й параметр    ↑ 2-й параметр
        return Summary{ID: id, Email: email}
    },
)

rows, err := orm.Select(db.Users, Summaries).All(ctx)
// []Summary — SELECT id, email FROM users
```

**Читайте вызов сверху вниз.** `Project2` принимает два выражения, поэтому функция принимает два параметра — в том же порядке. Первый параметр — первая колонка, второй — вторая.

**Типы параметров выбираете не вы.** `Users.ID` — это `bigint`, поэтому первый параметр обязан быть `int64`. `Users.Email` — это `text`, поэтому второй обязан быть `string`. Напишете `func(id string, ...)` — не скомпилируется, и несоответствие поймают там, где вы его написали, а не когда придёт строка.

Поэтому число стоит прямо в имени: `Project1` для одного выражения, `Project2` для двух и так до `Project8`.

## Всё, что даёт значение

Выражение не обязано быть простой колонкой. Агрегат — тоже выражение:

```go
type ByStatus struct {
    Status string
    Count  int64
}

var byStatus = orm.Project2(
    Orders.Status,        // колонка
    orm.Count[Order](),   // count(*)
    func(status string, n int64) ByStatus {
        return ByStatus{Status: status, Count: n}
    },
)

rows, err := orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    All(ctx)
// []ByStatus — SELECT status, count(*) FROM orders GROUP BY status
```

`orm.Count[Order]()` возвращает `int64`, поэтому второй параметр — `int64`. Правило то же самое.

## Где можно использовать проекцию

Проекция говорит, *что выбрать*. Откуда — говорит что-то другое:

```go
orm.Select(db.Users, Summaries)                     // из таблицы users
orm.SelectFrom(db.Users, Users.As("u"), Summaries)  // из её алиаса
orm.Compose(pool, shape)                            // из источников, которые вы соединили
```

`Projection` — это значение, а не запрос. Постройте её один раз на уровне пакета и используйте из скольких угодно запросов: она неизменяема и безопасна для совместного использования.

```go
// одна форма, три запроса
active, _ := orm.Select(db.Users, Summaries).Where(Users.Active.Eq(true)).All(ctx)
recent, _ := orm.Select(db.Users, Summaries).OrderBy(Users.ID.Desc()).Limit(10).All(ctx)
count,  _ := orm.Select(db.Users, Summaries).Count(ctx)
```

## Когда она нужна

| Что использовать | Когда |
| --- | --- |
| Запрос по сущности | Нужна строка и большая часть её колонок |
| Проекцию | Нужны несколько колонок, агрегат или форма, которая не является строкой таблицы |

Проекция — ещё и единственный способ выбрать то, что вообще не является колонкой: счётчик, сумму, выражение, значение из CTE.

## Агрегаты

```go
orm.Count[User]()                 // count(*)      -> int64
orm.CountOf(Users.Bio)            // count(bio)    -> int64
orm.Max(Orders.Total)             // max(total)    -> *T
orm.Min(Orders.Placed)            // min(placed)   -> *T
orm.SumInt32(Orders.Qty)          // sum(qty)      -> *int64
orm.AvgInt64(Orders.Qty)          // avg(qty)      -> *N
orm.SumNumeric[Order, Decimal](Orders.Total)
```

Большинство возвращает **указатель**, и это не осторожность. `max` по пустому множеству — это NULL, а результату без указателя его некуда положить:

```go
var maxTotal = orm.Project1(
    orm.Max(Orders.Total),
    func(v *int64) *int64 { return v },  // nil, когда таблица пуста
)
```

Исключение — `count`: по пустому множеству он ноль, поэтому это обычный `int64`.

## Группировка и HAVING

```go
orm.Select(db.Orders, byStatus).
    Where(Orders.Placed.Gte(cutoff)).
    GroupBy(Orders.Status).
    Having(orm.Count[Order]().Gt(100)).
    OrderBy(Orders.Status.Asc()).
    All(ctx)
```

Порядок группировки ваш и никогда не сортируется — он определяет группировку, которую выполнит PostgreSQL.

## DISTINCT

```go
orm.Select(db.Orders, shape).Distinct()
orm.Select(db.Orders, shape).DistinctOn(Orders.UserID)
```

`DISTINCT ON` оставляет первую строку каждой группы равных значений — это другая конструкция, чем `DISTINCT`. Одновременно их задать нельзя: строитель скажет об этом сам, а не выдаст SQL, который отвергнет сервер.

## Имена результатов

Когда проекция становится производной таблицей или CTE, её колонкам нужны имена, потому что к ним будут обращаться снаружи:

```go
userID := orm.Named("user_id", orm.Of(Orders.UserID))
total  := orm.Named("total", orm.Count[orm.Composed]())
```

Имя обязательно, а не выводится. У `count(*)` его нет, а придумывание имени по отрисованному выражению сделало бы колонку производной таблицы зависящей от того, как её записал компилятор. См. [Композицию](/ru/docs/composition/).

## Почему конструкторов восемь

Эта часть — про Go, а не про SQL, и она здесь для любопытных, а не потому, что она нужна для работы.

Go не умеет выразить «список выражений, у которых все типы разные и все запомнены». У вариадика один тип, а пакета параметров типа не существует. Библиотеки, которые делают вид, что умеют, делают это через `[]any` и приведения в рантайме — то есть переносят ошибку с компилятора на пользователя.

Выписанное число аргументов — от `Project1` до `Project8` — это и есть то, что покупает проверки выше. Оно же оставляет горячий путь строки без рефлексии, без карты и без приведений: сканирование — это N типизированных локальных переменных, один `Scan` и один вызов.

## Разобранные примеры

### Отчёт по выставленным счетам

Выручка по тарифам за месяц и число списаний рядом — та форма, которая нужна
странице финансов:

```go
type PlanRevenue struct {
    Plan    string
    Charges int64
    Total   *int64
}

var planRevenue = orm.Project3(
    Invoices.Plan,
    orm.Count[Invoice](),
    orm.SumInt32(Invoices.AmountCents),
    func(plan string, n int64, total *int64) PlanRevenue {
        return PlanRevenue{Plan: plan, Charges: n, Total: total}
    },
)

rows, err := orm.Select(db.Invoices, planRevenue).
    Where(Invoices.IssuedAt.Between(monthStart, monthEnd)).
    GroupBy(Invoices.Plan).
    OrderBy(Invoices.Plan.Asc()).
    All(ctx)
```

`Total` — это `*int64`, потому что `sum` по пустому множеству даёт NULL, а тариф
без счетов в этом окне — ровно такой случай. Счётчик рядом не указатель, потому
что `count` по пустому множеству равен нулю.

### Список устройств

Одна колонка в срез — без структуры, потому что держать нечего:

```go
var serials = orm.Project1(
    Devices.Serial,
    func(s string) string { return s },
)

offline, err := orm.Select(db.Devices, serials).
    Where(Devices.LastSeenAt.Lt(cutoff)).
    OrderBy(Devices.Serial.Asc()).
    All(ctx)
// []string
```

### Схема зала

Две колонки в ключ карты: тип результата — это то, что вернула функция, и он не
обязан быть структурой:

```go
type Seat struct{ Row, Number int32 }

var seats = orm.Project2(
    Tickets.SeatRow, Tickets.SeatNumber,
    func(r, n int32) Seat { return Seat{Row: r, Number: n} },
)

taken, err := orm.Select(db.Tickets, seats).
    Where(Tickets.EventID.Eq(eventID)).
    Where(Tickets.CancelledAt.IsNull()).
    All(ctx)

occupied := make(map[Seat]bool, len(taken))
for _, s := range taken {
    occupied[s] = true
}
```
