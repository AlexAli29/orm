// Package testdb builds throwaway PostgreSQL databases for tests.
//
// Tests that need a server read the administrative DSN from ORM_TEST_ADMIN_DSN
// and skip when it is unset, so that the suite is green on a machine without
// PostgreSQL and meaningful on one with it:
//
//	ORM_TEST_ADMIN_DSN=postgres://orm:orm@localhost:55432/orm go test ./...
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// EnvAdminDSN names the environment variable holding a DSN with permission to
// create and drop databases.
const EnvAdminDSN = "ORM_TEST_ADMIN_DSN"

// AdminDSN returns the administrative DSN, skipping the test when it is unset.
func AdminDSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvAdminDSN)
	if dsn == "" {
		t.Skipf("%s is not set; skipping the tests that need a PostgreSQL server", EnvAdminDSN)
	}
	return dsn
}

// Create builds a database, applies ddl to it and returns its DSN. The database
// is dropped when the test finishes.
func Create(t testing.TB, ddl string) string {
	t.Helper()
	ctx := t.Context()
	adminDSN := AdminDSN(t)

	adminCfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatalf("parsing %s: %v", EnvAdminDSN, err)
	}
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatalf("connecting with %s: %v", EnvAdminDSN, err)
	}
	t.Cleanup(func() { _ = admin.Close(context.WithoutCancel(ctx)) })

	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("generating a database name: %v", err)
	}
	name := "orm_test_" + hex.EncodeToString(raw[:])
	quoted := pgx.Identifier{name}.Sanitize()

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		t.Fatalf("creating database %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx := context.WithoutCancel(ctx)
		const terminate = `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`
		if _, err := admin.Exec(ctx, terminate, name); err != nil {
			t.Errorf("disconnecting clients of %s: %v", name, err)
		}
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoted); err != nil {
			t.Errorf("dropping database %s: %v", name, err)
		}
	})

	cfg := adminCfg.Copy()
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connecting to %s: %v", name, err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if ddl != "" {
		if _, err := conn.PgConn().Exec(ctx, ddl).ReadAll(); err != nil {
			t.Fatalf("applying DDL to %s: %v", name, err)
		}
	}
	return DSN(adminCfg, name)
}

// DSN renders a connection string for the database name using cfg's connection
// parameters.
//
// It builds a keyword/value string from the parsed fields rather than calling
// ConnConfig.ConnString, which returns the string the config was parsed from and
// so would ignore the database name entirely.
func DSN(cfg *pgx.ConnConfig, name string) string {
	sslmode := "prefer"
	if cfg.TLSConfig == nil {
		sslmode = "disable"
	}
	kv := [][2]string{
		{"host", cfg.Host},
		{"port", strconv.FormatUint(uint64(cfg.Port), 10)},
		{"user", cfg.User},
		{"password", cfg.Password},
		{"dbname", name},
		{"sslmode", sslmode},
	}
	parts := make([]string, 0, len(kv))
	for _, p := range kv {
		if p[1] == "" {
			continue
		}
		parts = append(parts, p[0]+"="+quoteDSNValue(p[1]))
	}
	return strings.Join(parts, " ")
}

// quoteDSNValue single-quotes a libpq connection parameter value, escaping the
// two characters that are special inside one.
func quoteDSNValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "'" + r.Replace(v) + "'"
}

// Connect opens a connection to dsn and closes it when the test finishes.
func Connect(t testing.TB, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}
