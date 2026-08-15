# Рецепты запросов

> Повседневные формы, выписанные целиком — фильтры, страницы, агрегаты, соединения, окна, upsert, поиск.

Source: https://ormgo.vercel.app/ru/docs/cookbook/queries/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
Каждый рецепт здесь достаточно полный, чтобы его вставить к себе. `db` — это порождённый `*domain.DB`; `Users`, `Orders` и прочие — порождённые дескрипторы. Предметные области меняются от рецепта к рецепту нарочно: важна форма, а форму, которую вы видели только на `users`, приходится сначала перевести на свою задачу.

## Фильтрация

### Необязательные фильтры из запроса

```go
func (s *Store) Search(ctx context.Context, f Filter) ([]User, error) {
    q := s.db.Users.Query()
    if f.Email != "" {
        q = q.Where(Users.Email.ILike("%" + f.Email + "%"))
    }
    if f.Active != nil {
        q = q.Where(Users.Active.Eq(*f.Active))
    }
    if !f.Since.IsZero() {
        q = q.Where(Users.CreatedAt.Gte(f.Since))
    }
    return q.OrderBy(Users.CreatedAt.Desc()).Limit(f.Limit).All(ctx)
}
```

Ни одного фильтра — значит вообще никакого `WHERE`, а не `WHERE TRUE`.

### Или одно, или другое

```go
db.Users.Query().Where(orm.Or(
    Users.Email.ILike("%@example.com"),
    Users.Email.ILike("%@example.org"),
))
```

### Всё, кроме

```go
db.Users.Query().Where(orm.Not(Users.ID.In(banned...)))
```

### NULL против пустоты

```go
db.Users.Query().Where(Users.Bio.IsNull())            // never set
db.Users.Query().Where(Users.Bio.Eq(""))              // set to empty
db.Users.Query().Where(orm.Or(
    Users.Bio.IsNull(), Users.Bio.Eq(""),
))                                                     // either
```

### Одна колонка против другой

Отправление, которое весит больше, чем было заявлено:

```go
db.Shipments.Query().Where(orm.OpPredicate[Shipment](
    ">", orm.ArgOf(Shipments.ActualGrams), orm.ArgOf(Shipments.QuotedGrams),
))
```

### Диапазон значений

```go
db.Readings.Query().Where(Readings.Celsius.Between(-10, 45))
```

Двусторонний и включающий — именно это `BETWEEN` и значит в SQL. Если нужна
строгая граница с одной стороны, пишите `Gte` и `Lt`: тогда читателю видно, какая.

### Множество значений из среза

```go
db.Flights.Query().Where(Flights.Origin.In("LHR", "CDG", "AMS"))
db.Flights.Query().Where(Flights.Origin.In(hubs...))
```

### Пустой срез — не ошибка

```go
// In() over nothing matches nothing, which is the SQL answer and rarely
// the one a caller expected. Decide it where the intent is.
if len(codes) == 0 {
    return nil, nil
}
db.Flights.Query().Where(Flights.Origin.In(codes...))
```

### Без учёта регистра и без функции на колонке

```go
db.Artists.Query().Where(Artists.Name.ILike(input))
```

`ILike` без шаблонных символов — это равенство без учёта регистра, и оно
остаётся пригодным для индекса на колонке `citext` или при подходящем
индексе по выражению, чего `lower(name) = lower($1)` не даёт, если именно
такого индекса нет.

### Поиск по префиксу, который может использовать индекс

```go
db.Artists.Query().Where(Artists.Name.Like(prefix + "%"))
```

Ведущий шаблонный символ (`"%" + s`) B-дерево использовать не даёт. Если он
нужен, вам нужен [полнотекстовый поиск](/ru/docs/fulltext/) или триграммный
индекс, а не `LIKE`.

### Три состояния nullable-булева

```go
db.Applications.Query().Where(Applications.Approved.Eq(true))   // approved
db.Applications.Query().Where(Applications.Approved.Eq(false))  // rejected
db.Applications.Query().Where(Applications.Approved.IsNull())   // undecided
```

### Вложенные и/или, оставшиеся читаемыми

```go
db.Tickets.Query().Where(orm.And(
    Tickets.EventID.Eq(eventID),
    orm.Or(
        Tickets.Status.Eq("reserved"),
        orm.And(
            Tickets.Status.Eq("pending"),
            Tickets.HeldUntil.Gt(time.Now()),
        ),
    ),
))
```

### Фильтр, собранный из набора

```go
q := db.Devices.Query()
for _, f := range []struct {
    want string
    eq   func(string) orm.Predicate[Device]
}{
    {model, Devices.Model.Eq},
    {region, Devices.Region.Eq},
} {
    if f.want != "" {
        q = q.Where(f.eq(f.want))
    }
}
```

### Исключение подзапросом

