---
title: Оконные функции
description: Ранжирование, соседние строки и накопительные итоги — по строке, не схлопывая их.
---

## Что они делают

Агрегат схлопывает строки: `count(*)` по десяти строкам вернёт одну. Оконная
функция считает по строкам и **сохраняет каждую** — у каждой строки свой ответ,
вычисленный по строкам вокруг неё.

```go
rn := orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Posts.AuthorID)).
    OrderBy(orm.Of(Posts.CreatedAt).Desc()))
```

```sql
row_number() OVER (PARTITION BY author_id ORDER BY created_at DESC)
```

Читается так: **начинать нумерацию заново для каждого автора** и **внутри автора
сортировать от новых к старым**.

## Две половины

Оконная функция — это всегда функция плюс окно:

```go
orm.RowNumber().Over(orm.Window()...)
//  └ функция            └ окно, через которое она смотрит
```

Окно строит `orm.Window()`:

| Метод | Добавляет |
| --- | --- |
| `PartitionBy(...)` | `PARTITION BY` — начинать заново для каждой группы |
| `OrderBy(...)` | `ORDER BY` — порядок внутри секции |
| `Rows(start, end)` | рамку `ROWS` |
| `Range(start, end)` | рамку `RANGE` |
| `Groups(start, end)` | рамку `GROUPS` |

Любое можно опустить. `Over(orm.Window())` без настроек — одно окно на весь
результат.

## Как её использовать

Оконная функция — это выражение, поэтому она идёт в проекцию как любое другое:

```go
type Ranked struct {
    Title string
    N     int64
}

rn := orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Posts.AuthorID)).
    OrderBy(orm.Of(Posts.CreatedAt).Desc()))

shape := orm.Project2(
    orm.Of(Posts.Title), rn,
    func(title string, n int64) Ranked { return Ranked{title, n} },
)

rows, err := orm.Compose(pool, shape).From(Posts.Source()).All(ctx)
```

## Функции

### Ранжирование

```go
orm.RowNumber()    // 1, 2, 3, 4        -> int64
orm.Rank()         // 1, 2, 2, 4        -> int64  (при равенстве делят и пропускают)
orm.DenseRank()    // 1, 2, 2, 3        -> int64  (делят без пропуска)
orm.PercentRank()  // 0.0 … 1.0         -> float64
orm.CumeDist()     // накопленная доля  -> float64
orm.Ntile(4)       // номер квартиля    -> int32
```

`Rank` и `DenseRank` различаются только тем, что происходит после равенства, — и
именно это чаще всего понимают неправильно: `Rank` оставляет дыру, `DenseRank`
нет.

### Доступ к другим строкам

```go
orm.Lag(Posts.Score)         // значение предыдущей строки
orm.LagN(Posts.Score, 3)     // на три строки назад
orm.Lead(Posts.Score)        // значение следующей строки
orm.LeadN(Posts.Score, 3)
orm.FirstValue(Posts.Score)  // первое в рамке
orm.LastValue(Posts.Score)   // последнее в рамке
orm.NthValue(Posts.Score, 2) // второе
```

Все они возвращают **nullable**-форму типа колонки. У первой строки нет
предыдущей, у последней нет следующей — поэтому `Lag` по `NOT NULL` колонке всё
равно `*T`, и тип это говорит, вместо того чтобы дать NULL прийти туда, где его
негде держать.

### Агрегаты как оконные функции

Любой агрегат становится оконной функцией через `Over`:

```go
running := orm.SumInt64[Order, int64](Orders.Total).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow()))
```

```sql
sum(total) OVER (ORDER BY placed ASC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)
```

Это накопительный итог: каждая строка видит себя и всё, что было до неё.

## Рамки

Рамка сужает набор строк секции, которые видит функция. Границы:

```go
orm.UnboundedPreceding()  // начало секции
orm.Preceding(3)          // на три строки назад
orm.CurrentRow()
orm.Following(3)
orm.UnboundedFollowing()  // конец секции
```

```go
// скользящее среднее по семи строкам
orm.AvgInt64[Order, float64](Orders.Total).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.Preceding(6), orm.CurrentRow()))
```

`Rows` считает строки. `Range` считает по значению, поэтому равные по `ORDER BY`
попадают вместе. `Groups` считает группы равных. На данных с повторами это
разные ответы — поэтому есть все три, а не одна.

## Куда оконную функцию поставить нельзя

Ни в `WHERE`, ни в `HAVING`. PostgreSQL вычисляет окна после этих конструкций,
поэтому значения там ещё не существует.

Чтобы отфильтровать по нему, вычислите его в производной таблице и фильтруйте
снаружи — это и есть рецепт Top-N:

```go
rank := orm.Named("rn", orm.RowNumber().Over(orm.Window().
    PartitionBy(orm.Of(Orders.UserID)).
    OrderBy(orm.Of(Orders.Placed).Desc())))

ranked := orm.Sub("ranked", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    rank,
).From(Orders.Source()))

rows, err := orm.Compose(pool, shape).
    From(ranked).
    Where(orm.Ref(ranked, rank).Lte(3)).   // три последних заказа каждого
    All(ctx)
```

Целиком этот рецепт — в разделе [Сложные запросы](/ru/docs/cookbook/insane/).
