# Запись данных

> Insert, update, delete и COPY — всё явно.

Source: https://ormgo.vercel.app/ru/docs/writing/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Insert

```go
user, err := db.Users.Insert(ctx, User{
    Email: "a@example.com",
    Active: true,
})
// user.ID заполнен из RETURNING
```

Много сразу:

```go
users, err := db.Users.InsertMany(ctx, []User{u1, u2, u3})
```

## Значения по умолчанию

Нулевое значение Go — это значение. Запрос значения по умолчанию отдельный и явный:

```go
db.Users.Insert(ctx, User{}, orm.Default(Users.Active, Users.CreatedAt))
```

Названные колонки полностью исключаются из `INSERT`, поэтому применяется `DEFAULT` колонки — или её последовательность, или NULL для nullable-колонки без умолчания.

## Update

```go
n, err := db.Users.Update(ctx).
    Set(Users.Active, false).
    Where(Users.CreatedAt.Lt(cutoff)).
    Exec(ctx)
```

Обновление без `WHERE` отвергается:

```go
_, err := db.Users.Update(ctx).Set(Users.Active, false).Exec(ctx)
// errors.Is(err, orm.ErrMissingWhere)
```

Если только вы не скажете, что имелись в виду все строки:

```go
db.Users.Update(ctx).Set(Users.Active, false).All().Exec(ctx)
```

Присвоение выражения вместо значения:

```go
db.Orders.Update(ctx).
    SetExpr(Orders.Total, Orders.Net.AddCol(Orders.Tax)).
    Where(Orders.ID.Eq(id))
```

## Delete

```go
n, err := db.Users.Delete(ctx).Where(Users.ID.Eq(id)).Exec(ctx)
```

То же правило `ErrMissingWhere` и по той же причине.

## Upsert

```go
db.Users.Insert(ctx, user,
    orm.OnConflict(Users.Email).DoUpdateSet(
        Users.Active.Set(true),
    ),
)

db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())
```

Цель конфликта — список колонок, по которому PostgreSQL его определяет; `DO UPDATE` видит конфликтную строку и `EXCLUDED`.

## Upsert подробно

`OnConflict` называет колонки, по которым PostgreSQL определяет конфликт, —
обычно это колонки уникального ограничения.

```go
// Ничего не делать, если запись уже есть.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoNothing())

// Взять значения пришедшей строки для названных колонок.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.Seen))

// Или задать их самому.
db.Users.Insert(ctx, user, orm.OnConflict(Users.Email).DoUpdateSet(
    Users.Seen.Set(time.Now()),
    Users.Hits.SetExpr(Users.Hits.Add(1)),
))
```

`DoUpdate` — частый случай, читается как «эти колонки берут новые значения».
`DoUpdateSet` — когда новое значение вычисляется: инкремент счётчика, выбор
большего из двух чисел, добавление в массив.

Частичному индексу нужен тот же предикат в конструкции конфликта:

```go
orm.OnConflict(Users.Email).Where(Users.Active.Eq(true)).DoNothing()
```

## RETURNING

PostgreSQL умеет возвращать строки, которых коснулась запись. Библиотека
использует это тремя способами, и разницу стоит знать: два из них происходят
сами, а третий — нет.

### Вставка возвращает всегда

```go
user, err := db.Users.Insert(ctx, User{Email: "a@example.com"})
// user.ID заполнен; как и CreatedAt, и всё, что заполнила база
```

Поэтому `Insert` возвращает `(E, error)`, а не одну ошибку. Ключ identity,
`DEFAULT now()`, генерируемая колонка и правки триггера — всё приходит в
возвращённом значении, поэтому назад вы получаете строку в том виде, в каком она
существует, а не ту структуру, которую отправили.

`InsertMany` делает то же для среза, по порядку:

```go
users, err := db.Users.InsertMany(ctx, []User{a, b, c})
// users[1].ID — ключ b
```

Список колонок всегда явный. `RETURNING *` определял бы порядок сканирования на
сервере, где сгенерированный сканер его не видит.

### Upsert возвращает выжившую строку

```go
user, err := db.Users.Insert(ctx, incoming,
    orm.OnConflict(Users.Email).DoUpdate(Users.Name, Users.Seen))
```

Вставила она или обновила — назад приходит строка, которая теперь лежит в
таблице. Это обычная причина брать `DoUpdate`, а не `DoNothing`: при конфликте
`DoNothing` не возвращает **ни одной** строки, поэтому вы получаете нулевую
сущность и по ней не отличите «уже была» от «только что записана».

### Обновление и удаление — только если попросить

`Exec` возвращает счётчик:

```go
n, err := db.Users.Update(ctx).
    Set(Users.Active, false).
    Where(Users.CreatedAt.Lt(cutoff)).
    Exec(ctx)
// n — сколько строк изменилось
```

Счётчик отвечает на «сколько». Когда нужно «какие», оберните строитель:

