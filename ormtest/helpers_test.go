package ormtest_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The fixtures these tests run against are the generated demo package's, which
// is a real generated project: its schema, its orm.yaml and its entities are
// the same ones the rest of the suite uses.

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolving the repository root: %v", err)
	}
	return dir
}

func schema(t *testing.T) string {
	t.Helper()
	ddl, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "gendemo", "schema.sql"))
	if err != nil {
		t.Fatalf("reading the fixture schema: %v", err)
	}
	return string(ddl)
}

func gendemoConfig(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "internal", "gendemo", "orm.yaml")
}

func fmtSprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
