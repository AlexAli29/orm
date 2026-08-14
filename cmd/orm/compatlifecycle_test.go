package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// M16.5 compatibility: a migration history that is lived through rather than
// generated once.
//
// Every other fixture builds a project, migrates it and converges. What is
// proven here is the shape a real project has after a few months: five versions,
// each adding, changing or dropping an index, each converging — and then the
// whole history replayed from an empty database, which must arrive at the same
// place. A migration that only works when applied to the state its author had is
// not a migration.
//
// Determinism is proven beside it, because the two questions share a fixture: an
// artifact that depends on the order files were read in is not portable, and the
// symptom — a lock that churns between developers — looks nothing like a bug in
// ordering until somebody diffs it.

// lifecycleEntities is the project at one version of its index set.
func lifecycleEntities(indexes string) string {
	return `package domain

//orm:table public.events
type Event struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Kind   string
	Actor  string
	Weight int64
}

//orm:materialized-view public.event_rollups
//orm:definition ` + "`SELECT id, kind, actor, weight FROM events`" + `
//orm:depends-on public.events
` + indexes + `type EventRollup struct {
	ID     int64
	Kind   string
	Actor  string
	Weight int64
}
`
}

// The five versions. Each moves exactly one thing, so a version that fails says
// which change broke rather than which release did.
var lifecycleVersions = []struct {
	name    string
	indexes string
}{
	{"v1-create", "//orm:index rollup_id_key (ID) unique\n"},
	{"v2-add", "//orm:index rollup_id_key (ID) unique\n" +
		"//orm:index rollup_kind_idx (Kind)\n"},
	{"v3-change", "//orm:index rollup_id_key (ID) unique\n" +
		"//orm:index rollup_kind_idx (Kind, Actor)\n"},
	{"v4-drop", "//orm:index rollup_id_key (ID) unique\n"},
	{"v5-add-another", "//orm:index rollup_id_key (ID) unique\n" +
		"//orm:index rollup_weight_brin_idx (Weight) using brin\n"},
}

// A five-version history, converging at every step and never recreating the
// relation.
func TestCompatLifecycle_everyVersionConverges(t *testing.T) {
	p := newProject(t, lifecycleEntities(lifecycleVersions[0].indexes))
	var oid uint32

	for i, v := range lifecycleVersions {
		p.Entities(lifecycleEntities(v.indexes))
		out := p.MustRun("makemigrations", "--name", v.name)
		if i > 0 && strings.Contains(out, "No schema changes detected") {
			t.Fatalf("%s planned nothing, so the version changed nothing:\n%s", v.name, out)
		}
		p.MustRun("migrate")
		p.MustRun("check")
		p.MustRun("generate")
		p.MustRun("check", "--generated")
		if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
			t.Fatalf("%s did not converge:\n%s", v.name, out)
		}

		got := relationOID(t, p, "public.event_rollups")
		if i == 0 {
			oid = got
			continue
		}
		if got != oid {
			t.Fatalf("%s recreated the materialized view: OID %d -> %d. An index change "+
				"discards every stored row if the relation is rebuilt", v.name, oid, got)
		}
	}

	// The history is what a colleague would clone.
	if n := len(p.Migrations()); n < len(lifecycleVersions) {
		t.Errorf("the history has %d files for %d versions", n, len(lifecycleVersions))
	}
}

// The same history, replayed from nothing, arrives at the same schema.
//
// This is the question a new developer and a fresh CI database ask. The state
// the migrations describe is rebuilt by replaying them in order, and if that
// disagrees with the state the author reached by applying them one at a time,
// the project converges for its author and for nobody else.
func TestCompatLifecycle_replayFromZeroReachesTheSameState(t *testing.T) {
	p := newProject(t, lifecycleEntities(lifecycleVersions[0].indexes))
	for _, v := range lifecycleVersions {
		p.Entities(lifecycleEntities(v.indexes))
		p.MustRun("makemigrations", "--name", v.name)
		p.MustRun("migrate")
	}
	applied := describeRelations(t, p)

	// A second project, same migrations, empty database, applied in one go.
	fresh := newProject(t, lifecycleEntities(lifecycleVersions[len(lifecycleVersions)-1].indexes))
	copyMigrations(t, p, fresh)
	fresh.MustRun("migrate")
	fresh.MustRun("check")
	if out := fresh.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("a database built by replaying the whole history does not match the "+
			"declarations:\n%s", out)
	}
	replayed := describeRelations(t, fresh)

	if applied != replayed {
		t.Errorf("replaying the history from zero produced a different schema:\n%s",
			diffText(applied, replayed))
	}
}

// describeRelations renders the parts of the live schema a migration decides, in
// a stable order, so two databases can be compared as text.
func describeRelations(t *testing.T, p *project) string {
	t.Helper()
	rows := p.Query(`
		SELECT n.nspname || '.' || c.relname || ' ' || c.relkind::text || ' ' ||
		       coalesce((SELECT string_agg(i.relname || ':' || pg_get_indexdef(i.oid), '; ' ORDER BY i.relname)
		                   FROM pg_class i
		                   JOIN pg_index x ON x.indexrelid = i.oid
		                  WHERE x.indrelid = c.oid), '')
		  FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r','m','v')
		   AND c.relname <> 'orm_migrations' AND c.relname NOT LIKE 'orm!_%' ESCAPE '!'
		 ORDER BY 1`)
	return strings.Join(rows, "\n")
}

