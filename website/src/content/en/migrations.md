---
title: Migrations
description: Planning, applying and proving schema change.
---

## The model

In managed mode the declarations are the desired state. `makemigrations` diffs them against the state the existing migration artifacts describe — not against the live database — and writes the difference as an artifact.

That distinction matters: planning against the live database would produce a migration that depends on the database it was planned on.

```bash
orm makemigrations              # plan and write
orm makemigrations --dry-run --sql   # show the SQL, write nothing
orm makemigrations --check      # fail if anything is unplanned
orm migrate                     # apply
orm migrate --plan              # show what would be applied
orm showmigrations              # what is applied, what is pending
```

## Artifacts are portable

A migration is JSON describing operations, not a SQL script. Two consequences:

- It replays identically on every supported PostgreSQL major.
- It contains nothing server-local: no OIDs, no database name, no server version, no deparsed definition, no absolute paths. An artifact carrying any of those converges on the machine that wrote it and nowhere else.

## Transactions

Every migration applies inside one transaction, together with its own history record. A migration that fails is rolled back and **not recorded** — so a failed run leaves the database exactly as it was, and running it again fails the same way rather than half-applying.

```text
Applying 0002_add_orders ... FAILED

orm migrate: migration 0002_add_orders failed at operation 1
(alter column public.orders.total: type text -> numeric); the transaction was
rolled back and the migration is not recorded: ERROR: column "total" cannot be
cast automatically to type numeric (SQLSTATE 42804)
```

Note what that is: the planner planned it, and **PostgreSQL** refused it. The ORM does not invent a `USING` expression to make such a migration succeed — that would be the tool deciding, on your behalf, what happens to the rows that do not convert.

## Destructive changes are gated

Dropping a column or a table is not planned silently. The gate exists because the cost of a wrong `DROP` is unbounded and the cost of an extra confirmation is one command.

## What migrations do not do

Two frozen boundaries:

- **Migrations do not create schemas.** `CREATE SCHEMA` is yours.
- **Migrations do not create domains or extensions.** Same reason.

They are prerequisites, not schema change, and pretending otherwise would make `orm migrate` a thing you run as a superuser.

## Materialized views

A materialized view holds rows computed from a body, so changing that body is not a column change — the rows are wrong afterwards. The planner refuses a definition change rather than silently rebuilding, and tells you to write an explicit migration.

Indexes on a materialized view are planned separately from the relation, which is what makes concurrent refresh eligibility a fact the generator can record. See [Views](/en/docs/views/).

## Checking in CI

```bash
orm makemigrations --check   # every declaration has a migration behind it
orm check --generated        # the committed generated code is current
```

The first fails when somebody changed a struct and forgot to plan. The second fails when somebody planned and forgot to regenerate.
