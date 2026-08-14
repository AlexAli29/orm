package postgres

import (
	"context"
	"errors"
	"fmt"

	"example.com/production/domain"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgconn"
)

// The data-access layer.
//
// Every function here takes an [orm.Executor] as its first argument rather than
// reaching for a stored one. That is the whole transaction design: a function
// cannot start a transaction of its own, so it cannot accidentally commit half
// of somebody's operation, and the caller that knows what the unit of work is
// decides what it is. A pool, a connection and a transaction all satisfy
// Executor, so the same function serves both cases without a second version of
// itself.
//
// The other thing this layer owns is translation. A PostgreSQL error is a
// *pgconn.PgError with a SQLSTATE; the layers above speak
// [domain.ErrConflict] and friends. Turning one into the other here is what
// keeps pgx out of the service and the transport — and it is done on the
// SQLSTATE, never on the message text, because the message is prose that
// changes between releases and quotes the data besides.

// Store reads and writes the application's tables.
//
// It holds no executor. Each method takes one, which is what makes the
// transaction boundary the caller's to draw.
type Store struct{}

// The SQLSTATEs this layer translates. PostgreSQL's codes are the contract;
// its messages are not.
const (
	sqlStateUniqueViolation     = "23505"
	sqlStateForeignKeyViolation = "23503"
)

// CreateUser inserts a user.
//
// A duplicate email is [domain.ErrConflict] rather than a driver error,
// because "that email is taken" is a thing the caller can act on and
// "SQLSTATE 23505 on users_email_key" is a thing it would have to decode.
func (Store) CreateUser(ctx context.Context, ex orm.Executor, u domain.User) (domain.User, error) {
	row, err := New(ex).Users.Insert(ctx, User{Email: u.Email, Name: u.Name})
	if err != nil {
		return domain.User{}, translate(err, "creating a user")
	}
	return toDomainUser(row), nil
}

// UserByID reads one user.
func (Store) UserByID(ctx context.Context, ex orm.Executor, id int64) (domain.User, error) {
	row, err := New(ex).Users.Query().Where(Users.ID.Eq(id)).One(ctx)
	if err != nil {
		return domain.User{}, translate(err, "reading a user")
	}
	return toDomainUser(row), nil
}

// CreateProject inserts a project.
//
// An owner that does not exist is a foreign-key violation, which is
// [domain.ErrInvalid]: the request named something that is not there, which is
// a fact about the request rather than a conflict with stored state.
func (Store) CreateProject(ctx context.Context, ex orm.Executor, p domain.Project) (domain.Project, error) {
	row, err := New(ex).Projects.Insert(ctx, Project{
		OwnerID: p.OwnerID, Slug: p.Slug, Name: p.Name,
	})
	if err != nil {
		return domain.Project{}, translate(err, "creating a project")
	}
	return toDomainProject(row), nil
}

// CreateTask inserts a task.
func (Store) CreateTask(ctx context.Context, ex orm.Executor, t domain.Task) (domain.Task, error) {
	row, err := New(ex).Tasks.Insert(ctx, Task{ProjectID: t.ProjectID, Title: t.Title})
	if err != nil {
		return domain.Task{}, translate(err, "creating a task")
	}
	return toDomainTask(row), nil
}

// RecordAudit writes the audit row.
func (Store) RecordAudit(ctx context.Context, ex orm.Executor, e domain.AuditEntry) error {
	if _, err := New(ex).AuditEntries.Insert(ctx, AuditEntry{
		Action: e.Action, Subject: e.Subject,
	}); err != nil {
		return translate(err, "recording an audit entry")
	}
	return nil
}

// ProjectWithTasks reads a project and the tasks belonging to it.
//
// The tasks come back through the declared relation, which is one extra
// statement whatever the number of projects — the loader batches, so this does
// not become a query per row.
func (Store) ProjectWithTasks(ctx context.Context, ex orm.Executor, slug string) (domain.Project, error) {
	row, err := New(ex).Projects.Query().
		Where(Projects.Slug.Eq(slug)).
		With(Projects.Tasks.OrderBy(Tasks.ID.Asc())).
		One(ctx)
	if err != nil {
		return domain.Project{}, translate(err, "reading a project")
	}
	out := toDomainProject(row)
	// Get reports whether the relation was loaded at all, which is a different
	// question from whether it has rows. It was asked for above, so it was.
	tasks, loaded := row.Tasks.Get()
	if !loaded {
		return domain.Project{}, fmt.Errorf("reading a project: the tasks relation did not load")
	}
	out.Tasks = make([]domain.Task, 0, len(tasks))
	for _, t := range tasks {
		out.Tasks = append(out.Tasks, toDomainTask(t))
	}
	return out, nil
}

// ProjectsForOwner lists an owner's projects, newest first.
func (Store) ProjectsForOwner(ctx context.Context, ex orm.Executor, ownerID int64, limit int) ([]domain.Project, error) {
	rows, err := New(ex).Projects.Query().
		Where(Projects.OwnerID.Eq(ownerID)).
		OrderBy(Projects.ID.Desc()).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, translate(err, "listing projects")
	}
	out := make([]domain.Project, 0, len(rows))
	for _, r := range rows {
		out = append(out, toDomainProject(r))
	}
	return out, nil
}

// CountAudit is used by the tests to prove a rollback undid everything.
func (Store) CountAudit(ctx context.Context, ex orm.Executor) (int64, error) {
	n, err := New(ex).AuditEntries.Query().Count(ctx)
	if err != nil {
		return 0, translate(err, "counting audit entries")
	}
	return n, nil
}

// translate turns a driver error into one the rest of the program understands.
//
// It reads the SQLSTATE and nothing else. The server's message is deliberately
// not inspected and deliberately not carried into the domain error: it quotes
// the values that caused the failure — "Key (email)=(someone@example.com)
// already exists" — and those belong in a log with the documented trust
// boundary, not in an error a handler might render.
func translate(err error, what string) error {
	if err == nil {
		return nil
	}
	// A cancelled request is not a database problem, and reporting it as one
	// would make a client disconnect look like an outage.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, orm.ErrNotFound) {
		return fmt.Errorf("%s: %w", what, domain.ErrNotFound)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case sqlStateUniqueViolation:
			return fmt.Errorf("%s: %w", what, domain.ErrConflict)
		case sqlStateForeignKeyViolation:
			return fmt.Errorf("%s: %w", what, domain.ErrInvalid)
		}
	}
	// Anything else keeps the original, wrapped. The transport decides what a
	// client is told; a log gets the whole thing.
	return fmt.Errorf("%s: %w", what, err)
}

func toDomainUser(u User) domain.User {
	return domain.User{ID: u.ID, Email: u.Email, Name: u.Name, CreatedAt: u.CreatedAt}
}

func toDomainProject(p Project) domain.Project {
	return domain.Project{
		ID: p.ID, OwnerID: p.OwnerID, Slug: p.Slug, Name: p.Name, CreatedAt: p.CreatedAt,
	}
}

func toDomainTask(t Task) domain.Task {
	return domain.Task{
		ID: t.ID, ProjectID: t.ProjectID, Title: t.Title, Done: t.Done, CreatedAt: t.CreatedAt,
	}
}
