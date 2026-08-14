---
title: Выражения
description: Условия, coalesce, приведения, строковые функции и аварийный выход для всего остального.
---

Всё здесь даёт `Expression` или `Value`, а значит, годится везде, где годятся
они: в списке выборки, в `WHERE`, в `ORDER BY`, в `GROUP BY` или внутри другого
выражения.

## Литералы

```go
orm.Val("pending")   // Expression[string, *string]
orm.Val(int64(0))
orm.Val(true)
```

Литерал становится параметром привязки, а не текстом в SQL. Это верно для любого
значения в пакете — и поэтому ни одна из этих функций не принимает строку
формата.

## CASE

```go
tier := orm.Case(orm.Cond(Orders.Total.Gte(1000)), orm.Val("gold")).
    When(orm.Cond(Orders.Total.Gte(100)), orm.Val("silver")).
    Else(orm.Val("bronze"))
```

```sql
CASE WHEN total >= $1 THEN $2 WHEN total >= $3 THEN $4 ELSE $5 END
```

`Case` принимает первое условие и его результат, `When` добавляет следующие,
`Else` закрывает. Все ветви несут один тип, поэтому `CASE`, смешивающий строку и
число, не компилируется.

**`Else` и `End` — разные завершения.** `Else` даёт запасное значение, поэтому
результат не может быть NULL и его тип — `T`. `End` закрывает без него, поэтому
результат равен NULL, когда ничего не совпало, и тип расширяется до `N`:

```go
grade := orm.Case(orm.Cond(Users.Score.Gte(90)), orm.Val("A")).End()
// Expression[*string, *string] — NULL, если балл ниже 90
```

## COALESCE и NULLIF

```go
orm.Coalesce(Users.Nickname, orm.Of(Users.Email))
// ник, а если он NULL — почта -> string, никогда не NULL
```

`Coalesce` принимает сначала nullable-значение, затем запасные. Результат не
nullable, потому что последнее запасное таковым не является, — в этом и смысл.

`CoalesceNull` — форма, где nullable все входы и результат тоже может им быть.

```go
orm.NullIf(orm.Of(Users.Bio), orm.Val(""))
// NULL, если bio — пустая строка, иначе bio
```

## Приведения типов

```go
orm.Cast(Users.ID, orm.Text)          // id::text     -> string
orm.Cast(Users.Score, orm.BigInt)     // score::bigint
orm.CastNull(Users.Bio, orm.Text)     // nullable-форма
```

Целевой тип — это значение `PGType`, а не строка, поэтому Go-тип результата
определяется приведением, а не утверждается после. Встроенные:

```go
orm.Text  orm.SmallInt  orm.Integer  orm.BigInt
orm.Boolean  orm.ByteA  orm.DoublePrecision
orm.Date  orm.Timestamptz
```

## Строковые функции

```go
orm.Upper(Users.Email)
orm.Lower(Users.Email)
orm.Trim(Users.Name)
orm.Concat(orm.Of(Users.First), orm.Val(" "), orm.Of(Users.Last))
```

У каждой есть форма `…Null`, принимающая nullable-колонку и возвращающая
nullable-результат, потому что `upper(NULL)` — это NULL.

## Арифметика

Упорядоченные колонки несут операторы прямо на себе:

```go
Orders.Total.Add(10)
Orders.Total.Sub(10)
Orders.Total.Mul(2)
Orders.Total.Div(2)
Orders.Total.AddCol(Orders.Tax)   // колонка + колонка
```

## Всё остальное, что есть в PostgreSQL

`Fn` вызывает функцию, которую пакет не оборачивает. Вы задаёте имя, аргументы
и — через параметр типа — то, что она возвращает:

```go
// pg_size_pretty(pg_total_relation_size('users'))
size := orm.Fn[User, string]("pg_size_pretty",
    orm.ArgRaw("pg_total_relation_size('users')"))

// greatest(score, 0)
floor := orm.Fn[User, int32]("greatest", orm.ArgOf(Users.Score), orm.ArgValue(0))
```

