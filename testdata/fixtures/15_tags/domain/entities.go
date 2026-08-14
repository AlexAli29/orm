// Package domain collects tags that are wrong in every way the grammar allows.
// The Go compiles; the tags do not mean anything.
package domain

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	ID   int64
	Name string
	// A directive that is not in the grammar.
	Unknown string `orm:"nope:1"`
	// fk: describes how a relation resolves and says nothing on a scalar.
	FKOnScalar string `orm:"fk:posts_author_id_fkey"`
	// side: takes local or remote.
	BadSide orm.One[Post] `orm:"side:sideways"`
}

//orm:table posts
type Post struct {
	ID       int64
	AuthorID int64
	Title    string
}
