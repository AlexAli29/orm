package uuidcompat_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The artifacts a uuid project produces, and what must not be in them.
//
// A migration artifact and an orm.lock are committed and replayed on machines
// that are not the one that wrote them, against servers that are not the one
// they were written against. Anything in them that came from a particular
// server — an OID, a database name, a version, a path — makes a project that
// converges here fail to converge there, and the failure looks like drift
// rather than like a portability bug.
//
// uuid is a type that reaches Go only through configuration, so the risk it
// carries specifically is that something records the resolved type by OID.
// PostgreSQL's uuid is a built-in with a fixed OID, which is what would make
// that mistake work everywhere and hide until somebody used a type whose OID
// is assigned per database.

// forbiddenInArtifacts are the things a portable artifact must never contain.
var forbiddenInArtifacts = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"an OID", regexp.MustCompile(`(?i)"?\boid"?\s*[:=]`)},
	{"the database name", regexp.MustCompile(`uuidcompat_?\w*"?\s*[,}\]]`)},
	{"a server version", regexp.MustCompile(`(?i)server_?version|PostgreSQL 1[45678]`)},
	{"a deparsed server definition", regexp.MustCompile(`(?i)servercanonical`)},
	{"an absolute path", regexp.MustCompile(`"/(home|tmp|var|Users)/`)},
	{"a temporary path", regexp.MustCompile(`(?i)/tmp/|TempDir|\bT\d{6,}\b`)},
}

func readArtifacts(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	ms, err := filepath.Glob("migrations/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatal("this module has no migration artifacts, so nothing below is checking anything")
	}
	for _, m := range ms {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		out[m] = string(b)
	}
	b, err := os.ReadFile("orm.lock")
	if err != nil {
		t.Fatalf("reading orm.lock: %v", err)
	}
	out["orm.lock"] = string(b)
	return out
}

// The committed artifacts carry uuid as a SQL type name, not as anything a
// server assigned.
func TestUUID_artifactsRecordTheSQLTypeName(t *testing.T) {
	arts := readArtifacts(t)

	var sawUUID, sawArray bool
	for name, body := range arts {
		if !strings.HasPrefix(name, "migrations/") {
			continue
		}
		if !json.Valid([]byte(body)) {
			t.Fatalf("%s is not JSON", name)
		}
		if strings.Contains(body, `"uuid"`) {
			sawUUID = true
		}
		if strings.Contains(body, `"uuid[]"`) || strings.Contains(body, `_uuid`) {
			sawArray = true
		}
	}
	if !sawUUID {
		t.Error(`no migration artifact names the type "uuid"; the artifacts do not ` +
			`record the SQL type and this file is checking nothing`)
	}
	if !sawArray {
		t.Error("no migration artifact names the uuid array type")
	}
}

// Nothing server-local reached an artifact.
func TestUUID_artifactsAreServerIndependent(t *testing.T) {
	for name, body := range readArtifacts(t) {
		for _, f := range forbiddenInArtifacts {
			if m := f.pattern.FindString(body); m != "" {
				t.Errorf("%s contains %s (%q): an artifact carrying it converges on the "+
					"machine that wrote it and nowhere else", name, f.name, m)
			}
		}
	}
}

// Generation is a function of the declarations and the schema, so running it
// again writes the same bytes.
func TestUUID_generationIsByteIdentical(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidartifact")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")

	first := s.generatedBytes()
	// Remove the output and generate again, so this compares generation rather
	// than a no-op that noticed the files were already there.
	for path := range first {
		if err := os.Remove(filepath.Join(s.dir, path)); err != nil {
			t.Fatal(err)
		}
	}
	s.mustORM("generate")
	second := s.generatedBytes()

	if len(first) != len(second) {
		t.Fatalf("generation wrote %d files and then %d", len(first), len(second))
	}
	for path, a := range first {
		b, ok := second[path]
		if !ok {
			t.Errorf("%s was written the first time and not the second", path)
			continue
		}
		if a != b {
			t.Errorf("%s differs between two generations of the same project", path)
		}
	}
}

// The project converges: a second makemigrations has nothing to say.
func TestUUID_projectConverges(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidconverge")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")

	out := s.mustORM("makemigrations", "--check")
	if !strings.Contains(out, "No schema changes") {
		t.Errorf("the project does not converge; a second plan says:\n%s", out)
	}
}

// A uuid column declared as something else moves the lock.
//
// The lock is what makes a schema change visible to review, so a declaration
// changing from uuid to text has to change it. If it did not, the two projects
// would be indistinguishable to anything reading the lock — and the lock is the
// thing a reviewer reads.
func TestUUID_changingTheTypeMovesTheLock(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidlock")
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	before := s.read("orm.lock")

	// tokens.tenant_id stops being a uuid domain and becomes text.
	p := filepath.Join(s.dir, "domain", "entities.go")
	src := s.read(filepath.Join("domain", "entities.go"))
	changed := strings.Replace(src,
		"TenantID uuid.UUID `orm:\"pgtype:public.tenant_uuid\"`",
		"TenantID string    `orm:\"pgtype:text\"`", 1)
	if changed == src {
		t.Fatal("the declaration this test edits is not in entities.go in the shape " +
			"it expects, so the edit would be a no-op and the comparison meaningless")
	}
	if err := os.WriteFile(p, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	after := s.read("orm.lock")

	if before == after {
		t.Error("orm.lock is byte-identical after a column changed from a uuid domain " +
			"to text; a reviewer reading the lock would see no schema change")
	}
}

// generatedBytes reads every generated file the staged project produced.
func (s *staged) generatedBytes() map[string]string {
	s.t.Helper()
	out := map[string]string{}
	for _, pkg := range []string{"domain", "tenanta", "tenantb"} {
		ms, err := filepath.Glob(filepath.Join(s.dir, pkg, "*.gen.go"))
		if err != nil {
			s.t.Fatal(err)
		}
		for _, m := range ms {
			rel, err := filepath.Rel(s.dir, m)
			if err != nil {
				s.t.Fatal(err)
			}
			out[rel] = s.read(rel)
		}
	}
	out["orm.lock"] = s.read("orm.lock")
	if len(out) < 4 {
		s.t.Fatalf("the staged project produced %d generated files, which is too few "+
			"for this comparison to mean anything", len(out))
	}
	return out
}

func (s *staged) read(rel string) string {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, rel))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}
