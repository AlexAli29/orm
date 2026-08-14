---
title: Рецепты запросов
description: Повседневные формы — фильтрация, страницы, агрегация, upsert, поиск.
---

Каждый рецепт достаточно полон, чтобы его скопировать. `db` — сгенерированный `*domain.DB`; `Users`, `Orders` и прочие — сгенерированные дескрипторы.

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

Ни одного фильтра — значит вообще нет `WHERE`, а не `WHERE TRUE`.

### Одно или другое

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

### NULL против пустого

```go
db.Users.Query().Where(Users.Bio.IsNull())            // не задано
db.Users.Query().Where(Users.Bio.Eq(""))              // задано пустым
db.Users.Query().Where(orm.Or(
    Users.Bio.IsNull(), Users.Bio.Eq(""),
))                                                     // любое из двух
```

## Страницы

### По смещению

```go
db.Users.Query().OrderBy(Users.ID.Asc()).Limit(20).Offset(page * 20)
```

### Keyset

Корректно на изменяющейся таблице и остаётся быстрым на 5000-й странице:

```go
q := db.Users.Query().OrderBy(Users.CreatedAt.Desc(), Users.ID.Desc()).Limit(20)
if cursor != nil {
    q = q.Where(orm.Or(
        Users.CreatedAt.Lt(cursor.At),
        orm.And(Users.CreatedAt.Eq(cursor.At), Users.ID.Lt(cursor.ID)),
    ))
}
```

### Всего и страница

```go
total, err := db.Users.Query().Where(cond).Count(ctx)
page,  err := db.Users.Query().Where(cond).Limit(20).All(ctx)
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

### Только крупные группы

```go
orm.Select(db.Orders, byStatus).
    GroupBy(Orders.Status).
    Having(orm.Count[Order]().Gt(100))
```

### Агрегаты по пустому множеству

```go
var maxShape = orm.Project1(orm.Max(Orders.Total), func(v *int64) *int64 { return v })
// nil, когда строк нет: max по пустому — это NULL, и тип это говорит
```

## Связи

### Пять последних на родителя

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

### И те, у кого нет

```go
db.Users.Query().Where(Users.Orders.None())
```

### Глубокая загрузка

```go
db.Users.Query().
    With(Users.Orders.With(Orders.Items.With(Items.Product))).
    All(ctx)
// четыре запроса независимо от количества строк
```

## Запись

### Upsert по естественному ключу

```go
db.Users.Insert(ctx, user,
    // Take the new row's values for these columns.
    orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.UpdatedAt),
)
```

### Инкремент без предварительного чтения

```go
db.Counters.Update(ctx).
    SetExpr(Counters.Hits, Counters.Hits.Add(1)).
    Where(Counters.Key.Eq(key)).
    Exec(ctx)
```

### Массовая загрузка

```go
n, err := db.Events.CopyFrom(ctx, batch)
```

### Безопасный разбор очереди

Два воркера никогда не возьмут одну строку:

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
        if _, err := tx.Jobs.Update(ctx).
            Set(Jobs.Status, "running").
            Where(Jobs.ID.Eq(j.ID)).
            Exec(ctx); err != nil {
            return err
        }
    }
    return nil
})
```

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

// текст по пути, приведённый к сравнимому типу
tier := orm.CastNull(orm.JSONPathText(meta, "billing", "tier"), orm.Text)

orm.Compose(pool, shape).From(Users.Source()).Where(
    orm.ArrayContains(tags, orm.Val([]string{"go", "sql"})),
    orm.ArrayOverlaps(tags, orm.Val([]string{"go"})),
)
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

## Время и диапазоны

```go
db.Bookings.Query().Where(Bookings.During.Overlaps(
    orm.ClosedOpen(from, to),
))

db.Events.Query().Where(Events.At.Between(dayStart, dayEnd))
```

## Аварийные выходы

### Фрагмент внутри построенного запроса

```go
db.Users.Query().Where(
    orm.Expr[User]("age(created_at) > interval ?", "1 year"),
)
```

### Целый запрос с сохранением сгенерированного сканера

```go
users, err := orm.Raw[User](db.Users, `
    SELECT * FROM users
    WHERE ctid = ANY (?)
`, ctids).All(ctx)
```

Оба принимают текст SQL намеренно. Ни один не принимает значения, вставленные в него.
