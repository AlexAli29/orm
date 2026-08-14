// Package domain declares the blog's schema.
//
// Nothing here refers to generated code. The declarations name Go fields, which
// is what lets a brand-new project describe its schema, create it, and only
// then generate the typed API that reads it.
package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

// PostStatus is the Go side of the enum. The directive names the PostgreSQL
// type and its labels, in the order PostgreSQL will sort them.
//
//orm:enum public.post_status (draft, published, archived)
type PostStatus string

const (
	PostDraft     PostStatus = "draft"
	PostPublished PostStatus = "published"
	PostArchived  PostStatus = "archived"
)

//orm:table users
//orm:check users_email_not_blank "email <> ''"
//orm:index users_active_created_idx (Active, CreatedAt desc)
type User struct {
	ID        int64  `orm:"pk,identity"`
	Email     string `orm:"unique"`
	Name      string
	Nickname  *string
	Active    bool      `orm:"default:true"`
	CreatedAt time.Time `orm:"default:now()"`

	Profile  orm.One[Profile]
	Posts    orm.Many[Post]
	Comments orm.Many[Comment]
}

//orm:table profiles
type Profile struct {
	ID     int64 `orm:"pk,identity"`
	UserID int64 `orm:"unique"`
	Bio    *string

	User orm.One[User] `orm:"side:local"`
}

//orm:table posts
//orm:index posts_feed_idx (AuthorID, CreatedAt desc) include (Title) where "status = 'published'"
//orm:check posts_title_not_blank "title <> ''"
type Post struct {
	ID        int64 `orm:"pk,identity"`
	AuthorID  int64
	Title     string
	Body      string
	Status    PostStatus `orm:"default:'draft'"`
	CreatedAt time.Time  `orm:"default:now()"`

	Author   orm.One[User] `orm:"side:local"`
	Comments orm.Many[Comment]
}

//orm:table comments
//orm:unique comments_post_author_key (PostID, AuthorID)
type Comment struct {
	ID        int64 `orm:"pk,identity"`
	PostID    int64
	AuthorID  int64
	Body      string
	CreatedAt time.Time `orm:"default:now()"`

	Post   orm.One[Post] `orm:"side:local"`
	Author orm.One[User] `orm:"side:local"`
}

// Booking declares every range family a managed schema can create.
//
// Period, Stay and Shift are all orm.Range[time.Time] and become three
// different PostgreSQL types. The Go type cannot decide between daterange,
// tsrange and tstzrange, so the tag does — and the one with no tag gets the
// zoned family, which is the same rule a bare time.Time field already follows.
//
//orm:table bookings
//orm:index bookings_period_gist (Period) using gist
type Booking struct {
	ID     int64 `orm:"pk,identity"`
	Room   string
	Period orm.Range[time.Time]
	Stay   orm.Range[time.Time] `orm:"pgtype:daterange"`
	Shift  orm.Range[time.Time] `orm:"pgtype:tsrange"`
	Quota  orm.Range[int32]
	Span   orm.Range[int64]

	Revised *orm.Range[time.Time]

	Lease orm.Interval
	Grace *orm.Interval

	Holds orm.Multirange[time.Time]
	Slots *orm.Multirange[int32]
}
