# Сложные запросы

> Те, ради которых обычно берутся за сырой SQL, — составленные, типизированные и всё ещё одним запросом.

Source: https://ormgo.vercel.app/ru/docs/cookbook/insane/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
Каждый из них — один SQL-оператор с одним списком параметров. Ни один не собран из строк.

## Top-N по группам

Классика. Оконная функция внутри производной таблицы, фильтрация снаружи — потому что оконная функция не может стоять в `WHERE`.

```go
rank := orm.Named("rn", orm.RowNumber[orm.Composed]().
    PartitionBy(orm.Of(Orders.UserID)).
    OrderBy(orm.Of(Orders.Placed).Desc()))

ranked := orm.Sub("ranked", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("user_id", orm.Of(Orders.UserID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
    rank,
).From(Orders.Source()))

rows, err := orm.Compose(pool, shape).
    From(ranked).
    Where(orm.Ref(ranked, rank).Lte(3)).
    OrderBy(orm.Ref(ranked, userID).Asc()).
    All(ctx)
```

## Накопительный итог

```go
running := orm.Named("running", orm.SumInt64[orm.Composed, int64](orm.Of(Orders.Total)).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

## Промежутки и острова

Подряд идущие отрезки активности — находятся по разнице между номером строки и датой.

```go
grp := orm.Named("grp", orm.Sub(
    orm.Of(Events.Day),
    orm.RowNumber[orm.Composed]().OrderBy(orm.Of(Events.Day).Asc()),
))

islands := orm.Sub("islands", orm.Rows(
    orm.Named("day", orm.Of(Events.Day)),
    grp,
).From(Events.Source()))

// затем группировка по grp и min(day), max(day)
```

## Рекурсивная иерархия

Оргструктура любой глубины, одним запросом:

```go
anchor := orm.Rows(
    orm.Named("id", orm.Of(Employees.ID)),
    orm.Named("manager_id", orm.Opt(Employees.ManagerID)),
    orm.Named("depth", orm.Val(0)),
).From(Employees.Source()).Where(orm.Cond(Employees.ManagerID.IsNull()))

tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("id", orm.Of(Employees.ID)),
        orm.Named("manager_id", orm.Opt(Employees.ManagerID)),
        orm.Named("depth", orm.Ref(self, depth).Add(1)),
    ).From(Employees.Source()).
        Join(self, orm.Eq(Employees.ManagerID, orm.Ref(self, id)))
})
```

`UNION` появляется здесь и только здесь — грамматика PostgreSQL требует его между якорем рекурсивного CTE и рекурсивным термом. Это не общая композиция множеств; она — [UNION ALL](/ru/docs/union-all/).

## Коррелированный подзапрос в списке выборки

Последний заказ пользователя без джойна:

```go
last := orm.Scalar[User, time.Time](
    db.Orders.Query().
        Where(orm.Eq(Orders.UserID, Users.ID)).
        OrderBy(Orders.Placed.Desc()).
        Limit(1),
)

var shape = orm.Project2(
    Users.Email, last,
    func(email string, at *time.Time) Row { return Row{email, at} },
)
```

Скалярный подзапрос всегда nullable, потому что ноль строк даёт NULL. Тип это говорит.

## Антиджойн двумя способами

```go
// NOT EXISTS — обычно любимый вариант планировщика
db.Users.Query().Where(orm.NotExists(
    db.Orders.Query().Where(orm.Eq(Orders.UserID, Users.ID)),
))

// LEFT JOIN ... IS NULL, когда нужны ещё и колонки правой стороны
orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Eq(Orders.UserID, Users.ID)).
    Where(orm.Opt(Orders.ID).IsNull())
```

## LATERAL

Два последних заказа каждого пользователя, подзапрос коррелирует со строкой слева:

```go
recent := orm.Sub("recent", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
).From(Orders.Source()).
    Where(orm.Cond(orm.Eq(Orders.UserID, Users.ID))).
    OrderBy(orm.Of(Orders.Placed).Desc()).
    Limit(2))

orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoinLateral(recent)
```

## Сводная таблица

Счётчики по статусам как колонки, а не строки:

```go
var pivot = orm.Project3(
    Orders.UserID,
    orm.Count[Order]().Filter(Orders.Status.Eq("paid")),
    orm.Count[Order]().Filter(Orders.Status.Eq("refunded")),
    func(id int64, paid, refunded int64) Pivot { return Pivot{id, paid, refunded} },
)

orm.Select(db.Orders, pivot).GroupBy(Orders.UserID)
```

`FILTER` здесь лучше `CASE WHEN`: он говорит, что имеется в виду, и планировщик читает его лучше.

## Композиция множеств между отношениями

Таблица, представление и матпредставление в одном результате — законно, потому что проекции одинаковы:

```go
shape := orm.Project2(/* uuid, text */)

email := orm.Named("email", orm.Of(Users.Email))

rows, err := orm.UnionAll[Row](
    orm.Compose(pool, shape).From(Users.Source()),
    orm.Compose(pool, shape).From(ActiveUsers.Source()),
    orm.Compose(pool, shape).From(UserSummaries.Source()),
).OrderBy(email.Asc()).Limit(50).All(ctx)
```

Никакой особой обработки по виду источника. Источник чтения — это источник чтения.

## Self-join через алиас

```go
mgr := Employees.As("mgr")

orm.Compose(pool, shape).
    From(Employees.Source()).
    LeftJoin(mgr.Source(), orm.Eq(mgr.ID, Employees.ManagerID))
```

`As` возвращает второй источник, и дескриптор от одного нельзя применить к другому. Именно это делает self-join безопасным, а не упражнением в именовании.

## Пакетный upsert целого набора

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    for chunk := range slices.Chunk(rows, 1000) {
        if _, err := tx.Prices.InsertMany(ctx, chunk,
            orm.OnConflict(Prices.SKU).DoUpdate(Prices.Amount),
        ); err != nil {
            return err
        }
    }
    return nil
})
```

## Прочитать план, прежде чем чему-то верить

```go
plan, err := q.Explain(ctx)              // EXPLAIN, запрос не выполняется
plan, err := q.ExplainAnalyze(ctx)       // выполняется, и имя об этом говорит
report, err := q.PerformanceReport(ctx)  // план, форма и отпечаток
```

Имена различаются, потому что опасно различается поведение. Ничто здесь ничего не советует: планирует PostgreSQL, а решение о схеме или сервере требует всей нагрузки, а не одного запроса.
