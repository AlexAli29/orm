// Package domain shows an ambiguous relation, the tag that resolves it, and the
// two ways naming a constraint can fail.
package domain

import "github.com/AlexAli29/orm"

//orm:table users
type User struct {
	ID   int64
	Name string
}

//orm:table tags
type Tag struct {
	ID    int64
	Label string
}

//orm:table posts
type Post struct {
	ID       int64
	AuthorID int64
	EditorID *int64
	Title    string

	// Two candidates and no tag.
	Author orm.One[User]
	// The same pair of tables, disambiguated.
	Editor orm.One[User] `orm:"fk:posts_editor_fkey"`
	// A constraint that exists nowhere.
	Wrong orm.One[User] `orm:"fk:posts_reviewer_fkey"`
	// No foreign key relates posts and tags at all.
	Tag orm.One[Tag]
	// The foreign keys to users are all on posts, so side:remote excludes
	// every one of them. The schema is fine; the tag is wrong.
	Backwards orm.One[User] `orm:"fk:posts_author_fkey,side:remote"`
}