```go
db.Users.Query().Where(orm.NotInSub(
    orm.Of(Users.ID),
    orm.Compose(pool, blockedIDs).From(Blocks.Source()),
))
```

## Сортировка

### Два ключа в разные стороны

```go
db.Leaderboard.Query().OrderBy(
    Leaderboard.Score.Desc(),
    Leaderboard.AchievedAt.Asc(),
)
```

Дополнительный ключ здесь не для красоты. Без него порядок равных очков — тот,
что выдал план, и он меняется от запуска к запуску.

### NULL там, где вы их хотите

```go
db.Tasks.Query().OrderBy(Tasks.DueAt.Asc())
```

PostgreSQL кладёт NULL последними при `ASC` и первыми при `DESC`. Если задачи
без срока должны быть в конце списка `DESC`, сортируйте по выражению с
подстановкой:

```go
due := orm.CoalesceNull(orm.Of(Tasks.DueAt), orm.Val(farFuture))

orm.Select(db.Tasks, shape).OrderBy(due.Desc())
```

### Сортировка по тому, что вы же и выбрали

```go
distance := postgis.OfGeog(Stops.Spot).Distance(postgis.GeogValue[Stop](here))

orm.Select(db.Stops, nearest).OrderBy(distance.Asc()).Limit(10)
```

### Сортировка по агрегату

```go
orm.Select(db.Orders, byCustomer).
    GroupBy(Orders.CustomerID).
    OrderBy(orm.Count[Order]().Desc()).
    Limit(25)
```

### Устойчивый порядок для выгрузок

```go
db.Invoices.Query().OrderBy(Invoices.IssuedAt.Asc(), Invoices.ID.Asc())
```

Любой выгрузке, которую сравнивают между двумя запусками, нужен полный порядок.
Первичный ключ в конце — самый дешёвый способ его гарантировать.

### Случайная выборка

```go
db.Photos.Query().
    OrderBy(orm.Fn[Photo, float64]("random").Asc()).
    Limit(10)
```

Годится на сотне тысяч строк и неверно на сотне миллионов: сортируется вся
таблица. На таких объёмах выбирайте по диапазону ключа.

## Постраничный вывод

### По смещению

```go
db.Users.Query().OrderBy(Users.ID.Asc()).Limit(20).Offset(page * 20)
```

### По ключу

Корректно на изменяющейся таблице и остаётся быстрым на пятитысячной странице:

```go
q := db.Users.Query().OrderBy(Users.CreatedAt.Desc(), Users.ID.Desc()).Limit(20)
if cursor != nil {
    q = q.Where(orm.Or(
        Users.CreatedAt.Lt(cursor.At),
        orm.And(Users.CreatedAt.Eq(cursor.At), Users.ID.Lt(cursor.ID)),
    ))
}
```

### По ключу, когда ключ уникален

Когда колонка сортировки уже уникальна, сравнение кортежей схлопывается:

```go
q := db.Events.Query().OrderBy(Events.Seq.Asc()).Limit(500)
if after > 0 {
    q = q.Where(Events.Seq.Gt(after))
}
```

### Всего и страница — по одному обращению на каждое

```go
total, err := db.Users.Query().Where(cond).Count(ctx)
page,  err := db.Users.Query().Where(cond).Limit(20).All(ctx)
```

### Есть ли следующая страница

Дешевле счётчика и обычно это всё, что нужно интерфейсу:

```go
rows, err := db.Users.Query().OrderBy(Users.ID.Asc()).Limit(21).All(ctx)
hasNext := len(rows) > 20
if hasNext {
    rows = rows[:20]
}
```

### Прочитать таблицу целиком, не держа её в памяти

```go
rows, err := db.Events.Query().OrderBy(Events.ID.Asc()).Rows(ctx)
if err != nil {
    return err
}
defer rows.Close()

for rows.Next() {
    e, err := rows.Value()
    if err != nil {
        return err
    }
    if err := sink(e); err != nil {
        return err
    }
}
return rows.Err()
```

## Агрегация

### Счётчик по группам

```go
type ByStatus struct {
    Status string
    N      int64
}

var byStatus = orm.Project2(
    Orders.Status, orm.Count[Order](),
    func(s string, n int64) ByStatus { return ByStatus{s, n} },
)

rows, _ := orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    OrderBy(Orders.Status.Asc()).
    All(ctx)
```

### Только нагруженные группы

```go
orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    Having(orm.Count[Order]().Gt(100))
```

### Агрегаты по пустому множеству

```go
var maxShape = orm.Project1(orm.Max(Orders.Total), func(v *int64) *int64 { return v })
// nil when there are no rows — max over nothing is NULL, and the type says so
```

### Счётчик nullable-колонки — это не count(*)

