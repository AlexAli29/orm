# Транзакции

> Один колбэк, одна транзакция, никакого скрытого состояния.

Source: https://ormgo.vercel.app/ru/docs/transactions/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## Форма

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    user, err := tx.Users.Insert(ctx, User{Email: email})
    if err != nil {
        return err
    }
    _, err = tx.Orders.Insert(ctx, Order{UserID: user.ID})
    return err
})
```

Колбэк получает `DB`, привязанный к транзакции. Тот, на котором его вызвали, не затрагивается — поэтому нет «текущей транзакции» в окружении и нет способа случайно записать мимо неё.

Возврат nil фиксирует транзакцию. Возврат ошибки откатывает её. Паника откатывает и паникует дальше. **Ничего не повторяется** — политика повторов зависит от того, чем была работа, а библиотека этого не знает.

## Опции

```go
err := db.TxOptions(ctx, pgx.TxOptions{
    IsoLevel:   pgx.Serializable,
    AccessMode: pgx.ReadWrite,
}, func(tx *domain.DB) error {
    return nil
})
```

## Ошибки сериализации

На уровне `Serializable` PostgreSQL может прервать транзакцию, которая нарушила бы сериализуемость. Это не ошибка для лога — это указание попробовать снова:

```go
for attempt := range 3 {
    err := db.TxOptions(ctx, opts, work)
    var pge *pgconn.PgError
    if errors.As(err, &pge) && pge.Code == "40001" {
        continue // serialization_failure
    }
    return err
}
```

Цикл повторов ваш, потому что задержка, предел и сама безопасность повтора — тоже ваши.

## Без сгенерированного кода

`RunTx` принимает любой исполнитель:

```go
err := orm.RunTx(ctx, pool, func(ex orm.Executor) error {
    repo := orm.NewRepo(ex, &meta)
    return nil
})
```

## Чем транзакция не является

Она не единица работы, отслеживающая ваши изменения. Нет ни грязного отслеживания, ни flush: оператор выполняется тогда, когда вы его вызвали. Благодаря этому порядок операторов в логе — это порядок операторов в вашем коде, а это то самое свойство, которое нужно в три часа ночи.

## Разобранные примеры

### Перевод между счетами

Обе части или ни одной. Классика — и причина, по которой у транзакции такая
форма с колбэком:

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    if _, err := tx.Accounts.Update(ctx).
        SetExpr(Accounts.Balance, Accounts.Balance.Sub(amount)).
        Where(Accounts.ID.Eq(from)).
        Where(Accounts.Balance.Gte(amount)).   // не даёт уйти в минус
        Exec(ctx); err != nil {
        return err
    }
    _, err := tx.Accounts.Update(ctx).
        SetExpr(Accounts.Balance, Accounts.Balance.Add(amount)).
        Where(Accounts.ID.Eq(to)).
        Exec(ctx)
    return err
})
```

Проверка баланса стоит в `WHERE`, а не в Go, поэтому овердрафт — это обновление,
не нашедшее строк, а не гонка.

### Заказ и его позиции

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    order, err := tx.Orders.Insert(ctx, Order{CustomerID: id})
    if err != nil {
        return err
    }
    for i := range lines {
        lines[i].OrderID = order.ID   // ключ, который вернула вставка
    }
    _, err = tx.OrderLines.InsertMany(ctx, lines)
    return err
})
```

### Воркер, забирающий пачку

`SKIP LOCKED` — то, что позволяет двум воркерам выполнять один и тот же запрос и
не сталкиваться:

```go
err := db.Tx(ctx, func(tx *domain.DB) error {
    jobs, err := tx.Jobs.Query().
        Where(Jobs.State.Eq("queued")).
        OrderBy(Jobs.Priority.Desc(), Jobs.QueuedAt.Asc()).
        Limit(20).
        Lock(orm.ForUpdateStrong, orm.SkipLocked()).
        All(ctx)
    if err != nil {
        return err
    }
    for _, j := range jobs {
        if _, err := tx.Jobs.Update(ctx).
            Set(Jobs.State, "running").
            Where(Jobs.ID.Eq(j.ID)).
            Exec(ctx); err != nil {
            return err
        }
    }
    return nil
})
```
