// Package reconcile proves that hand-written Go entities and an introspected
// PostgreSQL schema describe the same thing, and reports every place they do
// not.
//
// Reconciliation is not generation. Nothing here derives one representation
// from the other: the structs are the author's, the schema is the migrations',
// and this package's only job is to check the correspondence and explain the
// gaps. Every constraint it enforces comes from one side or the other. Where the
// specification could have invented a rule, it does not — a mandatory
// belongs-to must have its foreign key mapped, but only because a NOT NULL
// column with no default cannot be inserted without one, which is a fact about
// the schema rather than a preference of the tool.
//
// # Tiers
//
// Checks run in layers, each depending on the previous one:
//
//	entity    the table exists, has a primary key, is not shared with another
//	          entity, is not a view, and exposes its identity
//	column    each field has a column, each type is representable, each column
//	          is accounted for
//	enum      a PostgreSQL enum's labels and a Go type's constants match exactly
//	relation  each orm.One and orm.Many resolves to exactly one foreign key,
//	          with the direction derived from the catalog
//
// The column tier runs before the relation tier because a relation key column's
// mapped field index is an index into the column mappings. The unmapped-column
// report runs last, because whether an unmapped column matters depends on
// whether a relation needs it.
package reconcile
