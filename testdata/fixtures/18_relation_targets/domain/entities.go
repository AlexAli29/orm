// Package domain points a relation at a struct that is not an entity.
package domain

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	ID   int64
	Name string

	Posts orm.Many[Post]
}

// Draft carries no //orm:table directive, so there is no table to relate to.
type Draft struct {
	ID int64
}

//orm:table posts
type Post struct {
	ID       int64
	AuthorID int64
	Title    string

	Author orm.One[User]
	Draft  orm.One[Draft]
}
