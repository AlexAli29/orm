// Package domain holds three has-one relations, only one of which the schema
// actually guarantees.
package domain

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	ID   int64
	Name string

	Profile orm.One[Profile]
	Session orm.One[Session]
	Badge   orm.One[Badge]
}

//orm:table profiles
type Profile struct {
	ID     int64
	UserID int64
	Bio    *string
}

//orm:table sessions
type Session struct {
	ID     int64
	UserID int64
	Token  string
}

//orm:table badges
type Badge struct {
	ID     int64
	UserID int64
	Label  string
}
