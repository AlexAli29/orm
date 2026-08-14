package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// fixture builds a throwaway database from a fixture's schema and returns the
// path to a configuration file pointing a check at it.
func fixture(t *testing.T, name string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", name))
	if err != nil {
		t.Fatalf("resolving the fixture: %v", err)
	}
	ddl, err := os.ReadFile(filepath.Join(src, "schema.sql"))
	if err != nil {
		t.Fatalf("reading the fixture schema: %v", err)
	}
	t.Setenv("ORM_FIXTURE_DSN", testdb.Create(t, string(ddl)))
	return filepath.Join(src, "orm.yaml")
}

func TestRun_checkClean(t *testing.T) {
	testdb.AdminDSN(t)
	cfg := fixture(t, "01_clean")

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"check", "--config", cfg}, &stdout, &stderr)
	if code != exitClean {
		t.Errorf("exit code = %d, want %d\nstderr: %s", code, exitClean, stderr.String())
	}
	if !strings.Contains(stdout.String(), "reconciliation clean") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRun_checkFindings(t *testing.T) {
	testdb.AdminDSN(t)
	cfg := fixture(t, "02_nullability")

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"check", "--config", cfg}, &stdout, &stderr)
	if code != exitFindings {
		t.Errorf("exit code = %d, want %d\nstderr: %s", code, exitFindings, stderr.String())
	}
	if !strings.Contains(stdout.String(), "E004") {
		t.Errorf("stdout does not report the findings:\n%s", stdout.String())
	}
}

func TestRun_failOnThreshold(t *testing.T) {
	testdb.AdminDSN(t)

	// 11_generated carries one error and three warnings, so it fails either way.
	cfg := fixture(t, "11_generated")
	var out bytes.Buffer
	for _, threshold := range []string{"error", "warning"} {
		out.Reset()
		if got := run(t.Context(), []string{"check", "--config", cfg, "--fail-on", threshold}, &out, &out); got != exitFindings {
			t.Errorf("--fail-on=%s exit code = %d, want %d", threshold, got, exitFindings)
		}
	}

	// A clean report passes at both.
	cfg = fixture(t, "01_clean")
	for _, threshold := range []string{"error", "warning"} {
		out.Reset()
		if got := run(t.Context(), []string{"check", "--config", cfg, "--fail-on", threshold}, &out, &out); got != exitClean {
			t.Errorf("a clean report failed at --fail-on=%s: exit %d", threshold, got)
		}
	}

	// A warning-only report is the case that makes --fail-on a threshold
	// rather than a report-is-non-empty check: it passes at error and fails at
	// warning.
	cfg = fixture(t, "20_warnings_only")
	out.Reset()
	if got := run(t.Context(), []string{"check", "--config", cfg, "--fail-on", "error"}, &out, &out); got != exitClean {
		t.Errorf("a warning-only report failed at --fail-on=error: exit %d\n%s", got, out.String())
	}
	out.Reset()
	if got := run(t.Context(), []string{"check", "--config", cfg, "--fail-on", "warning"}, &out, &out); got != exitFindings {
		t.Errorf("a warning-only report passed at --fail-on=warning: exit %d", got)
	}
}

