package gendemo_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/emit"
	"github.com/AlexAli29/orm/internal/testdb"
)

// The generated files in this package are committed, so `go build ./...`
// compiles generated code on every run and the tests beside them query through
// it. That only means anything if the committed files are what the generator
// actually produces, which is what these tests check.

// generateHere runs the whole pipeline over this package and returns the
// rendered files, without writing them.
func generateHere(t *testing.T) []emit.File {
	t.Helper()
	dsn := testdb.Create(t, schema(t))
	t.Setenv("ORM_GENDEMO_DSN", dsn)

	cfg, err := config.Load("orm.yaml")
	if err != nil {
		t.Fatalf("loading orm.yaml: %v", err)
	}
	_, files, err := gen.Generate(t.Context(), cfg, diag.SeverityError)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	return files
}

func TestGenerate_matchesTheCommittedFiles(t *testing.T) {
	testdb.AdminDSN(t)
	files := generateHere(t)
	if len(files) != len(emit.GeneratedFiles) {
		t.Fatalf("generated %d files, want %d", len(files), len(emit.GeneratedFiles))
	}

	for _, f := range files {
		name := filepath.Base(f.Path)
		committed, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading the committed %s: %v", name, err)
		}
		if !bytes.Equal(committed, f.Content) {
			t.Errorf("the committed %s is stale; run orm generate --config internal/gendemo/orm.yaml\n--- generated ---\n%s\n--- committed ---\n%s",
				name, f.Content, committed)
		}
	}
}

func TestGenerate_isByteIdenticalAcrossRuns(t *testing.T) {
	testdb.AdminDSN(t)
	// Three runs, each against a freshly built database, so that anything
	// carried over from the catalog — an OID, a creation order — would show up
	// as a difference.
	var first []emit.File
	for run := range 3 {
		files := generateHere(t)
		if run == 0 {
			first = files
			continue
		}
		if len(files) != len(first) {
			t.Fatalf("run %d generated %d files, want %d", run+1, len(files), len(first))
		}
		for i := range files {
			if files[i].Path != first[i].Path {
				t.Fatalf("run %d generated %s where run 1 generated %s", run+1, files[i].Path, first[i].Path)
			}
			if !bytes.Equal(files[i].Content, first[i].Content) {
				t.Fatalf("run %d produced different bytes for %s", run+1, filepath.Base(files[i].Path))
			}
		}
	}
}

func TestGenerate_outputCarriesNothingThatWouldChangeOnItsOwn(t *testing.T) {
	testdb.AdminDSN(t)
	for _, f := range generateHere(t) {
		name := filepath.Base(f.Path)
		content := string(f.Content)

		if !strings.HasPrefix(content, emit.Header) {
			t.Errorf("%s does not open with the generated-code header", name)
		}
		// An absolute path would differ between machines; a version would
		// differ between releases. Either makes every regeneration a diff.
		if strings.Contains(content, os.TempDir()) || strings.Contains(content, "/home/") {
			t.Errorf("%s contains an absolute path", name)
		}
		if strings.Contains(strings.ToLower(content), "generated at") {
			t.Errorf("%s contains a timestamp", name)
		}
	}
}

func TestGenerate_refusesWhenTheMappingDoesNotHold(t *testing.T) {
	testdb.AdminDSN(t)
	// Build the schema without a column the entity expects. Reconciliation
	// reports it, and generation must not proceed: code emitted from a mapping
	// that does not hold would compile against a schema it disagrees with,
	// which is the failure this project exists to prevent.
	broken := strings.Replace(schema(t), "    age        integer NOT NULL,\n", "", 1)
	t.Setenv("ORM_GENDEMO_DSN", testdb.Create(t, broken))

	cfg, err := config.Load("orm.yaml")
	if err != nil {
		t.Fatalf("loading orm.yaml: %v", err)
	}
	result, files, err := gen.Generate(t.Context(), cfg, diag.SeverityError)
	if err == nil {
		t.Fatal("Generate succeeded over a mapping that does not hold")
	}
	if files != nil {
		t.Errorf("Generate returned %d files alongside its refusal", len(files))
	}
	if result == nil || result.Report.Len() == 0 {
		t.Fatal("Generate refused without a report explaining why")
	}
	var sawE001 bool
	for _, f := range result.Report.Findings() {
		if f.Code == diag.E001 {
			sawE001 = true
		}
	}
	if !sawE001 {
		t.Errorf("the report does not name the missing column: %v", result.Report.Findings())
	}
}

func TestGenerate_atomicWriteReplacesTheWholeFile(t *testing.T) {
	testdb.AdminDSN(t)
	files := generateHere(t)

	// Write into a scratch directory rather than over the committed files, and
	// start from content longer than the output so a partial write would leave
	// a tail behind.
	dir := t.TempDir()
	scratch := make([]emit.File, len(files))
	for i, f := range files {
		scratch[i] = emit.File{Path: filepath.Join(dir, filepath.Base(f.Path)), Content: f.Content}
		if err := os.WriteFile(scratch[i].Path, bytes.Repeat([]byte("x"), len(f.Content)*2), 0o644); err != nil {
			t.Fatalf("seeding %s: %v", scratch[i].Path, err)
		}
	}

	if err := gen.Write(scratch); err != nil {
		t.Fatalf("Write: %v", err)
	}
	for _, f := range scratch {
		got, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatalf("reading %s: %v", f.Path, err)
		}
		if !bytes.Equal(got, f.Content) {
			t.Errorf("%s was not fully replaced", filepath.Base(f.Path))
		}
	}

	// Nothing is left behind: a temporary file that survived would be picked up
	// by the Go tool as a stray source file.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the scratch directory: %v", err)
	}
	if len(entries) != len(scratch) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want only the generated files", names)
	}
}
