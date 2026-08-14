// Package domain holds a correct has-many and one declared over a foreign key
// pointing the wrong way.
package domain

import "github.com/AlexAli29/orm"

//orm:table orgs
type Org struct {
	ID   int64
	Name string
}

//orm:table users
type User struct {
	ID    int64
	Name  string
	OrgID *int64

	Posts orm.Many[Post]
	// users.org_id holds one value per row, so it cannot yield many orgs.
	Orgs orm.Many[Org]
}

//orm:table posts
type Post struct {
	ID       int64
	AuthorID int64
	Title    string
}
