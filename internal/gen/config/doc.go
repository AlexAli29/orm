// Package config loads and validates orm.yaml.
//
// A configuration names three things: where the PostgreSQL schema comes from,
// which Go packages hold entity structs, and how the two are allowed to differ.
//
//	version: 1
//
//	schema:
//	  dsn: ${DATABASE_URL}
//	  search_path:
//	    - public
//
//	packages:
//	  - path: ./internal/domain
//	    output: same
//
//	types:
//	  numeric:
//	    go: github.com/shopspring/decimal.Decimal
//	    codec: decimal
//
//	strict:
//	  unmapped_columns: warn
//	  timestamp_without_tz: warn
//
// The schema may instead be taken from a DDL file, in which case an
// administrative DSN is required so the tool can build a throwaway database and
// let PostgreSQL itself interpret the DDL:
//
//	schema:
//	  file: db/schema.sql
//	  admin_dsn: ${ADMIN_DATABASE_URL}
//
// Exactly one of dsn and file must be given.
//
// The file is executed by PostgreSQL, so it must be plain SQL. psql
// meta-commands — the backslash lines that pg_dump emits, such as \restrict and
// \connect — are not SQL and the server rejects them. Point schema.file at the
// DDL you maintain, or at a concatenation of your migrations, not at raw
// pg_dump output.
//
// # Environment expansion
//
// String values may reference environment variables as ${NAME}. A reference to
// an unset variable is a configuration error rather than an empty string,
// because an empty DSN fails much later and much less legibly than a missing
// one. A literal dollar sign is written $$.
package config
