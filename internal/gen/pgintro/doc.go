// Package pgintro reads a PostgreSQL schema out of the system catalogs.
//
// The catalogs are queried directly — pg_class, pg_namespace, pg_attribute,
// pg_type, pg_enum, pg_constraint, pg_index — rather than through
// information_schema, which is a standards-shaped view that loses exactly the
// facts reconciliation depends on: identity versus default, stored generated
// columns, whether a unique index is partial, and the ordinality of a composite
// foreign key.
//
// v1 introspects ordinary tables (relkind 'r') and partitioned tables
// (relkind 'p'). Views and materialized views are deliberately excluded: an
// entity is a thing you can insert, and a view is not one.
//
// # Schema files
//
// [FromFile] supports projects whose schema lives in DDL rather than in a
// running database. It creates a throwaway database, applies the DDL with
// PostgreSQL, introspects the result and drops the database again. There is no
// DDL parser here and there never will be — the only implementation of
// PostgreSQL's grammar that is worth trusting is PostgreSQL.
package pgintro
