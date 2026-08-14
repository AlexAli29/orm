# Contributing

## Getting a working checkout

```sh
docker run -d --name orm-pg -e POSTGRES_PASSWORD=orm -e POSTGRES_USER=orm \
    -e POSTGRES_DB=orm -p 55432:5432 postgres:17-alpine
export ORM_TEST_ADMIN_DSN='postgres://orm:orm@localhost:55432/orm'

go test ./...
```

Tests that need PostgreSQL skip without `ORM_TEST_ADMIN_DSN`, so the suite is
green without a server and meaningful with one. Everything worth knowing about
the layout is in the README's Development section.

## What a change should come with

- `gofmt -l .` printing nothing, and `go vet ./...` clean.
- `go test -race ./...` passing.
- A test that would have failed before the change. For a bug, the test comes
  first; for a feature, the test says what the feature promises.
- Regenerated demo output when the generator changed:
  `go run ./cmd/orm generate --config internal/gendemo/orm.yaml`, then commit
  the diff. CI fails if the committed output is not what the generator produces.

## Things this project deliberately does not do

Before proposing one of these, read the README's "What it is not" — they are
decisions rather than gaps:

- migrations or any schema mutation
- lazy loading, an identity map, dirty tracking, `Save`
- support for databases other than PostgreSQL
- inferring intent from a Go zero value

A feature that requires one of those is not a small change; open an issue and
let us talk about the design before writing it.

## Comments

Comments explain why, not what. If a comment restates the line under it, the
line should be clearer instead. The places worth commenting are the ones where
the obvious implementation is wrong, and the comment should say what goes wrong.

## Errors

Every error a caller might branch on is a sentinel or a typed error, wrapped
with `%w`. Nothing should require matching on a message, and PostgreSQL's own
errors must stay reachable with `errors.As`.

## The v1 contract gates

Four checks exist because v1 freezes a public contract. They run in CI as their
own job and they run without a database, so they fail in seconds.

### The public API manifest

`api/` holds a canonical structural snapshot of every exported symbol in every
published module. Change the public surface and the snapshot must change with it,
in the same commit:

```sh
go run ./internal/tools/apimanifest -dir . -out api/orm.txt ./...
go run ./internal/tools/apimanifest -dir ./ormotel -out api/ormotel.txt ./...
go run ./internal/tools/apimanifest -dir ./ormtest/postgres -out api/ormtest-postgres.txt ./...
git diff -- api/          # this is your API change, stated
```

Regenerating is how the change is *declared*, not how it is approved.
[docs/compatibility.md](docs/compatibility.md) says which changes need which
version bump. A reviewer who sees a line disappear from `api/orm.txt` is looking
at a breaking change.

### Diagnostic codes

Adding a code means adding a row to the register in `gen/diag`. The tests refuse
a duplicate, a code emitted but not registered, and a severity that disagrees
with the code's own name.

**A published code may be retired but never reassigned.** A project that
suppressed it would silently start suppressing something else.

### Documentation

Every exported symbol in a public package needs a comment that says something its
name does not:

```sh
go run ./internal/tools/godoclint -dir . ./...
```

It rejects `// Foo is Foo`. If a symbol is hard to describe usefully, that is
often the signal that it should not be exported.

### Supported versions

[docs/support.md](docs/support.md) lists the Go and PostgreSQL versions v1
claims. **Do not add a version to that table without adding it to the CI
matrix**, and do not remove one from CI while the table still claims it. A
supported version nothing runs against is a claim nobody should believe.

## Running the matrix locally

```sh
# One container per supported PostgreSQL major.
for v in 14 15 16 17 18; do
  docker run -d --name orm-pg$v -e POSTGRES_USER=orm -e POSTGRES_PASSWORD=orm \
      -e POSTGRES_DB=orm -p 555$v:5432 postgres:$v-alpine
done

for v in 14 15 16 17 18; do
  ORM_TEST_ADMIN_DSN="postgres://orm:orm@localhost:555$v/orm" go test -count=1 ./...
done
```

The spatial tests need PostGIS and skip without it; see
[docs/support.md](docs/support.md) for the combinations that are claimed.

To check the declared Go floor rather than the toolchain you happen to have:

```sh
GOTOOLCHAIN=go1.24.0 go build ./... && GOTOOLCHAIN=go1.24.0 go vet ./...
```

**Run `go mod tidy` on the floor too**, for the reason in
[docs/support.md](docs/support.md#running-go-mod-tidy): a newer toolchain
rewrites the `go` directive upward because of a test dependency nothing
imports.

```sh
GOTOOLCHAIN=go1.24.0 go mod tidy
```

## What a pull request is expected to carry

- `gofmt -l .` prints nothing;
- `go build ./...`, `go vet ./...` and `go test ./...` pass, in every module you
  touched;
- the API manifest is regenerated if the public surface moved;
- a changelog entry for anything a user would notice;
- a test that fails without the change. For a bug fix, write the test first and
  watch it fail — a regression test that never failed is a regression test that
  proves nothing.
