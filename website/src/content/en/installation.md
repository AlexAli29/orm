---
title: Installation
description: The module, the CLI and the configuration file.
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