| Форма | Возвращает | Для чего |
| --- | --- | --- |
| `Fn[E, T]` | `Value[E, T]` | значение в запросе по сущности |
| `FnNull[E, T]` | `Value[E, *T]` | когда может быть NULL |
| `FnExpr[T]` | `Expression[T, *T]` | значение в составном запросе |
| `FnExprNull[T]` | `Expression[*T, *T]` | |
| `FnPredicate[E]` | `Predicate[E]` | функция, возвращающая boolean |

Аргументы строятся, а не форматируются:

```go
orm.ArgValue(v)          // параметр привязки
orm.ArgOf(Users.Email)   // колонка
orm.ArgOpt(Users.Bio)    // nullable-колонка
orm.ArgCast(v, "uuid")   // параметр с явным приведением
orm.ArgRaw("now()")      // текст SQL, без значений внутри
```

Параметр типа — это обещание о том, что вернёт функция, и оно не проверяется по
PostgreSQL. Ошибётесь — упадёт сканирование. Это аварийный выход, и он честно
им является.

## Сырые фрагменты

Когда даже `Fn` не той формы:

```go
db.Users.Query().Where(orm.Expr[User]("age(created_at) > interval ?", "1 year"))
```

`Expr` принимает текст SQL намеренно. Значения, вставленные в него, — нет:
каждый `?` становится параметром привязки, а плейсхолдеры фрагмента считаются
против переданных аргументов, поэтому несоответствие — это ошибка сборки, а не
невнятная ошибка сервера.

## Разобранные примеры

### Тарифная категория посылки

`CASE`, превращающий число в метку прямо в SQL, чтобы по ней можно было
группировать:

```go
band := orm.Case(orm.Cond(Parcels.Grams.Lt(500)), orm.Val("letter")).
    When(orm.Cond(Parcels.Grams.Lt(2000)), orm.Val("small")).
    When(orm.Cond(Parcels.Grams.Lt(20000)), orm.Val("parcel")).
    Else(orm.Val("freight"))

var byBand = orm.Project2(
    band, orm.Count[orm.Composed](),
    func(b string, n int64) Band { return Band{b, n} },
)

orm.Compose(pool, byBand).From(Parcels.Source()).GroupBy(band).All(ctx)
```

Сделать это в Go значило бы вытащить все посылки, чтобы посчитать четыре числа.

### Отображаемое имя, которое никогда не пустое

```go
name := orm.Coalesce(Members.Nickname, orm.Of(Members.Email))
```

`Nickname` nullable, `Email` — нет, поэтому результат не может быть NULL и его
тип — `string`. Цепочка запасных значений это доказывает, а не подразумевает.

### Пустое как отсутствующее

```go
// Пустая заметка — не заметка.
note := orm.NullIf(orm.Of(Tickets.Note), orm.Val(""))
```

### Регистронезависимое сравнение, которое пользуется индексом

```go
// С индексом по lower(email) это им воспользуется; ILike — нет.
orm.Compose(pool, shape).From(Members.Source()).
    Where(orm.Eq(orm.Lower(Members.Email), orm.Lower(orm.Val(input))))
```

### То, что есть в PostgreSQL и не обёрнуто пакетом

```go
// greatest(stock - reserved, 0)
available := orm.Fn[Item, int32]("greatest",
    orm.ArgOf(Items.Stock.SubCol(Items.Reserved)),
    orm.ArgValue(int32(0)))

// Функция, возвращающая boolean, в роли предиката.
db.Items.Query().Where(orm.FnPredicate[Item]("pg_try_advisory_lock", orm.ArgOf(Items.ID)))
```

Параметр типа — ваше обещание о типе результата. По PostgreSQL оно не
проверяется, и именно это делает такой вызов аварийным выходом, а не основной
дорогой.
