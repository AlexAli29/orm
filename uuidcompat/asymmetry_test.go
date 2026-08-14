package uuidcompat_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The three ways a uuid column can be declared, and which of them work.
//
// A configured mapping is one direction. Database-first starts from a
// PostgreSQL type and looks up the Go one, so it applies the mapping on its
// own. Managed starts from a Go type, and there is no reverse lookup — nothing
// in the tool says "uuid.UUID means uuid", because a configuration that maps two
// Go types to one PostgreSQL type would then have two answers and no way to
// choose. So managed has to be told with pgtype:.
//
//	database-first, no tag        works    uuid -> uuid.UUID
//	managed, no tag               refused  no PostgreSQL type for uuid.UUID
//	managed, pgtype:uuid          works
//
// That asymmetry is a contract, not a defect, and it is pinned here so it
// cannot change quietly. Whether types: should become bidirectional is an API
// question and is recorded as one; it is not answered by this qualification.

// Managed generation refuses a uuid.UUID field with no pgtype tag, and says why.
func TestUUID_managedWithoutPgtypeIsRefused(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidasym_notag")

	pkg := filepath.Join(s.dir, "untagged")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	s.addPackage("./untagged")
	write(t, filepath.Join(pkg, "entities.go"), `package untagged

import "github.com/google/uuid"

//orm:table public.untagged
type Untagged struct {
	ID    int32     `+"`orm:\"pk\"`"+`
	Value uuid.UUID
}
`)

	out, err := s.orm("makemigrations")
	if err == nil {
		t.Fatalf("a uuid.UUID field with no pgtype tag was accepted in managed mode; "+
			"there is no reverse lookup from a Go type to a PostgreSQL one, so "+
			"something guessed:\n%s", out)
	}
	// The diagnostic has to name the Go type and point at the fix, or it is a
	// refusal a reader cannot act on.
	for _, want := range []string{"no PostgreSQL type for uuid.UUID", "configured mapping"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, out)
		}
	}
	t.Logf("managed, untagged uuid.UUID:\n%s", out)
}

// The same field with the tag is accepted, and produces a uuid column.
func TestUUID_managedWithPgtypeWorks(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidasym_tagged")

	pkg := filepath.Join(s.dir, "tagged")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	s.addPackage("./tagged")
	write(t, filepath.Join(pkg, "entities.go"), `package tagged

import "github.com/google/uuid"

//orm:table public.tagged
type TaggedRow struct {
	ID    int32       `+"`orm:\"pk\"`"+`
	Value uuid.UUID   `+"`orm:\"pgtype:uuid\"`"+`
	Tags  []uuid.UUID `+"`orm:\"pgtype:uuid[]\"`"+`
}
`)

	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	s.mustORM("check", "--generated")

	if got := s.columnType("public.tagged", "value"); got != "uuid" {
		t.Errorf("tagged.value is %s, want uuid", got)
	}
	if got := s.columnType("public.tagged", "tags"); got != "uuid[]" {
		t.Errorf("tagged.tags is %s, want uuid[]", got)
	}
}

// Database-first applies the configured mapping with no tag at all.
//
// The column is made by hand and the declaration says nothing about its type,
// so the only thing that could produce uuid.UUID is the types: entry.
func TestUUID_databaseFirstNeedsNoTag(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidasym_dbfirst")

	// Build the table by hand, then point a database-first project at it.
	s.exec(`CREATE TABLE public.dbfirst (id integer PRIMARY KEY, value uuid NOT NULL, tags uuid[] NOT NULL)`)

	dir := filepath.Join(s.dir, "dbfirst")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "entities.go"), `package dbfirst

import "github.com/google/uuid"

//orm:table public.dbfirst
type DBFirst struct {
	ID    int32 `+"`orm:\"pk\"`"+`
	Value uuid.UUID
	Tags  []uuid.UUID
}
`)
	// A configuration of its own: database-first, one package, the same types
	// entry. Nothing here carries a pgtype tag.
	write(t, filepath.Join(s.dir, "dbfirst.yaml"),
		"version: 1\n\nschema:\n  dsn: ${UUIDCOMPAT_DSN}\n  search_path:\n    - public\n\n"+
			"packages:\n  - path: ./dbfirst\n    output: same\n\n"+
			"types:\n  uuid:\n    go: github.com/google/uuid.UUID\n    codec: uuid\n")

	out, err := s.ormConfig("dbfirst.yaml", "generate")
	if err != nil {
		t.Fatalf("database-first generation failed with no pgtype tags:\n%s", out)
	}
	gen := s.read(filepath.Join("dbfirst", "orm_tables.gen.go"))
	for _, want := range []string{"uuid.UUID", "[]uuid.UUID"} {
		if !strings.Contains(gen, want) {
			t.Errorf("the generated code does not use %s; the configured mapping did "+
				"not apply:\n%s", want, gen)
		}
	}
}

// The codec name is recorded and never consumed.
//
// types.uuid.codec says "uuid" in this project's configuration, and nothing
// reads it: uuid.UUID reaches PostgreSQL through pgx, which handles the type
// natively. That is a finding rather than a defect — every value in this module
// round-trips — and stating it is not the same as showing it, so this removes
// the codec from a staged copy and compares.
//
// If the generated code and the lock are identical with the codec and without
// it, the field is not an input to anything the generator decides. It stays
// configured in this module because it is what the CLI's init template writes
// and because a future release may start consuming it; pinning the current
// answer here means that change is deliberate rather than a surprise.
func TestUUID_codecIsRecordedAndNotConsumed(t *testing.T) {
	dsn(t)
	if conf, err := os.ReadFile("orm.yaml"); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(conf), "codec: uuid") {
		t.Fatal("this project no longer configures a codec, so the finding this test " +
			"pins has nothing to pin")
	}

	s := stage(t, "uuidcodec")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	withCodec := s.generatedBytes()

	// Take the codec away and generate again from the same schema.
	conf := s.read("orm.yaml")
	stripped := strings.Replace(conf, "    codec: uuid\n", "", 1)
	if stripped == conf {
		t.Fatal("the staged config does not declare the codec in the shape this test " +
			"removes, so the comparison below would be between two identical runs")
	}
	if err := os.WriteFile(filepath.Join(s.dir, "orm.yaml"), []byte(stripped), 0o644); err != nil {
		t.Fatal(err)
	}
	for path := range withCodec {
		if err := os.Remove(filepath.Join(s.dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	s.mustORM("generate")
	withoutCodec := s.generatedBytes()

	for path, a := range withCodec {
		if b := withoutCodec[path]; a != b {
			t.Errorf("%s differs when the codec is removed, so the codec is consumed "+
				"after all and this finding needs rewriting rather than pinning", path)
		}
	}

	// And the project still works without it, which is the runtime half.
	s.mustORM("check", "--generated")
}
