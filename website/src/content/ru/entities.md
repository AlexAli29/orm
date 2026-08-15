---
title: Сущности и теги
description: Директивы и теги структур, которые читает генератор.
---

## Директивы

Директива — это комментарий над типом. Она говорит, чем тип является.

```go
//orm:table public.users
type User struct { /* ... */ }

//orm:view public.active_users
//orm:definition `SELECT id, email FROM users WHERE active`
//orm:depends-on public.users
type ActiveUser struct { /* ... */ }

//orm:materialized-view public.user_summaries
//orm:definition `SELECT user_id, count(*) AS orders FROM user_orders GROUP BY user_id`
//orm:depends-on public.user_orders
//orm:index user_summaries_key (UserID) unique
type UserSummary struct { /* ... */ }
```

Имя отношения указывается со схемой. `public` не подразумевается: в проекте с двумя схемами у одного имени было бы два смысла.

## Грамматика тегов

Всё остальное — теги структуры под ключом `orm`:

| Директива | Значение |
| --- | --- |
| `pk` | Часть первичного ключа |
| `identity` | Значение генерирует PostgreSQL (`identity:always` — `GENERATED ALWAYS`) |
| `unique` | Уникальное ограничение по одной колонке |
| `column:name` | Имя колонки, если отличается от поля |
| `pgtype:uuid` | Тип PostgreSQL, если Go-тип его не задаёт |
| `type:name` | Ключ настроенного отображения типов |
| `default:expr` | `DEFAULT` колонки |
| `generated:expr` | Генерируемая колонка |
| `fk:user_id` | Колонка внешнего ключа для связи |
| `side:...` | Сторона связи |
| `ondelete:cascade` | Действие `ON DELETE` для связи |
| `onupdate:cascade` | Действие `ON UPDATE` для связи |
| `-` | Полностью игнорировать поле |

```go
//orm:table public.orders
//orm:index orders_user_idx (UserID)
type Order struct {
    ID         uuid.UUID  `orm:"pk,pgtype:uuid"`
    UserID     uuid.UUID  `orm:"pgtype:uuid"`
    Label      string     `orm:"column:title"`
    Total      string     `orm:"pgtype:numeric"`
    CreatedAt  time.Time  `orm:"default:now()"`
    Internal   string     `orm:"-"`

    User orm.One[User] `orm:"fk:user_id"`
}
```


### Ссылочные действия

`ondelete` и `onupdate` принимают `cascade`, `restrict`, `setnull`, `setdefault`
или `noaction`. Пишутся без пробела, потому что для всего, что читает
структурный тег, это один токен:

```go
//orm:table comments
type Comment struct {
    ID     int64 `orm:"pk,identity"`
    PostID int64

    // Deleting a post deletes its comments, in the database, in one statement.
    Post orm.One[Post] `orm:"side:local,ondelete:cascade"`

    // Deleting a user with comments is refused instead.
    Author orm.One[User] `orm:"side:local,ondelete:restrict"`
}
```

Читаются только в управляемом режиме. В database-first ограничение уже
существует, и решает ответ PostgreSQL, так что тег с другим требованием был бы
пожеланием, а не фактом.

Ничего не написать — значит `NO ACTION`, умолчание PostgreSQL, и в управляемом
режиме это утверждение, а не отсутствие. База, где ограничение говорит
`CASCADE`, и объявление, которое молчит, расходятся, и `makemigrations`
запланирует замену каскада. Если вы переводите на управляемый режим базу, где
каскад уже есть, напишите тег до первого плана.

Это каскад уровня базы данных — не то же самое, что каскады уровня приложения,
которых в этой библиотеке нет. Схемой владеет PostgreSQL, и `ON DELETE CASCADE`
— часть схемы; ничто здесь не удаляет строки на стороне Go за вас.

## Nullability

Указатель — это nullable-колонка. Больше знать нечего:

