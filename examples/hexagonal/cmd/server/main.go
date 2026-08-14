// Command server wires the hexagon and runs it.
//
// This is the only file that knows every side at once, and that is what a
// composition root is for: the core is constructed from ports, the adapters are
// constructed from the outside world, and the two are joined here. Nothing else
// in the module imports both.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/hexagonal/adapter/httpin"
	"example.com/hexagonal/adapter/ormstore"
	"example.com/hexagonal/core/app"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	if err := run(); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	dsn := os.Getenv("HEXAGONAL_EXAMPLE_DSN")
	if dsn == "" {
		return errors.New("HEXAGONAL_EXAMPLE_DSN is not set")
	}
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	// The composition root, and the whole of it.
	var (
		ex    orm.Executor = pool
		store              = ormstore.NewStore()
		work               = ormstore.NewWork(ex)
	)
	svc, err := app.New(store, store, work, systemClock{})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           httpin.New(svc, log).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	// The HTTP server drains before the deferred pool.Close runs, so a request
	// that was still being served has a database until it is finished.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// systemClock is the real clock, and the only implementation of the port that
// reads a wall clock. A test supplies its own.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