```go
var coverage = orm.Project2(
    orm.Count[User](),          // every row
    orm.CountOf(Users.Bio),     // rows whose bio is not NULL
    func(all, withBio int64) Coverage { return Coverage{all, withBio} },
)
```

### Счётчик различных значений

```go
var uniqueVisitors = orm.Project1(
    orm.CountOf(Visits.SessionID).Distinct(),
    func(n int64) int64 { return n },
)
```

### Несколько агрегатов за один проход

Ровно то, зачем нужен `GROUP BY`: один проход, пять чисел:

```go
type Daily struct {
    Day     time.Time
    Orders  int64
    Revenue *int64
    Largest *int64
    Average *float64
}

day := orm.DateTrunc(orm.Day, Orders.PlacedAt)

var daily = orm.Project5(
    orm.Named("day", day),
    orm.Count[Order](),
    orm.SumInt32(Orders.TotalCents),
    orm.Max(Orders.TotalCents),
    orm.AvgInt32(Orders.TotalCents),
    func(d time.Time, n int64, sum, max *int64, avg *float64) Daily {
        return Daily{Day: d, Orders: n, Revenue: sum, Largest: max, Average: avg}
    },
)
```

### Условные агрегаты

Посчитать две вещи сразу, без двух запросов:

```go
paid := orm.Count[Order]().Filter(Orders.Status.Eq("paid"))
refunded := orm.Count[Order]().Filter(Orders.Status.Eq("refunded"))

var split = orm.Project3(
    Orders.CustomerID, paid, refunded,
    func(id int64, p, r int64) Split { return Split{id, p, r} },
)
```

`FILTER` — это конструкция ровно для такого случая. `sum(case when … then 1
else 0 end)` даёт тот же ответ, но записан менее внятно.

### Группировка по вычисленному значению

```go
month := orm.DateTrunc(orm.Month, Subscriptions.StartedAt)

orm.Select(db.Subscriptions, monthly).
    GroupBy(month).
    OrderBy(month.Asc())
```

Группируйте по выражению, а не по псевдониму: там, где вычисляется `GROUP BY`,
псевдонима ещё не существует.

### Два ключа группировки

```go
orm.Select(db.Sales, byRegionAndQuarter).
    GroupBy(Sales.Region, quarter).
    OrderBy(Sales.Region.Asc(), quarter.Asc())
```

### Средние, которые не врут

```go
orm.AvgInt32(Ratings.Stars)   // *float64 — NULL over no rows
orm.SumInt32(Ratings.Stars)   // *int64   — NULL over no rows
orm.Count[Rating]()           // int64    — zero over no rows
```

Указатель здесь не перестраховка. `avg` по пустой группе — это NULL, а у
`float64` нет значения, означающего «усреднять было нечего».

### Доля от общего

```go
var share = orm.Project2(
    Sales.Region,
    orm.Named("pct", orm.Op(
        orm.Op(orm.SumInt32(Sales.Cents), "*", orm.Val(int64(100))),
        "/",
        orm.Fn[Sale, int64]("sum", orm.Of(Sales.Cents)).Over(orm.Window()),
    )),
    func(region string, pct *int64) Share { return Share{region, pct} },
)
```

### Самый загруженный час каждого дня

```go
hour := orm.DateTrunc(orm.Hour, Rides.StartedAt)
day := orm.DateTrunc(orm.Day, Rides.StartedAt)

ranked := orm.RowNumber().Over(
    orm.Window().PartitionBy(day).OrderBy(orm.Count[Ride]().Desc()),
)
```

### DISTINCT ON: по одной строке на группу, дёшево

```go
orm.Select(db.Prices, latest).
    DistinctOn(Prices.SKU).
    OrderBy(Prices.SKU.Asc(), Prices.ObservedAt.Desc())
```

`ORDER BY` обязан начинаться с колонок `DISTINCT ON` — именно он решает, какая
строка каждой группы выживет. Здесь — самая свежая цена по каждому SKU.

## Оконные функции

### Нумерация строк внутри группы

```go
rank := orm.RowNumber().Over(
    orm.Window().
        PartitionBy(orm.Of(Results.HeatID)).
        OrderBy(orm.Of(Results.TimeMillis).Asc()),
)
```

### Rank, dense rank и разница между ними

```go
w := orm.Window().OrderBy(orm.Of(Scores.Points).Desc())

orm.Rank().Over(w)       // 1, 2, 2, 4  — gaps after ties
orm.DenseRank().Over(w)  // 1, 2, 2, 3  — no gaps
orm.RowNumber().Over(w)  // 1, 2, 3, 4  — arbitrary among ties
```

### Накопительный итог

```go
w := orm.Window().
    OrderBy(orm.Of(Entries.At).Asc()).
    Rows(orm.UnboundedPreceding(), orm.CurrentRow())

running := orm.Fn[Entry, int64]("sum", orm.Of(Entries.Cents)).Over(w)
```

