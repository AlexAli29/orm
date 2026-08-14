package managed

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/desired"
	"github.com/AlexAli29/orm/internal/gen/goscan"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/jackc/pgx/v5"
)

// The services a command is built from.
//
// Every managed command needs some subset of the same four things: what the
// models want, what the migrations say, what the database has, and what has
// been applied to it. Collecting them here keeps the commands to argument
// handling and output, and keeps one answer to "how is the desired schema
// built" rather than one per command.

// Project binds a configuration to the services the managed workflow needs.
type Project struct {
	cfg   *config.Config
	store *migrate.Store
}

// Open prepares the services for a configuration. Nothing is read or connected
// until it is asked for.
func Open(cfg *config.Config) *Project {
	return &Project{cfg: cfg, store: migrate.NewStore(cfg.MigrationsDir())}
}

// Config returns the configuration the project was opened with.
func (p *Project) Config() *config.Config { return p.cfg }

// Store returns the migrations directory.
func (p *Project) Store() *migrate.Store { return p.store }

// Managed reports whether the Go declarations own the schema.
func (p *Project) Managed() bool { return p.cfg.Schema.Mode == config.ModeManaged }

// Set loads the migration history from disk.
func (p *Project) Set() (*migrate.Set, error) { return p.store.Load() }

// Desired builds the schema the Go declarations ask for.
//
// It touches no database. That is the property the whole workflow rests on: two
// developers on one branch compute the same migration whatever database they
// happen to have running.
func (p *Project) Desired(ctx context.Context) (*schema.Schema, error) {
	if !p.Managed() {
		return nil, ErrNotManaged
	}
	targets := make([]goscan.Target, 0, len(p.cfg.Packages))
	for _, pkg := range p.cfg.Packages {
		targets = append(targets, goscan.Target{Dir: p.cfg.Dir(pkg), OutputDir: p.cfg.OutputDir(pkg)})
	}
	scanned, err := goscan.Scan(ctx, p.cfg.Root, targets)
	if err != nil {
		return nil, err
	}
	if len(scanned.TagErrors) > 0 {
		// A malformed tag is a statement about the schema that could not be
		// read. Building a desired schema without it would produce a migration
		// for a schema nobody asked for.
		msgs := make([]string, 0, len(scanned.TagErrors))
		for _, e := range scanned.TagErrors {
			msgs = append(msgs, e.Error())
		}
		return nil, fmt.Errorf("the schema declarations cannot be read:\n    %s", strings.Join(msgs, "\n    "))
	}
	if len(scanned.Entities) == 0 {
		paths := make([]string, 0, len(p.cfg.Packages))
		for _, pkg := range p.cfg.Packages {
			paths = append(paths, pkg.Path)
		}
		return nil, fmt.Errorf("no entities found in %s: mark your entity structs with //orm:table, or correct packages.path",
			strings.Join(paths, ", "))
	}
	return desired.Build(desired.Input{Config: p.cfg, Entities: scanned.Entities, Decls: scanned.Decls})
}

// ErrNotManaged reports a command that only means something in managed mode.
var ErrNotManaged = errors.New("this command needs schema.mode: managed, where the Go declarations own the schema")

// ErrNoDatabase reports a command that needs a live database in a project
// configured without one.
var ErrNoDatabase = errors.New("this command needs schema.dsn: a migration runs against a database, and a DDL file is not one")

// Connect opens the single connection a migration run owns.
//
// It is one connection rather than a pool on purpose: the advisory lock a
// migration holds lives on a connection, and a pool that handed the DDL to a
// different one would be holding a lock that protects nothing.
func (p *Project) Connect(ctx context.Context) (*pgx.Conn, error) {
	if p.cfg.Schema.DSN == "" {
		return nil, ErrNoDatabase
	}
	return pgintro.Connect(ctx, p.cfg.Schema.DSN)
}

// Actual reads the live schema, without the engine's own history table.
func (p *Project) Actual(ctx context.Context, conn *pgx.Conn) (*schema.Schema, error) {
	return Inspect(ctx, conn, p.cfg.Schema.SearchPath)
}

// Snapshot is the three states, gathered together.
//
// It is deliberately not called a status: a status is a verdict, and this is
// the evidence. What the evidence means differs by command — generation refuses
// on a difference a check merely reports — so the verdict belongs to whoever
// asked.
type Snapshot struct {
	// Set is the migration history on disk.
	Set *migrate.Set
	// Applied is what the database records as applied, in order. It is nil when
	// no database was consulted.
	Applied []migrate.Applied
	// Comparison holds the three states and the differences between them.
	Comparison Comparison
}

// SnapshotInput selects which states to gather.
type SnapshotInput struct {
	// Conn is the database to read; nil skips everything that needs one, which
	// is what makemigrations does.
	Conn *pgx.Conn
}

// Snapshot gathers the states and compares them.
func (p *Project) Snapshot(ctx context.Context, in SnapshotInput) (Snapshot, error) {
	var out Snapshot

	set, err := p.Set()
	if err != nil {
		return out, err
	}
	out.Set = set

	cmp := CompareInput{Set: set}
	if p.Managed() {
		d, err := p.Desired(ctx)
		if err != nil {
			return out, err
		}
		cmp.Desired = d
	}

	if in.Conn != nil {
		// Nothing here creates the history table: a check is a question, and a
		// database nobody has migrated has simply applied nothing.
		m := migrate.New(in.Conn, set)
		applied, err := m.Applied(ctx)
		if err != nil {
			return out, err
		}
		out.Applied = applied
		cmp.Applied = applied

		actual, err := p.Actual(ctx, in.Conn)
		if err != nil {
			return out, fmt.Errorf("reading the live schema: %w", err)
		}
		cmp.Actual = actual
	}

	c, err := Compare(cmp)
	if err != nil {
		return out, err
	}
	out.Comparison = c
	return out, nil
}
