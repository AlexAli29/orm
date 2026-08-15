# Архитектуры проектов

> Четыре рабочие компоновки и для чего на самом деле нужна каждая.

Source: https://ormgo.vercel.app/ru/docs/cookbook/architecture/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
Репозиторий содержит все четыре как компилирующиеся модули с тестами. Ниже — форма и рассуждение; код лежит в `examples/`.

## 1. Плоская — небольшой сервис

```text
cmd/api/main.go
internal/domain/entities.go     ← это пишете вы
internal/domain/orm_*.gen.go    ← генерируется рядом
internal/http/handlers.go
orm.yaml
migrations/
```

Обработчики принимают `*domain.DB` напрямую. Слоя репозиториев нет, потому что на таком размере интерфейс репозитория с одной реализацией — это файл, который сопровождают просто так.

Пора менять, когда в обработчиках заводятся бизнес-правила или когда их хочется тестировать без базы.

## 2. Гексагональная — порты и адаптеры

```text
internal/core/            ← сущности, сервисы, интерфейсы портов. Без SQL и HTTP.
internal/adapters/postgres/
internal/adapters/http/
cmd/api/
```

Ядро объявляет, что ему нужно:

```go
package core

type UserStore interface {
    ByID(ctx context.Context, id int64) (User, error)
    Save(ctx context.Context, u User) error
}
```

Адаптер реализует это через ORM. Ядро не импортирует ни `orm`, ни `net/http`, поэтому его тестам не нужно ни то, ни другое.

**Правило, на котором всё держится:** интерфейсы объявляются там, где их *используют*, а не там, где реализуют. `UserStore`, объявленный в пакете адаптера, — это `UserStore`, от которого ядро не сможет зависеть, не зависимя от адаптера.

Берите, когда у домена есть реальные правила или когда транспортов больше одного.

## 3. Модульный монолит — ограниченные контексты

```text
internal/billing/     domain/ · store/ · service.go · port.go
internal/catalog/     domain/ · store/ · service.go · port.go
internal/identity/    domain/ · store/ · service.go · port.go
cmd/api/main.go       ← связывает их вместе
```

Каждый контекст владеет своей схемой — своими сущностями, своей записью в `packages`, иногда своей схемой PostgreSQL. Контексты общаются через `port.go` и никогда не импортируют чужой `store/`.

```yaml
packages:
  - path: ./internal/billing/domain
    output: same
  - path: ./internal/catalog/domain
    output: same
```

Два контекста, владеющие таблицами с одинаковым именем, — это нормально, и кросс-схемные тесты существуют, чтобы это доказать: `billing.users` и `identity.users` дают разные дескрипторы, разное состояние миграций и разные результаты.

Это компоновка, которая переживает последующее разделение на сервисы, потому что швы уже прорезаны.

## 4. Production — полный стек

Модуль `examples/production` — эталон всего, что нужно настоящему развёртыванию:

- **Четыре HTTP-транспорта** — net/http, chi, gin и fiber — над одним сервисным слоем, чтобы доказать, что сервисный слой ни об одном из них не знает.
- **Наблюдаемость**, подключённая один раз на старте: `orm.Traced(pool, tracer)`, и ничто ниже не упоминает телеметрию.
- **Health-проверки** через `ormhealth`, включая то, применены ли миграции.
- **Мягкое завершение** в правильном порядке: перестать принимать, дорасследовать начатое, закрыть пул.

```go
func main() {
    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    // ...
    db := domain.New(orm.Traced(pool, observability.New(log, obsCfg)))
    svc := service.New(db)
    srv := server.New(httpapi.Routes(svc))
    // ...
}
```

Один вызов, на старте, на исполнителе. Транзакция, начатая от него, наследует трейсинг.

## Как выбрать

| | Плоская | Гексагональная | Модульный монолит | Production |
| --- | --- | --- | --- | --- |
| Правила домена | мало | много | много | много |
| Транспорты | один | один и больше | один и больше | несколько |
| Размер команды | 1–3 | 2–6 | 5+ | любой |
| Разделение потом | больно | возможно | заложено | заложено |

## Приёмы, общие для всех четырёх

Схема раскладки меняется, а это — нет.

### Сборка, один раз, при старте