### Изменение относительно предыдущей строки

```go
prev := orm.Lag(orm.Of(Readings.Celsius)).Over(
    orm.Window().
        PartitionBy(orm.Of(Readings.SensorID)).
        OrderBy(orm.Of(Readings.At).Asc()),
)
```

### Скользящее среднее

```go
w := orm.Window().
    OrderBy(orm.Of(Ticks.At).Asc()).
    Rows(orm.Preceding(6), orm.CurrentRow())

sevenDay := orm.Fn[Tick, float64]("avg", orm.Of(Ticks.Price)).Over(w)
```

### Первое и последнее в разделе

```go
w := orm.Window().
    PartitionBy(orm.Of(Events.SessionID)).
    OrderBy(orm.Of(Events.At).Asc()).
    Rows(orm.UnboundedPreceding(), orm.UnboundedFollowing())

orm.FirstValue(orm.Of(Events.Page)).Over(w)  // the landing page
orm.LastValue(orm.Of(Events.Page)).Over(w)   // the exit page
```

`LastValue` требует явной рамки. С рамкой по умолчанию он возвращает текущую
строку — это самый частый сюрприз оконных функций.

### Квартили

```go
orm.Ntile(4).Over(
    orm.Window().OrderBy(orm.Of(Customers.LifetimeCents).Desc()),
)
```

### Одно окно, использованное несколько раз

```go
w := orm.Window().
    PartitionBy(orm.Of(Orders.CustomerID)).
    OrderBy(orm.Of(Orders.PlacedAt).Asc())

seq := orm.RowNumber().Over(w)
prevAt := orm.Lag(orm.Of(Orders.PlacedAt)).Over(w)
firstAt := orm.FirstValue(orm.Of(Orders.PlacedAt)).Over(w)
```

## Соединения и композиция

### Внутреннее соединение с проекцией

```go
type Line struct {
    Order   int64
    Product string
    Qty     int32
}

var lines = orm.Project3(
    orm.Of(Items.OrderID), orm.Of(Products.Name), orm.Of(Items.Qty),
    func(o int64, p string, q int32) Line { return Line{o, p, q} },
)

rows, err := orm.Compose(pool, lines).
    From(Items.Source()).
    Join(Products.Source(), orm.Of(Items.ProductID).EqCol(orm.Of(Products.ID))).
    All(ctx)
```

### Левое соединение и та nullable-ность, которую оно навязывает

```go
var withLast = orm.Project2(
    orm.Of(Users.Email), orm.Opt(Orders.PlacedAt),
    func(email string, last *time.Time) Row { return Row{email, last} },
)

orm.Compose(pool, withLast).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Of(Users.ID).EqCol(orm.Of(Orders.UserID)))
```

`orm.Opt` здесь не «на всякий случай». Внешнее соединение может дать NULL для
каждой колонки правой стороны, а приёмник типа `time.Time` этого не вместит.

### Соединение трёх таблиц

```go
orm.Compose(pool, shape).
    From(Orders.Source()).
    Join(Customers.Source(), orm.Of(Orders.CustomerID).EqCol(orm.Of(Customers.ID))).
    Join(Regions.Source(), orm.Of(Customers.RegionID).EqCol(orm.Of(Regions.ID)))
```

### Самосоединение через псевдоним

Сотрудники и их руководители из одной таблицы:

```go
mgr := Employees.As("mgr")

orm.Compose(pool, pairs).
    From(Employees.Source()).
    LeftJoin(mgr.Source(), orm.Of(Employees.ManagerID).EqCol(orm.Of(mgr.ID)))
```

Псевдоним — это второе вхождение той же таблицы, и его дескрипторы привязаны
именно к нему, так что `mgr.ID` не может случайно означать `Employees.ID`.

### LATERAL для «первых N на каждую строку»

```go
recent := orm.Compose(pool, orderShape).
    From(Orders.Source()).
    Where(orm.Of(Orders.CustomerID).EqCol(orm.Of(Customers.ID))).
    OrderBy(orm.Of(Orders.PlacedAt).Desc()).
    Limit(3)

orm.Compose(pool, shape).
    From(Customers.Source()).
    LeftJoinLateral(recent.As("recent"))
```

### CTE, использованный дважды

```go
active := orm.CTE("active", orm.Compose(pool, userShape).
    From(Users.Source()).
    Where(orm.Of(Users.Active).Eq(true)))

orm.Compose(pool, shape).
    With(active).
    From(active.Source()).
    Join(Orders.Source(), orm.Of(Orders.UserID).EqCol(orm.Of(active.ID)))
```

### UNION ALL двух форм

```go
orm.UnionAll(
    orm.Compose(pool, feed).From(Posts.Source()),
    orm.Compose(pool, feed).From(Comments.Source()),
).OrderBy(orm.Of(feedAt).Desc()).Limit(50)
```

