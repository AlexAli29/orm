// Package postgres runs a real PostgreSQL in a container for a test.
//
// It is a thin, opinionated wrapper over the Testcontainers PostgreSQL module:
// it starts a container, waits for the server to be ready, opens a pool, and
// registers the teardown for both before returning.
//
//	func TestMain(m *testing.M) { ... }
//
//	func TestSomething(t *testing.T) {
//	    pg := ormtestpostgres.Run(t, ormtestpostgres.PostgreSQL17())
//	    db := domain.New(pg.Pool())
//	    // ...
//	}
//
// # It is a module of its own
//
// Testcontainers brings a Docker client and a large dependency tree, and a
// project that supplies its own database — a CI service container, a developer's
// local server — should not compile any of it. So this is a separate Go module.
// A project that never imports it never has Testcontainers in its graph, which
// is the whole reason for the extra go.mod.
//
// # Cleanup is registered before anything can fail
//
// The container's teardown is registered the moment the container exists, before
// the pool is opened and before this function can return an error. A test that
// fails during setup still terminates its container. That ordering is the only
// thing standing between a failing suite and a machine full of orphaned
// containers, which is why it is not left to a defer in the caller.
//
// # It does not hide the configuration
//
// [Instance] exposes the DSN and the pool configuration as well as the pool, so
// a test that needs a particular pool size, a particular AfterConnect, or a
// second connection can have one. A wrapper that only returned a ready-made
// handle would be unusable for exactly the tests that most need a real server.
package postgres

