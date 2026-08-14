// Package uuidcompat is the permanent qualification project for PostgreSQL's
// uuid type.
//
// It is a module of its own, and that is the point being made rather than a
// packaging detail. uuid reaches Go through a configured type mapping, because
// Go has no uuid type and the popular third-party ones are not interchangeable
// — so the ORM refuses to choose one. A project picks, and the choice is a
// consumer's dependency, never the ORM's. This module depends on
// github.com/google/uuid; the ORM module does not, and a test here proves it
// still does not.
//
// What lives here is one coherent topology rather than a drawer of toy
// fixtures: a table with a uuid primary key, a uuid array and a nullable uuid;
// a second table referencing it by uuid; a view over both; and a materialized
// view with a unique uuid index. Every claim about uuid support is made against
// that one project, so a claim cannot pass because its fixture was shaped to
// let it.
package uuidcompat