Обе ветви обязаны давать одну форму: то же число колонок, те же типы, ту же
nullable-ность. Это проверяется при сборке, а не когда запрос выполнит PostgreSQL.

### Антисоединение, двумя способами

```go
// Correlated NOT EXISTS — usually the plan you want.
orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.NotExists(orm.Compose(pool, one).
        From(Orders.Source()).
        Where(orm.Of(Orders.UserID).EqCol(orm.Of(Users.ID)))),
)

// Left join and test for NULL — the same rows, a different plan.
orm.Compose(pool, shape).
    From(Users.Source()).
    LeftJoin(Orders.Source(), orm.Of(Users.ID).EqCol(orm.Of(Orders.UserID))).
    Where(orm.Opt(Orders.ID).IsNull())
```

### Скалярный подзапрос в списке выборки

```go
orderCount := orm.Scalar(orm.Compose(pool, countShape).
    From(Orders.Source()).
    Where(orm.Of(Orders.UserID).EqCol(orm.Of(Users.ID))))

var withCount = orm.Project2(
    orm.Of(Users.Email), orm.Named("orders", orderCount),
    func(email string, n *int64) Row { return Row{email, n} },
)
```

### Перекрёстное соединение для плотного календаря

Каждый день на каждый товар, чтобы у дня без продаж всё равно была строка:

```go
orm.Compose(pool, shape).
    From(days.Source()).
    CrossJoin(Products.Source()).
    LeftJoin(Sales.Source(), orm.And(
        orm.Of(Sales.Day).EqCol(orm.Of(days.Day)),
        orm.Of(Sales.ProductID).EqCol(orm.Of(Products.ID)),
    ))
```

## Связи

### Пять самых свежих на каждого родителя

Один запрос, а не по одному на пользователя:

```go
db.Users.Query().
    With(Users.Orders.OrderBy(Orders.Placed.Desc()).Limit(5)).
    All(ctx)
```

### Родители, у которых есть потомок

```go
db.Users.Query().Where(Users.Orders.Any(Orders.Status.Eq("paid")))
```

### Родители, у которых потомков нет

```go
db.Users.Query().Where(Users.Orders.None())
```

### Глубокая загрузка

```go
db.Users.Query().
    With(Users.Orders.With(Orders.Items.With(Items.Product))).
    All(ctx)
// four statements regardless of row counts
```

### Загрузка отфильтрованной ветви

```go
db.Users.Query().
    With(Users.Orders.Where(Orders.Status.Eq("paid"))).
    All(ctx)
```

Фильтр применяется к загрузке потомков, а не к родителям. Пользователи без
оплаченных заказов всё равно вернутся — с пустым срезом.

### Фильтрация родителей по полю потомка

```go
db.Albums.Query().Where(Albums.Tracks.Any(Tracks.DurationMs.Gt(600_000)))
```

### Родители, у которых подходят все потомки

Выражено как «ни один потомок не подходит под обратное» — именно это SQL и умеет
проверить:

```go
db.Orders.Query().Where(orm.Not(Orders.Items.Any(Items.InStock.Eq(false))))
```

### Связь и агрегат рядом

```go
db.Playlists.Query().
    With(Playlists.Tracks.Limit(3)).       // a preview of the contents
    All(ctx)

// the true size, separately, because a limited load cannot tell you it
orm.Select(db.Tracks, byPlaylist).GroupBy(Tracks.PlaylistID)
```

### Многие-ко-многим через таблицу связи

```go
db.Students.Query().
    With(Students.Courses).
    All(ctx)
```

### Незагруженный случай виден

```go
u, _ := db.Users.Query().One(ctx)   // no With
// u.Orders is nil, and nil means "not loaded", not "none exist".
// Nothing fetches it behind the field access — a loop cannot become N queries.
```

## Запись

### Вставить одну строку и забрать то, что решила база

```go
u, err := db.Users.Insert(ctx, User{Email: "ada@example.com", Name: "Ada"})
if err != nil {
    return err
}
// the returned value carries what the database decided: u.ID, u.CreatedAt
```

### Вставить много одним запросом

```go
saved, err := db.Tags.InsertMany(ctx, tags)
if err != nil {
    return err
}
```

### Значение по умолчанию вместо нулевого значения

```go
db.Users.Insert(ctx, u, orm.Default(Users.Role))
```

`Role: ""` означает пустую строку, потому что нулевое значение Go — это
значение. Попросить умолчание колонки — отдельное намерение, поэтому и вызов
отдельный.

### Обновление по первичному ключу

```go
n, err := db.Users.Update().
    Set(Users.Name.Set("Ada Lovelace")).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

### Инкремент без предварительного чтения

```go
db.Counters.Update().
    Set(Counters.Hits.SetExpr(Counters.Hits.Add(1))).
    Where(Counters.Key.Eq(key)).
    Exec(ctx)
