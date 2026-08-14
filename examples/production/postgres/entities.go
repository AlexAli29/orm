// Package postgres is the only package in this example that knows the ORM
// exists.
//
// It holds three things: the entity declarations the generator reads, the
// generated descriptors it writes, and the functions that turn a domain type
// into a row and back. Everything above it — the service, the transports —
// speaks domain types and would not notice if this package were rewritten
// against a different store.
//
// The entities here are the schema. This is the ORM's managed mode: the Go
// declarations own the tables, `orm makemigrations` writes the migration that
// carries them to PostgreSQL, and reconciliation proves the two agree. Nothing
// in this file refers to generated code, which is what lets a fresh checkout
// create the schema before the code that reads it exists.
package postgres

import (
	"time"

	"github.com/AlexAli29/orm"
)

// User owns projects.
//
//orm:table users
type User struct {
	ID        int64  `orm:"pk,identity"`
	Email     string `orm:"unique"`
	Name      string
	CreatedAt time.Time `orm:"default:now()"`

	// Projects is the other side of projects.owner_id. Declaring it here is
	// what makes Users.Projects a loadable relation rather than a join
	// somebody writes out every time.
	Projects orm.Many[Project]
}

// Project belongs to a user and holds tasks.
//
//orm:table projects
//orm:index projects_owner_idx (owner_id)
type Project struct {
	ID        int64 `orm:"pk,identity"`
	OwnerID   int64
	Slug      string `orm:"unique"`
	Name      string
	CreatedAt time.Time `orm:"default:now()"`

	Owner orm.One[User] `orm:"fk:owner_id"`
	Tasks orm.Many[Task]
}

// Task belongs to a project.
//
//orm:table tasks
//orm:index tasks_project_idx (project_id)
type Task struct {
	ID        int64 `orm:"pk,identity"`
	ProjectID int64
	Title     string
	Done      bool      `orm:"default:false"`
	CreatedAt time.Time `orm:"default:now()"`

	Project orm.One[Project] `orm:"fk:project_id"`
}

// AuditEntry records what happened.
//
//orm:table audit_entries
type AuditEntry struct {
	ID       int64 `orm:"pk,identity"`
	Action   string
	Subject  string
	Recorded time.Time `orm:"default:now()"`
}
