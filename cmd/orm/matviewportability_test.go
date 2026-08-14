package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 G2 C1/C3/C4: the whole supported index surface, on PostgreSQL 14, and
// byte-identical everywhere.
//
// One fixture serves three questions. Does PG14 accept the complete managed
// index surface the project claims? Do the portable artifacts — the fingerprint,
// the generated Go, the lock — come out the same whichever major produced them?
// And is the qualifying index chosen by a stable rule rather than by whatever
// the catalog or the scanner happened to hand over first?

// surfaceEntities declares one materialized view carrying every supported index
// form, with the index block supplied so the order can be perturbed.
func surfaceEntities(indexes string) string {
	return `package domain

import "github.com/AlexAli29/orm"

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:table public.docs
type Doc struct {
	ID       int64          ` + "`orm:\"pk,identity\"`" + `
	OwnerID  int64          ` + "`orm:\"column:owner_id\"`" + `
	Title    string
	Score    int64
	Tags     []string
	Settings map[string]any ` + "`orm:\"pgtype:jsonb\"`" + `
	Doc      orm.TSVector   ` + "`orm:\"pgtype:tsvector\"`" + `
}

//orm:materialized-view public.doc_rollup
//orm:definition ` + "`SELECT d.id, d.owner_id, d.title, d.score, d.tags, d.settings, d.doc FROM docs d`" + `
//orm:depends-on public.docs
` + indexes + `type DocRollupRow struct {
	ID       int64
	OwnerID  int64          ` + "`orm:\"column:owner_id\"`" + `
	Title    string
	Score    int64
	Tags     []string
	Settings map[string]any ` + "`orm:\"pgtype:jsonb\"`" + `
	Doc      orm.TSVector   ` + "`orm:\"pgtype:tsvector\"`" + `
}
`
}

// The complete surface. Two of these qualify for a concurrent refresh — a plain
// unique index over plain columns — so selection is exercised as well as
// filtering.
const surfaceIndexes = "" +
	// Qualifying: plain, unique, no predicate. Two of them, deliberately.
	"//orm:index zzz_doc_id_key (ID) unique\n" +
	"//orm:index aaa_doc_id_key (ID) unique\n" +
	// Non-qualifying, one of each disqualifying reason.
	"//orm:index doc_owner_partial_key (OwnerID) unique where \"score > 0\"\n" +
	"//orm:index doc_lower_title_key (\"lower(title)\") unique\n" +
	// The rest of the surface, none of it unique.
	"//orm:index doc_multi_idx (OwnerID, Score desc nulls last)\n" +
	"//orm:index doc_score_asc_idx (Score, ID desc)\n" +
	"//orm:index doc_nulls_first_idx (Score nulls first)\n" +
	"//orm:index doc_tags_gin_idx (Tags) using gin\n" +
	"//orm:index doc_doc_gist_idx (Doc) using gist\n" +
	"//orm:index doc_score_brin_idx (Score) using brin\n" +
	"//orm:index doc_covering_idx (OwnerID) include (Title)\n" +
	"//orm:index doc_partial_idx (Title) where \"score > 10\"\n" +
	"//orm:index doc_expr_idx (\"upper(title)\")\n" +
	"//orm:index doc_opclass_idx (Title text_pattern_ops)\n"

// portableArtifacts is what must be identical whatever server produced it.
type portableArtifacts struct {
	lock      string
	generated string
	migration string
}

func capturePortable(t *testing.T, adminDSN, indexes string) portableArtifacts {
	t.Helper()
	// The major is chosen by pointing the admin DSN at that server, so each run
	// gets a throwaway database of its own. Rewriting the project's DSN to a
	// shared database instead makes every run inherit the last one's relations.
	t.Setenv("ORM_TEST_ADMIN_DSN", adminDSN)
	p := newProject(t, surfaceEntities(indexes))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")

	var mig string
	entries, err := os.ReadDir(filepath.Join(p.Dir, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			mig = readFile(t, filepath.Join(p.Dir, "migrations", e.Name()))
		}
	}
	gen := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
		readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the surface fixture did not converge:\n%s", out)
	}
	return portableArtifacts{
		lock:      readFile(t, filepath.Join(p.Dir, "orm.lock")),
		generated: gen,
		migration: mig,
	}
}

