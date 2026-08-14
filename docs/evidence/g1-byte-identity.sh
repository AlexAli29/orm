#!/usr/bin/env bash
# Part A: generate the same project with the pre-G2 and post-G2 CLI and diff
# every byte that a user would commit.
set -uo pipefail

# Everything machine-specific comes from the environment or from where this
# script is, because the version of this file that shipped hardcoded one
# developer's home directory and one container name — so it ran nowhere else, and
# the M16.5 E sweep had to adapt a copy of it to re-run the gate it exists to be.
#
#   ORM_ROOT        the module root; defaults to this file's repository
#   ORM_PARTA_PRE   the pre-change CLI to compare against (required)
#   ORM_PARTA_POST  the post-change CLI; defaults to one built from ORM_ROOT
#   ORM_PARTA_DSN   an admin DSN for a server to build throwaway databases on
#   ORM_PARTA_WORK  scratch directory; defaults to a fresh mktemp -d
ROOT=${ORM_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}
BASE=${ORM_PARTA_WORK:-$(mktemp -d)}
PRE=${ORM_PARTA_PRE:?set ORM_PARTA_PRE to the CLI built from the revision to compare against}
POST=${ORM_PARTA_POST:-$BASE/ormcli-post}

if [ ! -f "$ROOT/go.mod" ]; then
  echo "ORM_ROOT=$ROOT is not a module root" >&2; exit 2
fi
if [ ! -x "$PRE" ]; then
  echo "ORM_PARTA_PRE=$PRE is not an executable" >&2; exit 2
fi
if [ ! -x "$POST" ]; then
  ( cd "$ROOT" && go build -o "$POST" ./cmd/orm ) || exit 2
fi
if cmp -s "$PRE" "$POST"; then
  echo "ORM_PARTA_PRE and ORM_PARTA_POST are the same binary; the gate would compare nothing" >&2
  exit 2
fi

