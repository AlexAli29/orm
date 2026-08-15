---
title: Сложные запросы
description: Те, ради которых обычно берутся за сырой SQL, — собранные, типизированные и по-прежнему одним запросом.
---

Каждый из них — один SQL-запрос с одним списком параметров. Ни один не склеен из строк.

Здесь используется API композиции, а не запрос по сущности, потому что нужно именно оно: производная таблица, CTE, LATERAL, операция над множествами. Словарь небольшой и повторяется. `orm.Rows` перечисляет колонки, которые отдаёт подзапрос, `orm.Named` даёт одной из них имя, `orm.Sub` превращает это в производную таблицу, `orm.Ref` читает именованную колонку обратно, а `orm.Cond` поднимает предикат по сущности в область составного запроса. Всё дальше — эти пять вещей и соединения.

## Первые N в каждой группе

Классика. Оконная функция внутри производной таблицы, фильтрация снаружи — потому что оконная функция не может стоять в `WHERE`.

```go
rank := orm.Named("rn", orm.RowNumber().
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

### То же самое через LATERAL, что часто быстрее

Когда родительское множество невелико, а у дочерней таблицы есть индекс по ключу
соединения, LATERAL выигрывает у ранжирования всей дочерней таблицы с
последующим выбрасыванием почти всего:

```go
top := orm.Sub("top", orm.Rows(
    orm.Named("id", orm.Of(Orders.ID)),
    orm.Named("placed", orm.Of(Orders.Placed)),
).From(Orders.Source()).
    Where(orm.Eq(Orders.UserID, Users.ID)).
    OrderBy(orm.Of(Orders.Placed).Desc()).
    Limit(3))

orm.Compose(pool, shape).From(Users.Source()).LeftJoinLateral(top)
```

Два плана на один вопрос. Измеряйте, а не предполагайте — `Explain` под рукой.

## Накопительный итог

```go
running := orm.Named("running", orm.SumInt64[orm.Composed, int64](orm.Of(Orders.Total)).Over(orm.Window().
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

### Накопительный итог, сбрасывающийся каждый месяц

```go
month := orm.DateTrunc(orm.Month, Orders.Placed)

perMonth := orm.Named("mtd", orm.SumInt64[orm.Composed](orm.Of(Orders.Total)).Over(orm.Window().
    PartitionBy(month).
    OrderBy(orm.Of(Orders.Placed).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

Сброс — это раздел. Больше ничего не меняется.

### Баланс после каждой проводки

Форма, которая нужна банковской выписке: каждая строка несёт баланс на себя саму:

```go
balance := orm.Named("balance", orm.SumInt64[orm.Composed](orm.Of(Entries.Cents)).Over(orm.Window().
    PartitionBy(orm.Of(Entries.AccountID)).
    OrderBy(orm.Of(Entries.At).Asc(), orm.Of(Entries.ID).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())))
```

`ID` в сортировке здесь не для красоты. Две проводки в одну микросекунду иначе
получат порядок, который выбрал план, а выписка, где балансы меняются от запуска
к запуску, хуже, чем просто неверная.

## Разрывы и острова

Непрерывные периоды активности, найденные по разнице между номером строки и датой.

```go
grp := orm.Named("grp", orm.Sub(
    orm.Of(Events.Day),
    orm.RowNumber().OrderBy(orm.Of(Events.Day).Asc()),
))

islands := orm.Sub("islands", orm.Rows(
    orm.Named("day", orm.Of(Events.Day)),
    grp,
).From(Events.Source()))

// then group by grp and take min(day), max(day)
```

### Длина серии, числом

Группировка островов даёт длины серий — серия входов, окно бесперебойной работы,
число дней подряд с отгрузками:

```go
orm.Compose(pool, streaks).
    From(islands).
    GroupBy(orm.Ref(islands, grp)).
    Having(orm.Count[orm.Composed]().Gte(3)).
    OrderBy(orm.Ref(islands, grp).Asc())
```

### Разрывы: периоды, когда ничего не происходило

Обратная сторона той же идеи: каждая строка в паре с предыдущей и расстояние
между ними:

```go
prev := orm.Named("prev", orm.Lag(Readings.At).Over(orm.Window().
    PartitionBy(orm.Of(Readings.SensorID)).
    OrderBy(orm.Of(Readings.At).Asc())))

gaps := orm.Sub("gaps", orm.Rows(
    orm.Named("sensor_id", orm.Of(Readings.SensorID)),
    orm.Named("at", orm.Of(Readings.At)),
    prev,
).From(Readings.Source()))
```

Датчик, который должен отчитываться каждую минуту и имеет двухчасовой разрыв, —
это неисправность, и вот запрос, который её находит, не вытягивая в Go годовой
объём измерений.

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

`UNION` встречается здесь и только здесь: грамматика PostgreSQL требует его между якорем рекурсивного CTE и рекурсивной частью. Это не общая операция над множествами, она — [UNION ALL](/ru/docs/union-all/).

### Всё, что лежит под одним узлом

Тот же обход, начатый не от корня: поддерево, папка, ветка комментариев:

```go
anchor := orm.Rows(
    orm.Named("id", orm.Of(Categories.ID)),
    orm.Named("parent_id", orm.Opt(Categories.ParentID)),
).From(Categories.Source()).Where(orm.Cond(Categories.ID.Eq(root)))
```

### Материализованный путь, собираемый по дороге вниз

Чтобы «хлебные крошки» не стоили по запросу на уровень:

```go
tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("id", orm.Of(Categories.ID)),
        orm.Named("path", orm.Concat(orm.Ref(self, path), orm.Val(" / "), orm.Of(Categories.Name))),
    ).From(Categories.Source()).
        Join(self, orm.Eq(Categories.ParentID, orm.Ref(self, id)))
})
```

### Спецификация изделия, с умножением количеств вниз

Каждый шаг рекурсии умножает на количество родителя, поэтому число у листа — это
то, что действительно нужно закупить:

```go
tree := orm.RecursiveCTE("bom", anchor, func(self *orm.Source) orm.Term {
    return orm.Rows(
        orm.Named("part_id", orm.Of(Assemblies.ChildID)),
        orm.Named("qty", orm.Ref(self, qty).Mul(orm.Of(Assemblies.Qty))),
    ).From(Assemblies.Source()).
        Join(self, orm.Eq(Assemblies.ParentID, orm.Ref(self, partID)))
})
```

## Коррелированный подзапрос в списке выборки

Самый свежий заказ каждого пользователя, без соединения:

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

Скалярный подзапрос всегда nullable, потому что «ни одной строки» даёт NULL. Тип это и говорит.

### Счётчик рядом с каждой строкой

```go
n := orm.Scalar[User, int64](
    db.Orders.Query().Where(orm.Eq(Orders.UserID, Users.ID)),
)
```

Удобно — и по одному подзапросу на строку. Когда список длинный, один `GROUP BY`
с соединением даёт тот же ответ дешевле: эта форма оправдана на странице из
двадцати строк, а не на выгрузке из двухсот тысяч.

### Два коррелированных значения без двух подзапросов

```go
stats := orm.Sub("stats", orm.Rows(
    orm.Named("user_id", orm.Of(Orders.UserID)),
    orm.Named("n", orm.Count[orm.Composed]()),
    orm.Named("last", orm.Max(Orders.Placed)),
).From(Orders.Source()).GroupBy(orm.Of(Orders.UserID)))

orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(stats, orm.Eq(Users.ID, orm.Ref(stats, userID)))
```

## Антисоединение, двумя способами

```go
// NOT EXISTS — usually the planner's favourite
db.Users.Query().Where(orm.NotExists(
    db.Orders.Query().Where(orm.Eq(Orders.UserID, Users.ID)),
))

// LEFT JOIN ... IS NULL, when you also want columns from the right side
orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Eq(Orders.UserID, Users.ID)).
    Where(orm.Opt(Orders.ID).IsNull())
```

### Полусоединение: родители, у которых есть хотя бы одно совпадение, по разу каждый

```go
db.Users.Query().Where(orm.Exists[User](
    db.Orders.Query().Where(orm.And(
        orm.Eq(Orders.UserID, Users.ID),
        Orders.Status.Eq("paid"),
    )),
))
```

Соединение дало бы по строке на каждый подходящий заказ. `EXISTS` даёт по строке
на пользователя — а это и значит «пользователи, которые платили».

### Почему NOT IN — тот, которого стоит избегать

```go
db.Users.Query().Where(orm.NotExists[User](
    db.Blocks.Query().Where(orm.Eq(Blocks.UserID, Users.ID)),
))
```

Если подзапрос `NOT IN` вернёт хотя бы один NULL, весь результат окажется пуст —
молча и только в те дни, когда NULL в данных есть. У `NOT EXISTS` такого люка
нет, поэтому на этой странице показан он, а не первый.

## LATERAL

Два самых свежих заказа каждого пользователя: подзапрос коррелирует со строкой слева от него:

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

### Агрегат на строку, который соединением не выразить

Траты каждого клиента в окне, своём для каждой строки:

```go
window := orm.Sub("window", orm.Rows(
    orm.Named("spent", orm.SumInt64[orm.Composed](orm.Of(Orders.Total))),
).From(Orders.Source()).Where(orm.Cond(orm.And(
    orm.Eq(Orders.UserID, Users.ID),
    Orders.Placed.Between(from, to),
))))

