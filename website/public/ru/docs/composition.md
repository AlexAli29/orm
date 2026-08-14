# Композиция

> Джойны, CTE, производные таблицы и подзапросы — один компилятор, один запрос.

Source: https://ormgo.vercel.app/ru/docs/composition/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Типизированный запрос — это типизированный источник

В этом вся идея. `Sub` делает из запроса производную таблицу, `CTE` — элемент `WITH`, а `Compose` строит запрос над несколькими из них. Всё вкладывается через один компилятор, поэтому у запроса с CTE, производной таблицей, коррелированным подзапросом и оконной функцией **один список параметров**, пронумерованный в порядке написания SQL.

## Compose и джойны

```go
type Row struct {
    Email string
    Total *int64
}

shape := orm.Project2(
    orm.Of(Users.Email),
    orm.Opt(Orders.Total),
    func(email string, total *int64) Row { return Row{email, total} },
)

rows, err := orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Eq(Orders.UserID, Users.ID)).
    Where(orm.Cond(Users.Active.Eq(true))).
    OrderBy(orm.Of(Users.Email).Asc()).
    All(ctx)
```

Три функции переносят типизированные вещи в составной запрос:

- `orm.Of(col)` — сохраняет собственный тип колонки.
- `orm.Opt(col)` — nullable-форма, для outer-joined источника.
- `orm.Cond(pred)` — предикат сущности как составной.

## Nullability, наведённая источником

`orders.total` может быть `NOT NULL` и всё равно оказаться NULL здесь, потому что джойн способен дать строку, где правого источника нет вовсе. Поэтому тип расширяется, а чтение через `Of` отвергается:

```text
select-list expression 2 reads public.orders, which an outer join can leave with
no row, into a result that cannot hold NULL

an outer join makes every value of that source nullable, whatever the column's
own constraint says; read it with Opt or OptRef, which widen the result type
```

Это отказ на этапе сборки, а не ошибка сканирования, обнаруженная на той строке, которая случайно не совпала.

## Производные таблицы

```go
userID := orm.Named("user_id", orm.Of(Orders.UserID))
count  := orm.Named("order_count", orm.Count[orm.Composed]())

stats := orm.Sub("post_stats", orm.Rows(userID, count).
    From(Orders.Source()).
    GroupBy(orm.Of(Orders.UserID)))

rows, err := orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(stats, orm.Eq(orm.Ref(stats, userID), orm.Of(Users.ID))).
    All(ctx)
```

`Ref(src, out)` читает колонку источника строк, типизированную по объявлению, а не по строковому имени. `OptRef` — его форма для outer join.

## CTE

```go
active := orm.CTE("active_users", orm.Rows(
    orm.Named("id", orm.Of(Users.ID)),
).From(Users.Source()).Where(orm.Cond(Users.Active.Eq(true))))

rows, err := orm.Compose(pool, shape).
    With(active).
    From(active).
    Join(Orders.Source(), orm.Eq(Orders.UserID, orm.Ref(active, id))).
    All(ctx)
```

Возвращаемое значение — одновременно объявление, которое отрисует `With`, и ссылка, которую принимают `From` и джойны. Алиас через `As` даёт вторую ссылку на тот же элемент — так один CTE джойнится сам с собой.

`Materialized()` и `NotMaterialized()` доступны там, где оценка планировщика неверна. Оставить это незаданным — правильное умолчание.

## Рекурсивные CTE

```go
tree := orm.RecursiveCTE("tree",
    anchor,     // нерекурсивный терм
    recursive,  // терм, ссылающийся на "tree"
)
```

Это единственное место, где внутри библиотеки появляется `UNION`, потому что грамматика PostgreSQL требует его между якорем рекурсивного CTE и рекурсивным термом.

## Подзапросы

```go
orm.Exists[User](sub)        // EXISTS (...)
orm.NotExists[User](sub)
orm.InSub(Users.ID, sub)          // id IN (SELECT ...)
orm.Scalar[User, int64](sub) // скалярный подзапрос — всегда nullable
```

Скалярный подзапрос всегда nullable, и это семантика PostgreSQL, а не осторожность: ноль строк даёт NULL, одна — значение, а две — ошибку времени выполнения на сервере.

## Область видимости проверяется последовательно

Ссылка на вхождение, которого запрос не вводит, отвергается. Правила — собственные правила SQL: условие джойна видит источники, написанные левее, и тот, который присоединяет, и ничего правее.

```go
// отказ: c присоединяется после условия, которое его называет
q.From(a).Join(b, orm.Eq(b.X, c.X)).Join(c, ...)
```

Проверка по готовому множеству источников приняла бы это и оставила PostgreSQL жаловаться позже, своими словами, про колонку — а не про порядок написания.

## Один список параметров

```go
sql, args, _ := q.SQL()
// ... WHERE a = $1 AND b = $2 ... (SELECT ... WHERE c = $3) ... LIMIT ...
```

Вложенные запросы разделяют писателя, поэтому нумерация продолжается на всех уровнях. Ничто не отрисовывается отдельно и не склеивается.

## Разобранные примеры

### Панель автопарка

Каждая машина с последним известным показанием, причём машина, которая ни разу не
отчиталась, всё равно появляется — ради этого и нужен outer join.

```go
type Status struct {
    Plate string
    Fuel  *int32
}

shape := orm.Project2(
    orm.Of(Vehicles.Plate),
    orm.Opt(Readings.FuelPercent),
    func(plate string, fuel *int32) Status { return Status{plate, fuel} },
)

rows, err := orm.Compose(pool, shape).
    From(Vehicles.Source()).
    LeftJoin(Readings.Source(), orm.Eq(Readings.VehicleID, Vehicles.ID)).
    Where(orm.Cond(Vehicles.Retired.Eq(false))).
    OrderBy(orm.Of(Vehicles.Plate).Asc()).
    All(ctx)
```

`Opt`, а не `Of`: машина без показаний даёт строку, в которой источника показаний
нет вовсе. `*int32` — это тот же факт, выраженный типом.

### Каталог со счётчиками

Производная таблица считает отзывы и джойнится обратно, чтобы товары без отзывов
тоже попали в список:

```go
productID := orm.Named("product_id", orm.Of(Reviews.ProductID))
reviews   := orm.Named("reviews", orm.Count[orm.Composed]())

stats := orm.Sub("review_stats", orm.Rows(productID, reviews).
    From(Reviews.Source()).
    GroupBy(orm.Of(Reviews.ProductID)))

shape := orm.Project2(
    orm.Of(Products.Name),
    orm.OptRef(stats, reviews),
    func(name string, n *int64) Listing { return Listing{name, n} },
)

rows, err := orm.Compose(pool, shape).
    From(Products.Source()).
    LeftJoin(stats, orm.Eq(orm.Ref(stats, productID), orm.Of(Products.ID))).
    All(ctx)
```

`OptRef` — это `Ref` для источника, который outer join может оставить пустым.
Счётчик внутри производной таблицы не может быть NULL; прочитанный через этот
джойн — может.

### Когорта, названная один раз

CTE оправдан, когда один и тот же набор нужен дважды или когда имя делает запрос
читаемым:

```go
signups := orm.CTE("recent_signups", orm.Rows(
    orm.Named("id", orm.Of(Accounts.ID)),
).From(Accounts.Source()).
    Where(orm.Cond(Accounts.CreatedAt.Gte(weekStart))))

rows, err := orm.Compose(pool, shape).
    With(signups).
    From(signups).
    Join(Invoices.Source(), orm.Eq(Invoices.AccountID, orm.Ref(signups, id))).
    All(ctx)
```
