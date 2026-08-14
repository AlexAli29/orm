# Представления

> Источники чтения первого класса и жизненный цикл обновления.

Source: https://ormgo.vercel.app/ru/docs/views/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

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

## Как из него читать

`ViewRepo` даёт `Query` и `QueryFrom` — и больше ничего. Это тот же строитель
запросов, что и у таблицы: те же предикаты, сортировка, страницы, проекции и
композиция:

```go
rows, err := db.MonthlyRevenues.Query().
    Where(MonthlyRevenues.Plan.Eq("pro")).
    OrderBy(MonthlyRevenues.Month.Desc()).
    Limit(12).
    All(ctx)
```

Чего он **не** даёт — записи. У репозитория нет ни `Insert`, ни `Update`, ни
`Delete`, потому что их нет и у PostgreSQL для представления без правила или
триггера, — значит, генерировать тут нечего даже в принципе.

### Nullable-колонки меняют чтение предикатов

Все колонки представления — nullable-дескрипторы, поэтому `IsNull` есть у каждой,
а тип значения обычный:

```go
// Сравнение принимает обычную строку: nullable колонка, а не аргумент.
db.MonthlyRevenues.Query().Where(MonthlyRevenues.Plan.Eq("pro"))

// И это доступно у каждой колонки, чего не было бы у таблицы.
db.MonthlyRevenues.Query().Where(MonthlyRevenues.Cents.IsNull())
```

Если для представления, где колонки заведомо никогда не NULL, это лишний шум, —
решение в том, чтобы сканировать в указатели или спроецировать нужные колонки
своей формой, а не объявлять их не-nullable: доказать это библиотека не может.

### Матпредставление отдаёт снимок

```go
rows, err := db.SearchRows.Query().
    Where(SearchRows.Name.ILike("%lamp%")).
    Limit(20).
    All(ctx)
```

Ровно как запрос к таблице — в этом и смысл: работа была проделана во время
обновления. Строки настолько же старые, насколько давним было последнее удачное
обновление; это и есть размен, на который вы пошли, выбрав материализованное
представление вместо обычного.

### Джойн представления с таблицей

Представление — такой же источник, как любой другой, поэтому оно составляется:

```go
shape := orm.Project2(
    orm.Opt(MonthlyRevenues.Cents),
    orm.Of(Plans.Name),
    func(cents *int64, name string) Row { return Row{cents, name} },
)

rows, err := orm.Compose(pool, shape).
    From(Plans.Source()).
    LeftJoin(MonthlyRevenues.Source(), orm.Eq(MonthlyRevenues.Plan, Plans.Code)).
    OrderBy(orm.Of(Plans.Name).Asc()).
    All(ctx)
```

### Два вхождения одного представления

```go
thisYear := MonthlyRevenues.As("this_year")

orm.Compose(pool, shape).
    From(thisYear.Source()).
    Join(MonthlyRevenues.Source(), orm.Eq(MonthlyRevenues.Plan, thisYear.Plan))
```

`QueryFrom` — эквивалент для запроса по сущности, принимающий источник, который
вы отальясили.

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

## Разобранные примеры

### Отчётное представление

```go
//orm:view analytics.monthly_revenue
//orm:definition `SELECT date_trunc('month', issued_at) AS month,
//                       plan, sum(amount_cents) AS cents
//                  FROM billing.invoices GROUP BY 1, 2`
//orm:depends-on billing.invoices
type MonthlyRevenue struct {
    Month time.Time
    Plan  string
    Cents int64
}
```

Все колонки приходят nullable, потому что nullability результата представления
недоказуема: `sum` по пустому множеству — это NULL, а определение не хранит,
какие колонки такими быть могут.

### Матпредставление с конкурентным обновлением

```go
//orm:materialized-view analytics.search_index
//orm:definition `SELECT p.id, p.name, p.tags FROM catalog.products p WHERE p.listed`
//orm:depends-on catalog.products
//orm:index search_index_id_key (ID) unique
type SearchRow struct {
    ID   int64
    Name string
    Tags []string
}
```

Уникальный индекс по одной обычной колонке — то, что делает `Concurrently`
возможным. Без него обновление берёт монопольную блокировку, и сайт не отвечает,
пока оно идёт.

```go
if err := db.SearchRows.Refresh(ctx, orm.Concurrently()); err != nil {
    var pge *pgconn.PgError
    if errors.As(err, &pge) && pge.Code == "55000" {
        // индекса нет; перегенерировать и выкатить
    }
    return err
}
```

### Обновление по расписанию

```go
func refreshLoop(ctx context.Context, db *domain.DB) {
    t := time.NewTicker(5 * time.Minute)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            if err := db.SearchRows.Refresh(ctx, orm.Concurrently()); err != nil {
                log.Printf("refresh: %v", err)
            }
        }
    }
}
```

Конкурентное обновление не блокирует читателей, поэтому тик раз в пять минут —
это трата процессора, а не доступности.