// copyMigrations puts one project's committed history into another.
func copyMigrations(t *testing.T, from, to *project) {
	t.Helper()
	dst := filepath.Join(to.Dir, "migrations")
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(from.Dir, "migrations")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the source project has no migrations, so replaying them would prove nothing")
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dst, e.Name()), string(b))
	}
}

// Artifacts do not depend on the order the declarations were written in, nor on
// the order the files were discovered in.
//
// Both perturbations are things a reformat or a rename does by accident. An
// artifact that moves under either produces a lock that churns between
// developers who changed nothing, and the churn is indistinguishable from a real
// change until somebody reads the diff.
func TestCompatLifecycle_artifactsAreOrderIndependent(t *testing.T) {
	const indexes = "//orm:index rollup_id_key (ID) unique\n" +
		"//orm:index rollup_kind_idx (Kind)\n" +
		"//orm:index rollup_weight_brin_idx (Weight) using brin\n"

	capture := func(t *testing.T, entities string) (gen, lock, mig string) {
		t.Helper()
		p := newProject(t, entities)
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")
		p.MustRun("check")
		gen = readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
			readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go")) +
			readFile(t, filepath.Join(p.Dir, "domain", "orm_tables.gen.go"))
		lock = readFile(t, filepath.Join(p.Dir, "orm.lock"))
		entriesDir := filepath.Join(p.Dir, "migrations")
		names, err := os.ReadDir(entriesDir)
		if err != nil {
			t.Fatal(err)
		}
		var files []string
		for _, e := range names {
			if strings.HasSuffix(e.Name(), ".json") {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
		if len(files) != 1 {
			t.Fatalf("expected exactly one migration artifact, found %v", files)
		}
		mig = readFile(t, filepath.Join(entriesDir, files[0]))
		return gen, lock, mig
	}

	forwardGen, forwardLock, forwardMig := capture(t, lifecycleEntities(indexes))

	// The same index set, declared bottom to top.
	lines := strings.Split(strings.TrimRight(indexes, "\n"), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	reversed := strings.Join(lines, "\n") + "\n"
	if reversed == indexes {
		t.Fatal("reversing the declarations changed nothing, so this proves nothing")
	}
	revGen, revLock, revMig := capture(t, lifecycleEntities(reversed))

	if revGen != forwardGen {
		t.Errorf("reversing the index declarations changed the generated Go:\n%s",
			diffText(forwardGen, revGen))
	}
	if revLock != forwardLock {
		t.Errorf("reversing the index declarations changed the lock:\n%s",
			diffText(forwardLock, revLock))
	}
	if revMig != forwardMig {
		t.Errorf("reversing the index declarations changed the migration:\n%s",
			diffText(forwardMig, revMig))
	}

	// And generating twice from one project is stable, which is the weaker
	// property the stronger one would otherwise hide.
	againGen, againLock, againMig := capture(t, lifecycleEntities(indexes))
	if againGen != forwardGen || againLock != forwardLock || againMig != forwardMig {
		t.Error("two runs of the same project produced different artifacts")
	}
}

// The same declarations split across files in a different order produce the same
// artifacts.
//
// Package discovery reads a directory, and a directory's order is the
// filesystem's business. A project that renamed a file would otherwise get a new
// lock.
func TestCompatLifecycle_fileOrderDoesNotReachTheArtifacts(t *testing.T) {
	table := `package domain

//orm:table public.events
type Event struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Kind   string
	Actor  string
	Weight int64
}
`
	matview := `package domain

//orm:materialized-view public.event_rollups
//orm:definition ` + "`SELECT id, kind, actor, weight FROM events`" + `
//orm:depends-on public.events
//orm:index rollup_id_key (ID) unique
type EventRollup struct {
	ID     int64
	Kind   string
	Actor  string
	Weight int64
}
`
	capture := func(t *testing.T, first, second string, firstName, secondName string) (string, string) {
		t.Helper()
		p := newProject(t, "package domain\n")
		if err := os.Remove(filepath.Join(p.Dir, "domain", "entities.go")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p.Dir, "domain", firstName), first)
		writeFile(t, filepath.Join(p.Dir, "domain", secondName), second)
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")
		p.MustRun("check")
		return readFile(t, filepath.Join(p.Dir, "orm.lock")),
			readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
	}

	// a_table.go then z_matview.go, and the names swapped so the directory
	// hands them over the other way round.
	lockA, genA := capture(t, table, matview, "a_table.go", "z_matview.go")
	lockB, genB := capture(t, matview, table, "a_matview.go", "z_table.go")

	if lockA != lockB {
		t.Errorf("the lock depends on which file a declaration lives in:\n%s",
			diffText(lockA, lockB))
	}
	if genA != genB {
		t.Errorf("the generated handle depends on which file a declaration lives in:\n%s",
			diffText(genA, genB))
	}
}