```go
func main() {
    ctx := context.Background()

    cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
    if err != nil {
        log.Fatal(err)
    }
    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    if err != nil {
        log.Fatal(err)
    }
    defer pool.Close()

    db := domain.New(pool)
    svc := service.New(db)

    log.Fatal(http.ListenAndServe(":8080", routes(svc)))
}
```

Всё, что ниже `main`, получает нужное ему. Ничто не тянется к пакетной
переменной — поэтому любую часть можно протестировать на другой базе без
build-тегов.

### Сервис принимает хэндл, а не пул

```go
type Service struct {
    db *domain.DB
}

func New(db *domain.DB) *Service { return &Service{db: db} }
```

Сервис, держащий `*pgxpool.Pool`, вынужден был бы сам собирать хэндл, и тогда в
него нельзя было бы передать транзакцию — а это следующий приём.

### Транзакция поверх нескольких хранилищ

```go
func (s *Service) Checkout(ctx context.Context, cart Cart) error {
    return s.db.Tx(ctx, func(tx *domain.DB) error {
        order, err := tx.Orders.Insert(ctx, Order{UserID: cart.UserID})
        if err != nil {
            return err
        }
        for _, line := range cart.Lines {
            if _, err := tx.Items.Insert(ctx, Item{OrderID: order.ID, SKU: line.SKU}); err != nil {
                return err
            }
        }
        return nil
    })
}
```

`tx` — не тот же хэндл, что `s.db`, поэтому вызов, случайно ушедший через
внешний, виден на ревью, а не тихо оказывается вне транзакции.

### Функция, работающая и внутри транзакции, и вне её

Принимайте хэндл параметром и позвольте решать вызывающему:

```go
func reserve(ctx context.Context, db *domain.DB, sku string, n int32) error {
    _, err := db.Inventory.Update().
        Set(Inventory.OnHand.SetExpr(Inventory.OnHand.Sub(n))).
        Where(Inventory.SKU.Eq(sku)).
        Where(Inventory.OnHand.Gte(n)).
        Exec(ctx)
    return err
}
```

Вызванная с `s.db` она сама себе запрос; вызванная с `tx` внутри `Tx` — часть
транзакции. В самой функции ничего не меняется.

### Исполнитель, обёрнутый один раз

```go
db := domain.New(orm.Traced(pool, ormslog.New(logger)))
```

`orm.Traced` оборачивает исполнителя, поэтому каждый запрос через этот хэндл
трассируется, а всё ниже этой строки о телеметрии не знает. Транзакция,
начатая от него, наследует обёртку.

### Порты называют то, что нужно ядру

```go
package core

type UserStore interface {
    ByID(ctx context.Context, id int64) (User, error)
    Save(ctx context.Context, u User) error
}
```

### И адаптер, который их реализует

```go
package postgres

type UserStore struct{ db *domain.DB }

func (s UserStore) ByID(ctx context.Context, id int64) (core.User, error) {
    row, err := s.db.Users.Query().Where(Users.ID.Eq(id)).One(ctx)
    if err != nil {
        return core.User{}, err
    }
    return core.User{ID: row.ID, Email: row.Email}, nil
}
```

Перевод между строкой таблицы и доменным типом — вся работа адаптера. Пропустить
его — значит сделать типом ядра то, чем случайно оказалась таблица, и тогда порт
перестаёт быть границей.

### Отображение ошибок базы на границе

```go
func (s UserStore) ByID(ctx context.Context, id int64) (core.User, error) {
    row, err := s.db.Users.Query().Where(Users.ID.Eq(id)).One(ctx)
    switch {
    case errors.Is(err, orm.ErrNotFound):
        return core.User{}, core.ErrNotFound
    case err != nil:
        return core.User{}, fmt.Errorf("loading user %d: %w", id, err)
    }
    return core.User{ID: row.ID, Email: row.Email}, nil
}
```

Ядру не следует импортировать `orm`, чтобы узнать, что строки не нашлось. Один
перевод здесь это обеспечивает.

### Постраничность, не протекающая курсором в домен

```go
type Page[T any] struct {
    Items  []T
    Cursor string
}
```

### Контекст до самого низа

```go
func (s *Service) List(ctx context.Context, f Filter) ([]core.User, error) {
    return s.store.Search(ctx, f)
}
```

Каждый вызов библиотеки принимает контекст и ни один его не сохраняет. Отменённый
запрос останавливает начатый им SQL — но лишь если контекст протянут, а не
заменён где-то посередине на `context.Background()`.

