// Package tenanta declares the uuid relations of schema_a.
//
// It exists to be the other half of a pair. schema_a.users and schema_b.users
// have the same basename, the same column names and the same Go type for their
// key, and the only thing that distinguishes them is the schema they are in —
// which is exactly the situation where a generator that keys anything on a
// relation's name rather than on its identity produces one descriptor for two
// relations, or merges their migration state.
//
// They are separate Go packages because the generated handle is named after the
// relation, so two relations called users cannot both be Users in one package.
// That is the ordinary multi-package layout rather than anything to do with
// uuid.
package tenanta

import "github.com/google/uuid"

//orm:table schema_a.users
type User struct {
	ID    uuid.UUID `orm:"pk,pgtype:uuid"`
	Label string
}
