---
title: UNION ALL
description: Композиция двух типизированных SELECT в один, с сохранением дубликатов.
---

## Область

v1 составляет `UNION ALL` и ничего больше. `UNION`, `INTERSECT` и `EXCEPT` в неё не входят, и компилятор отвергает любую другую операцию, а не оставляет дыру, которую кто-то обнаружит в рантайме.

`UNION ALL` сохраняет дубликаты. В этом и состоит операция: если их нужно было убрать, вам нужна была другая.

## Как её написать

Ветвь — это любой типизированный запрос: `Query` по сущности, `SelectQuery`,
`ComposedQuery` или другой `UnionQuery`. Все ветви дают один и тот же Go-тип
результата, а аргумент типа пишется явно: Go не выводит его из интерфейса,
который ветвь случайно удовлетворяет.

```go
type Row struct {
    ID    int64
    Label string
}

// Одна форма на обе ветви. Имена важны: составной запрос сортируется по имени
// колонки результата, поэтому объявляем их через As.
shape := orm.Project2(
    orm.Of(Users.ID).As("thing_id"),
    orm.Of(Users.Email).As("label"),
    func(id int64, label string) Row { return Row{ID: id, Label: label} },
)

fromUsers := orm.Compose(pool, shape).From(Users.Source())
fromPosts := orm.Compose(pool, shape).From(Posts.Source())

rows, err := orm.UnionAll[Row](fromUsers, fromPosts).All(ctx)
```

```sql
SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users"
UNION ALL
SELECT "posts"."id" AS "thing_id", "posts"."title" AS "label" FROM "public"."posts"
```

`All`, `One`, `Rows` и `SQL` работают так же, как в любом другом запросе. Если
ветви строились без исполнителя, дайте его составному запросу через `Using`:

```go
rows, err := orm.UnionAll[Row](fromUsers, fromPosts).Using(pool).All(ctx)
```

## Сортировка результата

`OrderBy` у составного запроса принимает **объявление колонки результата**, а не
колонку, — потому что `ORDER BY` составного запроса может называть только
колонку результата:

```go
label := orm.Named("label", orm.Of(Users.Email))

rows, err := orm.UnionAll[Row](fromUsers, fromPosts).
    OrderBy(label.Asc()).
    Limit(20).
    All(ctx)
```

Сортировка по колонке не скомпилируется:

```go
.OrderBy(Users.ID.Asc())          // не компилируется
.OrderBy(orm.Of(Users.ID).Asc())  // тоже не компилируется
```

Это намеренно. Оба варианта отрисовали бы терм, который PostgreSQL отвергает
сразу, и `OutputOrder` существует, чтобы сделать их ненаписуемыми, а не чтобы
поймать позже.

## Правила

У ветвей должны быть **в точности** совместимые формы результата — то же число колонок, тот же порядок, те же Go-типы, та же nullability. Контракт v1 намеренно строже неявных приведений PostgreSQL:

- `int32` и `int64` не сливаются.
- `uuid.UUID` и `string` не сливаются.
- Nullable и не-nullable не сливаются.

Для части этих случаев PostgreSQL нашёл бы общий тип. Разрешить это значило бы поставить Go-тип результата в зависимость от правила приведения, которого никто не читает, поэтому ответ — отказ с указанием расхождения.

## ORDER BY и LIMIT внутри ветви

Ветвь может нести собственные `ORDER BY`, `LIMIT` и `OFFSET`, и это значит именно то, что написано:

```sql
(SELECT ... ORDER BY placed DESC LIMIT 2)
UNION ALL
(SELECT ... ORDER BY placed DESC LIMIT 2)
LIMIT 3
```

Скобки — это грамматика, а не стиль. Без них PostgreSQL относит эти конструкции ко всему составному запросу, и ветвь, которая выглядит ограниченной, таковой не является. Компилятор ставит скобки ровно тогда, когда ветвь несёт что-то из этого.

## ORDER BY и LIMIT составного запроса

`ORDER BY`, `LIMIT` и `OFFSET` на составном запросе применяются к полному результату, после обеих ветвей:

```sql
SELECT ... UNION ALL SELECT ... ORDER BY "email" ASC LIMIT 10
```

`ORDER BY` составного запроса может называть **колонку результата** и ничего больше. Это правило PostgreSQL: квалифицированная ссылка даёт `missing FROM-clause entry`, а выражение — `invalid UNION/INTERSECT/EXCEPT ORDER BY clause`. Без внешнего `ORDER BY` порядок результата не обещан.

## Плейсхолдеры глобальны

Ветви делят один список параметров:

```sql
SELECT ... WHERE email = $1
UNION ALL
SELECT ... WHERE label = $2
```

Перезапуск нумерации во второй ветви дал бы SQL, который PostgreSQL принимает и в который подставляет не те значения, — поэтому именно это свойство стоит первым в реализации.

## Область видимости — своя у каждой ветви

Ветвь видит элементы `WITH` составного запроса и собственные источники. Чужие — нет. Ветвь не является механизмом разделения области видимости, и это структурно: каждая ветвь открывает свой кадр области.

## Вложенность

`A UNION ALL B UNION ALL C` — это одна операция над тремя входами, поэтому она отрисовывается плоско. Построение как `A UNION ALL (B UNION ALL C)` заключает внутренний в скобки, потому что вы этого попросили — и потому что собственные `ORDER BY` и `LIMIT` внутреннего иначе привязались бы к внешнему.

## Один запрос

Составной запрос — это один SQL-оператор. Это никогда не два запроса, строки которых складываются в Go: так потерялись бы общий `ORDER BY` и `LIMIT`, а один поход к серверу стал бы двумя.

## Разобранные примеры

### Одна лента активности из трёх таблиц

Комментарии, лайки и подписки, перемешанные по времени. Одинаковую форму им
задаёт проекция:

```go
type Item struct {
    At   time.Time
    Kind string
    Text string
}

feed := func(at orm.Expression[time.Time, *time.Time], kind string,
    text orm.Expression[string, *string]) orm.Projection[orm.Composed, Item] {
    return orm.Project3(at, orm.Val(kind), text,
        func(a time.Time, k, t string) Item { return Item{a, k, t} })
}

when := orm.Named("at", orm.Of(Comments.PostedAt))

rows, err := orm.UnionAll[Item](
    orm.Compose(pool, feed(orm.Of(Comments.PostedAt), "comment", orm.Of(Comments.Body))).
        From(Comments.Source()),
    orm.Compose(pool, feed(orm.Of(Likes.LikedAt), "like", orm.Of(Likes.Target))).
        From(Likes.Source()),
    orm.Compose(pool, feed(orm.Of(Follows.At), "follow", orm.Of(Follows.Handle))).
        From(Follows.Source()),
).OrderBy(when.Desc()).Limit(50).All(ctx)
```

Три таблицы, один оператор, один `ORDER BY` по всему результату. Забирать каждую
отдельно и сливать в Go значило бы получить все три целиком, прежде чем взять
пятьдесят свежайших.

### Живые строки и архивные

Одна и та же форма из двух таблиц, которые суть одна таблица, разделённая по
возрасту:

```go
rows, err := orm.UnionAll[Row](
    orm.Compose(pool, shape).From(Orders.Source()).
        Where(orm.Cond(Orders.PlacedAt.Gte(cutoff))),
    orm.Compose(pool, shape).From(ArchivedOrders.Source()).
        Where(orm.Cond(ArchivedOrders.PlacedAt.Lt(cutoff))),
).All(ctx)
```

### Лимиты ветвей и лимит составного запроса

```go
// По две свежайших из каждой, затем три свежайших в целом.
rows, err := orm.UnionAll[Row](
    orm.Compose(pool, shape).From(Inbox.Source()).
        OrderBy(orm.Of(Inbox.At).Desc()).Limit(2),
    orm.Compose(pool, shape).From(Archive.Source()).
        OrderBy(orm.Of(Archive.At).Desc()).Limit(2),
).OrderBy(when.Desc()).Limit(3).All(ctx)
```

Забираются четыре строки, возвращаются три. Лимиты ветвей заключены в скобки,
поэтому остаются лимитами ветвей.