// C1: PostgreSQL 14 accepts the whole surface, and an index-only change
// converges without recreating the relation.
func TestSurface_pg14AcceptsTheWholeIndexSurface(t *testing.T) {
	dsn := os.Getenv("ORM_TEST_DSN_PG14")
	if dsn == "" {
		t.Skip("set ORM_TEST_DSN_PG14 to run the PostgreSQL 14 surface fixture")
	}
	t.Setenv("ORM_TEST_ADMIN_DSN", dsn)
	p := newProject(t, surfaceEntities(surfaceIndexes))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("PG14 did not converge:\n%s", out)
	}

	// Every declared index reached the catalog with the metadata declared.
	conn, err := pgx.Connect(t.Context(), p.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()

	var oidBefore uint32
	if err := conn.QueryRow(t.Context(), `SELECT 'public.doc_rollup'::regclass::oid`).Scan(&oidBefore); err != nil {
		t.Fatal(err)
	}
	rows, err := conn.Query(t.Context(),
		`SELECT indexname, indexdef FROM pg_indexes WHERE tablename = 'doc_rollup'`)
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]string{}
	for rows.Next() {
		var n, d string
		if err := rows.Scan(&n, &d); err != nil {
			t.Fatal(err)
		}
		defs[n] = d
	}
	rows.Close()

	for _, c := range []struct{ name, want string }{
		{"aaa_doc_id_key", "UNIQUE"},
		{"zzz_doc_id_key", "UNIQUE"},
		{"doc_owner_partial_key", "WHERE"},
		{"doc_lower_title_key", "lower"},
		{"doc_multi_idx", "DESC NULLS LAST"},
		{"doc_nulls_first_idx", "NULLS FIRST"},
		{"doc_tags_gin_idx", "gin"},
		{"doc_doc_gist_idx", "gist"},
		{"doc_score_brin_idx", "brin"},
		{"doc_covering_idx", "INCLUDE"},
		{"doc_partial_idx", "WHERE"},
		{"doc_expr_idx", "upper"},
		{"doc_opclass_idx", "text_pattern_ops"},
	} {
		def, ok := defs[c.name]
		if !ok {
			t.Errorf("%s is not in the catalog; PG14 got %v", c.name, keysOf(defs))
			continue
		}
		if !strings.Contains(def, c.want) {
			t.Errorf("%s does not carry %q on PG14:\n%s", c.name, c.want, def)
		}
	}

	// An index-only change converges and leaves the relation alone.
	p.Entities(surfaceEntities(strings.Replace(surfaceIndexes,
		`//orm:index doc_partial_idx (Title) where "score > 10"`,
		`//orm:index doc_partial_idx (Title) where "score > 20"`, 1)))
	p.MustRun("makemigrations", "--name", "widen")
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Errorf("the index-only change did not converge:\n%s", out)
	}
	var oidAfter uint32
	if err := conn.QueryRow(t.Context(), `SELECT 'public.doc_rollup'::regclass::oid`).Scan(&oidAfter); err != nil {
		t.Fatal(err)
	}
	if oidAfter != oidBefore {
		t.Errorf("an index-only change recreated the materialized view: OID %d -> %d",
			oidBefore, oidAfter)
	}
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// capture is one run's portable artifacts, with every field required.
type capture struct {
	major       string
	lock        string
	generated   string
	migration   string
	qualifying  string
	fingerprint string
}

// captureOn runs the whole workflow on one server and returns its artifacts,
// failing loudly if any of them is missing or empty.
//
// The existence gate is the point. The previous attempt at this comparison
// compared a populated side against a nearly empty one and reported a difference
// that meant nothing — the same false-green class Part A was built to prevent.
// So nothing is compared until every artifact is known to exist, at a path the
// harness computed rather than discovered, and exactly one of each.
func captureOn(t *testing.T, major, adminDSN, indexes string) capture {
	t.Helper()
	// The major is chosen only by which server the admin DSN names, so each run
	// gets a database of its own and no fixture inherits another's relations.
	t.Setenv("ORM_TEST_ADMIN_DSN", adminDSN)
	p := newProject(t, surfaceEntities(indexes))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	p.MustRun("generate")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("%s did not converge:\n%s", major, out)
	}

	c := capture{major: major}
	c.lock = mustRead(t, major, "orm.lock", filepath.Join(p.Dir, "orm.lock"))
	c.generated = mustRead(t, major, "generated handle",
		filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
		mustRead(t, major, "generated metadata",
			filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))
	c.migration = mustReadOnly(t, major, filepath.Join(p.Dir, "migrations"), ".json")

	// The lock's digest is the fingerprint; reading it from the artifact rather
	// than recomputing keeps this comparing what was committed.
	c.fingerprint = c.lock

	// The chosen qualifying index, taken from the generated descriptor rather
	// than guessed at: it is the argument the constructor was given.
	const marker = "NewMaterializedViewRepo("
	k := strings.Index(c.generated, marker)
	if k < 0 {
		t.Fatalf("%s: the generated code constructs no materialized-view repository", major)
	}
	rest := c.generated[k:]
	lo := strings.Index(rest, `"`)
	hi := strings.Index(rest[lo+1:], `"`)
	if lo < 0 || hi < 0 {
		t.Fatalf("%s: no qualifying index name in %.120s", major, rest)
	}
	c.qualifying = rest[lo+1 : lo+1+hi]
	if c.qualifying == "" {
		t.Fatalf("%s: the generated descriptor names no qualifying index, but the fixture "+
			"declares two that qualify", major)
	}
	return c
}

