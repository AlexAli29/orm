// Package port declares what the core needs from the outside, in the core's own
// vocabulary.
//
// These interfaces are defined here, next to the code that calls them, rather
// than next to the code that implements them. That is the direction that makes
// the arrangement work: the adapter depends on the core's idea of storage, and
// the core depends on nothing. Reversing it — an interface declared beside its
// database implementation — leaves the core importing the database package to
// name the type, which is the dependency the hexagon exists to remove.
//
// Like [domain], this package imports only the standard library.
package port

import (
	"context"
	"time"

	"example.com/hexagonal/core/domain"
)

// UserRepository is what the core needs to store and find users.
//
// Every method takes a [Tx]. The core decides what is atomic, and it cannot
// decide that if the repository quietly opens its own transaction — so the unit
// of work is a parameter, and a repository that ignored it would be visibly
// ignoring it rather than silently.
type UserRepository interface {
	CreateUser(ctx context.Context, tx Tx, u domain.User) (domain.User, error)
	UserByID(ctx context.Context, tx Tx, id int64) (domain.User, error)
	UserByEmail(ctx context.Context, tx Tx, email string) (domain.User, error)
}

// ProjectRepository is what the core needs to store and find projects.
type ProjectRepository interface {
	CreateProject(ctx context.Context, tx Tx, p domain.Project) (domain.Project, error)
	ProjectBySlug(ctx context.Context, tx Tx, slug string) (domain.Project, error)
	ProjectsByOwner(ctx context.Context, tx Tx, ownerID int64) ([]domain.Project, error)
}

// Tx is one unit of work, opaque to the core.
//
// It is deliberately empty. The core has to be able to say "these two writes
// are one operation" and to hand the same unit to two repositories; it does not
// have to know what a transaction is, and giving it Commit and Rollback methods
// would let it try to run one — which is [UnitOfWork]'s job, because a
// transaction that a caller can forget to close is a transaction that leaks.
type Tx any

// UnitOfWork runs work atomically.
//
// The callback shape is the point: there is no Begin for the core to call, so
// there is no path on which a transaction is opened and not finished. An error
// from the callback rolls back; a panic rolls back; returning nil commits.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error
}

// Clock is time, as a dependency.
//
// The core needs to stamp things and a test needs those stamps to be
// predictable, and calling time.Now in the middle of a rule makes the second
// impossible. It is a port rather than a domain type because it is an input
// from outside, which is what a port is.
type Clock interface {
	Now() time.Time
}
