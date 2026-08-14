# Отображение типов

> Как тип PostgreSQL становится типом Go и что происходит, когда эквивалента нет.

Source: https://ormgo.vercel.app/ru/docs/types/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Встроенные скаляры

Им не нужна настройка. Справа — тип, который выдаёт генератор.

| PostgreSQL | Go |
| --- | --- |
| `bool` | `bool` |
| `int2`, `int4`, `int8` | `int16`, `int32`, `int64` |
| `float4`, `float8` | `float32`, `float64` |
| `text`, `varchar`, `bpchar`, `citext`, `name` | `string` |
| `bytea` | `[]byte` |
| `date`, `timestamp`, `timestamptz` | `time.Time` |
| `uuid` | *настраивается* |
| `numeric` | *настраивается* |
| `json`, `jsonb` | `orm.JSON` / `orm.JSONB` |
| `inet`, `cidr` | `netip.Prefix` / `netip.Addr` |
| `macaddr` | `net.HardwareAddr` |
| `interval` | `orm.Interval` |
| `tsvector`, `tsquery` | `orm.TSVector`, `orm.TSQuery` |
| `int4range`, `daterange`, … | `orm.Range[T]` |
| `int4multirange`, … | `orm.Multirange[T]` |
| `T[]` | `[]T` |

## Типов, которых в Go нет

Два из них отвергаются, а не угадываются, и отказ здесь — это фича.

### numeric

Для десятичного числа произвольной точности в Go нет типа без потерь, а отображение в `float64` тихо испортило бы деньги. Поэтому его нужно настроить:

```yaml
types:
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
```

### uuid

В Go нет типа `uuid`, а популярные сторонние не взаимозаменяемы. Библиотека отказывается выбирать; выбирает проект, и этот выбор — зависимость проекта:

```yaml
types:
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid
```

Сама библиотека никогда не зависит от `google/uuid`. Это проверяется в CI, потому что обязательная зависимость от uuid — ровно то, ради чего существуют настраиваемые отображения.

## Одна асимметрия, о которой стоит знать

Настроенное отображение работает в одну сторону.

| Режим | Тег | Результат |
| --- | --- | --- |
| Database-first | нет | работает — `uuid` → `uuid.UUID` |
| Managed | нет | **отказ** — нет типа PostgreSQL для `uuid.UUID` |
| Managed | `pgtype:uuid` | работает |

Database-first начинает с типа PostgreSQL и ищет Go-тип, поэтому отображение применяется само. Managed начинает с Go-типа, а обратного поиска нет: конфигурация, отображающая два Go-типа в один тип PostgreSQL, дала бы два ответа и никакого способа выбрать. Поэтому managed нужно сказать явно:

```go
ID   uuid.UUID   `orm:"pk,pgtype:uuid"`
Tags []uuid.UUID `orm:"pgtype:uuid[]"`
```

## Домены

Домены поддержаны обобщённо: сверка идёт от домена к типу, на котором он построен, поэтому колонка типа `tenant_uuid` над `uuid` обслуживается единственным настроенным отображением `uuid` без отдельной записи.

Указывайте имя со схемой. Неквалифицированное написание мигрирует, а читается обратно квалифицированным, и эти два не равны — неизменившийся проект начнёт сообщать о дрейфе:

```go
TenantID uuid.UUID `orm:"pgtype:public.tenant_uuid"` // правильно
TenantID uuid.UUID `orm:"pgtype:tenant_uuid"`        // постоянный ложный дрейф
```

## Диапазоны сохраняют границы

Пара концов не скажет, включена граница, исключена или бесконечна, поэтому `Range[T]` несёт всю модель. Какой именно из `daterange`, `tsrange` и `tstzrange` перед вами, берётся из каталога, а не угадывается по Go-типу.

```go
r := orm.ClosedOpen(start, end)
db.Bookings.Query().Where(Bookings.During.Overlaps(r))
```

Значения, которые PostgreSQL канонизирует — дискретные диапазоны и все мультидиапазоны, — возвращаются такими, какими их держит сервер.

## Interval — это не Duration

`Interval` держит месяцы, дни и микросекунды раздельно и отказывается становиться `time.Duration`, когда содержит календарную составляющую. У месяца нет фиксированной длины, и ошибка говорит именно это, а не тихо берёт 30 дней.

```go
d, err := iv.Duration()
if errors.Is(err, orm.ErrCalendarInterval) {
    // есть месяцы или дни; что они значат, решает вызывающий
}
```

## Неподдержанные типы отвергаются

Колонка, для типа которой нет отображения, останавливает генерацию с диагностикой: имя колонки, тип и способ починки. Она никогда не деградирует до `any`, `string` или `[]byte` — заглушка, которая «сканируется», хуже упавшей сборки, потому что ломается позже и дальше от причины.

## Разобранные примеры

### Деньги без float

```go
//orm:table public.invoices
type Invoice struct {
    ID    int64   `orm:"pk,identity"`
    Cents int64                                  // простой ответ
    Total decimal.Decimal `orm:"pgtype:numeric"` // точный
}
```

Целые копейки годятся, пока не понадобится третий знак после запятой или ставка.
`numeric` точен при любом масштабе, и его отображение требует записи
`types.numeric` — библиотека не выберет пакет для десятичных за вас.

### Адреса и сети

```go
//orm:table public.sessions
type Session struct {
    ID     int64        `orm:"pk,identity"`
    Client netip.Addr   `orm:"pgtype:inet"`
    Subnet netip.Prefix `orm:"pgtype:cidr"`
    Device net.HardwareAddr `orm:"pgtype:macaddr"`
}
```

Они упорядочиваются и индексируются как адреса, а не как текст, поэтому диапазон
подсети — это диапазон, а не `LIKE`.

### Массивы, которые что-то значат

```go
//orm:table public.articles
type Article struct {
    ID      int64      `orm:"pk,identity"`
    Tags    []string                        // NOT NULL, может быть пустым
    Authors *[]int64                        // nullable: списка нет вовсе
}
```

Пустой массив и NULL-массив — разные значения, и библиотека их различает. Что
именно вам нужно — решение о схеме, а указатель — способ его высказать.

### Домен, чтобы правило несла сама схема

```go
// CREATE DOMAIN email AS citext CHECK (VALUE ~ '@');
type Contact struct {
    Address string `orm:"pgtype:public.email"`
}
```

Сверка идёт от домена к `citext` и отображает его в `string`. Имя указывайте со
схемой, иначе мигрированное и прочитанное имена не совпадут.
