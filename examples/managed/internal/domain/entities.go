// Package domain declares the journal's schema.
//
// This is the managed half of the story. In the blog example PostgreSQL owns
// the schema and the structs are checked against it; here the declarations own
// it, and `orm makemigrations` writes the migration that carries them to
// PostgreSQL.
//
// What the declarations add over the database-first example is only the part a
// Go type cannot say: which column is the primary key, what a default is, which
// index serves which query. Everything derivable from the Go type — the column
// name, the type, whether it accepts NULL — is still derived, because a
// declaration that repeated it would be a second place to get it wrong.
//
// Nothing here refers to generated code, which is what lets a fresh checkout
// create the schema before the code that reads it exists.
package domain

import (
	"time"

	"github.com/AlexAli29/orm"
)

// Status is the Go side of an enum. The directive names the PostgreSQL type and
// its labels, in the order PostgreSQL will sort them.
//
//orm:enum public.article_status (draft, published, archived)
type Status string

// The labels of article_status.
const (
	StatusDraft     Status = "draft"
	StatusPublished Status = "published"
	StatusArchived  Status = "archived"
)

//orm:table authors
//orm:check authors_email_not_blank "email <> ''"
type Author struct {
	ID    int64  `orm:"pk,identity"`
	Email string `orm:"unique"`
	Name  string
	// A pointer is a column that accepts NULL. Nothing has to say so twice.
	Bio      *string
	JoinedAt time.Time `orm:"default:now()"`

	Articles orm.Many[Article]
}

// Article carries the index this application actually reads by.
//
// A feed asks for one author's articles, newest first, published only, and
// wants the title without a heap fetch. That is a partial covering index over
// (author_id, published_at DESC), and it is the reason a schema is worth
// declaring in PostgreSQL's own vocabulary rather than a portable one.
//
//orm:table articles
//orm:index articles_feed_idx (AuthorID, PublishedAt desc) include (Title) where "status = 'published'"
//orm:check articles_title_not_blank "title <> ''"
type Article struct {
	ID          int64 `orm:"pk,identity"`
	AuthorID    int64
	Title       string
	Body        string
	Status      Status `orm:"default:'draft'"`
	PublishedAt *time.Time
	CreatedAt   time.Time `orm:"default:now()"`

	// The side the key is on is derived from the columns the entity declares:
	// this one has author_id, so this one owns the constraint.
	Author orm.One[Author] `orm:"side:local"`
}
