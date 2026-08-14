# Связи

> Загружается то, что попросили, за предсказуемое число запросов.

Source: https://ormgo.vercel.app/ru/docs/relations/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Объявление

```go
//orm:table public.users
type User struct {
    ID     int64 `orm:"pk,identity"`
    Orders orm.Many[Order]
}

//orm:table public.orders
type Order struct {
    ID     int64 `orm:"pk,identity"`
    UserID int64
    User   orm.One[User] `orm:"fk:user_id"`
}
```

Внешний ключ называется на той стороне, которая его держит. Генератор проверяет, что он существует и указывает туда, куда вы сказали.

## Загрузка

```go
users, err := db.Users.Query().With(Users.Orders).All(ctx)
```

`With` загружает то, что ему дали, и ничего больше. Ленивой загрузки нет, поэтому цикл по результату не превратится в запрос на строку.

## Предсказуемое число запросов

Загрузка идёт в ширину и пакетами. Число запросов зависит от формы запрошенного дерева, а не от количества строк:

```go
db.Users.Query().
    With(Users.Orders.With(Orders.Items)).
    All(ctx)
// три запроса: пользователи, затем все их заказы, затем все позиции
```

Десять пользователей или десять тысяч — их три.

## Настройка связи

`Rel` несёт опции, и они применяются к каждому родителю — именно поэтому «пять последних заказов каждого пользователя» это один запрос, а не N:

```go
db.Users.Query().
    With(Users.Orders.
        Where(Orders.Status.Eq("paid")).
        OrderBy(Orders.Placed.Desc()).
        Limit(5)).
    All(ctx)
```

## Фильтрация по связи без загрузки

```go
// пользователи хотя бы с одним оплаченным заказом
db.Users.Query().Where(Users.Orders.Any(Orders.Status.Eq("paid")))

// и без единого
db.Users.Query().Where(Users.Orders.None(Orders.Status.Eq("refunded")))
```

Компилируются в полусоединения. Ничего не загружают, поэтому и читать нечего.

## Чтение результата

```go
for _, u := range users {
    orders, ok := u.Orders.Get()
    if !ok {
        // не загружено — это не то же самое, что «загружено и пусто»
        continue
    }
    fmt.Println(len(orders))
}
```

Три различимых состояния: не загружено, загружено и пусто, загружено и есть. Нулевое значение — «не загружено», поэтому литерал без связи говорит «я не просил», а не «там пусто».

## Что определяет связанность

PostgreSQL. Строки связываются по тому, что база считает равными ключами, поэтому `citext`, `numeric`, домены и составные ключи ведут себя как в базе, а не как Go-шное равенство.

## Разобранные примеры

### Программа конференции

Каждый поток со своими докладами, каждый доклад со спикерами — три уровня, три
запроса, сколько бы ни было строк.

```go
tracks, err := db.Tracks.Query().
    Where(Tracks.ConferenceID.Eq(confID)).
    With(Tracks.Talks.
        OrderBy(Talks.StartsAt.Asc()).
        With(Talks.Speakers)).
    OrderBy(Tracks.Name.Asc()).
    All(ctx)
```

Сортировка внутри `With` — это сортировка самих докладов. Отсортировать их потом
в Go тоже можно, но это значит забрать их в том порядке, в каком их нашёл сервер.

### Инвентаризация склада

Товары, которые ни разу не пересчитывали, — фильтр по отсутствию связи, без
загрузки:

```go
uncounted, err := db.Products.Query().
    Where(Products.Counts.None()).
    OrderBy(Products.SKU.Asc()).
    All(ctx)
```

И наоборот, с условием на потомке:

```go
disputed, err := db.Products.Query().
    Where(Products.Counts.Any(Counts.Variance.Gt(0))).
    All(ctx)
```

Оба компилируются в полусоединения. Ни один не привезёт ни одной строки
пересчёта, потому что вы её не просили.

### Входящие поддержки

Открытые обращения только с последним сообщением — работу делает лимит на
родителя:

```go
tickets, err := db.Tickets.Query().
    Where(Tickets.Status.Eq("open")).
    With(Tickets.Messages.
        OrderBy(Messages.SentAt.Desc()).
        Limit(1)).
    OrderBy(Tickets.OpenedAt.Asc()).
    All(ctx)
```

`Limit(1)` относится к обращению, а не к результату. Один запрос возвращает
свежайшее сообщение для каждого из них.