# The admin DSN names the server; the databases are created through it rather
# than through docker exec, so this does not care how PostgreSQL was started.
ADMIN=${ORM_PARTA_DSN:?set ORM_PARTA_DSN to an admin DSN, e.g. postgres://orm:orm@localhost:5432/orm}
psql_admin() { psql "$ADMIN" -v ON_ERROR_STOP=1 -qtA "$@"; }
if ! psql_admin -c 'SELECT 1' >/dev/null 2>&1; then
  echo "cannot reach ORM_PARTA_DSN=$ADMIN" >&2; exit 2
fi

pass=0; fail=0
report() { if [ "$2" = "0" ]; then echo "  IDENTICAL  $1"; pass=$((pass+1)); else echo "  DIFFERS    $1"; fail=$((fail+1)); fi; }

# build <name> <entities-go> ; runs both CLIs against fresh databases
build() {
  local name="$1" body="$2"
  for side in pre post; do
    local dir="$BASE/$name-$side"
    rm -rf "$dir"; mkdir -p "$dir/domain" "$dir/migrations"
    cat > "$dir/go.mod" <<EOF
module example.com/$name

go 1.24.0

require github.com/AlexAli29/orm v0.0.0

replace github.com/AlexAli29/orm => $ROOT
EOF
    printf '%s' "$body" > "$dir/domain/entities.go"
    cat > "$dir/orm.yaml" <<EOF
version: 1
schema:
  mode: managed
  dsn: \${PARTA_DSN}
  search_path: [public]
migrations:
  dir: migrations
packages:
  - path: ./domain
    output: same
EOF
    ( cd "$dir" && go mod tidy >/dev/null 2>&1 )
    local db="parta_${name}_${side}"
    psql_admin -c "DROP DATABASE IF EXISTS $db" >/dev/null
    psql_admin -c "CREATE DATABASE $db" >/dev/null
    export PARTA_DSN="$(printf '%s' "$ADMIN" | sed -E "s#/[^/?]*(\\?|$)#/$db\\1#")"
    local cli=$PRE; [ "$side" = post ] && cli=$POST
    if ! ( cd "$dir" && "$cli" makemigrations --name initial >"$dir/mm.log" 2>&1 \
        && "$cli" migrate >>"$dir/mm.log" 2>&1 \
        && "$cli" generate >>"$dir/mm.log" 2>&1 ); then
      echo "  BROKEN     $name ($side): the project did not build"; sed -n '1,4p' "$dir/mm.log"
      fail=$((fail+1)); return
    fi
    if [ ! -f "$dir/orm.lock" ] || [ -z "$(ls -A "$dir/migrations")" ]; then
      echo "  BROKEN     $name ($side): produced no lock or no migration"
      fail=$((fail+1)); return
    fi
  done

  # Phase two: alter the declarations and migrate again. A fresh project carries
  # its indexes inside CreateTable, so nothing above ever emits CreateIndex —
  # and CreateIndex's state replay is exactly what G2 changed. Adding an index
  # to a relation that already exists is what exercises it.
  if [ -n "${ALTER:-}" ]; then
    for side in pre post; do
      local dir="$BASE/$name-$side"
      local db="parta_${name}_${side}"
      export PARTA_DSN="$(printf '%s' "$ADMIN" | sed -E "s#/[^/?]*(\\?|$)#/$db\\1#")"
      local cli=$PRE; [ "$side" = post ] && cli=$POST
      printf '%s' "$ALTER" > "$dir/domain/entities.go"
      if ! ( cd "$dir" && "$cli" makemigrations --name altered >>"$dir/mm.log" 2>&1 \
          && "$cli" migrate >>"$dir/mm.log" 2>&1 \
          && "$cli" generate >>"$dir/mm.log" 2>&1 ); then
        echo "  BROKEN     $name ($side): the altered project did not migrate"; tail -3 "$dir/mm.log"
        fail=$((fail+1)); return
      fi
      if [ "$(ls "$dir/migrations" | wc -l)" -lt 2 ]; then
        echo "  BROKEN     $name ($side): the alteration produced no second migration"
        fail=$((fail+1)); return
      fi
    done
  fi

  # Migration artifacts, generated Go, and the lock: everything committed.
  diff -r "$BASE/$name-pre/migrations" "$BASE/$name-post/migrations" >/dev/null 2>&1
  report "$name: migration bytes" $?
  diff "$BASE/$name-pre/orm.lock" "$BASE/$name-post/orm.lock" >/dev/null 2>&1
  report "$name: orm.lock" $?
  diff -r "$BASE/$name-pre/domain" "$BASE/$name-post/domain" >/dev/null 2>&1
  report "$name: generated Go" $?
}

echo "== table-only projects, every index shape the gate names"
BASEBODY='package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

//orm:table users
//orm:index users_active_idx (Active)
//orm:index users_active_created_idx (Active, CreatedAt desc)
//orm:index users_email_inc_idx (Active) include (Email)
//orm:index users_active_partial_idx (Email) where "active"
//orm:unique users_nick_key (Nickname)
type User struct {
	ID        int64  `orm:"pk,identity"`
	Email     string `orm:"unique"`
	Nickname  string
	Active    bool
	CreatedAt time.Time `orm:"default:now()"`
}

//orm:table posts
//orm:index posts_author_idx (AuthorID)
type Post struct {
	ID       int64 `orm:"pk,identity"`
	AuthorID int64
	Title    string

	Author orm.One[User] `orm:"fk:author_id"`
}
'
build tableidx "$BASEBODY"

echo "== table index add/drop against an existing table (exercises CreateIndex replay)"
ALTER='package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

//orm:table users
//orm:index users_active_idx (Active)
//orm:index users_active_created_idx (Active, CreatedAt desc)
//orm:index users_email_inc_idx (Active) include (Email)
//orm:index users_nick_active_idx (Nickname, Active)
//orm:unique users_nick_key (Nickname)
type User struct {
	ID        int64  `orm:"pk,identity"`
	Email     string `orm:"unique"`
	Nickname  string
	Active    bool
	CreatedAt time.Time `orm:"default:now()"`
}

//orm:table posts
//orm:index posts_author_idx (AuthorID)
//orm:index posts_title_idx (Title)
type Post struct {
	ID       int64 `orm:"pk,identity"`
	AuthorID int64
	Title    string

	Author orm.One[User] `orm:"fk:author_id"`
}
'
build tableidx2 "$BASEBODY"
unset ALTER

echo "== ordinary views: chain, multi-level"
build viewchain 'package domain

//orm:table users
type User struct {
	ID     int64 `orm:"pk,identity"`
	Email  string `orm:"unique"`
	Active bool
}

//orm:view public.active_users
//orm:definition "SELECT id, email FROM users WHERE active"
//orm:depends-on public.users
type ActiveUser struct {
	ID    int64
	Email string
}

//orm:view public.active_domains
//orm:definition "SELECT id, email FROM active_users"
//orm:depends-on public.active_users
type ActiveDomain struct {
	ID    int64
	Email string
}
'

echo
echo "identical: $pass   differs: $fail"
[ "$fail" -eq 0 ]
