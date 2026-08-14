---
title: Transactions
description: One callback, one transaction, no hidden state.
---

## The shape

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

The callback receives a `DB` bound to the transaction. The one it was called on is untouched, so there is no ambient "current transaction" and no way to accidentally write outside it.

Returning nil commits. Returning an error rolls back. A panic rolls back and re-panics. **Nothing is retried** — a retry policy depends on what the work was, and the library does not know.

## Options

```go
err := db.TxOptions(ctx, pgx.TxOptions{
    IsoLevel:   pgx.Serializable,
    AccessMode: pgx.ReadWrite,
}, func(tx *domain.DB) error {
    return nil
})
```

## Serialization failures

At `Serializable`, PostgreSQL may abort a transaction that would break serializability. That is not an error to log — it is an instruction to try again:

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

The retry loop is yours because the backoff, the cap and whether retrying is safe at all are yours.

## Without generated code

`RunTx` takes any executor:

```go
err := orm.RunTx(ctx, pool, func(ex orm.Executor) error {
    repo := orm.NewRepo(ex, &meta)
    return nil
})
```

## What a transaction is not

It is not a unit of work that tracks what you changed. There is no dirty tracking and no flush: a statement runs when you call it. That makes the statement order in the log the statement order in your code, which is the property you want at 3am.
