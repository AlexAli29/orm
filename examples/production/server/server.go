// Package server is the application's lifecycle: what is built at startup, in
// what order, and what is torn down in what order at shutdown.
//
// It exists as a package rather than as the body of main so that the ordering
// can be tested. The claim a production example makes about graceful shutdown —
// that the database outlives the requests using it — is not a claim a reader
// can check by looking at a main function; it is checked by starting the thing,
// putting a request in flight, stopping it, and seeing what the request got.
//
// Nothing here is ORM-specific except two lines: the tracer attached to the
// executor, and the health checks. The rest is what a Go service looks like.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"example.com/production/observability"
	"example.com/production/service"
	"example.com/production/transport/httpapi"
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/ormhealth"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/trace"
)

// Config is the whole of this program's configuration.
//
// It is a struct resolved in one place, because configuration read where it is
// used is configuration nobody can enumerate. No field has a default that
// contains a credential.
type Config struct {
	// DSN is the database. There is no default: a service that invents one
	// connects to the wrong database quietly.
	DSN string
	// Addr is what the HTTP server listens on.
	Addr string
	// ShutdownTimeout bounds how long in-flight requests are given to finish.
	ShutdownTimeout time.Duration
	// LogSQL turns on statement logging. It is off by default because the
	// volume is a production decision, not a default.
	LogSQL bool
	// SlowThreshold is the duration above which a query is logged as slow.
	SlowThreshold time.Duration
	// Traces is the application's OpenTelemetry tracer, or nil for none. It is
	// passed in rather than fetched from the global provider: which provider is
	// installed is the program's decision, and a package that reaches for the
	// global one takes it away.
	Traces trace.Tracer
	// ConfigPath and MigrationsDir are what the deep health check reads to
	// compare the running database against what the project expects.
	ConfigPath    string
	MigrationsDir string
}

// withDefaults fills in what was not set, and fails on what cannot be guessed.
func (c Config) withDefaults() (Config, error) {
	if c.DSN == "" {
		return Config{}, errors.New("no DSN: refusing to guess which database this is")
	}
	if c.Addr == "" {
		c.Addr = ":8080"
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 20 * time.Second
	}
	if c.SlowThreshold == 0 {
		c.SlowThreshold = 200 * time.Millisecond
	}
	if c.ConfigPath == "" {
		c.ConfigPath = "orm.yaml"
	}
	if c.MigrationsDir == "" {
		c.MigrationsDir = "migrations"
	}
	return c, nil
}

// Server is a started application: a listener, an HTTP server and a pool.
type Server struct {
	cfg  Config
	log  *slog.Logger
	pool *pgxpool.Pool
	http *http.Server
	ln   net.Listener

	// serveErr carries the listener's own failure, which is a different event
	// from a shutdown and must not be mistaken for one.
	serveErr chan error
}

// Start opens the database, builds the application and begins listening.
//
// It returns once the listener is open, so a caller — a process or a test —
// knows the port is accepting before it does anything else. The base context is
// the application's: cancelling it is what [Server.Wait] treats as the signal
// to stop.
func Start(ctx context.Context, cfg Config, log *slog.Logger) (*Server, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}

	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("opening the pool: %w", err)
	}

	// Tracing is attached to the executor, so it travels into transactions and
	// generated code never mentions it. Bind values never reach an event; the
	// SQL keeps its placeholders. See the observability package for what the
	// two adapters are given and what they are not.
	ex := orm.Traced(pool, observability.New(log, observability.Config{
		LogSQL:        cfg.LogSQL,
		SlowThreshold: cfg.SlowThreshold,
		Traces:        cfg.Traces,
	}))

	api := httpapi.New(service.New(ex), log)
	health := httpapi.NewHealth(ex,
		ormhealth.WithSchemaCheck(cfg.ConfigPath),
		ormhealth.WithMigrationState(cfg.MigrationsDir),
	)

	mux := api.Routes()
	health.Routes(mux)

	// The listener is opened here rather than inside ListenAndServe so that a
	// port already in use is an error from Start, not a message on a channel
	// after the caller has been told everything is fine. It is also what lets a
	// test ask for :0 and find out which port it got.
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("listening on %s: %w", cfg.Addr, err)
	}

	srv := &Server{
		cfg:  cfg,
		log:  log,
		pool: pool,
		ln:   ln,
		http: &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			// BaseContext is not the request context; a handler still uses
			// r.Context(). This is what makes the server's own goroutines
			// cancellable.
			BaseContext: func(net.Listener) context.Context { return ctx },
		},
		serveErr: make(chan error, 1),
	}

	go func() {
		log.Info("listening", "addr", ln.Addr().String())
		err := srv.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		srv.serveErr <- err
	}()

	return srv, nil
}

// Addr is the address actually being listened on, which is not the configured
// one when the configured one was :0.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Pool is the database, exposed so that a test can look at what the application
// did. Nothing in the application reaches for it: everything below the transport
// takes an [orm.Executor].
func (s *Server) Pool() *pgxpool.Pool { return s.pool }

// Wait blocks until the context is cancelled or the listener fails, then shuts
// down and returns.
func (s *Server) Wait(ctx context.Context) error {
	select {
	case err := <-s.serveErr:
		// The listener stopped on its own, which is a failure rather than a
		// shutdown. The pool still has to be closed.
		s.pool.Close()
		return err
	case <-ctx.Done():
		s.log.Info("shutting down")
		return s.Shutdown()
	}
}

// Shutdown stops the server and then the database, in that order.
//
// The order is the whole point of this method.
//
//  1. Shutdown stops accepting new connections and waits for the handlers that
//     are already running, up to the configured timeout.
//  2. Only then is the pool closed.
//
// Reversing them would close the database while requests were still using it,
// which turns a clean shutdown into a burst of 500s from requests that were
// almost done. The deferred Close is what guarantees the order: a defer runs
// after the function's last statement, and the http shutdown is a statement.
//
// The shutdown context is deliberately not the application's: the application's
// has just been cancelled, and passing it here would ask the server to wait for
// in-flight requests with a deadline that has already passed — an "immediate
// graceful shutdown", which is a contradiction with the graceful part removed.
func (s *Server) Shutdown() error {
	defer s.pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()

	if err := s.http.Shutdown(ctx); err != nil {
		// The timeout expired with requests still running. Close is what
		// actually stops them; without it the listener stays open.
		_ = s.http.Close()
		return fmt.Errorf("shutting down: %w", err)
	}
	<-s.serveErr
	s.log.Info("stopped")
	return nil
}
