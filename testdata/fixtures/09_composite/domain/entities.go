// Package domain holds composite primary and foreign keys.
package domain

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	ID       int64
	TenantID int64
	Name     string

	Posts orm.Many[Post]
}

//orm:table posts
type Post struct {
	TenantID int64
	ID       int64
	AuthorID int64
	Title    string

	Author orm.One[User]
}
