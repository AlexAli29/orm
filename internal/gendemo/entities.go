// Package gendemo is a worked example of a generated package.
//
// The entities here are hand-written and the orm_*.gen.go files beside them are
// not: they are committed output, so that `go build ./...` compiles generated
// code on every run and the integration tests can query through it. Regenerate
// them with TestGenerate_isDeterministic, which also asserts they do not change.
package gendemo

import (
	"database/sql"
	"net"
	"net/netip"
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
	ID        int64
	Email     string
	Age       int32
	Active    bool
	State     UserState
	Nickname  *string
	Score     *float64
	Tags      []string
	Settings  map[string]any
	Avatar    *[]byte
	ManagerID *int64
	Bio       sql.Null[string]
	Visits    sql.NullInt64
	DeletedAt *time.Time
	CreatedAt time.Time
	Slug      *string
	Metadata  *map[string]any

	Posts   orm.Many[Post]
	Profile orm.One[Profile]
	Manager orm.One[User]  `orm:"fk:users_manager_id_fkey,side:local"`
	Reports orm.Many[User] `orm:"fk:users_manager_id_fkey"`
}

//orm:table profiles
type Profile struct {
	ID     int64
	UserID int64
	Bio    *string

	Avatar orm.One[Avatar]
}

//orm:table avatars
type Avatar struct {
	ID  int64
	URL string `orm:"column:url"`
}

//orm:table categories
type Category struct {
	ID   int64
	Name string
}

// Comment maps neither of its foreign keys. Loading its author, or loading the
// comments of a post, therefore has to read the keys out of the statement that
// produced the parents rather than out of a Go field.
//
//orm:table comments
type Comment struct {
	ID        int64
	Body      string
	Visible   bool
	Score     int32
	CreatedAt time.Time

	Author    orm.One[User]
	Reactions orm.Many[Reaction]
}

//orm:table reactions
type Reaction struct {
	ID   int64
	Kind string

	Author orm.One[User]
}

// Document has two foreign keys to users, which reconciliation already told
// apart; each relation names the constraint it means.
//
//orm:table documents
type Document struct {
	ID    int64
	Title string
	// creator_id is mapped; editor_id deliberately is not, so that one entity
	// covers both a relation whose key the struct carries and one whose key
	// lives only in the schema.
	CreatorID int64

	Creator orm.One[User] `orm:"fk:documents_creator_id_fkey"`
	Editor  orm.One[User] `orm:"fk:documents_editor_id_fkey"`
}

//orm:table tenants
type Tenant struct {
	Code   string
	Region string
	Name   string

	Branches orm.Many[Branch]
	Events   orm.Many[BranchEvent]
}

// BranchEvent relates to Tenant on the same composite key the branches use, so
// a requested tree can carry a composite relation at more than one level.
//
//orm:table branch_events
type BranchEvent struct {
	ID     int64
	Label  string
	Code   string `orm:"column:event_code"`
	Region string `orm:"column:event_region"`
}

// Branch relates to Tenant on (region, code) — the order the constraint
// declares, which is not the order the columns appear in either table and not
// alphabetical either.
//
//orm:table branches
type Branch struct {
	ID     int64
	Label  string
	Code   string `orm:"column:branch_code"`
	Region string `orm:"column:branch_region"`

	Tenant orm.One[Tenant]
}

// Network carries PostgreSQL's network types, which map to the standard
// library rather than to strings: an inet and a cidr are both an address with a
// prefix length, and netip.Prefix is that.
//
//orm:table networks
type Network struct {
	ID       int64
	Label    string
	Subnet   netip.Prefix
	Host     netip.Prefix
	Fallback *netip.Prefix
	Hardware net.HardwareAddr
}

// Article carries a tsvector PostgreSQL maintains itself, which is what a
// generated column is for: the document and its parsed form cannot drift.
//
//orm:table articles
type Article struct {
	ID     int64
	Title  string
	Body   string
	Search *orm.TSVector
}

//orm:table posts
type Post struct {
	ID        int64
	AuthorID  *int64
	Title     string
	Published bool
	Score     int32
	CreatedAt time.Time

	Author   orm.One[User]
	Category orm.One[Category]
	Comments orm.Many[Comment]
}

// Team is keyed on citext, so PostgreSQL matches its relation
// case-insensitively and a Go-side comparison of the same keys would not.
//
//orm:table teams
type Team struct {
	Slug string
	Name string

	Members orm.Many[Member]
}

//orm:table members
type Member struct {
	ID       int64
	TeamSlug string
	Nickname string

	Team orm.One[Team]
}

// Booking exercises every range family, the two multirange families and both
// nullabilities of interval.
//
// Period, Stay and Shift are all orm.Range[time.Time] and all three columns are
// different PostgreSQL types. Nothing here says which: the catalog does, and
// reconciliation checks the Go side can hold whatever the catalog reports.
//
//orm:table bookings
type Booking struct {
	ID       int64
	Room     string
	StartsAt time.Time
	Period   orm.Range[time.Time]
	Stay     orm.Range[time.Time]
	Shift    orm.Range[time.Time]
	Quota    orm.Range[int32]
	Span     orm.Range[int64]

	// A nullable range is a pointer, the same way every other nullable column
	// is. Range[T] itself has no state that means SQL NULL.
	Revised *orm.Range[time.Time]

	Lease orm.Interval
	Grace *orm.Interval

	Holds orm.Multirange[time.Time]
	Slots *orm.Multirange[int32]
}
