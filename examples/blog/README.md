# blog

A small HTTP service over the ORM. It exists to be read: every handler is a few
lines of stdlib `net/http` around one query, so the query is what stands out.

## Running it

```sh
docker compose up -d

export BLOG_DSN='postgres://blog:blog@localhost:55433/blog'
psql "$BLOG_DSN" -f schema.sql

# The generated files are committed, so this is optional — but it is how they
# got here, and how you would check they still match.
go run github.com/AlexAli29/orm/cmd/orm check --generated
go run github.com/AlexAli29/orm/cmd/orm generate

go run .
```

Then:

```sh
curl -s localhost:8080/users -d '{"email":"ada@example.com","name":"Ada"}'
curl -s localhost:8080/posts -d '{"author_id":1,"title":"Notes","body":"...","comments":["first"]}'
curl -s 'localhost:8080/users?email=example.com&published=true'
curl -s localhost:8080/posts/1
curl -s 'localhost:8080/search/users?q=ada'
```

## What each endpoint is for

| Endpoint | Shows |
|---|---|
| `GET /users` | dynamic filters assembled from the request, and `Any` as a relation filter |
| `GET /users/{id}` | a four-level relation tree with options at each level |
| `POST /users` | a Go zero value is a value; `orm.Default` is how you ask for the column's |
| `PATCH /users/{id}` | assigning only what the request sent |
| `POST /posts` | a transaction spanning several inserts |
| `GET /posts` | nested relations over a list |
| `POST /posts/{id}/comments` | a plain insert, with PostgreSQL's foreign key doing the checking |
| `GET /search/users` | `orm.Raw` — a statement the typed API has no way to express, still scanned into `[]User` |

## The schema is not the ORM's

`schema.sql` is applied with `psql`. This ORM does not own migrations, does not
create tables, and does not alter them. It reads the schema and proves the
structs agree with it.

## Tests

`go test ./...` runs the handlers against a real database. It needs
`ORM_TEST_ADMIN_DSN` pointing at a PostgreSQL a database can be created on.
