// Package goscan discovers entity structs in Go packages.
//
// Entities are explicit. A struct becomes one by carrying a directive naming the
// table it corresponds to:
//
//	//orm:table users
//	type User struct { ... }
//
//	//orm:table analytics.events
//	type Event struct { ... }
//
// Nothing is inferred: no pluralisation, no snake-casing of the type name, no
// convention that a type called User must live in users. A table name that is
// not written down does not exist. Column names are the one exception, and only
// because a struct field's name is a much narrower thing than a type's: a field
// maps to the lower_snake_case of its name unless a column: tag says otherwise.
//
// //orm:view is recognised so that it can be rejected with a specific finding
// rather than being silently ignored as an unmarked struct.
//
// # Resolved types, not source text
//
// Everything here goes through golang.org/x/tools/go/packages with full type
// information. Relation fields are recognised by asking go/types whether a
// field's type is the generic type One or Many declared by this module's runtime
// package — not by matching the string "orm.One". Import aliases, dot imports
// and a local type also called One therefore all behave correctly.
package goscan