orm.Compose(pool, shape).From(Users.Source()).LeftJoinLateral(window)
```

### Внутренний LATERAL, чтобы отбросить строки без совпадений

```go
orm.Compose(pool, shape).From(Users.Source()).JoinLateral(recent)
```

`LeftJoinLateral` оставляет пользователей без заказов и даёт им NULL.
`JoinLateral` их отбрасывает. Выбор ровно тот же, что и у обычного соединения.

## Разворот в колонки

Счётчики по статусам — колонками, а не строками:

```go
var pivot = orm.Project3(
    Orders.UserID,
    orm.Count[Order]().Filter(Orders.Status.Eq("paid")),
    orm.Count[Order]().Filter(Orders.Status.Eq("refunded")),
    func(id int64, paid, refunded int64) Pivot { return Pivot{id, paid, refunded} },
)

orm.Select(db.Orders, pivot).GroupBy(Orders.UserID)
```

`FILTER` здесь лучше `CASE WHEN`: он говорит то, что имеется в виду, и планировщик читает его лучше.

### Суммы по корзинам, а не только счётчики

```go
var revenue = orm.Project4(
    Sales.Region,
    orm.SumInt32(Sales.Cents).Filter(Sales.Channel.Eq("web")),
    orm.SumInt32(Sales.Cents).Filter(Sales.Channel.Eq("retail")),
    orm.SumInt32(Sales.Cents).Filter(Sales.Channel.Eq("partner")),
    func(region string, web, retail, partner *int64) Revenue {
        return Revenue{region, web, retail, partner}
    },
)
```

Набор колонок фиксирован на этапе компиляции, и это честное ограничение: SQL не
умеет выдавать набор колонок, зависящий от данных, и типизированный API тоже не
умеет. Если корзины выясняются во время выполнения, ответ — строки, а разворот
происходит на стороне потребителя.

## Композиция множеств поверх разных источников

Таблица, представление и материализованное представление в одном результате — законно, потому что проекции совпадают:

```go
shape := orm.Project2(
    orm.Of(Users.ID), orm.Of(Users.Email),
    func(id uuid.UUID, email string) Row { return Row{id, email} },
)

email := orm.Named("email", orm.Of(Users.Email))

rows, err := orm.UnionAll[Row](
    orm.Compose(pool, shape).From(Users.Source()),
    orm.Compose(pool, shape).From(ActiveUsers.Source()),
    orm.Compose(pool, shape).From(UserSummaries.Source()),
).OrderBy(email.Asc()).Limit(50).All(ctx)
```

Никакой особой обработки по виду источника. Источник для чтения — это источник для чтения.

### Объединённая лента событий

Три таблицы, у которых общего — только отметка времени и метка:

```go
rows, err := orm.UnionAll[Item](
    orm.Compose(pool, feed).From(Posts.Source()),
    orm.Compose(pool, feed).From(Comments.Source()),
    orm.Compose(pool, feed).From(Follows.Source()),
).OrderBy(at.Desc()).Limit(50).All(ctx)
```

`ORDER BY` и `LIMIT` применяются к объединению, а не к ветви: одна отсортированная
лента, а не три отсортированных списка подряд.

### Строки, которые есть в одной таблице и нет в другой, в обе стороны

```go
orm.UnionAll[Diff](
    orm.Compose(pool, diff).From(Expected.Source()).Where(orm.NotExists[orm.Composed](
        orm.Compose(pool, one).From(Actual.Source()).Where(orm.Eq(Actual.Key, Expected.Key)))),
    orm.Compose(pool, diff).From(Actual.Source()).Where(orm.NotExists[orm.Composed](
        orm.Compose(pool, one).From(Expected.Source()).Where(orm.Eq(Expected.Key, Actual.Key)))),
)
```

Сверка — чего не хватает и что лишнее — одним запросом.

## Самосоединение через псевдоним

```go
mgr := Employees.As("mgr")

orm.Compose(pool, shape).
    From(Employees.Source()).
    LeftJoin(mgr.Source(), orm.Eq(mgr.ID, Employees.ManagerID))
```

`As` возвращает второй источник, и дескриптор, построенный от одного, нельзя применить к другому. Именно это делает самосоединение безопасным, а не просто упражнением в именовании.

### Поиск дублей сравнением таблицы с собой

```go
other := Contacts.As("other")

orm.Compose(pool, pairs).
    From(Contacts.Source()).
    Join(other.Source(), orm.And(
        orm.Eq(Contacts.Email, other.Email),
        orm.Cond(Contacts.ID.Lt(other.ID)),
    ))
