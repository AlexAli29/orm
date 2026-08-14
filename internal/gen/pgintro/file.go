package pgintro

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/jackc/pgx/v5"
)

// FromFile introspects a schema that lives in a DDL file.
//
// The DDL is not parsed. A throwaway database is created through adminDSN, the
// file is applied to it by PostgreSQL, the result is introspected and the
// database is dropped again. The temporary database is dropped even when the
// DDL fails, so a syntax error does not leave litter behind.
func FromFile(ctx context.Context, adminDSN, schemaFile string, searchPath []string) (schema *model.Schema, err error) {
	ddl, err := os.ReadFile(schemaFile)
	if err != nil {
		return nil, fmt.Errorf("reading schema file: %w", err)
	}

	adminCfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parsing schema.admin_dsn: %w", err)
	}

	dbName, err := tempDBName()
	if err != nil {
		return nil, err
	}

	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting with schema.admin_dsn: %w", err)
	}
	defer admin.Close(context.WithoutCancel(ctx))

	quoted := pgx.Identifier{dbName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		return nil, fmt.Errorf("creating temporary database %s: %w", dbName, err)
	}
	defer func() {
		// Use a context detached from the caller's so that a cancellation
		// during introspection still removes the database.
		err = errors.Join(err, dropDatabase(context.WithoutCancel(ctx), admin, dbName))
	}()

	tempCfg := adminCfg.Copy()
	tempCfg.Database = dbName
	conn, err := pgx.ConnectConfig(ctx, tempCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to temporary database %s: %w", dbName, err)
	}
	defer conn.Close(context.WithoutCancel(ctx))

	// The simple query protocol is what allows a whole DDL file, with many
	// statements, to be sent as one request. It carries no parameters, so
	// there is nothing here to interpolate.
	if _, err := conn.PgConn().Exec(ctx, string(ddl)).ReadAll(); err != nil {
		return nil, fmt.Errorf("applying %s: %w", schemaFile, err)
	}

	schema, err = Introspect(ctx, conn, searchPath)
	if err != nil {
		return nil, err
	}
	return schema, nil
}

// dropDatabase disconnects any leftover backends and drops the database.
func dropDatabase(ctx context.Context, admin *pgx.Conn, name string) error {
	const terminate = `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`
	if _, err := admin.Exec(ctx, terminate, name); err != nil {
		return fmt.Errorf("disconnecting clients of temporary database %s: %w", name, err)
	}
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()); err != nil {
		return fmt.Errorf("dropping temporary database %s: %w", name, err)
	}
	return nil
}

// tempDBName returns a name made only of characters that need no quoting, so
// that the identifier can never carry anything but itself.
func tempDBName() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating a temporary database name: %w", err)
	}
	return "orm_check_" + hex.EncodeToString(b[:]), nil
}
