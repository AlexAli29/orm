package uuidcompat_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The same uuid project, on every supported major.
//
// Two claims live here and they are different claims. The first is that the
// workflow runs: migrate, generate, check, write, read, join, refresh — real
// work against a real server of each major, not a catalog query that would pass
// on a server nothing had been done to. The second is that what the workflow
// produces is the same everywhere: a project generated against PostgreSQL 14 and
// one generated against 18 have to be byte-identical, or a team with mixed
// servers gets a diff on every checkout.
//
// A skip here would be the worst kind of false green, because "supported on five
// majors" is the claim and the number of servers that answered is the evidence.
// So the DSNs are required and their absence is a failure.

// theSupportedMajors are the environment variables each server is reached
// through, in the order they are reported.
var theSupportedMajors = []struct{ major, env string }{
	{"PG14", "ORM_TEST_DSN_PG14"},
	{"PG15", "ORM_TEST_DSN_PG15"},
	{"PG16", "ORM_TEST_DSN_PG16"},
	{"PG17", "ORM_TEST_DSN_PG17"},
	{"PG18", "ORM_TEST_DSN_PG18"},
}

// majorDSNs returns one DSN per supported major, failing if any is missing.
func majorDSNs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	var missing []string
	for _, m := range theSupportedMajors {
		v := os.Getenv(m.env)
		if v == "" {
			missing = append(missing, m.env)
			continue
		}
		out[m.major] = v
	}
	if len(missing) > 0 {
		t.Fatalf("the five-major matrix needs every supported major and %v %s unset. "+
			"Skipping would report a five-major claim proven by however many servers "+
			"happened to be running", missing,
			map[bool]string{true: "is", false: "are"}[len(missing) == 1])
	}
	return out
}

// capture is what one major produced.
type capture struct {
	major     string
	version   string
	generated map[string]string // path -> bytes
	lock      string
	migration string
}

// runMajor performs the whole workflow against one server and captures what it
// wrote.
func runMajor(t *testing.T, major, admin string) capture {
	t.Helper()
	s := stageAt(t, admin, "uuidmajor")

	// The server says which major it is. Trusting the environment variable's
	// name would let a mislabelled DSN prove five majors with one server.
	got := s.serverMajor()
	if !strings.HasPrefix(got, strings.TrimPrefix(major, "PG")) {
		t.Fatalf("%s points at a server reporting major %s", major, got)
	}

	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	s.mustORM("check")
	s.mustORM("check", "--generated")

	// Real work, not a catalog query: the whole uuid surface exercised through
	// the generated code against this server.
	s.workload()

	// A second plan has nothing to say.
	if out := s.mustORM("makemigrations", "--check"); !strings.Contains(out, "No schema changes") {
		t.Fatalf("%s does not converge:\n%s", major, out)
	}

	c := capture{major: major, version: got, generated: s.generatedBytes()}
	c.lock = c.generated["orm.lock"]
	delete(c.generated, "orm.lock")

	ms, err := filepath.Glob(filepath.Join(s.dir, "migrations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		t.Fatalf("%s produced no migration artifact; a comparison against a missing "+
			"file is not a comparison", major)
	}
	sort.Strings(ms)
	var b strings.Builder
	for _, m := range ms {
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		if len(body) == 0 {
			t.Fatalf("%s wrote %s and it is empty", major, filepath.Base(m))
		}
		b.WriteString(filepath.Base(m))
		b.WriteString("\n")
		b.Write(body)
	}
	c.migration = b.String()

	if c.lock == "" {
		t.Fatalf("%s produced no orm.lock, or it is empty", major)
	}
	if len(c.generated) == 0 {
		t.Fatalf("%s produced no generated Go, or the capture is missing it", major)
	}
	return c
}

// Every major runs the whole workflow, and every major produces the same bytes.
func TestUUID_everySupportedMajor(t *testing.T) {
	dsns := majorDSNs(t)

	var caps []capture
	for _, m := range theSupportedMajors {
		t.Run(m.major, func(t *testing.T) {
			c := runMajor(t, m.major, dsns[m.major])
			caps = append(caps, c)
			t.Logf("%s (server major %s): workflow complete, %d generated files",
				c.major, c.version, len(c.generated))
		})
	}
	if len(caps) != len(theSupportedMajors) {
		t.Fatalf("captured %d majors, want %d: a comparison over fewer proves less "+
			"than it claims", len(caps), len(theSupportedMajors))
	}

	// Distinct servers, so this is five answers and not one answer five times.
	seen := map[string]string{}
	for _, c := range caps {
		if prev, ok := seen[c.version]; ok {
			t.Errorf("%s and %s both report server major %s; the matrix is not "+
				"covering five majors", prev, c.major, c.version)
		}
		seen[c.version] = c.major
	}

	base := caps[0]
	for _, c := range caps[1:] {
		if c.lock != base.lock {
			t.Errorf("orm.lock differs between %s and %s; the fingerprint is not "+
				"portable and a mixed-server team gets a diff on every checkout",
				base.major, c.major)
		}
		if c.migration != base.migration {
			t.Errorf("the migration artifact differs between %s and %s",
				base.major, c.major)
		}
		if len(c.generated) != len(base.generated) {
			t.Errorf("%s wrote %d generated files and %s wrote %d",
				base.major, len(base.generated), c.major, len(c.generated))
			continue
		}
		for path, want := range base.generated {
			got, ok := c.generated[path]
			if !ok {
				t.Errorf("%s wrote %s and %s did not", base.major, path, c.major)
				continue
			}
			if got != want {
				t.Errorf("%s differs between %s and %s", path, base.major, c.major)
			}
		}
	}

	// And nothing server-local reached any of them, on any major.
	for _, c := range caps {
		bodies := map[string]string{"orm.lock": c.lock, "the migration artifact": c.migration}
		for p, b := range c.generated {
			bodies[p] = b
		}
		for name, body := range bodies {
			for _, f := range forbiddenInArtifacts {
				if m := f.pattern.FindString(body); m != "" {
					t.Errorf("on %s, %s contains %s (%q)", c.major, name, f.name, m)
				}
			}
		}
	}
}