```go
updated, err := orm.UpdateReturningEntity(
    db.Users.Update(ctx).Set(Users.Active, false).Where(Users.CreatedAt.Lt(cutoff)),
).All(ctx)
// []User — каждая совпавшая строка в том виде, в каком она после обновления
```

```go
deleted, err := orm.DeleteReturningEntity(
    db.Users.Delete(ctx).Where(Users.ID.Eq(id)),
).One(ctx)
// строка в том виде, в каком она была прямо перед тем, как перестать существовать
```

Обратите внимание на время. Обновление возвращает **новые** значения, удаление —
строку, которой уже нет. И то и другое — единственный шанс: после оператора одну
уже не запросить, а другая больше не хранит старых значений.

### Вернуть форму, а не сущность

Когда нужны только две колонки изменившегося:

```go
type Changed struct {
    ID    int64
    Email string
}

var changed = orm.Project2(
    Users.ID, Users.Email,
    func(id int64, email string) Changed { return Changed{id, email} },
)

rows, err := orm.UpdateReturning(
    db.Users.Update(ctx).Set(Users.Active, false).Where(cond),
    changed,
).All(ctx)
// []Changed
```

`DeleteReturning` принимает проекцию так же.

### Терминалы

У `Returning` их три и больше никаких:

| Метод | Для чего |
| --- | --- |
| `All(ctx)` | все строки, которых коснулась запись |
| `One(ctx)` | ровно одна или `ErrNotFound`; больше одной — ошибка |
| `SQL()` | оператор и аргументы, без выполнения |

`Exec` у `Returning` нет: оператор, у которого вы запросили строки и тут же их
выбросили, — это оператор, которому с самого начала был нужен `Exec`.

### Это по-прежнему запись

`ErrMissingWhere` действует ровно так же, как и без `RETURNING`: обёртка не
делает безусловное обновление безопасным:

```go
_, err := orm.UpdateReturningEntity(
    db.Users.Update(ctx).Set(Users.Active, false),
).All(ctx)
// errors.Is(err, orm.ErrMissingWhere)
```

И это один оператор. Строки приходят из самой записи, а не из `SELECT` после
неё, — именно поэтому это те строки, которых коснулась запись, даже при
конкурентном доступе, а не те, что подходят сейчас.

## COPY

Для массовой загрузки `COPY` на порядок быстрее `INSERT`:

```go
n, err := db.Events.CopyFrom(ctx, events)
```

Потоком, чтобы строки не существовали все сразу:

```go
n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event, error) bool) {
    for scanner.Scan() {
        ev, err := parse(scanner.Text())
        if !yield(ev, err) {
            return
        }
    }
})
```

Подмножество колонок:

```go
n, err := orm.CopyColumns(ctx, db.Events, events, Events.ID, Events.Kind)
```

Упавший `COPY` падает как один оператор — не применяется ничего. Если он должен быть успешным вместе с другой работой, оберните в транзакцию.

## Разобранные примеры

### Импорт, который запускают дважды

Идемпотентен по построению: второй запуск обновляет, а не дублирует.

```go
for _, row := range parsed {
    _, err := db.Products.Insert(ctx, row,
        orm.OnConflict(Products.SKU).DoUpdate(
            Products.Name, Products.PriceCents, Products.UpdatedAt))
    if err != nil {
        return err
    }
}
```

Для большого файла — один оператор на пачку вместо одного на строку:

```go
for chunk := range slices.Chunk(parsed, 1000) {
    if _, err := db.Products.InsertMany(ctx, chunk,
        orm.OnConflict(Products.SKU).DoUpdate(Products.PriceCents)); err != nil {
        return err
    }
}
```

### Бронирование, которое нельзя продать дважды

Запись и проверка — один оператор, поэтому между ними ничто не вклинится:

```go
seat, err := db.Seats.Update(ctx).
    Set(Seats.HeldBy, customerID).
    Set(Seats.HeldUntil, time.Now().Add(10*time.Minute)).
    Where(Seats.ID.Eq(seatID)).
    Where(Seats.HeldBy.IsNull()).
    Exec(ctx)
if seat == 0 {
    return ErrAlreadyHeld  // успел кто-то другой
}
```

`HeldBy.IsNull()` в `WHERE` и есть блокировка. У «прочитать, потом записать» был
бы зазор; здесь его нет.

### Задача очистки

Удалить и сохранить удалённое для журнала аудита:

```go
gone, err := orm.DeleteReturningEntity(
    db.Sessions.Delete(ctx).Where(Sessions.ExpiresAt.Lt(time.Now())),
).All(ctx)

for _, s := range gone {
    recordAudit("session.expired", s.ID, s.UserID)
}
```

### Счётчик, который не читает сначала

```go
db.PageViews.Update(ctx).
    SetExpr(PageViews.Hits, PageViews.Hits.Add(1)).
    Where(PageViews.Path.Eq(path)).
    Exec(ctx)
```

Прочитать строку, прибавить единицу в Go и записать обратно — значит терять
инкременты при конкурентном доступе. Здесь это невозможно.
