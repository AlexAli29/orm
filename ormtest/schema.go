package ormtest

import (
	"bytes"
	"context"
	"fmt"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/jackc/pgx/v5"
)

// Schema helpers.
//
// Both of these run the production path and nothing else. Migrate runs the
// migration engine the CLI runs; RequireSchemaClean runs the reconciliation the
// check command runs, with the same configuration file and the same
// diagnostics. A helper that reimplemented either would be able to pass while
// the real thing failed, which is the one thing a test helper must never do.

// Migrate applies every pending migration from dir and fails the test if any of
// them does not apply.
//
// It is the engine the CLI uses, not a reimplementation: the same ordering, the
// same advisory lock, the same history table, the same atomicity decisions. A
// migration that applies here applies in production for the same reasons.
//
// The error is reported with its cause intact. "database setup failed" with the
// reason swallowed is the least useful thing a test helper can print, and it is
// what a caller most needs when a migration is the thing that broke.
func Migrate(t TB, conn *pgx.Conn, dir string) {
	t.Helper()
	if conn == nil {
		t.Fatalf("ormtest.Migrate: no connection")
		return
	}

	set, err := migrate.NewStore(dir).Load()
	if err != nil {
		t.Fatalf("ormtest.Migrate: loading migrations from %s: %v", dir, err)
		return
	}
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("ormtest.Migrate: applying migrations from %s: %v", dir, err)
	}
}

// SchemaError is what [CheckSchema] returns when the schema and the Go types
// disagree.
//
// It carries the whole report rather than a message, so a caller can decide
// what to do with it — count the findings, filter by code, render it another
// way — and the rendered text is there for the common case of printing it.
type SchemaError struct {
	// Report is the reconciliation's findings.
	Report *diag.Report
	// Rendered is the report as the check command would print it.
	Rendered string
}

// Error summarises how many findings the check produced. The findings
// themselves are in Report, and Rendered holds the text a person should read.
func (e *SchemaError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return "ormtest: the schema and the Go types disagree:\n" + e.Rendered
}

// CheckSchema reconciles the Go types against the live database and returns a
// [SchemaError] when they disagree.
//
// It runs the same path `orm check` runs, from the same configuration file, so
// a test that passes here is a project whose check command passes. It reads the
// database and changes nothing.
func CheckSchema(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("ormtest: loading %s: %w", configPath, err)
	}
	result, err := gen.Check(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ormtest: checking the schema: %w", err)
	}
	// The threshold is the check command's: a warning is worth reading and is
	// not drift, and failing on one would make the assertion unusable in a
	// project that has accepted a warning deliberately.
	if result.Report == nil || !result.Report.Failed(diag.SeverityError) {
		return nil
	}
	var b bytes.Buffer
	if err := diag.RenderText(&b, result.Report); err != nil {
		return fmt.Errorf("ormtest: rendering the findings: %w", err)
	}
	return &SchemaError{Report: result.Report, Rendered: b.String()}
}

// RequireSchemaClean fails the test unless the Go types and the live database
// agree.
//
// This is the assertion a project wants in CI: it turns schema drift into a
// failing test at the moment it appears, rather than into a runtime error the
// first time somebody reads the column that moved. The failure prints the same
// diagnostics the check command prints, so the fix is the same fix.
//
// It reads the database. It does not migrate, and it does not change anything.
func RequireSchemaClean(t TB, configPath string) {
	t.Helper()
	if err := CheckSchema(t.Context(), configPath); err != nil {
		t.Fatalf("%v", err)
	}
}
