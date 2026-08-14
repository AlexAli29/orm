// Package ormstore is the driven adapter: it implements the core's ports using
// this ORM and PostgreSQL.
//
// It is the only package in this module that imports the ORM, and it is the
// outermost layer in the sense that matters — everything else could keep
// working if it were replaced. What it may not do is leak: no ORM type, no pgx
// type and no SQLSTATE appears in anything it returns, because the core's
// vocabulary is [domain.ErrNotFound], [domain.ErrConflict] and
// [domain.ErrInvalid], and translating into that vocabulary is this package's
// job.
//
// The entity declarations below are the schema. This is the ORM's managed mode:
// these structs own the tables, `orm makemigrations` writes the migration that
// carries them to PostgreSQL, and reconciliation proves the two agree. They are
// deliberately not the domain types: the domain says what a project is, and
// this says how one is stored, and conflating them is how a column rename turns
// into a change to a business rule.
package ormstore

import (
	"time"

	"github.com/AlexAli29/orm"
)

// userRow is the users table.
//
//orm:table users
type userRow struct {
	ID        int64  `orm:"pk,identity"`
	Email     string `orm:"unique"`
	Name      string
	CreatedAt time.Time `orm:"default:now()"`

	Projects orm.Many[projectRow]
}

// projectRow is the projects table.
//
//orm:table projects
//orm:index projects_owner_idx (owner_id)
type projectRow struct {
	ID        int64 `orm:"pk,identity"`
	OwnerID   int64
	Slug      string `orm:"unique"`
	Name      string
	CreatedAt time.Time `orm:"default:now()"`

	Owner orm.One[userRow] `orm:"fk:owner_id"`
}
