// Package aliased imports the runtime package under a different name, and
// declares local types with the same names as the relation types, to prove that
// relation detection is by resolved identity rather than by source text.
package aliased

import (
	rel "github.com/AlexAli29/orm"
)

// One is a local decoy with the same name as the relation type.
type One[T any] struct{ V T }

// Many is the other decoy.
type Many[T any] struct{ V []T }

//orm:table users
type User struct {
	ID    int64
	Posts rel.Many[Post]
	Decoy One[Post]
}

//orm:table posts
type Post struct {
	ID     int64
	Author rel.One[User]
	Decoys Many[User]
}
