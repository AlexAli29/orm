# Migrations

> Planning, applying and proving schema change.

Source: https://ormgo.vercel.app/en/docs/migrations/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

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

## Data, and the raw escape hatch

A generated migration describes schema operations, and there is nowhere in
`create table` or `add column` to put rows. That is not the whole story though,
and these docs have been letting people believe it was.

`orm makemigrations --empty` writes one for you, so you are editing a file
rather than inventing one:

```console
$ orm makemigrations --empty --name seed_tags
wrote migrations/0002_seed_tags.json

Fill in Up with the SQL to run, and Down with the SQL that undoes it.
```

It is written whether or not the models moved, because data is exactly the case
the schema diff cannot see. The stub it leaves behind raises an exception rather
than doing nothing, so a migration created and then forgotten fails instead of
being recorded as applied.

The artifact is JSON, and the operation is `raw_sql`:

```json
{
  "op": "raw_sql",
  "args": {
    "Up": "INSERT INTO user_tags (text) VALUES ('music'), ('sports') ON CONFLICT (text) DO NOTHING",
    "Down": "DELETE FROM user_tags WHERE text IN ('music', 'sports')",
    "Atomic": true,
    "Description": "seed the starting tags"
  }
}
```

`Down` is optional, and its absence is what makes the operation irreversible —
stated rather than faked with a no-op that claims to have undone something.
`Atomic` says whether it may run inside a transaction.

This is also what makes the three-step column change possible, which is otherwise
out of reach for any tool that models only schema:

1. add the column, nullable
2. `raw_sql` to backfill it
3. set it `NOT NULL`

The engine does not parse the SQL, so `raw_sql` changes nothing in the migration
state and reports itself as destructive — the cautious assumption about something
it cannot read. If your SQL *does* change the schema, pair it with `state_only`
so the state stays true; otherwise the next plan tries to make the change again.

Seed data is the easier case and the same mechanism. Whether it belongs in a
migration or in a separate seed step is a real choice: a migration runs once per
database and is reviewed with the schema change it belongs to, while a seed file
re-run on every deploy needs `ON CONFLICT DO NOTHING` and a unique constraint for
it to key on. Reference data the schema is meaningless without belongs in the
migration. A developer's convenience fixtures do not.

## Materialized views

A materialized view holds rows computed from a body, so changing that body is not a column change — the rows are wrong afterwards. The planner refuses a definition change rather than silently rebuilding, and tells you to write an explicit migration.

Indexes on a materialized view are planned separately from the relation, which is what makes concurrent refresh eligibility a fact the generator can record. See [Views](/en/docs/views/).

## Checking in CI

```bash
orm makemigrations --check   # every declaration has a migration behind it
orm check --generated        # the committed generated code is current
```

The first fails when somebody changed a struct and forgot to plan. The second fails when somebody planned and forgot to regenerate.

## Worked examples

### Adding a column safely

Adding a nullable column is instant. Adding a `NOT NULL` one with no default
rewrites the table and blocks writes while it does — so it is three migrations,
not one:

```go
// 1. Add it nullable.
Currency *string

// 2. Backfill, outside a migration, in batches.
// 3. Then make it NOT NULL.
Currency string `orm:"default:'EUR'"`
```

`orm makemigrations --dry-run --sql` shows which of these PostgreSQL will do
cheaply, before you find out on production.

### Renaming without downtime

The planner sees a dropped column and an added one, not a rename, and dropping a
column deletes its data. Add, dual-write, backfill, drop — four deploys:

```bash
orm makemigrations --dry-run --sql   # read it before you believe it
```

### Checking a deploy is complete

```bash
orm showmigrations         # what is applied, what is pending
orm migrate --plan         # exactly what the next run would do
orm check --generated      # committed code matches the schema
```

### The CI gate

```yaml
- run: orm makemigrations --check   # a declaration nobody planned
- run: orm check --generated        # a plan nobody regenerated for
```

The first fails when somebody changed a struct and forgot. The second fails when
somebody planned and forgot to regenerate. Between them the three
representations cannot drift apart.
