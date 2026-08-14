// Package service is where the application's operations live, and where
// transactions are owned.
//
// The rule this package exists to demonstrate is one line long: a transaction
// belongs to whoever knows what the unit of work is. That is the operation, not
// the query. [Store.CreateProject] cannot open a transaction because it takes
// an executor and has no way to begin one; [Service.CreateProject] opens
// exactly one and passes it down. So a three-table operation is atomic without
// anybody having thought about it at the call site, and a data-access function
// cannot commit a third of somebody else's work.
//
// The other thing to notice is what is absent. There is no ProjectRepository
// interface, no UseCaseFactory and no ServiceInterface — the pragmatic layout
// this example demonstrates uses a concrete type because there is exactly one
// implementation and inventing a seam for it would be inventing work. The
// hexagonal example next door shows the other choice, and says when it is worth
// making.
package service

import (
	"context"
	"fmt"

	"example.com/production/domain"
	"example.com/production/postgres"
	"github.com/AlexAli29/orm"
)

// Service performs the application's operations.
//
// It holds an executor and the store. Both arrive through the constructor:
// there is no package-level database in this example, because a package-level
// database is a thing tests cannot replace and a program cannot have two of.
//
// The executor is an [orm.Executor] rather than a pool, which is what keeps pgx
// out of this package's signatures — and what lets a test hand it a transaction
// so that everything the test did rolls away.
type Service struct {
	ex    orm.Executor
	store postgres.Store
}

// New builds a service over an executor, which in production is a pool.
func New(ex orm.Executor) *Service { return &Service{ex: ex} }

// CreateProject creates a project, its first task and an audit entry, all or
// nothing.
//
// This is the operation the example exists for. Three tables change, and either
// all three do or none does — a project whose first task failed to insert is
// not a state this application has. The transaction is opened here because here
// is where that requirement is known.
//
// The context is the caller's, so a client that disconnects cancels the
// transaction and PostgreSQL rolls it back.
func (s *Service) CreateProject(ctx context.Context, in domain.NewProject) (domain.Project, error) {
	if err := in.Validate(); err != nil {
		return domain.Project{}, err
	}

	// orm.RunTx commits when the callback returns nil and rolls back when it
	// returns an error or panics. The three steps below therefore happen
	// together or not at all, and no data-access function had to know that.
	var project domain.Project
	var task domain.Task

	err := orm.RunTx(ctx, s.ex, func(tx orm.Executor) error {
		var err error
		project, err = s.store.CreateProject(ctx, tx, domain.Project{
			OwnerID: in.OwnerID, Slug: in.Slug, Name: in.Name,
		})
		if err != nil {
			return err
		}

		task, err = s.store.CreateTask(ctx, tx, domain.Task{
			ProjectID: project.ID, Title: in.FirstTask,
		})
		if err != nil {
			return err
		}

		return s.store.RecordAudit(ctx, tx, domain.AuditEntry{
			Action: "project.created", Subject: project.Slug,
		})
	})
	if err != nil {
		return domain.Project{}, err
	}

	project.Tasks = []domain.Task{task}
	return project, nil
}

// CreateUser creates a user.
//
// One statement, so no transaction: the pool is handed straight down. A
// transaction around a single statement adds two round trips and no atomicity
// that PostgreSQL did not already give it.
func (s *Service) CreateUser(ctx context.Context, u domain.User) (domain.User, error) {
	if u.Email == "" {
		return domain.User{}, fmt.Errorf("%w: a user needs an email", domain.ErrInvalid)
	}
	return s.store.CreateUser(ctx, s.ex, u)
}

// User reads one user.
func (s *Service) User(ctx context.Context, id int64) (domain.User, error) {
	return s.store.UserByID(ctx, s.ex, id)
}

// Project reads a project and its tasks.
func (s *Service) Project(ctx context.Context, slug string) (domain.Project, error) {
	if slug == "" {
		return domain.Project{}, fmt.Errorf("%w: a slug is required", domain.ErrInvalid)
	}
	return s.store.ProjectWithTasks(ctx, s.ex, slug)
}

// ProjectsForOwner lists an owner's projects.
func (s *Service) ProjectsForOwner(ctx context.Context, ownerID int64) ([]domain.Project, error) {
	const pageSize = 50
	return s.store.ProjectsForOwner(ctx, s.ex, ownerID, pageSize)
}

// AuditCount is here for the tests that prove a rollback undid the audit row
// along with everything else.
func (s *Service) AuditCount(ctx context.Context) (int64, error) {
	return s.store.CountAudit(ctx, s.ex)
}