import (
	"context"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Images this package knows about.
//
// They are named rather than defaulted to "latest" on purpose: a test suite
// whose database version changes when an upstream tag moves is a suite that
// fails for reasons nobody changed. Anything not listed here is reachable with
// [Image].
const (
	// ImagePostgres14 and its siblings are the versions this project supports.
	ImagePostgres14 = "postgres:14-alpine"
	ImagePostgres16 = "postgres:16-alpine"
	ImagePostgres17 = "postgres:17-alpine"

	// ImagePostGIS16 and ImagePostGIS17 carry the PostGIS extension. They are
	// separate images rather than an extension installed after startup: the
	// image has it compiled and available, which is deterministic, and
	// installing one at runtime is a step that can fail on a slow machine.
	ImagePostGIS16 = "postgis/postgis:16-3.5"
	ImagePostGIS17 = "postgis/postgis:17-3.5"
)

// Option configures the container this package starts.
type Option func(*config)

type config struct {
	image        string
	database     string
	username     string
	password     string
	initScripts  []string
	initSQL      []string
	poolConfig   func(*pgxpool.Config)
	startTimeout time.Duration
}

// PostgreSQL14, PostgreSQL16 and PostgreSQL17 select a supported version.
func PostgreSQL14() Option { return Image(ImagePostgres14) }
func PostgreSQL16() Option { return Image(ImagePostgres16) }
func PostgreSQL17() Option { return Image(ImagePostgres17) }

// PostGIS selects an image with PostGIS available, defaulting to the newest
// supported pairing. [Image] takes any other.
func PostGIS() Option { return Image(ImagePostGIS17) }

// PostGIS16 selects the PostGIS image built on PostgreSQL 16.
func PostGIS16() Option { return Image(ImagePostGIS16) }

// Image runs an arbitrary image.
//
// It exists so that this package never stands between a project and the
// database it actually deploys: a fork, a private registry, a version released
// after this package was written.
func Image(ref string) Option { return func(c *config) { c.image = ref } }

// Database, Username and Password name the credentials the container is created
// with. Each has a working default.
func Database(name string) Option { return func(c *config) { c.database = name } }
func Username(name string) Option { return func(c *config) { c.username = name } }
func Password(pass string) Option { return func(c *config) { c.password = pass } }

// InitScripts runs the given SQL files inside the container before the server is
// declared ready. It is the container's own initialisation hook.
func InitScripts(paths ...string) Option {
	return func(c *config) { c.initScripts = append(c.initScripts, paths...) }
}

// InitSQL runs the given statements once, after the pool is open.
//
// It is for the small things — creating an extension, applying a schema held in
// a string — and it deliberately does not grow into a lifecycle framework.
// Anything larger belongs in a migration, which [github.com/AlexAli29/orm/ormtest.Migrate]
// applies through the engine that will apply it in production.
func InitSQL(statements ...string) Option {
	return func(c *config) { c.initSQL = append(c.initSQL, statements...) }
}

// PoolConfig adjusts the pool before it is opened.
//
// It is the escape hatch that keeps this package usable for the tests that need
// a particular pool: a size, an AfterConnect that registers a project's custom
// types, a tracer.
func PoolConfig(fn func(*pgxpool.Config)) Option {
	return func(c *config) { c.poolConfig = fn }
}

// StartTimeout bounds how long the container may take to become ready.
func StartTimeout(d time.Duration) Option {
	return func(c *config) { c.startTimeout = d }
}

// Instance is a running PostgreSQL container and a pool connected to it.
type Instance struct {
	// Container is the Testcontainers handle, for a test that needs to reach
	// past this package — to exec a command, to snapshot, to read logs.
	Container *tcpostgres.PostgresContainer
	// DSN is the connection string the pool was opened with.
	DSN string
	// Config is the pool configuration, after any [PoolConfig] ran.
	Config *pgxpool.Config

	pool *pgxpool.Pool
}

// Pool returns the connected pool.
//
// It is an [github.com/AlexAli29/orm.Executor], so a generated database handle
// binds to it directly.
func (i *Instance) Pool() *pgxpool.Pool { return i.pool }

// Run starts a container, waits for it, opens a pool, and registers the teardown
// for both.
//
// The default is the newest supported PostgreSQL. Every failure fails the test
// with the underlying cause attached: "starting the container" with Docker's own
// error is what a caller needs, and a generic "database setup failed" is what
// they do not.
func Run(t testing.TB, opts ...Option) *Instance {
	t.Helper()

	cfg := config{
		image:        ImagePostgres17,
		database:     "orm_test",
		username:     "orm",
		password:     "orm",
		startTimeout: 2 * time.Minute,
	}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), cfg.startTimeout)
	defer cancel()

	customizers := []tc.ContainerCustomizer{
		tcpostgres.WithDatabase(cfg.database),
		tcpostgres.WithUsername(cfg.username),
		tcpostgres.WithPassword(cfg.password),
		// The module's own readiness strategy: it waits for the log line and
		// then for a successful connection, rather than sleeping for a guess.
		tcpostgres.BasicWaitStrategies(),
	}
	if len(cfg.initScripts) > 0 {
		customizers = append(customizers, tcpostgres.WithInitScripts(cfg.initScripts...))
	}

	ctr, err := tcpostgres.Run(ctx, cfg.image, customizers...)
	// Registered before the error is checked, because a container that failed
	// to become ready may still exist and still need terminating.
	tc.CleanupContainer(t, ctr)
	if err != nil {
		t.Fatalf("ormtest/postgres: starting %s: %v", cfg.image, err)
		return nil
	}

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("ormtest/postgres: reading the connection string: %v", err)
		return nil
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("ormtest/postgres: parsing %s: %v", dsn, err)
		return nil
	}
	if cfg.poolConfig != nil {
		cfg.poolConfig(poolCfg)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("ormtest/postgres: opening a pool: %v", err)
		return nil
	}
	t.Cleanup(pool.Close)

	inst := &Instance{Container: ctr, DSN: dsn, Config: poolCfg, pool: pool}

	for _, stmt := range cfg.initSQL {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("ormtest/postgres: running an init statement: %v\nstatement: %s", err, truncate(stmt))
			return nil
		}
	}
	return inst
}

func truncate(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// Snapshot records the database's current state so that [Instance.Restore] can
// return to it.
//
// It is the Testcontainers module's own snapshot, exposed rather than
// reimplemented, and it is an advanced convenience: a suite that needs a clean
// database per test is usually better served by a transaction that rolls back,
// which costs nothing and needs no snapshot at all. This is for the cases where
// the work under test commits.
func (i *Instance) Snapshot(t testing.TB) {
	t.Helper()
	if err := i.Container.Snapshot(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("ormtest/postgres: taking a snapshot: %v", err)
	}
}

// Restore returns the database to the last [Instance.Snapshot].
//
// Restoring drops and recreates the database, so every connection the pool holds
// is closed first. That is why the pool is reset here rather than left to the
// caller.
func (i *Instance) Restore(t testing.TB) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	i.pool.Reset()
	if err := i.Container.Restore(ctx); err != nil {
		t.Fatalf("ormtest/postgres: restoring the snapshot: %v", err)
	}
}

// Exec runs a statement on the instance, failing the test if it does not apply.
func (i *Instance) Exec(t testing.TB, sql string, args ...any) {
	t.Helper()
	if _, err := i.pool.Exec(context.WithoutCancel(t.Context()), sql, args...); err != nil {
		t.Fatalf("ormtest/postgres: %v\nstatement: %s", err, truncate(sql))
	}
}