```

### Колонка из другой колонки

```go
db.Invoices.Update().
    Set(Invoices.BalanceCents.SetExpr(Invoices.TotalCents.SubCol(Invoices.PaidCents))).
    Where(Invoices.ID.Eq(id)).
    Exec(ctx)
```

### Очистить nullable-колонку

```go
db.Users.Update().
    Set(Users.DeactivatedAt.SetNull()).
    Where(Users.ID.Eq(id)).
    Exec(ctx)
```

### Обновить и сразу прочитать результат

```go
type Moved struct {
    ID    int64
    State string
}

var moved = orm.Project2(
    Jobs.ID, Jobs.Status,
    func(id int64, s string) Moved { return Moved{id, s} },
)

rows, err := orm.UpdateReturning(
    db.Jobs.Update().Set(Jobs.Status.Set("running")).Where(Jobs.Status.Eq("pending")),
    moved,
).All(ctx)
```

Один запрос. Альтернатива — обновить, а потом выбрать то, что обновили, — это
два запроса и гонка между ними.

### Удалить и сохранить удалённое

```go
gone, err := orm.DeleteReturningEntity(
    db.Sessions.Delete().Where(Sessions.ExpiresAt.Lt(time.Now())),
).All(ctx)
```

### Массовая загрузка

```go
n, err := db.Events.CopyFrom(ctx, batch)
```

`COPY`, а не многострочный `INSERT`. Он несравнимо быстрее и не поддерживает
`ON CONFLICT` — если нужно и то и другое, грузите во временную таблицу и
сливайте оттуда.

### Массовая загрузка из потока

```go
n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event) bool) {
    for scanner.Scan() {
        e, err := parse(scanner.Text())
        if err != nil {
            return
        }
        if !yield(e) {
            return
        }
    }
})
```

Целиком файл нигде не держится. Строки уходят на сервер по мере разбора.

### Удаление ограниченными порциями

Удаление десяти миллионов строк как последовательность коротких транзакций,
чтобы ничто не держало блокировку час:

```go
for {
    n, err := db.Events.Delete().
        Where(Events.ID.In(nextIDs...)).
        Exec(ctx)
    if err != nil {
        return err
    }
    if n == 0 {
        return nil
    }
}
```

### TRUNCATE, осознанно

```go
if err := ormtest.TruncateWith(ctx, pool,
    []ormtest.TruncateOption{ormtest.RestartIdentity()},
    Staging,
); err != nil {
    return err
}
```

Это не `DELETE` без `WHERE`. Это другой оператор с другими блокировками и без
построчной работы, и пишется он иначе — так что до него нельзя добраться, забыв
условие.

## Upsert и конфликты

### Upsert по естественному ключу

```go
db.Users.Insert(ctx, user,
    // Take the new row's values for these columns.
    orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.UpdatedAt),
)
```

### Вставить, если нет; промолчать, если есть

```go
db.Tags.Insert(ctx, tag, orm.OnConflict(Tags.Slug).DoNothing())
```

### Upsert, который вычисляет новое значение

Для счётчика «побеждает последний» неверно; здесь значение прибавляется:

```go
db.Counters.Insert(ctx, c,
    orm.OnConflict(Counters.Key).DoUpdateSet(
        Counters.Hits.SetExpr(Counters.Hits.AddCol(orm.Excluded(Counters.Hits))),
    ),
)
```

`orm.Excluded` — это строка, которую предложили и отвергли, псевдотаблица
`EXCLUDED`, названная так же, как в SQL.

### Перезаписывать, только если пришедшее свежее

```go
db.Prices.Insert(ctx, p,
    orm.OnConflict(Prices.SKU).
        DoUpdate(Prices.Cents, Prices.ObservedAt).
        Where(Prices.ObservedAt.Lt(orm.Excluded(Prices.ObservedAt))),
)
```

### Upsert целой пачки

```go
db.Inventory.InsertMany(ctx, rows,
    orm.OnConflict(Inventory.SKU, Inventory.WarehouseID).
        DoUpdate(Inventory.OnHand),
)
```

Цель конфликта — колонки уникального ограничения, в любом порядке, — но оно
должно существовать, иначе PostgreSQL нечем обнаруживать конфликт.

## JSON и массивы

Это свободные функции, дающие `Predicate[Composed]`, поэтому они идут в
составной запрос. `orm.Opt` поднимает не-nullable колонку:

```go
meta := orm.Opt(Users.Meta)
tags := orm.Opt(Users.Tags)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.JSONHasKey(meta, "plan"),
)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.JSONContains(meta, orm.Val(map[string]any{"plan": "pro"})),
)

