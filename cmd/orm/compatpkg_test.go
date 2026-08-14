package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M16.5 compatibility: materialized views in a real multi-package project.
//
// Schema dependencies do not respect Go package boundaries. A table declared in
// one package is read by a view in another and summarised by a materialized view
// in a third, and the tool has to discover all three, order the operations by
// what depends on what rather than by which package it read first, and generate
// each relation's code into the package that declares it.
//
// The existing multi-package evidence stops at ordinary views. That is the half
// that was already frozen; what is unproven is the same chain ending in a
// materialized view, which is the relation with indexes, a refresh, a
// creation policy and a generated descriptor carrying a chosen index name.
//
// Source identity gets its own pressure here too. Two packages declare a
// relation with the same basename in different schemas, each carrying an index
// of the same name, because a project that resolves relations by name alone gets
// every single-schema fixture right and this one wrong.

// The three packages, and the chain through them:
//
//	accounts.User            public.users            table
//	reporting.ActiveUser     public.active_users     view
//	analytics.UserSummary    public.user_summaries   materialized view
//
// plus, in a fourth package and a second schema:
//
//	mirror.UserSummary       other.user_summaries    materialized view
//
// which shares the basename and the index name with the third.
const (
	compatPkgAccounts = `package accounts

//orm:table public.users
type User struct {
	ID       int64 ` + "`orm:\"pk,identity\"`" + `
	Email    string
	Active   bool
	Verified bool
}
`
	compatPkgReporting = `package reporting

//orm:view public.active_users
//orm:definition ` + "`SELECT id, email, verified FROM users WHERE active`" + `
//orm:depends-on public.users
type ActiveUser struct {
	ID       int64
	Email    string
	Verified bool
}
`
	compatPkgAnalytics = `package analytics

//orm:materialized-view public.user_summaries
//orm:definition ` + "`SELECT id, email, verified FROM active_users`" + `
//orm:depends-on public.active_users
//orm:index summary_id_key (ID) unique
//orm:index summary_email_idx (Email)
type UserSummary struct {
	ID       int64
	Email    string
	Verified bool
}
`
	// The same basename, the same index name, a different schema and a
	// different Go package. Everything that could collide, does.
	compatPkgMirror = `package mirror

//orm:materialized-view other.user_summaries
//orm:definition ` + "`SELECT id, email, verified FROM public.users WHERE verified`" + `
//orm:depends-on public.users
//orm:index summary_id_key (ID) unique
//orm:index summary_email_idx (Email)
type UserSummary struct {
	ID       int64
	Email    string
	Verified bool
}
`
)

// compatPkgProject writes the four-package project and the schema the mirror
// lives in.
//
// The second schema is created by hand because migrations do not create schemas:
// that is a frozen boundary, and inventing a CREATE SCHEMA here would be testing
// something the tool does not claim.
func compatPkgProject(t *testing.T) *project {
	t.Helper()
	p := newProject(t, compatPkgAccounts)
	if err := os.Remove(filepath.Join(p.Dir, "domain", "entities.go")); err != nil {
		t.Fatal(err)
	}
	for dir, src := range map[string]string{
		"accounts":  compatPkgAccounts,
		"reporting": compatPkgReporting,
		"analytics": compatPkgAnalytics,
		"mirror":    compatPkgMirror,
	} {
		if err := os.MkdirAll(filepath.Join(p.Dir, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(p.Dir, dir, "entities.go"), src)
	}
	p.SQL(`CREATE SCHEMA other`)
	writeFile(t, p.Conf, "version: 1\n\nschema:\n  mode: managed\n  dsn: \""+p.DSN+"\"\n"+
		"  search_path:\n    - public\n    - other\n\nmigrations:\n  dir: migrations\n\npackages:\n"+
		"  - path: ./accounts\n    output: same\n"+
		"  - path: ./reporting\n    output: same\n"+
		"  - path: ./analytics\n    output: same\n"+
		"  - path: ./mirror\n    output: same\n")
	return p
}

// The chain is discovered, ordered, applied and converges.
func TestCompatPkg_materializedViewAcrossPackages(t *testing.T) {
	p := compatPkgProject(t)

	out := p.MustRun("makemigrations", "--name", "initial")
	// Dependency order, not package order: the table before the view before the
	// materialized views that read them.
	iTable := strings.Index(out, "create table")
	iView := strings.Index(out, "create view public.active_users")
	iMat := strings.Index(out, "create materialized view public.user_summaries")
	iMirror := strings.Index(out, "create materialized view other.user_summaries")
	for _, c := range []struct {
		what string
		at   int
	}{{"the table", iTable}, {"the view", iView},
		{"the materialized view", iMat}, {"the mirrored materialized view", iMirror}} {
		if c.at < 0 {
			t.Fatalf("%s is missing from the plan:\n%s", c.what, out)
		}
	}
	if !(iTable < iView && iView < iMat) {
		t.Errorf("operations are out of dependency order across packages "+
			"(table@%d view@%d matview@%d):\n%s", iTable, iView, iMat, out)
	}
	if !(iTable < iMirror) {
		t.Errorf("the mirrored materialized view is planned before the table it reads:\n%s", out)
	}

	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")
	p.MustRun("check", "--generated")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the multi-package project did not converge:\n%s", out)
	}

	// Each relation's code is generated into the package that declares it, and
	// nowhere else.
	for _, c := range []struct{ dir, want string }{
		{"accounts", "Users *orm.Repo[User]"},
		{"reporting", "ActiveUsers *orm.ViewRepo[ActiveUser]"},
		{"analytics", "UserSummaries *orm.MaterializedViewRepo[UserSummary]"},
		{"mirror", "UserSummaries *orm.MaterializedViewRepo[UserSummary]"},
	} {
		gen := readFile(t, filepath.Join(p.Dir, c.dir, "orm_db.gen.go"))
		if !strings.Contains(strings.Join(strings.Fields(gen), " "), strings.Join(strings.Fields(c.want), " ")) {
			t.Errorf("%s does not declare %q:\n%s", c.dir, c.want, gen)
		}
	}
	// A package does not gain another package's relations.
	for _, c := range []struct{ dir, forbidden string }{
		{"accounts", "ActiveUser"},
		{"accounts", "UserSummary"},
		{"reporting", "UserSummaries"},
		{"analytics", "ActiveUsers"},
	} {
		gen := readFile(t, filepath.Join(p.Dir, c.dir, "orm_db.gen.go"))
		if strings.Contains(gen, c.forbidden) {
			t.Errorf("%s gained %q, which another package declares:\n%s", c.dir, c.forbidden, gen)
		}
	}
}

