// Package domain holds a belongs-to relation over a non-unique foreign key,
// which is legal and produces no findings.
package domain

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	ID   int64
	Name string
}

//orm:table posts
type Post struct {
	ID       int64
	AuthorID *int64
	Title    string

	Author orm.One[User]
}
