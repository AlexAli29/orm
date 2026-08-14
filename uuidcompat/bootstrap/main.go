// Command bootstrap prepares a database for the UUID qualification.
//
//	go run ./bootstrap <admin-dsn> <database>
//
// It drops and recreates the database, then creates the two schemas the
// cross-schema tenants live in and the domain over uuid. Migrations create
// neither schemas nor domains — those are frozen boundaries of the tool — so
// somebody has to, and it is this rather than psql so that the qualification
// runs the same way on a developer machine as on CI without a client package
// being installed.
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: bootstrap <admin-dsn> <database>")
		os.Exit(2)
	}
	admin, dbname := os.Args[1], os.Args[2]
	if !regexp.MustCompile(`^[a-z_][a-z0-9_]*$`).MatchString(dbname) {
		fmt.Fprintf(os.Stderr, "bootstrap: %q is not a plain database name\n", dbname)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := run(ctx, admin, dbname); err != nil {
		fmt.Fprintf(os.Stderr, "bootstrap: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, admin, dbname string) error {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return fmt.Errorf("connecting as admin: %w", err)
	}
	defer conn.Close(context.Background())

	// FORCE is PostgreSQL 13 and later, which every supported major is.
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+dbname+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("dropping %s: %w", dbname, err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE `+dbname); err != nil {
		return fmt.Errorf("creating %s: %w", dbname, err)
	}

	target, err := pgx.Connect(ctx, swapDatabase(admin, dbname))
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", dbname, err)
	}
	defer target.Close(context.Background())

	for _, s := range []string{"schema_a", "schema_b"} {
		if _, err := target.Exec(ctx, `CREATE SCHEMA `+s); err != nil {
			return fmt.Errorf("creating schema %s: %w", s, err)
		}
	}

	// A domain over uuid, for the same reason as the schemas: migrations do not
	// create domains either, and a column of one still has to reconcile.
	if _, err := target.Exec(ctx, `CREATE DOMAIN tenant_uuid AS uuid`); err != nil {
		return fmt.Errorf("creating the domain: %w", err)
	}
	return nil
}

// swapDatabase replaces the database in a DSN, keeping everything else.
var dbPath = regexp.MustCompile(`(://[^/?]+)/[^?]*`)

func swapDatabase(dsn, dbname string) string {
	return dbPath.ReplaceAllString(dsn, "${1}/"+dbname)
}
