// Command server is the canonical production example.
//
// It is an ordinary Go program. There is no framework, no lifecycle manager and
// no dependency-injection container: configuration is read from the
// environment, the pool is built once and passed down, and shutdown is
// signal.NotifyContext with a Shutdown call. What the ORM contributes is a
// tracer and two health checks; everything else is what a Go service looks like.
//
// Run it with:
//
//	PRODUCTION_EXAMPLE_DSN=postgres://... go run ./cmd/server
//
// main is deliberately thin. Everything it would be worth testing — what is
// built, in what order, and what is torn down in what order — lives in
// [example.com/production/server], where a test can start
// it and stop it.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/production/server"
)

func main() {
	if err := run(); err != nil {
		slog.Error("exiting", "error", err)
		os.Exit(1)
	}
}

// run is the program, separated from main so that everything in it can return
// an error instead of calling os.Exit.
func run() error {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// The signal context is established before anything is opened, so a signal
	// arriving during startup is not lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := server.Start(ctx, cfg, log)
	if err != nil {
		return err
	}
	return srv.Wait(ctx)
}

// loadConfig reads the environment in one place, because configuration read
// where it is used is configuration nobody can enumerate.
func loadConfig() (server.Config, error) {
	dsn := os.Getenv("PRODUCTION_EXAMPLE_DSN")
	if dsn == "" {
		return server.Config{}, errors.New("PRODUCTION_EXAMPLE_DSN is not set")
	}
	return server.Config{
		DSN:             dsn,
		Addr:            envOr("HTTP_ADDR", ":8080"),
		ShutdownTimeout: 20 * time.Second,
		LogSQL:          os.Getenv("LOG_SQL") == "1",
	}, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
