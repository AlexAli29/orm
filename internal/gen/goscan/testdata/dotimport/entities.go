// Package dotimport dot-imports the runtime package, which is the other way a
// relation type can appear under a name the scanner never sees.
package dotimport

import (
	. "github.com/AlexAli29/orm"
)

//orm:table users
type User struct {
	ID    int64
	Posts Many[Post]
}

//orm:table posts
type Post struct {
	ID     int64
	Author One[User]
}