### У фонового обработчика свой хэндл

```go
func worker(ctx context.Context, db *domain.DB) {
    tick := time.NewTicker(time.Minute)
    defer tick.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-tick.C:
            if err := sweep(ctx, db); err != nil {
                log.Error("sweep", "err", err)
            }
        }
    }
}
```

Делить пул правильно, делить транзакцию — нет. Обработчик, взявший `tx`, держал
бы её открытой между тиками.

### Проверки здоровья отвечают на разные вопросы

```go
mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
    if rep := ormhealth.Quick(r.Context(), pool); !rep.OK() {
        http.Error(w, rep.String(), http.StatusServiceUnavailable)
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

```go
mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
    rep := ormhealth.Deep(r.Context(), pool, ormhealth.WithSchemaCheck("public"))
    if !rep.OK() {
        http.Error(w, rep.String(), http.StatusServiceUnavailable)
        return
    }
    w.WriteHeader(http.StatusOK)
})
```

`Quick` спрашивает, отвечает ли база. `Deep` — та ли это база, которую ожидает
эта сборка, включая применённость миграций. Направить liveness-пробу на глубокую
значит перезапускать здоровый процесс из-за неприменённой миграции, то есть ровно
обратное задуманному.

### Завершение в том порядке, который важен

```go
srv := &http.Server{Addr: ":8080", Handler: routes(svc)}

go func() {
    <-ctx.Done()
    shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
    defer cancel()
    _ = srv.Shutdown(shutdown)   // stop accepting, let in-flight requests finish
    pool.Close()                 // only then close the pool
}()
```

Закрыть пул первым — значит превратить каждый запрос в полёте в ошибку, а это
хуже, чем двадцать секунд ожидания.

### Мультиарендность по схемам

```yaml
packages:
  - path: ./internal/tenanta/domain
    schema: tenant_a
    output: same
  - path: ./internal/tenantb/domain
    schema: tenant_b
    output: same
```

Два пакета, два набора дескрипторов, один бинарник. Запрос, собранный от одного,
нельзя выполнить против другого, потому что дескрипторы несут схему.

### Реплика для отчётов

```go
reports := domain.New(replicaPool)

rows, err := orm.Select(reports.Orders, monthly).GroupBy(Orders.Status).All(ctx)
```

Второй хэндл поверх второго пула. Больше ничего не меняется, а попытка записи
через него упадёт на сервере, а не уйдёт молча не туда.

### Тестирование ядра без базы

```go
type fakeUsers struct{ byID map[int64]core.User }

func (f fakeUsers) ByID(_ context.Context, id int64) (core.User, error) {
    u, ok := f.byID[id]
    if !ok {
        return core.User{}, core.ErrNotFound
    }
    return u, nil
}
```

Ровно это и покупает порт. Тесты ядра — это карта и никакого сервера.

### Тестирование адаптера против настоящей базы

```go
func TestUserStore(t *testing.T) {
    ex := ormtest.Tx(t, pool)
    store := postgres.UserStore{DB: domain.New(ex)}
    // ...
}
```

Адаптер — тот слой, чья работа целиком состоит в разговоре с PostgreSQL, поэтому
его тесты разговаривают с PostgreSQL. Подделка базы здесь тестировала бы подделку.

### Конфигурация, прочитанная один раз

```go
type Config struct {
    DatabaseURL string
    MaxConns    int32
    Addr        string
}
```

Структура, заполняемая при старте и передаваемая вниз, вместо `os.Getenv`,
рассыпанного по всем пакетам, которым случилось понадобиться значение.

## Правила, верные во всех четырёх

**Сгенерированный код лежит рядом со своими сущностями.** `output: same` кладёт дескрипторы в тот же пакет, что и структуры, поэтому цикл импортов невозможен.

**Один исполнитель, передаваемый вниз.** Никакой глобальной базы, соединения в `init()` и транзакции в окружении. Функция может дотянуться до того, что ей передали.

**Миграции — это код-ревью.** Это закоммиченные артефакты, спланированные командой и прочитанные человеком до запуска.

**Две команды в CI не опциональны:**

```bash
orm makemigrations --check
orm check --generated
```

Первая падает, когда структура изменилась и никто этого не спланировал. Вторая — когда спланировали и забыли перегенерировать. Вдвоём они не дают трём представлениям разойтись.
