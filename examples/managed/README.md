# journal — the managed schema example

The [blog example](../blog) is database-first: PostgreSQL owns the schema, and
the ORM proves your structs agree with it. This one is the other mode. The Go
declarations own the schema, `orm makemigrations` writes the migration that
carries them to PostgreSQL, and the runtime is the same runtime.

Nothing about reading or writing rows differs between the two. What differs is
who decides what the schema is.

## Run it

```bash
docker compose up -d
export JOURNAL_DSN='postgres://journal:journal@localhost:55434/journal?sslmode=disable'

go run ../../cmd/orm migrate --config orm.yaml
go run .
```

There is no `schema.sql` to apply by hand. The migration in `migrations/` is the
schema, it is committed, and it is what every machine runs.

```
Ada <ada@example.com>
    2024-04-01  On engines
    2024-03-01  On notation
```

## What is declared, and what is not

[`internal/domain/entities.go`](internal/domain/entities.go) declares only what
a Go type cannot say:

```go
//orm:table articles
//orm:index articles_feed_idx (AuthorID, PublishedAt desc) include (Title) where "status = 'published'"
//orm:check articles_title_not_blank "title <> ''"
type Article struct {
	ID          int64 `orm:"pk,identity"`
	AuthorID    int64
	Title       string
	Status      Status `orm:"default:'draft'"`
	PublishedAt *time.Time

	Author orm.One[Author] `orm:"side:local"`
}
```

The column name, the PostgreSQL type and the nullability all come from the Go
field, as they do in database-first mode — `PublishedAt *time.Time` is a
`timestamptz` that accepts NULL because the pointer says so, not because a tag
repeats it.

A default is SQL and never a Go value: `default:'draft'` is the expression
PostgreSQL stores, and a Go zero value is not a default.

## Changing the schema

```bash
# edit a declaration, then:
go run ../../cmd/orm makemigrations --config orm.yaml
go run ../../cmd/orm migrate --plan --config orm.yaml     # read it first
go run ../../cmd/orm sqlmigrate 0002_articles --config orm.yaml
go run ../../cmd/orm migrate --config orm.yaml
go run ../../cmd/orm check --config orm.yaml
go run ../../cmd/orm generate --config orm.yaml
```

`makemigrations` never looks at the database to decide what changed: it compares
the declarations with what the committed migrations already describe. Two people
on one branch therefore get the same migration whatever database each has
running.

If you rename a field, it will ask:

```
Did you rename articles.body to content?
    both are text, NOT NULL, with the same default. [y/N]
```

Answering yes keeps the data. Answering no drops the column and adds another,
which the summary then says in as many words.

## In CI

```bash
orm makemigrations --check   # a declaration with no migration fails the build
orm check                    # models, migrations, database, mapping, generated
go test ./...
```

## Migrating from your own program

The command-line tool is not the only way to run these. `main_test.go` applies
the same committed migrations through the Go API, which is what a service that
migrates on deploy would do:

```go
set, err := migrate.NewStore("migrations").Load()
_, err = migrate.New(conn, set).Migrate(ctx, "")
```

One connection, because the advisory lock that keeps two deploys from migrating
at once lives on a connection.

## The files

| | |
|---|---|
| `internal/domain/entities.go` | the schema declarations, hand-written |
| `internal/domain/orm_*.gen.go` | generated, committed |
| `migrations/0001_initial.json` | the migration, generated and committed |
| `orm.yaml` | `mode: managed` |
| `orm.lock` | the mapping fingerprint the generated code came from |
