# Трейсинг и здоровье

> Один трейсер, подключённый один раз, и health-проверка, знающая про миграции.

Source: https://ormgo.vercel.app/ru/docs/observability/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Контракт

Библиотека определяет интерфейс и два типа событий и не импортирует ни одной телеметрической библиотеки. ORM, который импортировал бы такую, заставил бы каждый использующий его проект зависеть от её версии, её транзитивного дерева и её мнений.

```go
type Tracer interface {
    Start(ctx context.Context, e observe.StartEvent) context.Context
    End(ctx context.Context, e observe.EndEvent)
}
```

## Правило, которое стоит назвать первым

**Стартовое событие никогда не несёт значения параметров.** Не по соглашению и не за опцией, которую можно выключить, — такого поля просто нет.

Трейсинг ORM видит каждый запрос программы. Трейсер, получающий значения, положил бы каждый пароль, токен и адрес, который обрабатывает программа, туда, куда он пишет. SQL при этом есть — с плейсхолдерами, потому что `WHERE email = $1` полезен и ничего не говорит о том, чей это адрес.

Исключение — SQL, который написали вы: библиотека не может вычистить литерал из сырого запроса, не разобрав SQL, а написать разборщик SQL значило бы построить ровно то, ради отсутствия чего она существует. `StartEvent.Raw` показывает, какие запросы такие.

## Подключение

```go
db := domain.New(orm.Traced(pool, tracer))
```

Один вызов, на старте, на исполнителе. Ничто ниже — ни сервис, ни хранилище, ни сгенерированный код — не упоминает телеметрию, а транзакция от этого исполнителя её наследует.

## Два назначения

Исполнитель несёт **один** трейсер. Двойная обёртка даёт исполнитель, у которого трейсер внешний, а внутренний не вызывается никогда — молча, без ошибки:

```go
ex := orm.Traced(orm.Traced(pool, logging), tracing) // НЕВЕРНО
```

Чтобы попасть в два места, нужен один трейсер, раздающий в оба:

```go
type Multi []observe.Tracer

func (m Multi) Start(ctx context.Context, e observe.StartEvent) context.Context {
    for _, t := range m {
        if t != nil {
            ctx = t.Start(ctx, e) // контекст протягивается: второй видит то, что добавил первый
        }
    }
    return ctx
}

func (m Multi) End(ctx context.Context, e observe.EndEvent) {
    for i := len(m) - 1; i >= 0; i-- { // в обратном порядке: вложенность закрывается изнутри
        if m[i] != nil {
            m[i].End(ctx, e)
        }
    }
}
```

## slog

```go
import "github.com/AlexAli29/orm/ormslog"

tracer := ormslog.New(log,
    ormslog.WithSQL(true),
    ormslog.WithSlowThreshold(200*time.Millisecond),
    ormslog.WithRawSQL(false), // тумблер, который пустил бы литералы в лог
)
```

## OpenTelemetry

Отдельный модуль, поэтому проект, который им не пользуется, его не компилирует:

```go
import "github.com/AlexAli29/orm/ormotel"

tracer := ormotel.New(otelTracer,
    ormotel.WithSQL(true),
    ormotel.WithRawSQL(false),
    ormotel.WithErrorMessages(false),
)
```

`WithErrorMessages(false)` — умолчание, которое стоит оставить: сообщение PostgreSQL может процитировать значение из строки, нарушившей ограничение, а спан уходит туда, где аудитория другая, чем у лога приложения.

## Health

```go
import "github.com/AlexAli29/orm/ormhealth"

// Дешёвый ответ про живость: дотягивается ли пул до PostgreSQL.
report := ormhealth.Quick(ctx, pool)

// Ответ про готовность: он же спрашивает, та ли это схема, которую описывают
// декларации, и все ли миграции применены.
report := ormhealth.Deep(ctx, pool,
    ormhealth.WithMigrationState(migrationsDir),
    ormhealth.WithSchemaCheck("orm.yaml"),
)
```

`WithMigrationState` ловит наполовину случившийся деплой: пул поднят, запросы работают, а схема на версию отстала. Liveness скажет «нормально»; эта проверка — нет.

## Разобранные примеры

### Подключение один раз, на старте

```go
func main() {
    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()

    tracer := ormslog.New(logger,
        ormslog.WithSQL(true),
        ormslog.WithSlowThreshold(200*time.Millisecond),
        ormslog.WithRawSQL(false))

    db := domain.New(orm.Traced(pool, tracer))
    // ниже этой строки телеметрия не упоминается
}
```

### Эндпоинт готовности, знающий про миграции

```go
http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
    report := ormhealth.Deep(r.Context(), pool,
        ormhealth.WithMigrationState("migrations"),
        ormhealth.WithSchemaCheck("orm.yaml"))
    if report.Status != ormhealth.StatusUp {
        w.WriteHeader(http.StatusServiceUnavailable)
    }
    writeJSON(w, report)
})

http.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
    if ormhealth.Quick(r.Context(), pool).Status != ormhealth.StatusUp {
        w.WriteHeader(http.StatusServiceUnavailable)
    }
})
```

Liveness спрашивает, надо ли перезапустить процесс. Readiness — надо ли слать ему
трафик; поду, у которого схема на версию отстала, слать не надо, и ради этого
случая существует `WithMigrationState`.

### И лог, и спан

```go
type Multi []observe.Tracer

func (m Multi) Start(ctx context.Context, e observe.StartEvent) context.Context {
    for _, t := range m {
        ctx = t.Start(ctx, e)
    }
    return ctx
}

func (m Multi) End(ctx context.Context, e observe.EndEvent) {
    for i := len(m) - 1; i >= 0; i-- {
        m[i].End(ctx, e)
    }
}

db := domain.New(orm.Traced(pool, Multi{slogTracer, otelTracer}))
```

Двойная обёртка `Traced` этого не даёт: внутренний трейсер не вызывается никогда,
и об ошибке никто не сообщает.
