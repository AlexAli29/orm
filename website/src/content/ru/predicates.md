---
title: Предикаты
description: Какие сравнения даёт каждый тип колонки и почему наборы различаются.
---

## Набор зависит от типа

Предикат есть у дескриптора тогда, когда PostgreSQL определяет операцию для этого типа. Поэтому списки различаются, и поэтому разница — ошибка компиляции, а не рантайма.

### Любая колонка

```go
Users.Email.Eq("a@example.com")
Users.Email.Ne("a@example.com")
Users.Email.In("a@example.com", "b@example.com")
Users.ID.InSlice(ids)          // для готового среза
```

`NotIn` намеренно нет. `orm.Not(Users.ID.In(...))` говорит то же самое и говорит один раз.

### Упорядоченные колонки

Любой тип, который PostgreSQL упорядочивает, — целые, дробные, текст, даты, `uuid`, `inet`, `interval`:

```go
Users.CreatedAt.Gt(t)
Users.CreatedAt.Gte(t)
Users.CreatedAt.Lt(t)
Users.CreatedAt.Lte(t)
Users.CreatedAt.Between(from, to)
Users.CreatedAt.Asc()
Users.CreatedAt.Desc()
```

У `jsonb` и `bytea` есть полный порядок для индексов, но сравнение двух таких значений не отвечает ни на чей вопрос, поэтому они остаются на равенстве.

### Текстовые колонки

```go
Users.Email.Like("%@example.com")
Users.Email.ILike("%@EXAMPLE.com")
Users.Email.NotLike("%@spam.test")
Users.Email.HasPrefix("admin")
Users.Email.HasSuffix(".org")
Users.Email.Contains("example")
```

### Nullable-колонки

Только у них есть эти методы, потому что на `NOT NULL` колонке они отвечали бы на невозможный вопрос:

```go
Users.Bio.IsNull()
Users.Bio.IsNotNull()
Users.Bio.Eq("hello")   // тоже доступно: это bio = 'hello'
```

### Массивы

```go
Users.Tags.Contains([]string{"go"})     // @>
Users.Tags.ContainedBy(all)             // <@
Users.Tags.Overlaps([]string{"a", "b"}) // &&
Users.Tags.HasElement("go")             // = ANY
Users.Tags.Len().Eq(3)
```

### JSONB

```go
Users.Meta.HasKey("plan")
Users.Meta.Contains(orm.JSONB(`{"plan":"pro"}`))
Users.Meta.Path("billing", "tier").AsText().Eq("gold")
```

### Диапазоны

```go
Bookings.During.Overlaps(r)
Bookings.During.ContainsElement(t)
Bookings.During.StrictlyLeftOf(other)
Bookings.During.Adjacent(other)
```

### Полнотекстовый поиск

```go
Docs.Search.Matches(orm.PlainToTSQuery("russian", "postgres маппер"))
Docs.Search.Rank(query).Desc()
```

## Комбинирование

```go
orm.And(a, b, c)
orm.Or(a, b)
orm.Not(a)
```

Они вкладываются, и компилятор удерживает их на одной сущности: `orm.And`, смешивающий `Predicate[User]` и `Predicate[Order]`, не компилируется. Это не педантизм — такой предикат дал бы SQL, называющий таблицу, которой в запросе нет.

## Сравнение двух колонок

Сравнения «колонка с колонкой» — это свободные функции, а не методы, и они дают
`Predicate[Composed]`, поэтому их место в составном запросе, а не в `Where` по
сущности:

```go
orm.Compose(pool, shape).
    From(Orders.Source()).
    Where(orm.Gt(Orders.Total, Orders.Paid))
```

`Eq`, `Ne`, `Gt`, `Gte`, `Lt` и `Lte` принимают два типизированных значения, и
обе стороны обязаны нести один тип значения. Справа может стоять выражение:

```go
orm.Eq(Orders.Total, Orders.Net.AddCol(Orders.Tax))
```

Арифметика на колонке — это метод: `Add`, `Sub`, `Mul`, `Div` со значением и
`AddCol` или `SubCol` с другой колонкой той же сущности.

## Сырые фрагменты

Когда типизированной формы нет:

```go
db.Users.Query().Where(orm.Expr[User]("age(created_at) > interval ?", "1 year"))
```

`Expr` принимает текст SQL намеренно. Значения, вставленные в него, — нет: каждый `?` становится параметром привязки, а плейсхолдеры фрагмента проверяются против переданных аргументов.
