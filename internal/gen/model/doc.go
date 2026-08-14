// Package model defines the neutral vocabulary shared by Go analysis,
// PostgreSQL introspection and reconciliation.
//
// The package holds three groups of types and no behaviour beyond small
// accessors:
//
//   - The Go side — [GoEntity], [GoField], [GoType], [GoRel], [FieldTags] —
//     describes what gen/goscan resolved from hand-written entity structs.
//   - The PostgreSQL side — [PGTable], [PGColumn], [PGType], [PGForeignKey],
//     [PGUnique] — describes what gen/pgintro read out of the catalogs.
//   - The [Mapping] group records the correspondence gen/reconcile proved
//     between the two.
//
// Neither side is derived from the other, so neither set of types embeds the
// other's assumptions. In particular relation direction is not part of the Go
// model: [GoRel] carries only a [Cardinality] and a target, and [FKSide] is
// established during reconciliation from the foreign keys PostgreSQL actually
// declares.
package model
