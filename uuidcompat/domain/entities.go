// Package domain declares the UUID topology this module qualifies.
//
// Every uuid field carries an explicit pgtype tag, and that asymmetry is
// deliberate evidence rather than an oversight. Database-first generation
// applies the configured types.uuid mapping on its own, because it starts from
// a PostgreSQL type and looks up the Go one. Managed generation starts from the
// Go type and has no reverse lookup, so it needs to be told. The tests pin all
// three behaviours — with the tag, without it, and database-first — so the
// difference stays a stated contract until the final API review decides whether
// configured mappings should work in both directions.
package domain

import (
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
)

// User is the root of the topology: a uuid primary key, a uuid array, and a
// nullable uuid that exists to keep the zero UUID and SQL NULL apart.
//
//orm:table public.users
type User struct {
	ID         uuid.UUID   `orm:"pk,pgtype:uuid"`
	Email      string      `orm:"unique"`
	ExternalID uuid.UUID   `orm:"pgtype:uuid"`
	OptionalID *uuid.UUID  `orm:"pgtype:uuid"`
	Tags       []uuid.UUID `orm:"pgtype:uuid[]"`

	Orders orm.Many[Order]
}

// Order references User by uuid, so the foreign key is a real uuid key rather
// than a surrogate integer beside one.
//
//orm:table public.orders
//orm:index orders_user_idx (UserID)
type Order struct {
	ID         uuid.UUID `orm:"pk,pgtype:uuid"`
	UserID     uuid.UUID `orm:"pgtype:uuid"`
	Label      string
	OptionalID *uuid.UUID `orm:"pgtype:uuid"`

	User orm.One[User] `orm:"fk:user_id"`
}

// Token exercises the two boundaries that are about where a uuid comes from
// rather than about what it is.
//
// TenantID is typed as a domain over uuid. A domain is a distinct PostgreSQL
// type with its own name, and reconciliation resolves it to what it is built on
// — so the configured uuid mapping serves it without a second entry, and a
// column of it is a uuid.UUID in Go. The domain is created by the runner,
// because migrations do not create domains any more than they create schemas.
//
// Value is left to the server. gen_random_uuid() is in core PostgreSQL from 13
// and every supported major is 14 or later, so it needs no extension — this is
// not a claim that some UUID-generating function is always available, only that
// this one is on the majors this project supports. A project that wants
// uuid-ossp installs it; the portable contract is that the application may
// always supply the value itself, and this column shows the other option
// working where the server offers it.
//
//orm:table public.tokens
type Token struct {
	ID       uuid.UUID `orm:"pk,pgtype:uuid"`
	TenantID uuid.UUID `orm:"pgtype:public.tenant_uuid"`
	Value    uuid.UUID `orm:"pgtype:uuid,default:gen_random_uuid()"`
}

// UserOrder is an ordinary view carrying uuid across a source boundary.
//
//orm:view public.user_orders
//orm:definition `SELECT u.id AS user_id, o.id AS order_id, o.label FROM users u JOIN orders o ON o.user_id = u.id`
//orm:depends-on public.users
//orm:depends-on public.orders
type UserOrder struct {
	UserID  uuid.UUID `orm:"pgtype:uuid"`
	OrderID uuid.UUID `orm:"pgtype:uuid"`
	Label   string
}

// UserSummary is the materialized view, and its unique index is over a uuid
// column. That index is what REFRESH CONCURRENTLY needs, and a uuid key has to
// qualify for it exactly as any other plain column would.
//
// It groups rather than selecting distinct, so the relation carries a column
// whose type nothing in this schema stores: count(*) is bigint and every column
// declared here is uuid or text. That is not a uuid claim — it is the shape that
// found the type-discovery defect, and it stays in the fixture so a uuid project
// keeps proving it does not come back.
//
//orm:materialized-view public.user_summaries
//orm:definition `SELECT user_id, min(label) AS label, count(*) AS orders FROM user_orders GROUP BY user_id`
//orm:depends-on public.user_orders
//orm:index user_summaries_key (UserID) unique
type UserSummary struct {
	UserID uuid.UUID `orm:"pgtype:uuid"`
	Label  string
	Orders int64
}
