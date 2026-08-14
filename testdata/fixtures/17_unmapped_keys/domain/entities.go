// Package domain leaves relation key columns unmapped on purpose.
//
// A relation is a fact about PostgreSQL, so its key is a column, not a field.
// The generator has already proved the foreign key from the catalog; making the
// author restate it in Go would ask them to duplicate what reconciliation
// verified. What the schema does force is narrower and comes from the schema
// itself: a NOT NULL key with no default must be mapped, because otherwise no
// row can be inserted.
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
	ID    int64
	Title string

	// posts.author_id is nullable and unmapped: legal, read-only.
	Author orm.One[User]
}

//orm:table docs
type Doc struct {
	ID    int64
	Title string

	// docs.owner_id is NOT NULL with no default and unmapped: not insertable.
	Owner orm.One[User]
}

//orm:table comments
type Comment struct {
	ID       int64
	TenantID int64
	Body     string

	// (tenant_id, author_id): the first is mapped, the second is not.
	Author orm.One[User] `orm:"fk:comments_author_fkey"`
}

//orm:table notes
type Note struct {
	Body string
}
