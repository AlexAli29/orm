// Package app is the use cases: the operations the application offers, written
// against ports.
//
// This is where transactions are decided, because this is where the meaning of
// "one operation" lives. A repository cannot know whether its write is the whole
// of something or a third of it; the use case knows, and says so by wrapping
// what belongs together in one unit of work.
//
// It imports the domain and the ports. It does not import the ORM, a driver or
// a transport, and a test enumerates its imports to keep it that way.
package app

import (
	"context"
	"errors"
	"fmt"

	"example.com/hexagonal/core/domain"
	"example.com/hexagonal/core/port"
)

// Service is the application, assembled from its ports.
//
// Everything it needs arrives in the constructor. There is no package-level
// database, no init and no global clock — which is what makes two of these
// able to exist at once, in the same process, against different databases.
type Service struct {
	users    port.UserRepository
	projects port.ProjectRepository
	uow      port.UnitOfWork
	clock    port.Clock
}

// New builds the application. Every dependency is required.
func New(users port.UserRepository, projects port.ProjectRepository, uow port.UnitOfWork, clock port.Clock) (*Service, error) {
	switch {
	case users == nil:
		return nil, errors.New("no user repository")
	case projects == nil:
		return nil, errors.New("no project repository")
	case uow == nil:
		return nil, errors.New("no unit of work")
	case clock == nil:
		return nil, errors.New("no clock")
	}
	return &Service{users: users, projects: projects, uow: uow, clock: clock}, nil
}

// Register creates a user.
//
// The rule runs before the write, so an invalid user never reaches the
// database. That the database would also have refused some of these is not a
// reason to skip it: the database refuses in its own vocabulary, at a moment
// when the caller has already been told the request was accepted.
func (s *Service) Register(ctx context.Context, u domain.User) (domain.User, error) {
	u.CreatedAt = s.clock.Now()
	if err := u.Validate(); err != nil {
		return domain.User{}, err
	}
	var out domain.User
	err := s.uow.Do(ctx, func(ctx context.Context, tx port.Tx) error {
		created, err := s.users.CreateUser(ctx, tx, u)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if err != nil {
		return domain.User{}, err
	}
	return out, nil
}

// StartProject creates a project for an existing owner.
//
// The check that the owner exists and the write that assumes it are in one unit
// of work, which is the whole reason the unit of work is a port. Split across
// two, the owner could be deleted in between and the write would land against a
// user who is not there — or fail on a foreign key with an error that says
// nothing about what the caller did wrong.
func (s *Service) StartProject(ctx context.Context, p domain.Project) (domain.Project, error) {
	p.CreatedAt = s.clock.Now()
	if err := p.Validate(); err != nil {
		return domain.Project{}, err
	}
	var out domain.Project
	err := s.uow.Do(ctx, func(ctx context.Context, tx port.Tx) error {
		if _, err := s.users.UserByID(ctx, tx, p.OwnerID); err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return errors.Join(domain.ErrInvalid, fmt.Errorf("no such owner"))
			}
			return err
		}
		created, err := s.projects.CreateProject(ctx, tx, p)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	if err != nil {
		return domain.Project{}, err
	}
	return out, nil
}

// User reads one user.
//
// A read is still a unit of work. It costs a transaction that does nothing
// interesting, and it buys one shape for every operation: nothing in this
// package has to remember which calls need a tx and which do not.
func (s *Service) User(ctx context.Context, id int64) (domain.User, error) {
	var out domain.User
	err := s.uow.Do(ctx, func(ctx context.Context, tx port.Tx) error {
		u, err := s.users.UserByID(ctx, tx, id)
		out = u
		return err
	})
	return out, err
}

// Projects lists what a user owns.
func (s *Service) Projects(ctx context.Context, ownerID int64) ([]domain.Project, error) {
	var out []domain.Project
	err := s.uow.Do(ctx, func(ctx context.Context, tx port.Tx) error {
		ps, err := s.projects.ProjectsByOwner(ctx, tx, ownerID)
		out = ps
		return err
	})
	return out, err
}

// ProjectBySlug reads one project.
func (s *Service) ProjectBySlug(ctx context.Context, slug string) (domain.Project, error) {
	var out domain.Project
	err := s.uow.Do(ctx, func(ctx context.Context, tx port.Tx) error {
		p, err := s.projects.ProjectBySlug(ctx, tx, slug)
		out = p
		return err
	})
	return out, err
}