```

`Lt` — то, что не даёт каждой паре появиться дважды, а каждой строке совпасть с самой собой.

## Дедупликация

### Оставить самую свежую строку по ключу

```go
rank := orm.Named("rn", orm.RowNumber().
    PartitionBy(orm.Of(Imports.ExternalID)).
    OrderBy(orm.Of(Imports.SeenAt).Desc()))

deduped := orm.Sub("deduped", orm.Rows(
    orm.Named("id", orm.Of(Imports.ID)),
    orm.Named("external_id", orm.Of(Imports.ExternalID)),
    rank,
).From(Imports.Source()))

orm.Compose(pool, shape).From(deduped).Where(orm.Ref(deduped, rank).Eq(1))
```

### Или через DISTINCT ON, что короче

```go
orm.Compose(pool, shape).
    From(Imports.Source()).
    DistinctOn(orm.Of(Imports.ExternalID)).
    OrderBy(orm.Of(Imports.ExternalID).Asc(), orm.Of(Imports.SeenAt).Desc())
```

Ответ тот же. Вариант с оконной функцией переносится на другие базы; этот быстрее
и говорит то, что имеется в виду. Поскольку библиотека всё равно только под
PostgreSQL, предпочитайте его.

### Удалить дубли, а не отфильтровывать их

```go
db.Imports.Delete().Where(orm.InSub(
    Imports.ID,
    orm.Compose(pool, ids).From(deduped).Where(orm.Ref(deduped, rank).Gt(1)),
)).Exec(ctx)
```

## Аналитические формы

### Гистограмма

```go
bucket := orm.Named("bucket", orm.Fn[orm.Composed, int32]("width_bucket",
    orm.ArgOf(Response.Millis), orm.ArgValue(0), orm.ArgValue(1000), orm.ArgValue(10)))

orm.Compose(pool, histogram).
    From(Response.Source()).
    GroupBy(bucket).
    OrderBy(bucket.Asc())
```

### Когортное удержание

```go
cohort := orm.Named("cohort", orm.DateTrunc(orm.Month, Users.CreatedAt))
active := orm.Named("active", orm.DateTrunc(orm.Month, Events.At))

orm.Compose(pool, retention).
    From(Users.Source()).
    Join(Events.Source(), orm.Eq(Events.UserID, Users.ID)).
    GroupBy(cohort, active).
    OrderBy(cohort.Asc(), active.Asc())
```

Два округления и группировка. Сетка, которую образует результат, и есть когортная
таблица, а разворот в колонки — дело потребителя.

### Ближайшие по расстоянию

```go
distance := postgis.OfGeog(Stops.Spot).Distance(postgis.GeogValue[Stop](here))

orm.Select(db.Stops, nearest).
    Where(postgis.OfGeog(Stops.Spot).DWithin(postgis.GeogValue[Stop](here), 2000)).
    OrderBy(distance.Asc()).
    Limit(5)
```

`DWithin` перед сортировкой — то, что позволяет индексу сделать работу.

## Запись, составными запросами

### Удалить и прочитать удалённое, одним запросом

```go
archived := orm.WritingCTE("archived", db.Events.Delete().
    Where(Events.At.Lt(cutoff)))

orm.Compose(pool, shape).With(archived).From(archived)
```

Сделать это двумя запросами — значит дать строкам измениться между ними.

### Узнать, какие строки действительно изменились

```go
changed, err := orm.UpdateReturning(
    db.Prices.Update().
        Set(Prices.Cents.Set(cents)).
        Where(Prices.SKU.Eq(sku)).
        Where(Prices.Cents.Ne(cents)),
    priceShape,
).All(ctx)
```

Хитрость во втором `Where`: обновление, которое ничего бы не изменило, ни во что
не попадает, поэтому вернувшиеся строки — ровно те, что сдвинулись. Именно по
этому множеству стоит публиковать события.

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

## Прочитать план, прежде чем чему-либо здесь верить

```go
plan, err := q.Explain(ctx)              // EXPLAIN, never runs the statement
plan, err := q.ExplainAnalyze(ctx)       // runs it, and the name says so
report, err := q.PerformanceReport(ctx)  // plan, shape and fingerprint
```

Имена различаются, потому что опасно различается поведение. Здесь ничего не советуется: планирует PostgreSQL, а решение о том, что менять, требует всей нагрузки, а не одного запроса.

### Сравнить два написания одного вопроса

```go
a, _ := withNotExists.Explain(ctx)
b, _ := withLeftJoin.Explain(ctx)
```

У антисоединения выше две формы, и эта страница отказывается говорить, какая
быстрее: это зависит от ваших объёмов и ваших индексов. Вот как выяснить — на
своих данных, примерно за минуту.

### Проверить, что SQL — тот самый

```go
sql, args, err := q.SQL()
```

В эту строку не подставлено ни одно значение: аргументы возвращаются рядом, и это ровно то, что получает сервер.