// The two same-named materialized views are two relations, everywhere.
func TestCompatPkg_sameBasenameAcrossSchemasStaysDistinct(t *testing.T) {
	p := compatPkgProject(t)
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	// The catalog holds both, each with its own index of the shared name.
	rel := p.Query(`SELECT n.nspname || '.' || c.relname
	                  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	                 WHERE c.relkind = 'm' ORDER BY 1`)
	if len(rel) != 2 || rel[0] != "other.user_summaries" || rel[1] != "public.user_summaries" {
		t.Fatalf("the catalog holds %v, want both schemas' relations", rel)
	}
	idx := p.Query(`SELECT n.nspname || '.' || i.relname
	                  FROM pg_class i
	                  JOIN pg_index x ON x.indexrelid = i.oid
	                  JOIN pg_class r ON r.oid = x.indrelid
	                  JOIN pg_namespace n ON n.oid = i.relnamespace
	                 WHERE r.relkind = 'm' AND i.relname = 'summary_id_key' ORDER BY 1`)
	if len(idx) != 2 {
		t.Fatalf("the shared index name resolved to %v, want one in each schema", idx)
	}

	// Dropping one relation's index leaves the other's alone — the defect a
	// name-only owner resolver produces, seen from outside.
	oidPublic := relationOID(t, p, "public.user_summaries")
	oidOther := relationOID(t, p, "other.user_summaries")
	if oidPublic == oidOther {
		t.Fatal("both declarations resolved to one relation")
	}

	// Changing the index set of one of them plans an index change against that
	// relation only, and leaves its namesake in the other schema untouched. This
	// is the cross-schema owner defect seen from outside the resolver.
	writeFile(t, filepath.Join(p.Dir, "analytics", "entities.go"),
		strings.Replace(compatPkgAnalytics,
			"//orm:index summary_email_idx (Email)\n",
			"//orm:index summary_email_idx (Email, Verified)\n", 1))
	out := p.MustRun("makemigrations", "--name", "widen")
	if strings.Contains(out, "No schema changes detected") {
		t.Fatalf("changing one relation's index planned nothing:\n%s", out)
	}
	if strings.Contains(out, "other.user_summaries") {
		t.Errorf("changing public.user_summaries planned work against other.user_summaries, "+
			"which nothing declared differently:\n%s", out)
	}
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the cross-schema project did not converge after the change:\n%s", out)
	}

	// Neither relation was recreated, and the mirror's index still has its
	// original shape.
	if got := relationOID(t, p, "public.user_summaries"); got != oidPublic {
		t.Errorf("public.user_summaries was recreated: OID %d -> %d", oidPublic, got)
	}
	if got := relationOID(t, p, "other.user_summaries"); got != oidOther {
		t.Errorf("other.user_summaries was recreated by a change to its namesake: OID %d -> %d",
			oidOther, got)
	}
	defs := p.Query(`SELECT schemaname || ': ' || indexdef FROM pg_indexes
	                  WHERE indexname = 'summary_email_idx' ORDER BY 1`)
	if len(defs) != 2 {
		t.Fatalf("the shared index name resolved to %v", defs)
	}
	for _, d := range defs {
		switch {
		case strings.HasPrefix(d, "other: "):
			if strings.Contains(d, "verified") {
				t.Errorf("the mirror's index gained a key from its namesake's change: %s", d)
			}
		case strings.HasPrefix(d, "public: "):
			if !strings.Contains(d, "verified") {
				t.Errorf("the changed index did not gain its new key: %s", d)
			}
		}
	}
}