// the text at a path, cast to something comparable
tier := orm.CastNull(orm.JSONPathText(meta, "billing", "tier"), orm.Text)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.ArrayContains(tags, orm.Val([]string{"go", "sql"})),
    orm.ArrayOverlaps(tags, orm.Val([]string{"go"})),
)
```

### Любой из ключей, все ключи

```go
orm.JSONHasAnyKeys(meta, "plan", "trial")
orm.JSONHasAllKeys(meta, "plan", "seats")
```

### Достать вложенное значение

```go
city := orm.JSONPathText(orm.Opt(Profiles.Data), "address", "city")

var byCity = orm.Project2(
    orm.Of(Profiles.UserID), orm.Named("city", city),
    func(id int64, city *string) Row { return Row{id, city} },
)
```

### Элемент по индексу

```go
first := orm.JSONIndexText(orm.Opt(Orders.Lines), 0)
```

### Запись внутрь JSON-документа

```go
db.Profiles.Update().
    Set(Profiles.Data.SetExpr(orm.JSONSet(
        Profiles.Data, []string{"verified"}, orm.Val(true), true,
    ))).
    Where(Profiles.UserID.Eq(id)).
    Exec(ctx)
```

### Убрать null перед сохранением

```go
orm.JSONStripNulls(orm.Opt(Profiles.Data))
```

### Длина массива

```go
tagCount := orm.Fn[Post, int32]("array_length", orm.ArgOf(Posts.Tags), orm.ArgValue(1))

orm.Select(db.Posts, shape).Where(tagCount.Gt(3))
```

### Массив, содержащий все перечисленные значения

```go
orm.ArrayContains(orm.Opt(Posts.Tags), orm.Val([]string{"go", "postgres"}))
```

«Содержит» значит «является надмножеством». Для «есть хотя бы одно из» нужен
`ArrayOverlaps` — это разные вопросы, и операторы у них тоже разные.

### Массив, вложенный в список разрешённого

```go
orm.ArrayContainedBy(orm.Opt(Roles.Granted), orm.Val(allowed))
```

## Полнотекстовый поиск

```go
q := orm.PlainToTSQuery(orm.English, input)

type Hit struct {
    ID    int64
    Title string
    Rank  float32
}

var hits = orm.Project3(
    Docs.ID, Docs.Title, orm.TSRank(Docs.Search, q),
    func(id int64, title string, rank float32) Hit { return Hit{id, title, rank} },
)

orm.Select(db.Docs, hits).
    Where(orm.Matches(Docs.Search, q)).
    OrderBy(orm.TSRank(Docs.Search, q).Desc()).
    Limit(20).
    All(ctx)
```

### Приём поискового синтаксиса от пользователя

```go
q := orm.WebSearchToTSQuery(orm.English, input)
```

`websearch_to_tsquery` принимает кавычки для фраз, `or` и `-исключение` и
никогда не падает с синтаксической ошибкой на бессмыслице — именно это и нужно
текстовому полю. `to_tsquery` падает, поэтому направлять его на пользовательский
ввод неправильно.

### Фраза, в порядке слов

```go
q := orm.PhraseToTSQuery(orm.English, "ada lovelace")
```

### Комбинирование запросов

```go
must := orm.PlainToTSQuery(orm.English, required)
nice := orm.PlainToTSQuery(orm.English, optional)

orm.AndTSQuery(must, orm.NotTSQuery(nice))
```

### Заголовок весомее тела

```go
vec := orm.Concat2TSVector(
    orm.SetWeight(orm.ToTSVector(orm.English, orm.Of(Docs.Title)), "A"),
    orm.SetWeight(orm.ToTSVector(orm.English, orm.Of(Docs.Body)), "B"),
)
```

### Ранжирование с учётом расстояния

```go
orm.TSRankCD(Docs.Search, q).Desc()
```

## Время, даты и диапазоны

```go
db.Bookings.Query().Where(Bookings.During.Overlaps(
    orm.ClosedOpen(from, to),
))

db.Events.Query().Where(Events.At.Between(dayStart, dayEnd))
```

### Округление до периода

```go
month := orm.DateTrunc(orm.Month, Invoices.IssuedAt)
```

### Достать поле

```go
dow := orm.Extract(orm.DayOfWeek, Rides.StartedAt, orm.Integer)
year := orm.Extract(orm.Year, Rides.StartedAt, orm.Integer)
```

### Время сервера, а не клиента

```go
db.Sessions.Update().
    Set(Sessions.SeenAt.SetExpr(orm.Now())).
    Where(Sessions.ID.Eq(id)).
    Exec(ctx)
