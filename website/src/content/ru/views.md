---
title: Представления
description: Источники чтения первого класса и жизненный цикл обновления.
---

## Представления

Представление объявляется как таблица плюс определение и зависимости:

```go
//orm:view public.user_orders
//orm:definition `SELECT u.id AS user_id, o.id AS order_id, o.label
//                  FROM users u JOIN orders o ON o.user_id = u.id`
//orm:depends-on public.users
//orm:depends-on public.orders
type UserOrder struct {
    UserID  int64
    OrderID int64
    Label   string
}
```

`depends-on` задаёт порядок в плане миграции. Представление, созданное раньше таблицы, из которой оно выбирает, — это миграция, падающая на чистой базе и работающая на вашей.

## Колонки представления nullable

Nullability результата представления недоказуема по определению, поэтому все колонки приходят nullable-дескрипторами:

```go
UserOrders.UserID // NullOrdCol[UserOrder, int64], а не OrdCol
```

Это честно, а не осторожно: `SELECT ... FROM a LEFT JOIN b` может дать NULL в колонке, база которой `NOT NULL`, а представление не хранит, какая именно.

## Материализованные представления

```go
//orm:materialized-view public.user_summaries
//orm:definition `SELECT user_id, count(*) AS orders
//                  FROM user_orders GROUP BY user_id`
//orm:depends-on public.user_orders
//orm:index user_summaries_key (UserID) unique
type UserSummary struct {
    UserID int64
    Orders int64
}
```

`db.UserSummaries` — это `MaterializedViewRepo`. Он даёт то же, что представление, плюс `Refresh`, и никаких записей: в PostgreSQL нет `INSERT` для матпредставления, поэтому генерировать тут нечего даже в принципе.

## Обновление

```go
err := db.UserSummaries.Refresh(ctx)                     // REFRESH MATERIALIZED VIEW
err := db.UserSummaries.Refresh(ctx, orm.Concurrently()) // ... CONCURRENTLY
err := db.UserSummaries.Refresh(ctx, orm.WithNoData())   // ... WITH NO DATA
```

`CONCURRENTLY` требует уникального индекса по непартиальному набору обычных колонок. Генератор вычисляет это и записывает ответ в дескриптор, поэтому проверка не стоит обращения к серверу:

```text
orm: Refresh public.user_summaries: CONCURRENTLY needs a unique index over plain
columns covering every row, and this materialized view has none. A partial or
expression unique index does not qualify. Add one, or refresh without Concurrently
```

## Два способа устареть

Ответ о пригодности — факт о схеме **на момент генерации**, а схема продолжает меняться. Два получающихся состояния ломаются в противоположные стороны, и понимать, в каком вы находитесь, — главная причина перегенерировать.

**Позади базы.** Индекс появился, дескриптор не перегенерирован. Код отказывает локально и ничего не отправляет. Ничего не сломано — что-то недоступно. `orm check --generated` это показывает.

**Впереди базы.** Индекс исчез, дескриптор всё ещё говорит «да». Запрос уходит, и PostgreSQL его отвергает:

```go
if err := db.UserSummaries.Refresh(ctx, orm.Concurrently()); err != nil {
    var pge *pgconn.PgError
    if errors.As(err, &pge) && pge.Code == "55000" {
        // object not in prerequisite state — индекса больше нет
    }
}
```

Ошибка приходит собственная, от PostgreSQL. Переписывание её в общее «обновление не удалось» потеряло бы SQLSTATE и всё, на что вызывающий мог бы отреагировать.

## Выбор индекса детерминирован

Когда подходит несколько индексов, побеждает наименьшее имя. Иначе нельзя: сгенерированный дескриптор и отпечаток, посчитанный по нему, обязаны называть один и тот же индекс на двух прогонах по одной схеме, иначе каждая перегенерация даёт диф.
