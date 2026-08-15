# Installation

> The module, the CLI and the configuration file.

Source: https://ormgo.vercel.app/en/docs/installation/
Symbols: https://ormgo.vercel.app/api/orm.txt — the generated list of every exported name.

---
## The module

```bash
go get github.com/AlexAli29/orm
```

The runtime depends only on the standard library and `github.com/jackc/pgx/v5`. That boundary is enforced by a test in the suite, so it cannot rot.

## The CLI

The generator and the migration planner are one binary:

```bash
go install github.com/AlexAli29/orm/cmd/orm@latest
```

Or run it without installing, which is what most projects put in their `Makefile`:

```bash
go run github.com/AlexAli29/orm/cmd/orm generate
```

Pin it in `tools.go` if you want the version tracked with everything else.

## Optional adapters

Each is a module of its own, so a project that does not use one never compiles it:

```bash
go get github.com/AlexAli29/orm/ormotel          # OpenTelemetry tracing
go get github.com/AlexAli29/orm/ormtest/postgres # Testcontainers helpers
```

`ormslog`, `ormhealth` and `ormtest` live in the core module and cost nothing until imported.

## orm.yaml

The configuration file sits at the project root:

```yaml
version: 1

schema:
  # Managed: the declarations own the schema and migrations apply it.
  # Omit `mode` for database-first, where the database is authoritative.
  mode: managed
  dsn: ${DATABASE_URL}
  search_path:
    - public

migrations:
  dir: migrations

packages:
  - path: ./internal/domain
    output: same

# Types Go has no equivalent for reach it through configuration. The ORM
# refuses to choose a uuid package for you, because the popular ones are not
# interchangeable.
types:
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid
```

`${DATABASE_URL}` is expanded from the environment, so the file carries no credentials and can be committed.

## Verifying the install

```bash
orm check
```

With an empty project this reports that it found no declarations, which is the correct answer and proves the CLI can reach the database.

## Worked examples

### Database-first, against an existing database

No `mode`, no migrations directory — the database is authoritative and you write
declarations that describe it:

```yaml
version: 1

schema:
  dsn: ${DATABASE_URL}
  search_path:
    - public
    - reporting

packages:
  - path: ./internal/domain
    output: same
```

### Managed, with several bounded contexts

Each context owns its own package, and the generator writes beside each:

```yaml
version: 1

schema:
  mode: managed
  dsn: ${DATABASE_URL}
  search_path:
    - public
    - billing
    - identity

migrations:
  dir: migrations

packages:
  - path: ./internal/billing/domain
    output: same
  - path: ./internal/identity/domain
    output: same
  - path: ./internal/catalog/domain
    output: same
```

Two contexts may own tables with the same name in different schemas; they produce
separate descriptors and separate migration state.

### Types Go does not have

```yaml
types:
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
```

These are the two the ORM refuses to choose for you, because the popular packages
are not interchangeable and a wrong `numeric` silently corrupts money.

### A Makefile that keeps everything in step

```makefile
generate:
	go run github.com/AlexAli29/orm/cmd/orm makemigrations
	go run github.com/AlexAli29/orm/cmd/orm migrate
	go run github.com/AlexAli29/orm/cmd/orm generate

check:
	go run github.com/AlexAli29/orm/cmd/orm makemigrations --check
	go run github.com/AlexAli29/orm/cmd/orm check --generated
```

## Running the CLI from a container

If you would rather not install Go on a CI runner, the CLI is published as an
image. There are two, because the commands differ in what they need:

```console
$ docker run --rm -v "$PWD":/work -e DATABASE_URL \
    ghcr.io/alexali29/orm:latest migrate --config /work/orm.yaml
Applying 0001_initial ... OK
```

`ghcr.io/alexali29/orm` is the CLI on distroless — about 16 MB, no shell, no
libc, running as a non-root user. It covers `migrate`, `sqlmigrate` and
`inspect`, which is the deploy-time set.

`ghcr.io/alexali29/orm:latest-toolchain` adds Go. `check`, `generate` and
`makemigrations` read your entity source through `go list`, so they need a
toolchain and a module that resolves — and mounting your module is not enough if
its dependencies are not there:

```console
$ docker run --rm -v "$PWD":/work -e DATABASE_URL \
    ghcr.io/alexali29/orm:latest-toolchain check --config /work/orm.yaml
reconciliation clean: the entities and the schema agree
```

Asking the small image for one of those three fails immediately and says why —
`go command required, not found` — rather than producing a wrong answer. That is
the reason the split exists rather than one image that sometimes works.

Both are built for `linux/amd64` and `linux/arm64`. Tags follow releases:
`1.2.3`, `1.2`, `latest`, and `edge` for the tip of main.

### In a pipeline

```yaml
migrate:
  image: ghcr.io/alexali29/orm:latest
  script:
    - orm migrate --config orm.yaml
```

The entrypoint is the binary, so the arguments are the command. Nothing needs a
shell, which is worth having in a step whose environment holds a database URL.