```

Часы базы — те, с которыми уже согласована каждая строка. Часы сервера
приложения могут отличаться на секунды, а в кластере — отличаться друг от друга.

### Прибавить интервал

```go
expires := orm.AddInterval(Tokens.IssuedAt, orm.Val(orm.IntervalOf(0, 1, 0)))
```

### Всё, что истекает в ближайший час

```go
db.Tokens.Query().Where(Tokens.ExpiresAt.Between(now, now.Add(time.Hour)))
```

### Диапазон, содержащий точку

```go
db.Rates.Query().Where(Rates.Effective.Contains(when))
```

### Две брони, которые столкнутся

```go
db.Bookings.Query().Where(orm.And(
    Bookings.RoomID.Eq(room),
    Bookings.During.Overlaps(orm.ClosedOpen(from, to)),
))
```

Вопрос именно про пересечение, и диапазонный тип отвечает на него одним
оператором. Записанное как четыре сравнения по двум колонкам — тот же запрос, но
с большим числом мест, где можно ошибиться в границе.

### Границы, включающие и нет

```go
orm.ClosedOpen(from, to)   // [from, to)  — the usual one for time
orm.Closed(from, to)       // [from, to]
orm.Open(from, to)         // (from, to)
orm.OpenClosed(from, to)   // (from, to]
```

Полуоткрытый — правильное умолчание для времени: два соседних `[a, b)` смыкаются
без пересечения, а `[a, b]` — нет.

### Пустой и неограниченный

```go
orm.RangeFrom(start)     // [start, ∞)
orm.RangeUntil(end)      // (-∞, end)
orm.EmptyRange[Booking]() // matches nothing, and is not the same as NULL
```

## Транзакции и блокировки

### Транзакция

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    if err := tx.Accounts.Update().
        Set(Accounts.Cents.SetExpr(Accounts.Cents.Sub(amount))).
        Where(Accounts.ID.Eq(from)).
        Exec(ctx); err != nil {
        return err
    }
    return tx.Accounts.Update().
        Set(Accounts.Cents.SetExpr(Accounts.Cents.Add(amount))).
        Where(Accounts.ID.Eq(to)).
        Exec(ctx)
})
```

Возврат ошибки откатывает. Глобальной транзакции нет, и ничего не происходит
неявно: `tx` — другой хэндл, не `db`, поэтому случайный вызов `db` внутри
замыкания виден на ревью.

### Безопасно забрать работу

Шаблон очереди, в котором два обработчика никогда не возьмут одну строку:

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    jobs, err := tx.Jobs.Query().
        Where(Jobs.Status.Eq("pending")).
        OrderBy(Jobs.Created.Asc()).
        Limit(10).
        Lock(orm.ForUpdateStrong, orm.SkipLocked()).
        All(ctx)
    if err != nil {
        return err
    }
    for _, j := range jobs {
        if _, err := tx.Jobs.Update().
            Set(Jobs.Status.Set("running")).
            Where(Jobs.ID.Eq(j.ID)).
            Exec(ctx); err != nil {
            return err
        }
    }
    return nil
})
```

### Упасть, а не ждать

```go
db.Accounts.Query().
    Where(Accounts.ID.Eq(id)).
    Lock(orm.ForUpdateStrong, orm.NoWait()).
    One(ctx)
```

### Чтение, которое не должно блокировать пишущих

```go
db.Reports.Query().Lock(orm.ForShare).All(ctx)
```

### Уровень изоляции

```go
err := db.TxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, func(tx *domain.DB) error {
    return transfer(ctx, tx)
})
```

Serializable может завершиться ошибкой сериализации, которую безопасно
повторить. Этот повтор — в вашем коде, потому что только вы знаете, идемпотентна
ли работа.

## Аварийные выходы

### Фрагмент внутри собранного запроса

```go
db.Users.Query().Where(
    orm.Expr[User]("age(created_at) > interval ?", "1 year"),
)
```

### Целый запрос, но с сохранённым порождённым сканером

```go
users, err := orm.Raw[User](db.Users, `
    SELECT * FROM users
    WHERE ctid = ANY (?)
`, ctids).All(ctx)
```

Оба принимают текст SQL осознанно. Ни один не принимает значения, вставленные в
этот текст.

### Вызов функции, которую библиотека не оборачивает

```go
soundex := orm.Fn[Person, string]("soundex", orm.ArgOf(People.Surname))
wanted := orm.Fn[Person, string]("soundex", orm.ArgValue(input))

orm.Select(db.People, shape).Where(soundex.EqCol(wanted))
```

### Оператор, который библиотека не оборачивает

```go
similar := orm.OpPredicate[Product]("%>", orm.ArgOf(Products.Name), orm.ArgValue(input))
```

### Посмотреть SQL до выполнения

```go
sql, args, err := db.Users.Query().Where(Users.Active.Eq(true)).SQL()
```

### Прочитать план

```go
plan, err := db.Users.Query().Where(Users.Email.Eq(addr)).Explain(ctx)
```

`ExplainAnalyze` выполняет запрос. На `SELECT` это обычно не страшно; на всём,
что пишет, это не предпросмотр — работа будет сделана.
