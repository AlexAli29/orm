---
title: Тестирование
description: Настоящий PostgreSQL, одноразовые базы и никакого мокинга SQL.
---

## Позиция

Не мокайте базу. Мок SQL-драйвера проверяет, что вы умеете писать моки; а то, что ломается в проде — ограничения, типы, семантика NULL, изоляция транзакций, — ровно то, о чём у мока нет мнения.

Всё здесь сделано так, чтобы запускаться против настоящего PostgreSQL и чтобы это было дёшево.

## База на тестовый бинарник

`ormtest` не создаёт базы. Он даёт то, что делает настоящую базу удобной:
применение миграций, проверку, что схема — это та, которую описывает ваш
конфиг, и запуск теста внутри транзакции, которая всегда откатывается.

```go
import "github.com/AlexAli29/orm/ormtest"

func TestMain(m *testing.M) {
    conn, _ := pgx.Connect(ctx, os.Getenv("TEST_DSN"))
    ormtest.Migrate(t, conn, "migrations")   // применить закоммиченные артефакты
    os.Exit(m.Run())
}
```

`ormtest.CheckSchema(ctx, "orm.yaml")` — та проверка, которую стоит поставить в
CI: она падает, когда база не соответствует схеме из деклараций, а иначе это
всплывает как странно сломавшийся посторонний тест.

`RequireSchemaClean` — та же проверка в виде жёсткого требования.

## Контейнеры

```go
import ormpg "github.com/AlexAli29/orm/ormtest/postgres"

func TestMain(m *testing.M) {
    ormpg.Run(m, ormpg.WithImage("postgres:17"))
}
```

Отдельный модуль, потому что Testcontainers — тяжёлая зависимость, и проект, у которого PostgreSQL уже есть, платить за неё не должен.

## Очистка таблиц между тестами

`Truncate` живёт в `ormtest`, а не в API запросов, потому что очистка таблицы —
это операция фикстуры, а не то, что делает приложение:

```go
import "github.com/AlexAli29/orm/ormtest"

ormtest.MustTruncate(t, pool, domain.Users, domain.Orders)

err := ormtest.TruncateWith(ctx, pool,
    []ormtest.TruncateOption{ormtest.RestartIdentity(), ormtest.Cascade()},
    domain.Users,
)
```

Она принимает сгенерированные ручки таблиц напрямую. `RestartIdentity` сбрасывает
последовательности identity — обычно этого фикстура и хочет; `Cascade` идёт по
внешним ключам и опустошит таблицы, которых вы не называли, поэтому включается
явно.

Очистка нескольких таблиц одним вызовом — не удобство: это единственный способ
опустошить таблицы, ссылающиеся друг на друга, без `Cascade`.

## Транзакция на тест

Быстро и изолированно, без удаления чего-либо:

```go
func TestSomething(t *testing.T) {
    ormtest.TxFunc(t, pool, func(ex orm.Executor) {
        db := domain.New(ex)
        // ... тест ... транзакция откатывается при выходе
    })
}
```

## Проверка SQL без базы

```go
sql, args, err := q.SQL()
if !strings.Contains(sql, "LEFT JOIN") { /* ... */ }
```

Полезно для формы запроса. Не замена запуску: SQL, который выглядит правильным и возвращает не те строки, — это тот самый режим отказа, против которого выстроен весь проект.

## Что должно быть в CI

```bash
orm makemigrations --check   # декларация без миграции
orm check --generated        # сгенерированный код разошёлся
go test -race ./...
```

## Тесты на всех мажорах

Собственный набор совместимости проекта отказывается запускаться меньше чем на всех пяти поддерживаемых мажорах, и идею стоит позаимствовать: матрица, тихо отработавшая на том сервере, который случайно был поднят, доказывает меньше, чем заявляет.

```yaml
strategy:
  matrix:
    postgres: ['14', '15', '16', '17', '18']
```

## Разобранные примеры

### Тест, который ничего после себя не оставляет

```go
func TestPlaceOrder(t *testing.T) {
    ormtest.TxFunc(t, pool, func(ex orm.Executor) {
        db := domain.New(ex)

        customer, err := db.Customers.Insert(t.Context(), Customer{Email: "a@example.com"})
        if err != nil {
            t.Fatal(err)
        }
        order, err := db.Orders.Insert(t.Context(), Order{CustomerID: customer.ID})
        if err != nil {
            t.Fatal(err)
        }
        if order.ID == 0 {
            t.Error("the insert returned no key")
        }
    })
}
```

Всё откатывается при выходе из колбэка, поэтому тесты можно запускать в любом
порядке и ни один не видит строк другого.

### Сброс фикстур между наборами

```go
func resetFixtures(t *testing.T, pool *pgxpool.Pool) {
    ormtest.MustTruncate(t, pool, domain.OrderLines, domain.Orders, domain.Customers)
}
```

Перечисление таблиц одним вызовом и позволяет им ссылаться друг на друга без
`Cascade`.

### Проверка, что схема — та, которую вы объявили

```go
func TestMain(m *testing.M) {
    if err := ormtest.CheckSchema(context.Background(), "orm.yaml"); err != nil {
        log.Fatalf("the test database is not the declared schema: %v", err)
    }
    os.Exit(m.Run())
}
```

Это превращает «тест странно упал» в «база отстала на миграцию», а это гораздо
более короткий разбор.

### Проверка запроса без базы

```go
sql, args, err := db.Orders.Query().
    Where(Orders.CustomerID.Eq(7)).
    OrderBy(Orders.PlacedAt.Desc()).
    SQL()

if !strings.Contains(sql, "ORDER BY") {
    t.Error("the ordering was dropped")
}
if len(args) != 1 {
    t.Errorf("args = %d, want the customer id as a parameter", len(args))
}
```

Полезно для формы. Не замена запуску: SQL, который выглядит правильным и
возвращает не те строки, — тот самый режим отказа, против которого выстроен весь
проект.