func TestRun_formats(t *testing.T) {
	testdb.AdminDSN(t)
	cfg := fixture(t, "02_nullability")

	t.Run("json", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		run(t.Context(), []string{"check", "--config", cfg, "--format", "json"}, &stdout, &stderr)
		var doc struct {
			Version  int              `json:"version"`
			Findings []map[string]any `json:"findings"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
			t.Fatalf("the JSON output does not parse: %v\n%s", err, stdout.String())
		}
		if doc.Version != 1 || len(doc.Findings) != 3 {
			t.Errorf("version = %d with %d findings, want 1 and 3", doc.Version, len(doc.Findings))
		}
	})

	t.Run("github", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		run(t.Context(), []string{"check", "--config", cfg, "--format", "github"}, &stdout, &stderr)
		if !strings.HasPrefix(stdout.String(), "::error file=") {
			t.Errorf("github output = %q", stdout.String())
		}
	})
}

func TestRun_deterministicAcrossRuns(t *testing.T) {
	testdb.AdminDSN(t)
	cfg := fixture(t, "17_unmapped_keys")

	var first string
	for i := range 3 {
		var stdout, stderr bytes.Buffer
		run(t.Context(), []string{"check", "--config", cfg, "--format", "json"}, &stdout, &stderr)
		if i == 0 {
			first = stdout.String()
			continue
		}
		if stdout.String() != first {
			t.Fatalf("run %d produced different bytes than run 1", i+1)
		}
	}
}

func TestRun_toolFailures(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no arguments", args: nil, want: "usage:"},
		{name: "unknown command", args: []string{"frobnicate"}, want: "unknown command"},
		{name: "missing config", args: []string{"check", "--config", "no-such-file.yaml"}, want: "reading configuration"},
		{name: "bad format", args: []string{"check", "--format", "yaml"}, want: "invalid format"},
		{name: "bad threshold", args: []string{"check", "--fail-on", "info"}, want: "invalid severity"},
		{name: "generate with a missing config", args: []string{"generate", "--config", "no-such-file.yaml"}, want: "reading configuration"},
		{name: "generate with a bad threshold", args: []string{"generate", "--fail-on", "info"}, want: "invalid severity"},
		{name: "explain with no entity", args: []string{"explain"}, want: "name an entity"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(t.Context(), tt.args, &stdout, &stderr); got != exitFailure {
				t.Errorf("exit code = %d, want %d", got, exitFailure)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRun_version(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := run(t.Context(), []string{"version"}, &stdout, &stderr); got != exitClean {
		t.Errorf("exit code = %d, want %d", got, exitClean)
	}
	if !strings.HasPrefix(stdout.String(), "orm ") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestRun_init(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orm.yaml")

	var stdout, stderr bytes.Buffer
	if got := run(t.Context(), []string{"init", "--config", path}, &stdout, &stderr); got != exitClean {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", got, exitClean, stderr.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("orm init wrote nothing: %v", err)
	}

	// A second run must not silently overwrite the author's configuration.
	stderr.Reset()
	if got := run(t.Context(), []string{"init", "--config", path}, &stdout, &stderr); got != exitFailure {
		t.Errorf("overwriting exit code = %d, want %d", got, exitFailure)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if got := run(t.Context(), []string{"init", "--config", path, "--force"}, &stdout, &stderr); got != exitClean {
		t.Errorf("--force exit code = %d, want %d", got, exitClean)
	}
}

// generateProject copies a package of entities, a schema and a configuration
// into a temporary module, so that orm generate can be run over it end to end
// without touching the repository.
func generateProject(t *testing.T, entities string) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	dir := t.TempDir()

	ownMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("reading go.mod: %v", err)
	}
	mod := strings.Replace(string(ownMod), "module github.com/AlexAli29/orm", "module ormgentest", 1) +
		"\nrequire github.com/AlexAli29/orm v0.0.0\n\nreplace github.com/AlexAli29/orm => " + root + "\n"
	writeFile(t, filepath.Join(dir, "go.mod"), mod)

	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatalf("reading go.sum: %v", err)
	}
	writeFile(t, filepath.Join(dir, "go.sum"), string(sum))

	if err := os.MkdirAll(filepath.Join(dir, "domain"), 0o755); err != nil {
		t.Fatalf("creating the entity package: %v", err)
	}
	writeFile(t, filepath.Join(dir, "domain", "entities.go"), entities)
	writeFile(t, filepath.Join(dir, "orm.yaml"), `version: 1
schema:
  dsn: ${ORM_CLI_DSN}
  search_path:
    - public
packages:
  - path: ./domain
    output: same
`)
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

const cliDDL = `CREATE TABLE users (
    id    bigint PRIMARY KEY,
    email text NOT NULL,
    age   integer NOT NULL
);`

const cliEntities = `package domain

//orm:table users
type User struct {
	ID    int64
	Email string
	Age   int32
}
`

func TestRun_generate(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml")}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d\nstderr: %s", code, exitClean, stderr.String())
	}
	for _, name := range []string{"orm_tables.gen.go", "orm_meta.gen.go", "orm_db.gen.go"} {
		if !strings.Contains(stdout.String(), name) {
			t.Errorf("stdout does not mention %s:\n%s", name, stdout.String())
		}
		path := filepath.Join(dir, "domain", name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if !bytes.HasPrefix(content, []byte("// Code generated by orm. DO NOT EDIT.")) {
			t.Errorf("%s does not carry the generated-code header", name)
		}
	}

	// The generated package must compile. That is the whole promise: output
	// that needs a fix before it builds is output nobody can use.
	cmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated package does not compile: %v\n%s", err, out)
	}
}

func TestRun_generateIsIdempotent(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	var out bytes.Buffer
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("first run: exit %d\n%s", code, out.String())
	}
	first := readGenerated(t, dir)

	// The second run sees its own output already in the package. Treating
	// those identifiers as collisions would make generation succeed exactly
	// once.
	out.Reset()
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("second run: exit %d\n%s", code, out.String())
	}
	second := readGenerated(t, dir)

	for name, content := range first {
		if second[name] != content {
			t.Errorf("regenerating changed %s", name)
		}
	}
}

func readGenerated(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, name := range []string{"orm_tables.gen.go", "orm_meta.gen.go", "orm_db.gen.go"} {
		content, err := os.ReadFile(filepath.Join(dir, "domain", name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		out[name] = string(content)
	}
	return out
}

func TestRun_generateRefusesAnUnprovenMapping(t *testing.T) {
	testdb.AdminDSN(t)
	// The schema has no age column, so reconciliation reports E001.
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, `CREATE TABLE users (id bigint PRIMARY KEY, email text NOT NULL);`))
	dir := generateProject(t, cliEntities)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml")}, &stdout, &stderr)
	if code != exitFindings {
		t.Errorf("exit code = %d, want %d", code, exitFindings)
	}
	if !strings.Contains(stderr.String(), "do not agree") {
		t.Errorf("stderr = %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "E001") {
		t.Errorf("stderr does not show what stopped it:\n%s", stderr.String())
	}
	// Nothing may be written from a mapping that does not hold.
	for _, name := range []string{"orm_tables.gen.go", "orm_meta.gen.go", "orm_db.gen.go"} {
		if _, err := os.Stat(filepath.Join(dir, "domain", name)); err == nil {
			t.Errorf("%s was written despite the refusal", name)
		}
	}
}

func TestRun_generateDryRunWritesNothing(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml"), "--dry-run"}, &stdout, &stderr)
	if code != exitClean {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "would write") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "domain", "orm_tables.gen.go")); err == nil {
		t.Error("--dry-run wrote a file")
	}
}

func TestRun_generateRefusesAnIdentifierTheAuthorDeclared(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	// The package already declares Users, which is the name the descriptor
	// would take.
	dir := generateProject(t, cliEntities+"\nvar Users = 1\n")

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml")}, &stdout, &stderr)
	if code != exitFailure {
		t.Errorf("exit code = %d, want %d", code, exitFailure)
	}
	if !strings.Contains(stderr.String(), "would redeclare Users") {
		t.Errorf("stderr = %q, want it to name the collision", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "domain", "orm_tables.gen.go")); err == nil {
		t.Error("a file was written despite the collision")
	}
}

// warnDDL leaves a nullable column the entity does not map, which is W003 and
// nothing worse.
const warnDDL = `CREATE TABLE users (
    id       bigint PRIMARY KEY,
    email    text NOT NULL,
    age      integer NOT NULL,
    nickname text
);`

func TestRun_generateThreshold(t *testing.T) {
	testdb.AdminDSN(t)

	t.Run("warnings do not block generation by default", func(t *testing.T) {
		// This is the case that matters. An unmapped nullable column is
		// ordinary — an entity is allowed to ignore parts of its table — so a
		// tool that refused to generate over W003 would be unusable on most
		// real schemas.
		t.Setenv("ORM_CLI_DSN", testdb.Create(t, warnDDL))
		dir := generateProject(t, cliEntities)

		var stdout, stderr bytes.Buffer
		if code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml")}, &stdout, &stderr); code != exitClean {
			t.Fatalf("exit code = %d, want %d\nstderr: %s", code, exitClean, stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "domain", "orm_tables.gen.go")); err != nil {
			t.Errorf("nothing was generated: %v", err)
		}
	})

	t.Run("warnings block generation at the warning threshold", func(t *testing.T) {
		t.Setenv("ORM_CLI_DSN", testdb.Create(t, warnDDL))
		dir := generateProject(t, cliEntities)

		var stdout, stderr bytes.Buffer
		code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml"), "--fail-on", "warning"}, &stdout, &stderr)
		if code != exitFindings {
			t.Fatalf("exit code = %d, want %d", code, exitFindings)
		}
		if !strings.Contains(stderr.String(), "W003") {
			t.Errorf("stderr does not show what blocked it:\n%s", stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dir, "domain", "orm_tables.gen.go")); err == nil {
			t.Error("a file was written despite the refusal")
		}
	})
}

func TestRun_generateFollowsTheSchema(t *testing.T) {
	testdb.AdminDSN(t)
	// Regenerating after the schema moves is the whole working loop: migrate,
	// check, generate. The output has to follow.
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	var out bytes.Buffer
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("first run: exit %d\n%s", code, out.String())
	}
	before := readGenerated(t, dir)
	if strings.Contains(before["orm_tables.gen.go"], "Nickname") {
		t.Fatal("the first run already knows about a column that does not exist yet")
	}

	// The migration lands: a nullable column appears, and the entity maps it.
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, warnDDL))
	writeFile(t, filepath.Join(dir, "domain", "entities.go"), `package domain

//orm:table users
type User struct {
	ID       int64
	Email    string
	Age      int32
	Nickname *string
}
`)

	out.Reset()
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("second run: exit %d\n%s", code, out.String())
	}
	after := readGenerated(t, dir)

	if !strings.Contains(after["orm_tables.gen.go"], "Nickname orm.NullTextCol[User]") {
		t.Errorf("the new column did not reach the descriptors:\n%s", after["orm_tables.gen.go"])
	}
	if !strings.Contains(after["orm_meta.gen.go"], `{Name: "nickname", Field: "Nickname"}`) {
		t.Errorf("the new column did not reach the metadata:\n%s", after["orm_meta.gen.go"])
	}
	if !strings.Contains(after["orm_meta.gen.go"], "return &e.Nickname") {
		t.Errorf("the new column did not reach the scanner:\n%s", after["orm_meta.gen.go"])
	}

	// And the package still compiles, which is what makes regeneration safe to
	// do as part of a migration rather than a thing to review by hand.
	cmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	cmd.Dir = dir
	if got, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the regenerated package does not compile: %v\n%s", err, got)
	}
}

func TestRun_generateAcrossSeveralPackages(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL+"\nCREATE TABLE orders (id bigint PRIMARY KEY, total integer NOT NULL);"))
	dir := generateProject(t, cliEntities)

	// A second entity package, generating into its own directory.
	if err := os.MkdirAll(filepath.Join(dir, "billing"), 0o755); err != nil {
		t.Fatalf("creating the second package: %v", err)
	}
	writeFile(t, filepath.Join(dir, "billing", "entities.go"), `package billing

//orm:table orders
type Order struct {
	ID    int64
	Total int32
}
`)
	writeFile(t, filepath.Join(dir, "orm.yaml"), `version: 1
schema:
  dsn: ${ORM_CLI_DSN}
  search_path:
    - public
packages:
  - path: ./domain
    output: same
  - path: ./billing
    output: same
`)

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"generate", "--config", filepath.Join(dir, "orm.yaml")}, &stdout, &stderr); code != exitClean {
		t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
	}

	// Each package gets its own three files and its own DB.
	for _, pkg := range []string{"domain", "billing"} {
		for _, name := range []string{"orm_tables.gen.go", "orm_meta.gen.go", "orm_db.gen.go"} {
			if _, err := os.Stat(filepath.Join(dir, pkg, name)); err != nil {
				t.Errorf("%s/%s: %v", pkg, name, err)
			}
		}
	}
	billing, err := os.ReadFile(filepath.Join(dir, "billing", "orm_db.gen.go"))
	if err != nil {
		t.Fatalf("reading the billing DB: %v", err)
	}
	if !strings.Contains(string(billing), "Orders *orm.Repo[Order]") {
		t.Errorf("the billing DB does not hold its own repository:\n%s", billing)
	}
	if strings.Contains(string(billing), "User") {
		t.Errorf("the billing DB mentions the other package's entity:\n%s", billing)
	}

	cmd := exec.CommandContext(t.Context(), "go", "build", "./...")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the generated project does not compile: %v\n%s", err, out)
	}
}

// The lock file, orm explain, and the staleness check they exist for.

func TestRun_generateWritesTheLock(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	var out bytes.Buffer
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("generate: exit %d\n%s", code, out.String())
	}
	data, err := os.ReadFile(filepath.Join(dir, lock.Name))
	if err != nil {
		t.Fatalf("reading the lock: %v", err)
	}
	var f lock.File
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("decoding the lock: %v", err)
	}
	if f.Version != lock.Version {
		t.Errorf("version = %d, want %d", f.Version, lock.Version)
	}
	if len(f.Mapping) != 64 {
		t.Errorf("fingerprint = %q, want a sha256 digest", f.Mapping)
	}
	// The lock records what was generated, so a check straight afterwards
	// finds nothing to say.
	out.Reset()
	if code := run(t.Context(), []string{"check", "--config", cfg, "--generated"}, &out, &out); code != exitClean {
		t.Fatalf("check after generate: exit %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "stale") {
		t.Errorf("check reported staleness right after generating:\n%s", out.String())
	}
}

// A project that has never generated is a project mid-setup. Its check reports
// the mapping and says nothing about generated code unless asked.
func TestRun_checkWithoutALock(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	var out bytes.Buffer
	if code := run(t.Context(), []string{"check", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("check: exit %d\n%s", code, out.String())
	}
	if strings.Contains(out.String(), lock.Name) {
		t.Errorf("a first check complained about a lock nobody has written yet:\n%s", out.String())
	}

	// Asked directly, it says so and fails, which is what CI wants.
	out.Reset()
	if code := run(t.Context(), []string{"check", "--config", cfg, "--generated"}, &out, &out); code != exitFindings {
		t.Fatalf("check --generated: exit %d, want %d\n%s", code, exitFindings, out.String())
	}
	if !strings.Contains(out.String(), "has not generated code yet") {
		t.Errorf("output does not explain the missing lock:\n%s", out.String())
	}
}

// A change that reconciles cleanly but changes what would be generated is what
// staleness detection is for: nothing else in the toolchain notices it.
func TestRun_checkDetectsStaleGeneration(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, cliDDL)
	t.Setenv("ORM_CLI_DSN", dsn)
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	var out bytes.Buffer
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("generate: exit %d\n%s", code, out.String())
	}

	// A default on a mapped column changes whether an insert may leave it out,
	// which changes generated metadata. The Go struct is untouched, so nothing
	// stops compiling.
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if _, err := conn.Exec(t.Context(), `ALTER TABLE users ALTER COLUMN age SET DEFAULT 0`); err != nil {
		t.Fatalf("altering the schema: %v", err)
	}

	out.Reset()
	if code := run(t.Context(), []string{"check", "--config", cfg, "--generated"}, &out, &out); code != exitFindings {
		t.Fatalf("check --generated: exit %d, want %d\n%s", code, exitFindings, out.String())
	}
	if !strings.Contains(out.String(), "stale") {
		t.Errorf("output does not report staleness:\n%s", out.String())
	}

	// Regenerating settles it.
	out.Reset()
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("regenerate: exit %d\n%s", code, out.String())
	}
	out.Reset()
	if code := run(t.Context(), []string{"check", "--config", cfg, "--generated"}, &out, &out); code != exitClean {
		t.Fatalf("check after regenerating: exit %d\n%s", code, out.String())
	}
}

// A column nobody mapped is catalog noise. It is reported as an unmapped
// column, as it always was, and it does not make generated code stale — the
// fingerprint covers the mapping, not everything the database happens to hold.
func TestRun_unmappedColumnIsNotStaleness(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, cliDDL)
	t.Setenv("ORM_CLI_DSN", dsn)
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	var out bytes.Buffer
	if code := run(t.Context(), []string{"generate", "--config", cfg}, &out, &out); code != exitClean {
		t.Fatalf("generate: exit %d\n%s", code, out.String())
	}

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()
	if _, err := conn.Exec(t.Context(), `ALTER TABLE users ADD COLUMN scratch text`); err != nil {
		t.Fatalf("altering the schema: %v", err)
	}

	out.Reset()
	code := run(t.Context(), []string{"check", "--config", cfg, "--generated"}, &out, &out)
	if strings.Contains(out.String(), "stale") {
		t.Errorf("an unmapped column was reported as stale generation:\n%s", out.String())
	}
	if code != exitClean {
		t.Errorf("exit code = %d, want %d; an unmapped nullable column is a warning\n%s", code, exitClean, out.String())
	}
}

func TestRun_explain(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "text",
			args: []string{"explain", "User", "--config", cfg},
			want: []string{"Entity: User", "Table:  public.users", "Columns", "ID", "int8", "PK"},
		},
		{
			// The flag before the operand has to work too, because both are
			// natural to write.
			name: "flags first",
			args: []string{"explain", "--config", cfg, "User"},
			want: []string{"Entity: User"},
		},
		{
			name: "json",
			args: []string{"explain", "User", "--config", cfg, "--json"},
			want: []string{`"entity": "User"`, `"table": "users"`, `"primary_key": true`},
		},
		{
			name: "sql",
			args: []string{"explain", "User", "--config", cfg, "--sql"},
			want: []string{`SELECT`, `"users"."id"`, `FROM "public"."users"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), tt.args, &stdout, &stderr); code != exitClean {
				t.Fatalf("exit code = %d\nstderr: %s", code, stderr.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

// The same mapping explains the same way every time, so the output can be
// diffed and committed.
func TestRun_explainIsDeterministic(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	for _, mode := range [][]string{
		{"explain", "User", "--config", cfg},
		{"explain", "User", "--config", cfg, "--json"},
	} {
		var first bytes.Buffer
		if code := run(t.Context(), mode, &first, &first); code != exitClean {
			t.Fatalf("%v: exit %d\n%s", mode, code, first.String())
		}
		for range 3 {
			var again bytes.Buffer
			if code := run(t.Context(), mode, &again, &again); code != exitClean {
				t.Fatalf("%v: exit %d\n%s", mode, code, again.String())
			}
			if again.String() != first.String() {
				t.Fatalf("%v differs between runs:\n%s\n\n%s", mode, first.String(), again.String())
			}
		}
	}
}

// Naming nothing, or naming something that is not there, is a request the tool
// cannot serve — the same failure class as a bad configuration.
func TestRun_explainFailures(t *testing.T) {
	testdb.AdminDSN(t)
	t.Setenv("ORM_CLI_DSN", testdb.Create(t, cliDDL))
	dir := generateProject(t, cliEntities)
	cfg := filepath.Join(dir, "orm.yaml")

	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no entity", args: []string{"explain", "--config", cfg}, want: "name an entity"},
		{name: "unknown entity", args: []string{"explain", "Nothing", "--config", cfg}, want: "no entity named"},
		{name: "two entities", args: []string{"explain", "User", "Post", "--config", cfg}, want: "unexpected argument"},
		{name: "both modes", args: []string{"explain", "User", "--config", cfg, "--json", "--sql"}, want: "pick one"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), tt.args, &stdout, &stderr); code != exitFailure {
				t.Fatalf("exit code = %d, want %d", code, exitFailure)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr does not mention %q:\n%s", tt.want, stderr.String())
			}
		})
	}
}

// The version comes from Go's build information rather than from a constant, so
// a release binary reports its tag and a local build reports its commit. What
// must never happen is a stale number baked in at some point in the past.
func TestVersion_comesFromTheBuild(t *testing.T) {
	got := Version()
	if got == "" {
		t.Fatal("Version is empty")
	}
	// A test binary is built from the working tree, so this is the devel path.
	if !strings.HasPrefix(got, "(devel") && !strings.HasPrefix(got, "v") {
		t.Errorf("Version = %q, want a tag or a devel build stamp", got)
	}

	var out bytes.Buffer
	if code := run(t.Context(), []string{"version"}, &out, &out); code != exitClean {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(out.String(), got) {
		t.Errorf("orm version printed %q, want it to contain %q", out.String(), got)
	}
}
