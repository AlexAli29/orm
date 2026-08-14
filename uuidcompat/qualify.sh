#!/usr/bin/env bash
#
# Run the UUID qualification against one PostgreSQL server.
#
# CI runs this and so does a developer, with the same arguments, because a
# qualification that is reproduced by a different sequence from the one that
# gates it is two things that can disagree.
#
#   ./qualify.sh 'postgres://orm:orm@localhost:5432/postgres' uuidcompat
#
# The first argument is a DSN with rights to create a database; the second is
# the database to create and qualify in. The database is dropped and remade, so
# do not point this at anything you care about.
#
# The CLI is built from the ORM module and then invoked as a binary. Running it
# with `go run ../cmd/orm` from inside this module asks this module's graph to
# supply the generator's build dependencies — golang.org/x/tools — which it does
# not have and should not: a consumer that installs the tool does not take on
# what the tool is built from.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
root="$(cd "$here/.." && pwd)"

admin="${1:?usage: qualify.sh <admin-dsn> <database>}"
dbname="${2:?usage: qualify.sh <admin-dsn> <database>}"

# The DSN the suite connects with is the admin one with the database swapped.
dsn="$(printf '%s' "$admin" | sed -E "s#(://[^/]+)/[^?]*#\1/$dbname#")"

say() { printf '\n== %s\n' "$*"; }

# The database and the two schemas the cross-schema tenants live in. Migrations
# do not create schemas — a frozen boundary of the tool — so this does, in Go,
# so that a machine without a psql client runs the same sequence CI does.
say "database $dbname, with schema_a and schema_b"
(cd "$here" && go run ./bootstrap "$admin" "$dbname")

say "building the CLI from the ORM module"
orm="$(mktemp -d)/orm"
(cd "$root" && go build -o "$orm" ./cmd/orm)

cd "$here"
export UUIDCOMPAT_DSN="$dsn"

say "module builds"
go build ./...
go vet ./...

say "migrate"
"$orm" migrate --config orm.yaml

say "check --generated"
"$orm" check --config orm.yaml --generated

say "tests"
go test -count=1 ./...

say "the project converges"
"$orm" makemigrations --check --config orm.yaml

say "OK"
