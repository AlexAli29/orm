# Ключевые идеи

> Словарь, которым пользуется остальная документация.

Source: https://ormgo.vercel.app/ru/docs/concepts/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Сущность (entity)

Go-структура с директивой `//orm:table`. Её пишете вы; ничто её не генерирует.

## Дескриптор

Сгенерированная типизированная ручка колонки: `Users.Email`, `Orders.Placed`. Её Go-тип кодирует то, что сказал каталог, — тип значения, nullable ли она и какие сравнения PostgreSQL для неё определяет.

```go
Users.Email   // orm.TextCol[User]              — есть Like, ILike
Users.ID      // orm.OrdCol[User, int64]        — есть Gt, Between, Asc
Users.Bio     // orm.NullTextCol[User]          — есть IsNull
Users.Tags    // orm.Col[User, []string]        — только равенство
```

Дескрипторы доступны только для чтения и безопасны для совместного использования. Всё, что похоже на мутацию — алиас таблицы, настройка связи, — возвращает копию.

## Capability

Что тип колонки умеет в SQL, а не что с ним умеет Go. `uuid` упорядочен, потому что его упорядочивает PostgreSQL; `jsonb` — нет, потому что сравнение двух jsonb-документов не отвечает ни на чей вопрос. Поэтому порядок не выводится из Go-шного `cmp.Ordered`.

## Источник (source)

Одно вхождение отношения в запрос. Алиас таблицы порождает второй источник, и дескриптор, построенный от одного, нельзя применить к другому — именно это делает self-join безопасным.

## Repo

`Repo[E]` связывает сгенерированные метаданные с исполнителем: `*pgxpool.Pool`, `*pgx.Conn` или `pgx.Tx`. Сгенерированный код даёт структуру `DB`, где по одному на сущность.

## Query, SelectQuery, ComposedQuery

Три строителя, три задачи:

- `Query[E]` читает целые сущности из одной таблицы.
- `SelectQuery[E, R]` читает проекцию — выбранную форму результата — из одной таблицы.
- `ComposedQuery[R]` читает проекцию из источников, которые вы собрали сами: джойны, CTE, производные таблицы.

Все трое изменяемы, одноразовы и не потокобезопасны. `Clone` ответвляет копию.

## Проекция

Две вещи вместе: **какие выражения выбрать** и **функция, превращающая эти
значения в ваш тип результата**. `Project2` принимает два выражения, поэтому его
функция принимает два параметра — в том же порядке и с теми типами, которые
имеют эти колонки.

```go
type Summary struct {
    ID    int64
    Email string
}

var Summaries = orm.Project2(
    Users.ID,     // 1-е выражение → 1-й параметр, int64, потому что id — bigint
    Users.Email,  // 2-е выражение → 2-й параметр, string, потому что email — text
    func(id int64, email string) Summary { return Summary{ID: id, Email: email} },
)

rows, _ := orm.Select(db.Users, Summaries).All(ctx)
// []Summary — SELECT id, email FROM users
```

Это значение, а не запрос: постройте одну и используйте из многих запросов.
Подробно — в разделе [Проекции](/ru/docs/projections/).

## Nullability и откуда она берётся

Значение становится nullable по двум разным причинам:

1. **Колонка** nullable. `Users.Bio` — это `NullTextCol`.
2. **Запрос** делает её такой. Колонка `NOT NULL`, прочитанная через `LEFT JOIN`, может быть NULL для строки без совпадения.

Второе — nullability, наведённая источником. Ради неё существует `orm.Opt`, и поэтому список выборки, читающий outer-joined источник через `orm.Of`, отвергается.

## Ошибки

Ошибки-сентинелы (`ErrNotFound`, `ErrMissingWhere`) оборачиваются, а не подменяются, поэтому `errors.Is` работает сквозь контекст каждого слоя, а собственный `*pgconn.PgError` остаётся доступен через `errors.As`. Ни один экспортированный API не паникует из-за плохого запроса, ошибки базы или неудачного сканирования.

## Разобранные примеры

### Как читать тип дескриптора

Тип и есть документация. Когда непонятно, что умеет колонка, объявление отвечает:

```go
Products.Name      // orm.TextCol[Product]              — Like, ILike
Products.PriceCents // orm.OrdCol[Product, int32]       — Gt, Between, Asc
Products.Discount  // orm.NullOrdCol[Product, int32]    — то же плюс IsNull
Products.Tags      // orm.Col[Product, []string]        — только равенство
Products.Meta      // orm.Col[Product, map[string]any]  — только равенство
```

Метод, который вы ожидали и не нашли, — обычно ответ «PostgreSQL не определяет
этого для такого типа».

### Один источник или два

```go
managers := Employees.As("mgr")

orm.Compose(pool, shape).
    From(Employees.Source()).
    LeftJoin(managers.Source(), orm.Eq(managers.ID, Employees.ManagerID))
```

`Employees.ID` и `managers.ID` — одна и та же колонка двух разных вхождений, и
компилятор не позволит подменить одно другим. Именно это делает self-join
безопасным, а не упражнением в именовании.

### Три состояния связи

```go
p, _ := db.Products.Query().Where(Products.ID.Eq(id)).One(ctx)
reviews, ok := p.Reviews.Get()

switch {
case !ok:
    // не загружено — никто не просил
case len(reviews) == 0:
    // загружено, и их действительно нет
default:
    // загружено, и вот они
}
```

Первый случай другие библиотеки схлопывают во второй — так «отзывов нет»
оказывается на странице, которая отзывов не запрашивала.
