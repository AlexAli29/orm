package ormstore

import (
	"context"
	"errors"
	"fmt"

	"example.com/hexagonal/core/domain"
	"example.com/hexagonal/core/port"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgconn"
)

// Store implements the core's repository ports.
//
// It holds nothing. The executor arrives with the unit of work, which is what
// lets the same Store serve a request inside a transaction and one outside it
// without two constructions and without a field that has to be right.
type Store struct{}

// NewStore returns the adapter. It has no dependencies by design: everything it
// needs to reach the database is in the [port.Tx] it is handed.
func NewStore() Store { return Store{} }

// Compile-time proof that the adapter satisfies the ports it claims. Without
// these, a signature drifting apart from its port is discovered by whoever next
// tries to build the wiring, which is further away than it needs to be.
var (
	_ port.UserRepository    = Store{}
	_ port.ProjectRepository = Store{}
	_ port.UnitOfWork        = (*Work)(nil)
)

// Work runs units of work against an executor.
//
// This is the only place a transaction is started, and it is started by the
// ORM's own helper: the callback gets an executor that is the transaction, so
// everything reached through it takes part, and nothing has to remember to
// commit.
type Work struct{ ex orm.Executor }

// NewWork binds the unit of work to an executor — a pool in a server, a
// transaction in a test that rolls everything back.
func NewWork(ex orm.Executor) *Work { return &Work{ex: ex} }

// Do runs fn in a transaction, committing when it returns nil.
//
// The core sees [port.Tx], which is `any`. What travels through it is an
// [orm.Executor], and only this package unwraps it — which is what keeps the
// ORM out of the core while still letting the core decide what is atomic.
func (w *Work) Do(ctx context.Context, fn func(ctx context.Context, tx port.Tx) error) error {
	return orm.RunTx(ctx, w.ex, func(ex orm.Executor) error {
		return fn(ctx, ex)
	})
}

// executor recovers the executor from the opaque unit of work.
//
// A Tx that did not come from this package is a wiring mistake — two adapters
// in one process, or a hand-made value in a test — and it is worth a clear
// error rather than a panic three frames down inside pgx.
func executor(tx port.Tx) (orm.Executor, error) {
	ex, ok := tx.(orm.Executor)
	if !ok {
		return nil, fmt.Errorf("this unit of work did not come from ormstore (%T)", tx)
	}
	return ex, nil
}

// db binds the generated repositories to the unit of work's executor.
func db(tx port.Tx) (*DB, error) {
	ex, err := executor(tx)
	if err != nil {
		return nil, err
	}
	return New(ex), nil
}

// CreateUser writes a user and returns it with what the database decided.
func (Store) CreateUser(ctx context.Context, tx port.Tx, u domain.User) (domain.User, error) {
	d, err := db(tx)
	if err != nil {
		return domain.User{}, err
	}
	row, err := d.Users.Insert(ctx, userRow{
		Email:     u.Email,
		Name:      u.Name,
		CreatedAt: u.CreatedAt,
	})
	if err != nil {
		return domain.User{}, translate(err, "creating a user")
	}
	return toUser(row), nil
}

// UserByID reads one user.
func (Store) UserByID(ctx context.Context, tx port.Tx, id int64) (domain.User, error) {
	d, err := db(tx)
	if err != nil {
		return domain.User{}, err
	}
	row, err := d.Users.Query().Where(Users.ID.Eq(id)).One(ctx)
	if err != nil {
		return domain.User{}, translate(err, "reading a user")
	}
	return toUser(row), nil
}

// UserByEmail reads one user by the address that identifies them.
func (Store) UserByEmail(ctx context.Context, tx port.Tx, email string) (domain.User, error) {
	d, err := db(tx)
	if err != nil {
		return domain.User{}, err
	}
	row, err := d.Users.Query().Where(Users.Email.Eq(email)).One(ctx)
	if err != nil {
		return domain.User{}, translate(err, "reading a user")
	}
	return toUser(row), nil
}

// CreateProject writes a project.
func (Store) CreateProject(ctx context.Context, tx port.Tx, p domain.Project) (domain.Project, error) {
	d, err := db(tx)
	if err != nil {
		return domain.Project{}, err
	}
	row, err := d.Projects.Insert(ctx, projectRow{
		OwnerID:   p.OwnerID,
		Slug:      p.Slug,
		Name:      p.Name,
		CreatedAt: p.CreatedAt,
	})
	if err != nil {
		return domain.Project{}, translate(err, "creating a project")
	}
	return toProject(row), nil
}

// ProjectBySlug reads one project.
func (Store) ProjectBySlug(ctx context.Context, tx port.Tx, slug string) (domain.Project, error) {
	d, err := db(tx)
	if err != nil {
		return domain.Project{}, err
	}
	row, err := d.Projects.Query().Where(Projects.Slug.Eq(slug)).One(ctx)
	if err != nil {
		return domain.Project{}, translate(err, "reading a project")
	}
	return toProject(row), nil
}

// ProjectsByOwner lists what a user owns, newest first.
func (Store) ProjectsByOwner(ctx context.Context, tx port.Tx, ownerID int64) ([]domain.Project, error) {
	d, err := db(tx)
	if err != nil {
		return nil, err
	}
	rows, err := d.Projects.Query().
		Where(Projects.OwnerID.Eq(ownerID)).
		OrderBy(Projects.CreatedAt.Desc(), Projects.ID.Desc()).
		All(ctx)
	if err != nil {
		return nil, translate(err, "listing projects")
	}
	out := make([]domain.Project, 0, len(rows))
	for _, r := range rows {
		out = append(out, toProject(r))
	}
	return out, nil
}

func toUser(r userRow) domain.User {
	return domain.User{ID: r.ID, Email: r.Email, Name: r.Name, CreatedAt: r.CreatedAt}
}

func toProject(r projectRow) domain.Project {
	return domain.Project{
		ID: r.ID, OwnerID: r.OwnerID, Slug: r.Slug, Name: r.Name, CreatedAt: r.CreatedAt,
	}
}

// translate turns a storage failure into the core's vocabulary.
//
// It reads the SQLSTATE and nothing else. PostgreSQL's message for a unique
// violation quotes the key — "Key (email)=(someone@example.com) already
// exists" — so a translation that matched on the message would be carrying one
// user's data up through the core and out to whoever gets the response. The
// five-character code says everything this needs to know.
//
// The context string is for the log. It names the operation, never a value.
func translate(err error, doing string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, orm.ErrNotFound) {
		return fmt.Errorf("%s: %w", doing, domain.ErrNotFound)
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505": // unique_violation
			return fmt.Errorf("%s: %w", doing, domain.ErrConflict)
		case "23503", // foreign_key_violation
			"23514", // check_violation
			"22001": // string_data_right_truncation
			return fmt.Errorf("%s: %w", doing, domain.ErrInvalid)
		}
	}
	// Everything else is a failure the core has no opinion about — a dead
	// connection, a cancelled context, a bug. It travels as itself, wrapped, so
	// the log has the cause; the transport is what decides not to show it.
	return fmt.Errorf("%s: %w", doing, err)
}
