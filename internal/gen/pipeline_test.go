package gen_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/testdb"
)

// project writes a self-contained project — a configuration, a schema and an
// entity package — into a temporary directory and returns the configuration
// path. An empty ddl means the database is created but left bare.
func project(t *testing.T, cfgYAML, ddl, entities string) string {
	t.Helper()
	dir := t.TempDir()

	if ddl != "" || entities != "" {
		t.Setenv("ORM_PIPELINE_DSN", testdb.Create(t, ddl))
	}
	if entities != "" {
		if err := os.MkdirAll(filepath.Join(dir, "domain"), 0o755); err != nil {
			t.Fatalf("creating the entity package: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "domain", "entities.go"), []byte(entities), 0o600); err != nil {
			t.Fatalf("writing the entities: %v", err)
		}
	}
	path := filepath.Join(dir, "orm.yaml")
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	return path
}

const dsnConfig = `version: 1
schema:
  dsn: ${ORM_PIPELINE_DSN}
  search_path:
    - public
packages:
  - path: ./domain
    output: same
`

const usersDDL = `CREATE TABLE users (id bigint PRIMARY KEY, name text NOT NULL);`

// The entity package lives in a temporary directory outside this module, so it
// needs its own module file to resolve the runtime import. goscan runs the go
// tool, which reads it.
func withModule(t *testing.T, cfgPath string) {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	dir := filepath.Dir(cfgPath)
	mod := "module example.com/pipelinetest\n\ngo 1.24\n\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => " + root + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o600); err != nil {
		t.Fatalf("writing go.mod: %v", err)
	}
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.sum"), sum, 0o600); err != nil {
		t.Fatalf("writing go.sum: %v", err)
	}
}

func load(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

func TestCheck_happyPath(t *testing.T) {
	testdb.AdminDSN(t)
	path := project(t, dsnConfig, usersDDL, `package domain

//orm:table users
type User struct {
	ID   int64
	Name string
}
`)
	withModule(t, path)

	result, err := gen.Check(t.Context(), load(t, path))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Report.Len() != 0 {
		t.Errorf("report is not clean: %+v", result.Report.Findings())
	}
	if len(result.Entities) != 1 || result.Entities[0].Name != "User" {
		t.Errorf("entities = %+v, want one User", result.Entities)
	}
	if len(result.Mapping.Entities) != 1 {
		t.Errorf("mapped %d entities, want 1", len(result.Mapping.Entities))
	}
	if result.Schema == nil || len(result.Schema.Tables) != 1 {
		t.Errorf("schema = %+v, want one table", result.Schema)
	}
}

func TestCheck_noEntitiesIsAFailureNotACleanReport(t *testing.T) {
	testdb.AdminDSN(t)
	// The package compiles and holds a struct; nothing marks it as an entity.
	// A packages.path pointing at the wrong directory looks exactly like this,
	// and reporting it as clean would claim a proof the tool never attempted.
	path := project(t, dsnConfig, usersDDL, `package domain

// User carries no //orm:table directive.
type User struct {
	ID int64
}
`)
	withModule(t, path)

	_, err := gen.Check(t.Context(), load(t, path))
	if err == nil {
		t.Fatal("Check succeeded with no entities; a check that examined nothing must not report agreement")
	}
	if !strings.Contains(err.Error(), "no entities found") {
		t.Errorf("error = %v, want it to say no entities were found", err)
	}
	if !strings.Contains(err.Error(), "./domain") {
		t.Errorf("error = %v, want it to name the package it searched", err)
	}
	if !strings.Contains(err.Error(), "packages.path") {
		t.Errorf("error = %v, want it to point at the likely cause", err)
	}
}

func TestCheck_unreachableDatabase(t *testing.T) {
	path := project(t, `version: 1
schema:
  dsn: postgres://nobody@127.0.0.1:1/nothing?connect_timeout=2&sslmode=disable
packages:
  - path: ./domain
`, "", "package domain\n")
	withModule(t, path)

	_, err := gen.Check(t.Context(), load(t, path))
	if err == nil {
		t.Fatal("Check succeeded against an unreachable database")
	}
	if !strings.Contains(err.Error(), "connecting to PostgreSQL") {
		t.Errorf("error = %v, want it to name the failing step", err)
	}
}

func TestCheck_missingSchemaFile(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_PIPELINE_ADMIN_DSN", testdb.AdminDSN(t))
	path := project(t, `version: 1
schema:
  file: db/absent.sql
  admin_dsn: ${ORM_PIPELINE_ADMIN_DSN}
packages:
  - path: ./domain
`, "", "package domain\n")
	withModule(t, path)

	_, err := gen.Check(t.Context(), load(t, path))
	if err == nil {
		t.Fatal("Check succeeded with a schema file that does not exist")
	}
	if !strings.Contains(err.Error(), "db/absent.sql") {
		t.Errorf("error = %v, want it to name the missing file", err)
	}
}

func TestCheck_schemaIsLoadedBeforeThePackagesAreScanned(t *testing.T) {
	// The schema is the more expensive and more likely failure, and a database
	// that is unreachable makes the scan pointless, so it must be reported
	// first rather than after a package error that is only a symptom.
	path := project(t, `version: 1
schema:
  dsn: postgres://nobody@127.0.0.1:1/nothing?connect_timeout=2&sslmode=disable
packages:
  - path: ./no_such_directory
`, "", "package domain\n")
	withModule(t, path)

	_, err := gen.Check(t.Context(), load(t, path))
	if err == nil {
		t.Fatal("Check succeeded with both a bad DSN and a bad package path")
	}
	if !strings.Contains(err.Error(), "connecting to PostgreSQL") {
		t.Errorf("error = %v, want the database failure rather than the package one", err)
	}
}

func TestCheck_reportsFindingsWithoutFailing(t *testing.T) {
	testdb.AdminDSN(t)
	// Findings are the tool working, not the tool failing: Check returns them
	// with a nil error, and only the caller's threshold turns them into an
	// exit code.
	path := project(t, dsnConfig, usersDDL, `package domain

//orm:table users
type User struct {
	ID    int64
	Name  string
	Extra string
}
`)
	withModule(t, path)

	result, err := gen.Check(t.Context(), load(t, path))
	if err != nil {
		t.Fatalf("Check returned an error for a report full of findings: %v", err)
	}
	if result.Report.Len() == 0 {
		t.Fatal("expected findings for the unmatched field")
	}
	if got := string(result.Report.Findings()[0].Code); got != "E001" {
		t.Errorf("finding = %s, want E001", got)
	}
}
