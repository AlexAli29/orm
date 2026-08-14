// Package domain is a schema and a set of structs that agree completely.
package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

// UserState mirrors the user_state enum.
type UserState string

// The labels of user_state.
const (
	UserStatePending UserState = "pending"
	UserStateActive  UserState = "active"
	UserStateBanned  UserState = "banned"
)

//orm:table users
type User struct {
	ID          int64
	Email       string
	DisplayName *string
	State       UserState
	Tags        []string
	CreatedAt   time.Time

	Posts orm.Many[Post]
}

//orm:table posts
type Post struct {
	ID          int64
	AuthorID    int64
	Title       string
	Body        *string
	PublishedAt *time.Time

	Author orm.One[User]
}