```go
Bio        *string     // bio text
OptionalID *uuid.UUID  // optional_id uuid
Tags       []string    // tags text[] NOT NULL
```

Пустой срез и NULL-массив — разные значения, и библиотека их различает. Если колонка-массив nullable, пишите `*[]string`.

## Нулевые значения — это значения

```go
db.Users.Insert(ctx, User{Active: false})               // сохранит FALSE
db.Users.Insert(ctx, User{}, orm.Default(Users.Active)) // сохранит DEFAULT колонки
```

Библиотека не отличит «false» от «поля, которого никто не касался», а угадывание — это способ получить строку со значением, которого никто не выбирал. Запрос значения по умолчанию — отдельное явное действие.

## Связи

`One` и `Many` объявляют связи и различают три состояния: не загружено, загружено и пусто, загружено и есть.

```go
type User struct {
    ID     int64 `orm:"pk,identity"`
    Orders orm.Many[Order]
}

type Order struct {
    ID     int64 `orm:"pk,identity"`
    UserID int64
    User   orm.One[User] `orm:"fk:user_id"`
}
```

Нулевое значение — «не загружено», поэтому литерал структуры без связи говорит «я этого не просил», а не «там пусто». Читается через `Get() ([]T, bool)` или `MustGet()`.

## Индексы

Объявляются на типе, потому что индекс принадлежит отношению, а не колонке:

```go
//orm:index users_email_key (Email) unique
//orm:index users_active_idx (Active, CreatedAt)
//orm:index users_lower_email_idx ("lower(email)")
//orm:index users_tags_gin_idx (Tags) using gin
//orm:index users_paid_idx (CreatedAt) where "paid_at IS NOT NULL"
```

Поля называются по Go-именам; строка в кавычках — выражение SQL.

## Разобранные примеры

### Мультиарендная таблица

Всё, что нужно колонке арендатора: тег, составной ключ и индекс, который делает
выборку ограниченной, а не отфильтрованной.

```go
//orm:table public.documents
//orm:index documents_tenant_idx (TenantID, UpdatedAt)
//orm:index documents_slug_key (TenantID, Slug) unique
type Document struct {
    TenantID  int64     `orm:"pk"`
    ID        int64     `orm:"pk,identity"`
    Slug      string
    Title     string
    Body      *string
    UpdatedAt time.Time `orm:"default:now()"`
}
```

Два поля с `pk` — это составной ключ. Уникальный индекс построен по паре, поэтому
два арендатора могут использовать один и тот же slug, а один арендатор — нет.

### Таблица, где колонки названы иначе

```go
//orm:table billing.invoice_lines
type InvoiceLine struct {
    ID        int64  `orm:"pk,identity"`
    InvoiceID int64  `orm:"column:inv_id"`
    Cents     int32  `orm:"column:amount_cents"`
    Note      string `orm:"-"`   // вообще не колонка
}
```

`column:` — для схемы, которую выбирали не вы. `-` — для поля, которое ваше и
только ваше: кэш, помощник форматирования; генератор его искать не станет.

### Генерируемые колонки и значения по умолчанию

```go
//orm:table public.people
type Person struct {
    ID       int64  `orm:"pk,identity:always"`
    First    string
    Last     string
    Full     string    `orm:"generated:first || ' ' || last"`
    JoinedAt time.Time `orm:"default:now()"`
    Ref      uuid.UUID `orm:"pgtype:uuid,default:gen_random_uuid()"`
}
```

`identity:always` означает, что PostgreSQL отвергнет присланное вами значение, —
это строже обычного `identity`.

### Индексы, которые стоит объявлять

```go
//orm:index orders_open_idx (PlacedAt) where "shipped_at IS NULL"
//orm:index orders_lower_ref_idx ("lower(reference)")
//orm:index orders_tags_gin_idx (Tags) using gin
```

Частичный индекс по открытым заказам меньше индекса по всем и остаётся
маленьким, пока таблица растёт.
