// Package basic exercises entity discovery: both doc-comment placements,
// relations, tags, enums and null-capable types.
package basic

import (
	"database/sql"
	"time"

	"github.com/AlexAli29/orm"
)

// Status is a string enum reconciled against a PostgreSQL enum type.
type Status string

// The labels of Status.
const (
	StatusPending Status = "pending"
	StatusActive  Status = "active"
)

// Level is an integer enum.
type Level int

// The values of Level.
const (
	LevelLow  Level = 1
	LevelHigh Level = 2
)

// notAnEnum has no constants and must not be treated as one.
type notAnEnum string

//orm:table users
type User struct {
	ID        int64
	Email     string `orm:"column:email_address"`
	Nickname  *string
	State     Status
	Rank      Level
	Bio       sql.Null[string]
	Legacy    sql.NullString
	Tags      []string
	Blob      []byte
	Meta      map[string]any
	CreatedAt time.Time

	Posts   orm.Many[Post]   `orm:"fk:posts_author_fkey"`
	Profile orm.One[Profile] `orm:"side:remote"`
	Manager orm.One[User]    `orm:"fk:users_manager_fkey,side:local"`
	Ignored string           `orm:"-"`
	Untyped notAnEnum
	hidden  string //nolint:unused // an unexported field is not part of the mapped surface
}

// A grouped declaration attaches the doc comment to the TypeSpec instead of the
// GenDecl, which is the placement the scanner must also handle.
type (
	//orm:table posts
	Post struct {
		ID       int64
		AuthorID *int64
		Title    string
		Author   orm.One[User]
	}

	//orm:table analytics.events
	Event struct {
		ID   int64
		Name string
	}
)

//orm:view active_users
type ActiveUser struct {
	ID int64
}

//orm:table profiles
type Profile struct {
	ID     int64
	UserID int64
	Bio    string
}

// Unmarked carries no directive and is therefore not an entity.
type Unmarked struct {
	ID int64
}

func (u *User) touch() { _ = u.hidden }
