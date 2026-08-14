// Package domain is the vocabulary of the example application.
//
// It is deliberately the smallest thing that still demonstrates what a real
// application does with an ORM: a user owns projects, a project holds tasks, a
// task belongs to exactly one project. That is enough to need a foreign key, a
// unique constraint, a relation load, and an operation that must be atomic
// across three tables.
//
// Nothing here imports the ORM or the driver. These are the types the rest of
// the program agrees about; where they are stored is somebody else's problem,
// and keeping that true is what lets the service layer be tested without a
// database and read without one either.
package domain

import (
	"errors"
	"time"
)

// User is somebody who owns projects.
type User struct {
	ID        int64
	Email     string
	Name      string
	CreatedAt time.Time
}

// Project is a container for tasks.
type Project struct {
	ID        int64
	OwnerID   int64
	Slug      string
	Name      string
	CreatedAt time.Time

	// Tasks is filled in when the caller asked for them, and is nil when they
	// were not loaded. A project with no tasks and a project whose tasks were
	// not fetched are different things, and an empty non-nil slice says the
	// first.
	Tasks []Task
}

// Task is one thing to do inside a project.
type Task struct {
	ID        int64
	ProjectID int64
	Title     string
	Done      bool
	CreatedAt time.Time
}

// AuditEntry records that something happened, and exists so that the example's
// central operation has a third table to be atomic across.
type AuditEntry struct {
	ID       int64
	Action   string
	Subject  string
	Recorded time.Time
}

// The errors the application layer speaks.
//
// They are declared here, in the package that has no dependencies, so that the
// PostgreSQL adapter can translate a driver error into one of them and the
// transport can map one of them to a status code — without either of those two
// packages knowing about the other.
var (
	// ErrNotFound is returned when the thing asked for is not there.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a write would break a uniqueness rule the
	// database enforces.
	ErrConflict = errors.New("conflict")
	// ErrInvalid is returned when the request could not be honoured because of
	// what it asked for rather than because of what is stored.
	ErrInvalid = errors.New("invalid")
)

// NewProject is the input to creating a project and its first task.
type NewProject struct {
	OwnerID   int64
	Slug      string
	Name      string
	FirstTask string
}

// Validate checks what can be checked without a database.
//
// The database enforces the rules that need it — the foreign key, the unique
// slug — and this catches the ones that do not, so that an obviously empty
// request costs no round trip.
func (n NewProject) Validate() error {
	switch {
	case n.OwnerID == 0:
		return errors.Join(ErrInvalid, errors.New("a project needs an owner"))
	case n.Slug == "":
		return errors.Join(ErrInvalid, errors.New("a project needs a slug"))
	case n.Name == "":
		return errors.Join(ErrInvalid, errors.New("a project needs a name"))
	case n.FirstTask == "":
		return errors.Join(ErrInvalid, errors.New("a new project needs its first task"))
	}
	return nil
}
