# Releasing

Every step here is a command, because a checklist you cannot run is a checklist
nobody runs.

## Before tagging

```sh
# The database the integration tests need.
docker run -d --name orm-pg -e POSTGRES_PASSWORD=orm -e POSTGRES_USER=orm \
    -e POSTGRES_DB=orm -p 55432:5432 postgres:17-alpine
export ORM_TEST_ADMIN_DSN='postgres://orm:orm@localhost:55432/orm'

# The spatial tests need PostGIS, and skip without it. Run the suite once
# against each supported combination: a function that exists on 3.5 and not on
# 3.4 would otherwise ship as supported.
#
#   docker run -d --name orm-gis17 ... postgis/postgis:17-3.5   # port 55517
#   docker run -d --name orm-gis16 ... postgis/postgis:16-3.4   # port 55516
#   docker run -d --name orm-gis14 ... postgis/postgis:14-3.4   # port 55514

gofmt -l .            # must print nothing
go build ./...
go vet ./...
go test ./...
go test -race ./...
```

Then the v1 contract gates, which need no database:

```sh
# The public API has not moved without the snapshot moving with it.
go run ./internal/tools/apimanifest -dir . -out api/orm.txt ./...
go run ./internal/tools/apimanifest -dir ./ormotel -out api/ormotel.txt ./...
go run ./internal/tools/apimanifest -dir ./ormtest/postgres -out api/ormtest-postgres.txt ./...
git diff --exit-code -- api/

# The manifest tool still notices breaking changes.
go test -count=1 ./internal/tools/apimanifest/

# No duplicate, unregistered or reassigned diagnostic code.
go test -count=1 ./gen/diag/

# Every exported symbol in a public package is documented.
go run ./internal/tools/godoclint -dir . ./...
```

And the support matrix, which is what docs/support.md claims:

```sh
# Every PostgreSQL major upstream still supports.
for v in 14 15 16 17 18; do
  ORM_TEST_ADMIN_DSN="postgres://orm:orm@localhost:555$v/orm" go test -count=1 ./... || break
done

# The declared Go floor, with the toolchain pinned so it cannot self-upgrade.
GOTOOLCHAIN=go1.24.0 go build ./...
GOTOOLCHAIN=go1.24.0 go vet ./...
GOTOOLCHAIN=go1.24.0 go test -run '^$' ./...
```

Then the checks that are specific to a generator:

```sh
# Generated output is byte-identical across runs, and the lock with it.
go run ./cmd/orm generate --config internal/gendemo/orm.yaml
git diff --exit-code internal/gendemo

# The committed generated code matches the schema it was generated from.
go run ./cmd/orm check --config internal/gendemo/orm.yaml --generated

# The example still builds and still works.
go test ./examples/...
```

`git diff --exit-code` after `orm generate` is the check CI runs. It is
deliberately separate from the fingerprint: the fingerprint says the *mapping*
changed, and the diff says the *output* did. Both should be quiet.

## Supported versions

Claim only what CI runs. Today that is:

- Go 1.24 (the `go.mod` floor), and the current stable Go
- PostgreSQL 14, 15, 16, 17 and 18
- PostGIS 3.5 on PostgreSQL 17, and PostGIS 3.4 on 16 and 14

`ormtest/postgres` declares a 1.25 floor of its own, because Testcontainers
does. It is the one module a consumer can decline to use, which is why the
exception is confined to it.

Adding a version to the README means adding it to `.github/workflows/ci.yml`
first.

## The tag

0. `go run ./internal/tools/releasecheck -release`. It fails while anything
   blocks publication: no LICENSE, or an optional module still standing on a
   local `replace` and a `v0.0.0` require. That require is the one nobody sees
   coming — Go ignores a `replace` in a dependency's `go.mod`, so a consumer of
   `ormotel` resolves the require instead and gets `unknown revision v0.0.0`.
   Bump each optional module to the version being tagged and drop the replace.
1. Update `CHANGELOG.md`: move the unreleased section under the new version and
   date it. Describe what changed for a user, not which files moved.
2. Confirm the changelog claims nothing that is not implemented.
3. `git tag -a v0.1.0 -m 'v0.1.0'` and push the tag.
4. `go install github.com/AlexAli29/orm/cmd/orm@v0.1.0` and run `orm version` to
   confirm the build stamp resolves. It reads Go's own build info, so a version
   only appears once the tag exists.

## After a release

Verify the module is fetchable as published, from a clean module cache:

```sh
GOFLAGS=-mod=mod go run github.com/AlexAli29/orm/cmd/orm@latest version
```
