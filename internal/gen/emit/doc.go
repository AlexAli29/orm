// Package emit renders generated Go code from a proven mapping.
//
// It discovers nothing. Every fact it needs — which table an entity maps to,
// which columns it exposes, what each column's PostgreSQL type is and whether
// it is nullable — was established by gen/reconcile and arrives in a
// [model.Mapping]. The emitter's whole job is to write that down in Go, which
// is why it can be tested against a mapping built by hand and why generation
// can never disagree with the check that preceded it.
//
// # What gets generated
//
// For each entity package, three files:
//
//	orm_tables.gen.go   the table descriptor and its typed columns
//	orm_meta.gen.go     the entity metadata and its scanner
//	orm_db.gen.go       the DB struct that wires repositories to metadata
//
// # Capabilities come from PostgreSQL
//
// Which descriptor a column receives is decided by its PostgreSQL type, not by
// whatever Go type sits opposite it. A jsonb column does not get Gt merely
// because some Go representation of it happens to be comparable, and a text
// column gets ILike because PostgreSQL has ILIKE, not because Go strings have
// methods. The capability lattice is the schema's, and the generator is where
// that is enforced.
//
// # Determinism
//
// Two runs over the same structs and the same schema produce identical bytes.
// Entities and columns come out in a fixed order, imports are sorted, no map is
// ranged without sorting, and nothing carries a timestamp or a version. The
// output is formatted with go/format before it is written, so a formatting
// change in the emitter cannot show up as a diff.
package emit
