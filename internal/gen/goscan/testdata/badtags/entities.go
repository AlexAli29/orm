// Package badtags collects every way an orm tag can be wrong. The package is
// valid Go: the tags are wrong, not the code.
package badtags

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	Unknown   string         `orm:"colum:email"`
	NoValue   string         `orm:"column"`
	EmptyVal  string         `orm:"column:"`
	FKOnField string         `orm:"fk:users_manager_fkey"`
	SideOnCol string         `orm:"side:local"`
	Duplicate string         `orm:"column:a,column:b"`
	Stray     string         `orm:"column:a,,type:x"`
	BadSide   orm.One[Post]  `orm:"side:sideways"`
	ColOnRel  orm.Many[Post] `orm:"column:posts"`
	Empty     string         `orm:""`
}

//orm:table posts
type Post struct {
	ID int64
}
