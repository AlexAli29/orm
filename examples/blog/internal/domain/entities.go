// Package domain holds the blog's entities.
//
// The structs are ordinary Go: no base type to embed, no tags carrying a
// schema, nothing about them written for the ORM's benefit. What makes them
// entities is the //orm:table directive, and what makes them correct is that
// orm check proved every field against the real column.
//
// The orm_*.gen.go files beside this one are generated from those structs and
// the schema together. They are committed, so the package builds without a
// database, and orm check --generated reports when they stop matching.
package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

// PostStatus mirrors the post_status enum. A named string type is all the ORM
// needs; the labels are proved against pg_enum during reconciliation, so a typo
// here is a check failure rather than a runtime error.
type PostStatus string

// The labels of post_status.
const (
	PostDraft     PostStatus = "draft"
	PostPublished PostStatus = "published"
	PostArchived  PostStatus = "archived"
)

//orm:table users
type User struct {
	ID        int64
	Email     string
	Name      string
	Active    bool
	CreatedAt time.Time

	Profile  orm.One[Profile]
	Posts    orm.Many[Post]
	Comments orm.Many[Comment]
}

//orm:table profiles
type Profile struct {
	ID     int64
	UserID int64
	Bio    *string
}

//orm:table categories
type Category struct {
	ID   int64
	Name string
}

//orm:table posts
type Post struct {
	ID         int64
	AuthorID   int64
	CategoryID *int64
	Title      string
	Body       string
	Status     PostStatus
	CreatedAt  time.Time

	Author   orm.One[User]
	Category orm.One[Category]
	Comments orm.Many[Comment]
}

// Comment maps both of its foreign keys, because this application writes
// comments and a row it inserts has to be able to say which post it is on.
//
// Mapping them is a choice rather than a requirement: a relation whose key has
// no Go field still loads, from the key the statement selects. Leaving them out
// would make comments read-only through this entity, which orm check says in so
// many words.
//
//orm:table comments
type Comment struct {
	ID        int64
	PostID    int64
	AuthorID  int64
	Body      string
	CreatedAt time.Time

	Author orm.One[User]
}
