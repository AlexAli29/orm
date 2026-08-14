---
title: Диапазоны и мультидиапазоны
description: Промежуток значений вместе с границами, а не две колонки, притворяющиеся одной.
---

## Зачем нужен отдельный тип

Пара концов не скажет, включена граница, исключена или бесконечна. `[1,10)` и
`(1,10]` содержат разные числа, а двум `int`-колонкам `lo` и `hi` негде записать,
что именно имелось в виду.

`Range[T]` несёт всю модель: два значения и два вида границ, плюс пустой
диапазон — а это не то же самое, что диапазон нулевой ширины.

## Как построить

```go
orm.Closed(1, 10)        // [1,10]  оба конца включены
orm.ClosedOpen(1, 10)    // [1,10)  обычный вариант для дат и времени
orm.OpenClosed(1, 10)    // (1,10]
orm.RangeFrom(t)         // [t,)    без верхней границы
orm.RangeUntil(t)        // (,t)    без нижней границы
orm.UnboundedRange[int]() // (,)
orm.EmptyRange[int]()    // пустой
```

`NewRange` — явная форма, когда границы вычисляются:

```go
orm.NewRange(lo, orm.BoundInclusive, hi, orm.BoundExclusive)
```

Чтение обратно:

```go
lo, loKind := r.LowerBound()
hi, hiKind := r.UpperBound()
if r.IsEmpty() { /* ... */ }
```

## Объявление колонки

```go
//orm:table public.bookings
type Booking struct {
    ID     int64          `orm:"pk,identity"`
    During orm.Range[time.Time] `orm:"pgtype:tstzrange"`
    Prices *orm.Range[int32]    `orm:"pgtype:int4range"`
}
```

Какой именно тип у `Range[time.Time]` — `daterange`, `tsrange` или `tstzrange` —
берётся из каталога, а не угадывается по Go-типу, поэтому его называет тег.

## Запросы

```go
db.Bookings.Query().Where(Bookings.During.Overlaps(r))         // &&
db.Bookings.Query().Where(Bookings.During.Contains(t))         // @> значение
db.Bookings.Query().Where(Bookings.During.ContainsRange(r))    // @> диапазон
db.Bookings.Query().Where(Bookings.During.ContainedBy(r))      // <@
db.Bookings.Query().Where(Bookings.During.Adjacent(r))         // -|-
db.Bookings.Query().Where(Bookings.During.StrictlyLeftOf(r))   // <<
db.Bookings.Query().Where(Bookings.During.StrictlyRightOf(r))  // >>
db.Bookings.Query().Where(Bookings.During.NotLeftOf(r))        // &>
db.Bookings.Query().Where(Bookings.During.NotRightOf(r))       // &<
```

`Contains` принимает значение, `ContainsRange` — диапазон. В PostgreSQL это
разные операторы, и здесь это разные методы, поэтому вы получаете именно тот,
который имели в виду.

Сравнение с другой колонкой той же сущности:

```go
Bookings.During.OverlapsCol(Bookings.Requested)
Bookings.During.ContainsCol(Bookings.Requested)
```

## Чтение границ в SQL

```go
Bookings.During.Lower()      // lower(during)     -> *T
Bookings.During.Upper()      // upper(during)     -> *T
Bookings.During.LowerInc()   // lower_inc(during) -> bool
Bookings.During.LowerInf()   // lower_inf(during) -> bool
Bookings.During.IsEmpty()    // isempty(during)   -> bool
```

`Lower` и `Upper` nullable, потому что у бесконечного конца нет значения.

Чтобы фильтровать по пустоте, используйте форму-предикат, а не сравнение значения:

```go
db.Bookings.Query().Where(Bookings.During.IsEmptyIs(true))
```

## Мультидиапазоны

Мультидиапазон — это упорядоченный набор непересекающихся диапазонов: то, что
получается при объединении двух диапазонов, которые не соприкасаются.

```go
//orm:table public.schedules
type Schedule struct {
    Free orm.Multirange[time.Time] `orm:"pgtype:tstzmultirange"`
}
```

```go
Schedules.Free.Contains(t)                 // значение
Schedules.Free.ContainsRange(r)            // один диапазон
Schedules.Free.ContainsMultirange(m)       // целый мультидиапазон
Schedules.Free.Overlaps(m)
Schedules.Free.OverlapsRange(r)
Schedules.Free.Merge()                     // range_merge -> Range[T]
Schedules.Free.IsEmpty()
```

`Merge` схлопывает мультидиапазон в один охватывающий диапазон — наименьший,
содержащий все элементы вместе с промежутками.

## Что канонизирует PostgreSQL

Дискретные диапазоны — `int4range`, `int8range`, `daterange` — возвращаются в
канонической форме, поэтому `[1,10]` приходит как `[1,11)`. Мультидиапазоны
канонизируются все. Это нормализация сервера, а не пакета, и вы читаете именно
те значения, которые он держит.