// mustRead requires a file to exist and to be non-empty.
func mustRead(t *testing.T, major, what, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %s is missing at %s: %v", major, what, path, err)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		t.Fatalf("%s: %s at %s is empty; comparing it would prove nothing", major, what, path)
	}
	return string(b)
}

// mustReadOnly requires exactly one matching artifact in a directory.
func mustReadOnly(t *testing.T, major, dir, suffix string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: no migrations directory at %s: %v", major, dir, err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			found = append(found, e.Name())
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: expected exactly one %s artifact in %s, found %v", major, suffix, dir, found)
	}
	return mustRead(t, major, "migration", filepath.Join(dir, found[0]))
}

// majorDSNs returns the servers the environment offers.
func majorDSNs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, v := range []string{"14", "15", "16", "17", "18"} {
		if dsn := os.Getenv("ORM_TEST_DSN_PG" + v); dsn != "" {
			out["PG"+v] = dsn
		}
	}
	return out
}

// C3: the portable artifacts are the same bytes on every supported major.
func TestSurface_portableAcrossMajors(t *testing.T) {
	dsns := majorDSNs(t)
	if len(dsns) < 2 {
		t.Skip("set ORM_TEST_DSN_PG14..PG18 to compare across majors")
	}
	majors := make([]string, 0, len(dsns))
	for m := range dsns {
		majors = append(majors, m)
	}
	sort.Strings(majors)

	var ref capture
	for _, m := range majors {
		got := captureOn(t, m, dsns[m], surfaceIndexes)
		t.Logf("%s: qualifying=%s lock=%d bytes generated=%d bytes migration=%d bytes",
			m, got.qualifying, len(got.lock), len(got.generated), len(got.migration))
		if ref.major == "" {
			ref = got
			continue
		}
		if got.qualifying != ref.qualifying {
			t.Errorf("%s chose %s and %s chose %s: the qualifying index depends on the server",
				ref.major, ref.qualifying, m, got.qualifying)
		}
		if got.fingerprint != ref.fingerprint {
			t.Errorf("%s and %s produced different fingerprints:\n%s",
				ref.major, m, diffText(ref.fingerprint, got.fingerprint))
		}
		if got.generated != ref.generated {
			t.Errorf("%s and %s produced different generated Go:\n%s",
				ref.major, m, diffText(ref.generated, got.generated))
		}
		if got.migration != ref.migration {
			t.Errorf("%s and %s produced different migrations:\n%s",
				ref.major, m, diffText(ref.migration, got.migration))
		}
	}

	// Nothing a server or a machine could have contributed.
	for _, forbidden := range []string{"/tmp/", "/home/", "server_version", "Canonical", "oid"} {
		if strings.Contains(ref.lock+ref.generated+ref.migration, forbidden) {
			t.Errorf("the portable artifacts contain %q", forbidden)
		}
	}
}

// C4: the same, under reversed declaration order on one server.
func TestSurface_qualifyingSelectionIsOrderIndependent(t *testing.T) {
	dsns := majorDSNs(t)
	dsn, ok := dsns["PG16"]
	if !ok {
		t.Skip("set ORM_TEST_DSN_PG16")
	}
	forward := captureOn(t, "forward", dsn, surfaceIndexes)

	lines := strings.Split(strings.TrimRight(surfaceIndexes, "\n"), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	reversed := captureOn(t, "reversed", dsn, strings.Join(lines, "\n")+"\n")

	if reversed.qualifying != forward.qualifying {
		t.Errorf("reversing the declarations changed the qualifying index: %s -> %s. "+
			"Selection follows encounter order rather than a stable rule",
			forward.qualifying, reversed.qualifying)
	}
	// The fixture declares zzz before aaa on purpose, so the lexically first is
	// not the first encountered: choosing aaa is the rule being explicit.
	if forward.qualifying != "aaa_doc_id_key" {
		t.Errorf("the qualifying index is %s; the stable rule is the lexically first "+
			"qualifying name, and the fixture declares zzz_doc_id_key first to prove it",
			forward.qualifying)
	}
	if reversed.generated != forward.generated {
		t.Errorf("reversing the declarations changed the generated Go:\n%s",
			diffText(forward.generated, reversed.generated))
	}
	if reversed.lock != forward.lock {
		t.Errorf("reversing the declarations changed the lock:\n%s",
			diffText(forward.lock, reversed.lock))
	}
	if reversed.migration != forward.migration {
		t.Errorf("reversing the declarations changed the migration:\n%s",
			diffText(forward.migration, reversed.migration))
	}
}
