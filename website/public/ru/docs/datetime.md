# Даты и интервалы

> Усечение, извлечение и тип интервала, который отказывается врать про месяцы.

Source: https://ormgo.vercel.app/ru/docs/datetime/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Interval — это не Duration

`time.Duration` — это количество наносекунд. `interval` в PostgreSQL состоит из
трёх независимых частей — месяцы, дни и микросекунды — и держит их порознь,
потому что они не переводятся друг в друга:

- в месяце от 28 до 31 дня
- в сутках 23, 24 или 25 часов на границе перевода часов

```go
iv := orm.IntervalOf(months, days, micros)
iv := orm.IntervalFromDuration(90 * time.Minute) // без месяцев и дней
```

Обратное преобразование работает, только если внутри нет ничего календарного:

```go
d, err := iv.Duration()
if errors.Is(err, orm.ErrCalendarInterval) {
    // есть месяцы или дни; что они значат, зависит от точки отсчёта,
    // и библиотека не выберет 30 дней за вас
}
```

Эта ошибка и есть весь замысел. Библиотека, которая молча вернула бы 720 часов
за месяц, была бы права почти всегда и неправа на каждой границе месяца.

## Арифметика

```go
orm.AddInterval(Events.At, orm.Val(iv))   // метка времени + интервал
orm.SubInterval(Events.At, orm.Val(iv))
orm.IntervalPlus(a, b)                    // interval + interval
orm.IntervalMinus(a, b)
orm.IntervalTimes(a, 3)                   // interval * n
```

У каждой есть форма `…Null` для nullable-входов, потому что арифметика с NULL
даёт NULL.

## Усечение

```go
orm.DateTrunc(orm.Month, Events.At)   // date_trunc('month', at)
orm.DateTrunc(orm.Day, Events.At)
orm.DateTrunc(orm.Hour, Events.At)
```

Классическое применение — группировка временного ряда по корзинам:

```go
bucket := orm.DateTrunc(orm.Day, Events.At)

var perDay = orm.Project2(
    bucket, orm.Count[Event](),
    func(day time.Time, n int64) Bucket { return Bucket{day, n} },
)

orm.Select(db.Events, perDay).
    Where(Events.At.Gte(since)).
    GroupBy(bucket).
    OrderBy(bucket.Asc()).
    All(ctx)
```

Группируйте по **тому же выражению**, которое выбрали. Два вызова `DateTrunc` с
одинаковыми аргументами дают одинаковый SQL, и PostgreSQL их сопоставит, — но
переменная говорит это явно и читается лучше.

## Извлечение

```go
orm.Extract(orm.Year, Events.At, orm.Integer)         // -> int32
orm.Extract(orm.DayOfWeek, Events.At, orm.Integer)    // 0 = воскресенье
orm.Extract(orm.EpochSecond, Events.At, orm.BigInt)   // -> int64
```

Третий аргумент — тип, который вы хотите получить, в виде значения `PGType`.
`extract` в PostgreSQL возвращает `numeric`, поэтому кто-то должен сказать, во что
его привести; сказанное здесь означает, что Go-тип определён, а не утверждён
задним числом.

Поля:

```go
orm.Year   orm.Quarter  orm.Month  orm.Week  orm.Day
orm.Hour   orm.Minute   orm.Second
orm.DayOfWeek  orm.DayOfYear  orm.EpochSecond
```

## Сравнение

Метки времени — упорядоченные колонки, поэтому работают обычные предикаты:

```go
db.Events.Query().Where(Events.At.Between(dayStart, dayEnd))
db.Events.Query().Where(Events.At.Gte(cutoff))
db.Events.Query().OrderBy(Events.At.Desc())
```

Для «за последние N» вычисляйте границу в Go, а не в SQL, когда это возможно:
параметр привязки — лучший вход для планировщика, чем выражение, которое ему
приходится вычислять на каждой строке.

## Разобранные примеры

### График регистраций по дням

```go
day := orm.DateTrunc(orm.Day, Accounts.CreatedAt)

var perDay = orm.Project2(
    day, orm.Count[Account](),
    func(d time.Time, n int64) Point { return Point{d, n} },
)

rows, err := orm.Select(db.Accounts, perDay).
    Where(Accounts.CreatedAt.Gte(since)).
    GroupBy(day).
    OrderBy(day.Asc()).
    All(ctx)
```

По месяцам — тот же запрос с одним изменённым словом; ради этого корзину и
называют.

### Часы посещаемости

```go
hour := orm.Extract(orm.Hour, Visits.At, orm.Integer)
dow  := orm.Extract(orm.DayOfWeek, Visits.At, orm.Integer)

var heat = orm.Project3(
    dow, hour, orm.Count[Visit](),
    func(d, h int32, n int64) Cell { return Cell{d, h, n} },
)

orm.Select(db.Visits, heat).GroupBy(dow, hour).All(ctx)
```

### Пробный период, который истекает

```go
// Пробные периоды, кончающиеся в ближайшие трое суток.
soon := time.Now().Add(72 * time.Hour)
db.Trials.Query().Where(Trials.EndsAt.Between(time.Now(), soon))

// Продление — в SQL, без предварительного чтения.
db.Trials.Update().
    Set(Trials.EndsAt.SetExpr(orm.AddInterval(Trials.EndsAt, orm.Val(orm.IntervalOf(0, 14, 0))))).
    Where(Trials.ID.Eq(id)).
    Exec(ctx)
```

`IntervalOf(0, 14, 0)` — это четырнадцать **дней**, а не 336 часов. На границе
перевода часов это разные моменты, и интервал сохраняет различие, которое
`Duration` выбросил бы.
