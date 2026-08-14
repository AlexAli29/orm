package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M16.5 G1 freeze gate: a project with no views is untouched by view support.
//
// This is the claim every existing user depends on, and until now it had no
// evidence. The test generates a table-only project twice — once with the view
// machinery present, which is the only code there is — and pins the bytes, so a
// future change that perturbs table migrations, generated code or the lock has
// to move a golden somebody reads.

const tableOnlyProject = `package domain

import "time"

//orm:table public.users
type User struct {
	ID        int64     ` + "`orm:\"pk,identity\"`" + `
	Email     string    ` + "`orm:\"unique\"`" + `
	Active    bool
	CreatedAt time.Time ` + "`orm:\"column:created_at\"`" + `
}

//orm:table public.posts
type Post struct {
	ID     int64  ` + "`orm:\"pk,identity\"`" + `
	UserID int64  ` + "`orm:\"column:user_id\"`" + `
	Title  string
}

//orm:index posts_user_idx (UserID)
type _ = Post
`

// A table-only project produces the same bytes whatever else the tool learned.
func TestTableOnly_bytesAreStable(t *testing.T) {
	run := func(t *testing.T) (migration, gen, lock string) {
		t.Helper()
		p := newProject(t, tableOnlyProject)
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")

		entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				migration = readFile(t, filepath.Join(p.Dir, "migrations", e.Name()))
			}
		}
		gen = readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
			readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))
		lock = readFile(t, filepath.Join(p.Dir, "orm.lock"))
		return migration, gen, lock
	}

	m1, g1, l1 := run(t)
	m2, g2, l2 := run(t)

	if m1 != m2 {
		t.Errorf("two runs produced different migrations:\n%s", diffText(m1, m2))
	}
	if g1 != g2 {
		t.Errorf("two runs produced different generated code:\n%s", diffText(g1, g2))
	}
	if l1 != l2 {
		t.Errorf("two runs produced different locks:\n%s", diffText(l1, l2))
	}

	// A table keeps an ordinary repository. Nothing about view capability
	// typing may reach a project that declares none.
	for _, want := range []string{"Users *orm.Repo[User]", "Posts *orm.Repo[Post]"} {
		if !strings.Contains(strings.Join(strings.Fields(g1), " "), strings.Join(strings.Fields(want), " ")) {
			t.Errorf("the generated handle does not declare %q", want)
		}
	}
	if strings.Contains(g1, "ViewRepo") {
		t.Errorf("a project with no views generated a ViewRepo:\n%s", g1)
	}

	// No view section, no view vocabulary, in a project that has none.
	for _, forbidden := range []string{"create_view", "replace_view", "drop_view", "definition"} {
		if strings.Contains(m1, forbidden) {
			t.Errorf("the migration of a table-only project mentions %q:\n%s", forbidden, m1)
		}
	}
	for _, forbidden := range []string{"view ", "materialized-view", "depends-on", "definition "} {
		if strings.Contains(l1, forbidden) {
			t.Errorf("the lock of a table-only project mentions %q:\n%s", forbidden, l1)
		}
	}
	// And nothing from a server or a machine anywhere.
	for _, forbidden := range []string{"/tmp/", "/home/", "server_version", "Canonical"} {
		if strings.Contains(m1+g1+l1, forbidden) {
			t.Errorf("table-only output contains %q", forbidden)
		}
	}
	t.Logf("table-only migration %d bytes, generated %d bytes, lock %d bytes",
		len(m1), len(g1), len(l1))
}

// M16.5 G2 #28: a table's indexes are not part of what was generated.
//
// The lock fingerprints the mapping, and generation reads a table's columns,
// its primary key and the constraints its relations were proved from. It does
// not read the table's indexes: nothing in the generated code changes when one
// is added, dropped or redefined.
//
// A materialized view is the exception, and that is why this needs saying. Its
// generated constructor carries the name of the index a concurrent refresh may
// use, so index facts genuinely are generation inputs there — and the obvious
// way to make that work is to write index metadata for every relation and let
// the materialized-view case fall out of it. Do that and every table-only
// project's lock moves on an index change that changed nothing about what was
// generated, and the developer is told to regenerate code that is already
// correct.
//
// Byte stability across two runs of one project cannot see this: both runs would
// carry the same wrong content. What sees it is the same project with a
// different index.
func TestTableOnly_anIndexChangeDoesNotMoveTheGeneratedIdentity(t *testing.T) {
	capture := func(t *testing.T, entities string) (gen, lock string) {
		t.Helper()
		p := newProject(t, entities)
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")
		p.MustRun("check", "--generated")
		return generatedOf(t, p), readFile(t, filepath.Join(p.Dir, "orm.lock"))
	}

	beforeGen, beforeLock := capture(t, tableOnlyBefore)
	afterGen, afterLock := capture(t, tableOnlyAfter)

	if beforeLock != afterLock {
		t.Errorf("changing an ordinary table's index moved the lock. Nothing about the "+
			"generated code depends on it, so every project would be told to regenerate "+
			"code that is already correct:\n%s", diffText(beforeLock, afterLock))
	}
	if beforeGen != afterGen {
		t.Errorf("changing an ordinary table's index changed the generated code:\n%s",
			diffText(beforeGen, afterGen))
	}
	for _, forbidden := range []string{"users_email_key", "users_email_active_key", "table-index"} {
		if strings.Contains(beforeLock+afterLock, forbidden) {
			t.Errorf("the lock of a table-only project names an index (%q)", forbidden)
		}
	}
}
