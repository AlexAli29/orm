package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// M16.5 compatibility: the frozen materialized-view work, on every supported
// server, through the commands a user actually runs.
//
// The portability harness compares artifacts across majors, which proves the
// bytes agree. It does not prove the workflow works: it captures once per server
// and compares. What is proven here is the whole loop on each major — generate,
// migrate, check, read rows back through the generated API, refresh, refresh
// concurrently, change an index, converge — because a server can produce
// identical artifacts and still reject the statements that build them.
//
// Nothing here may skip. A compatibility claim about five majors that quietly
// tested three is the false-green this milestone kept finding, so the servers
// are required rather than discovered.

// compatEntities is one declaration set carrying the frozen supported surface:
// a table, an ordinary view over it, and a materialized view over that, with the
// scalar types the project claims and the index shapes it supports.
//
// It is deliberately one fixture rather than several. Each dimension gets its
// own assertions, but they run against the same relations, so a server that
// accepts the types and rejects the indexes is caught by the same workflow that
// proves the rest.
func compatEntities(indexes string) string {
	return `package domain

import (
	"time"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgtype"
)

//orm:table public.accounts
type Account struct {
	ID        int64          ` + "`orm:\"pk,identity\"`" + `
	Email     string
	Active    bool
	Rank      int32
	Score     float64
	Balance   pgtype.Numeric ` + "`orm:\"pgtype:numeric\"`" + `
	CreatedAt time.Time
	Tags      []string
	Settings  map[string]any ` + "`orm:\"pgtype:jsonb\"`" + `
	Doc       orm.TSVector   ` + "`orm:\"pgtype:tsvector\"`" + `
}

//orm:view public.active_accounts
//orm:definition ` + "`SELECT id, email, rank, score, balance, created_at, tags, settings, doc FROM accounts WHERE active`" + `
//orm:depends-on public.accounts
type ActiveAccount struct {
	ID        int64
	Email     string
	Rank      int32
	Score     float64
	Balance   pgtype.Numeric ` + "`orm:\"pgtype:numeric\"`" + `
	CreatedAt time.Time
	Tags      []string
	Settings  map[string]any ` + "`orm:\"pgtype:jsonb\"`" + `
	Doc       orm.TSVector   ` + "`orm:\"pgtype:tsvector\"`" + `
}

//orm:materialized-view public.account_summaries
//orm:definition ` + "`SELECT id, email, rank, score, balance, created_at, tags, settings, doc FROM active_accounts`" + `
//orm:depends-on public.active_accounts
` + indexes + `type AccountSummary struct {
	ID        int64
	Email     string
	Rank      int32
	Score     float64
	Balance   pgtype.Numeric ` + "`orm:\"pgtype:numeric\"`" + `
	CreatedAt time.Time
	Tags      []string
	Settings  map[string]any ` + "`orm:\"pgtype:jsonb\"`" + `
	Doc       orm.TSVector   ` + "`orm:\"pgtype:tsvector\"`" + `
}
`
}

// compatConfig writes an orm.yaml carrying the configured numeric mapping.
//
// numeric has no built-in Go mapping on purpose: there is no lossless built-in
// type for an arbitrary-precision decimal, and the project refuses with E013
// rather than silently rounding money into a float64. Compatibility for numeric
// therefore means compatibility of the configured-type path, so the fixture
// configures one — with a type already in the module graph, so the evidence does
// not depend on a dependency nobody else has.
func compatConfig(t *testing.T, p *project) {
	t.Helper()
	writeFile(t, p.Conf, "version: 1\n\nschema:\n  mode: managed\n  dsn: \""+p.DSN+"\"\n"+
		"  search_path:\n    - public\n\nmigrations:\n  dir: migrations\n\n"+
		"types:\n  numeric:\n    go: github.com/jackc/pgx/v5/pgtype.Numeric\n    codec: decimal\n\n"+
		"packages:\n  - path: ./domain\n    output: same\n")
}

// compatIndexes is the supported index surface on the materialized view, with
// exactly one shape that qualifies for a concurrent refresh.
const compatIndexes = "" +
	"//orm:index acct_id_key (ID) unique\n" +
	"//orm:index acct_rank_idx (Rank, Score desc nulls last)\n" +
	"//orm:index acct_tags_gin_idx (Tags) using gin\n" +
	"//orm:index acct_doc_gist_idx (Doc) using gist\n" +
	"//orm:index acct_score_brin_idx (Score) using brin\n" +
	"//orm:index acct_cover_idx (Rank) include (Email)\n" +
	"//orm:index acct_partial_idx (Email) where \"rank > 0\"\n" +
	"//orm:index acct_expr_idx (\"lower(email)\")\n" +
	"//orm:index acct_opclass_idx (Email text_pattern_ops)\n"

// compatIndexesChanged is the same surface with exactly one index-only change:
// the partial predicate widens. Nothing else moves.
var compatIndexesChanged = strings.Replace(compatIndexes,
	`//orm:index acct_partial_idx (Email) where "rank > 0"`,
	`//orm:index acct_partial_idx (Email) where "rank > 5"`, 1)

// compatProbe reads rows back through the generated API and refreshes, so the
// workflow is proven through the code a user is given rather than through SQL
// this test wrote.
const compatProbe = `package main

import (
	"context"
	"fmt"
	"os"

	"example.com/managed/domain"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DSN"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()
	db := domain.New(pool)

	if _, err := pool.Exec(ctx, ` + "`" +
	`INSERT INTO accounts (email, active, rank, score, balance, created_at, tags, settings, doc)
	 VALUES ('a@example.com', true, 3, 1.5, 10.25, now(), ARRAY['x','y'], '{"k":1}', to_tsvector('english','alpha')),
	        ('b@example.com', true, 1, 2.5, 20.50, now(), ARRAY['z'],     '{"k":2}', to_tsvector('english','beta')),
	        ('c@example.com', false, 9, 3.5, 30.75, now(), ARRAY[]::text[], '{}',    to_tsvector('english','gamma'))` +
	"`" + `); err != nil {
		panic(err)
	}
	if err := db.AccountSummaries.Refresh(ctx); err != nil {
		panic("refresh: " + err.Error())
	}

	// The read API, on a materialized view, through the ordinary compiler.
	all, err := db.AccountSummaries.Query().All(ctx)
	if err != nil {
		panic("all: " + err.Error())
	}
	fmt.Printf("rows=%d\n", len(all))

	one, err := db.AccountSummaries.Query().
		Where(domain.AccountSummaries.Email.Eq("a@example.com")).One(ctx)
	if err != nil {
		panic("one: " + err.Error())
	}
	fmt.Printf("one_rank=%d one_tags=%d one_balance_valid=%t\n", one.Rank, len(one.Tags), one.Balance.Valid)

	ordered, err := db.AccountSummaries.Query().
		OrderBy(domain.AccountSummaries.Rank.Desc()).Limit(1).All(ctx)
	if err != nil {
		panic("order: " + err.Error())
	}
	fmt.Printf("top_rank=%d\n", ordered[0].Rank)

	filtered, err := db.AccountSummaries.Query().
		Where(domain.AccountSummaries.Rank.Gt(1)).All(ctx)
	if err != nil {
		panic("where: " + err.Error())
	}
	fmt.Printf("gt1=%d\n", len(filtered))

	// A qualifying index exists, so this must reach PostgreSQL and succeed.
	if err := db.AccountSummaries.Refresh(ctx, orm.Concurrently()); err != nil {
		panic("concurrent refresh: " + err.Error())
	}
	fmt.Printf("concurrent=ok\n")

	// JSONB and timestamptz survived the whole chain.
	if one.Settings == nil {
		panic("jsonb lost")
	}
	if one.CreatedAt.IsZero() {
		panic("timestamptz lost")
	}
	fmt.Printf("types=ok\n")
}
`

// requireEveryMajor returns the five supported servers, failing rather than
// skipping when one is missing.
//
// A compatibility claim about PG14 through PG18 that quietly tested three is
// exactly the false-green this milestone kept finding, and the fix is the same
// one the PostGIS job uses: the absence of a prerequisite is a failure.
func requireEveryMajor(t *testing.T) map[string]string {
	t.Helper()
	got := majorDSNs(t)
	var missing []string
	for _, v := range []string{"14", "15", "16", "17", "18"} {
		if got["PG"+v] == "" {
			missing = append(missing, "ORM_TEST_DSN_PG"+v)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the compatibility matrix requires every supported major and %v %s unset. "+
			"Skipping here would report a five-major claim proven by however many servers "+
			"happened to be running", missing,
			map[bool]string{true: "is", false: "are"}[len(missing) == 1])
	}
	return got
}

// The matrix is five majors, and the count is asserted so a row quietly removed
// from the required set is a failure rather than a smaller green run.
//
// requireEveryMajor is the only thing standing between this file and a
// compatibility claim proven by four servers, so what it returns is checked
// rather than trusted.
func TestCompat_theMajorMatrixIsComplete(t *testing.T) {
	got := requireEveryMajor(t)
	want := []string{"PG14", "PG15", "PG16", "PG17", "PG18"}
	if len(got) != len(want) {
		t.Fatalf("the matrix resolved %d servers (%v); the project claims %v",
			len(got), keysOf(got), want)
	}
	for _, w := range want {
		if got[w] == "" {
			t.Errorf("the matrix is missing %s", w)
		}
	}
}

// The whole user workflow, on every supported major.
func TestCompat_userWorkflowOnEveryMajor(t *testing.T) {
	majors := requireEveryMajor(t)
	names := make([]string, 0, len(majors))
	for m := range majors {
		names = append(names, m)
	}
	sort.Strings(names)

	for _, major := range names {
		t.Run(major, func(t *testing.T) {
			// The major is chosen only by which server the admin DSN names, so
			// each run gets a database of its own.
			t.Setenv("ORM_TEST_ADMIN_DSN", majors[major])
			p := newProject(t, compatEntities(compatIndexes))
			compatConfig(t, p)

			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("check")
			p.MustRun("generate")
			p.MustRun("check", "--generated")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Fatalf("%s did not converge after the initial migration:\n%s", major, out)
			}

			// The server really built what was declared.
			serverMajor := p.Query(`SELECT (current_setting('server_version_num')::int / 10000)::text`)
			if len(serverMajor) != 1 || "PG"+serverMajor[0] != major {
				t.Fatalf("%s: the server reports major %v, so this row tested the wrong server",
					major, serverMajor)
			}

			// Reading, refreshing and refreshing concurrently, through the
			// generated API rather than through SQL written here.
			writeFile(t, filepath.Join(p.Dir, "main.go"), compatProbe)
			got := runProgram(t, p)
			for _, want := range []string{
				"rows=2", "one_rank=3", "one_tags=2", "one_balance_valid=true",
				"top_rank=3", "gt1=1", "concurrent=ok", "types=ok",
			} {
				if !strings.Contains(got, want) {
					t.Errorf("%s: the generated API did not produce %q:\n%s", major, want, got)
				}
			}

			// An index-only change: planned, applied, converged, and the
			// relation is the same relation afterwards.
			before := relationOID(t, p, "public.account_summaries")
			p.Entities(compatEntities(compatIndexesChanged))
			out := p.MustRun("makemigrations", "--name", "widen")
			if strings.Contains(out, "No schema changes detected") {
				t.Fatalf("%s planned nothing for an index change:\n%s", major, out)
			}
			if strings.Contains(out, "materialized view") {
				t.Errorf("%s: an index-only change touched the relation:\n%s", major, out)
			}
			p.MustRun("migrate")
			p.MustRun("check")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Errorf("%s did not converge after the index change:\n%s", major, out)
			}
			if after := relationOID(t, p, "public.account_summaries"); after != before {
				t.Errorf("%s recreated the materialized view for an index change: OID %d -> %d",
					major, before, after)
			}
		})
	}
}

// Every index shape reached the catalog with the metadata declared, on every
// major — read from pg_am and pg_indexes rather than from the plan's own words.
func TestCompat_indexSurfaceReachesEveryCatalog(t *testing.T) {
	majors := requireEveryMajor(t)
	names := make([]string, 0, len(majors))
	for m := range majors {
		names = append(names, m)
	}
	sort.Strings(names)

	want := map[string]string{
		"acct_id_key":         "btree",
		"acct_rank_idx":       "btree",
		"acct_tags_gin_idx":   "gin",
		"acct_doc_gist_idx":   "gist",
		"acct_score_brin_idx": "brin",
		"acct_cover_idx":      "btree",
		"acct_partial_idx":    "btree",
		"acct_expr_idx":       "btree",
		"acct_opclass_idx":    "btree",
	}
	for _, major := range names {
		t.Run(major, func(t *testing.T) {
			t.Setenv("ORM_TEST_ADMIN_DSN", majors[major])
			p := newProject(t, compatEntities(compatIndexes))
			compatConfig(t, p)
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			p.MustRun("check")

			got := map[string]string{}
			for _, row := range p.Query(`
				SELECT i.relname || '=' || am.amname
				  FROM pg_class i
				  JOIN pg_index x  ON x.indexrelid = i.oid
				  JOIN pg_class r  ON r.oid = x.indrelid
				  JOIN pg_am    am ON am.oid = i.relam
				 WHERE r.relname = 'account_summaries' AND r.relkind = 'm'`) {
				k, v, _ := strings.Cut(row, "=")
				got[k] = v
			}
			for name, method := range want {
				if got[name] == "" {
					t.Errorf("%s: %s is not in the catalog; present: %v", major, name, keysOf(got))
					continue
				}
				if got[name] != method {
					t.Errorf("%s: %s uses %q, want %q", major, name, got[name], method)
				}
			}

			// And the shapes that are not access methods.
			defs := map[string]string{}
			for _, row := range p.Query(
				`SELECT indexname || '|~|' || indexdef FROM pg_indexes WHERE tablename = 'account_summaries'`) {
				k, v, _ := strings.Cut(row, "|~|")
				defs[k] = v
			}
			for _, c := range []struct{ name, want string }{
				{"acct_id_key", "UNIQUE"},
				{"acct_rank_idx", "DESC NULLS LAST"},
				{"acct_cover_idx", "INCLUDE"},
				{"acct_partial_idx", "WHERE"},
				{"acct_expr_idx", "lower"},
				{"acct_opclass_idx", "text_pattern_ops"},
			} {
				if !strings.Contains(defs[c.name], c.want) {
					t.Errorf("%s: %s does not carry %q:\n%s", major, c.name, c.want, defs[c.name])
				}
			}
		})
	}
}

var _ = fmt.Sprintf

// The compatibility project's portable artifacts are the same bytes on every
// supported major.
//
// The frozen portability harness proves this for the index surface. This one
// carries the type surface as well — numeric through a configured mapping, jsonb,
// a text array, timestamptz, tsvector — because a type whose rendering differed
// between majors would move the generated code and the lock without touching an
// index at all.
func TestCompat_portableArtifactsAcrossEveryMajor(t *testing.T) {
	majors := requireEveryMajor(t)
	names := make([]string, 0, len(majors))
	for m := range majors {
		names = append(names, m)
	}
	sort.Strings(names)

	type capture struct{ major, lock, gen, mig string }
	var ref capture
	for _, major := range names {
		t.Setenv("ORM_TEST_ADMIN_DSN", majors[major])
		p := newProject(t, compatEntities(compatIndexes))
		compatConfig(t, p)
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("check")
		p.MustRun("generate")
		if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
			t.Fatalf("%s did not converge:\n%s", major, out)
		}

		got := capture{major: major}
		got.lock = mustRead(t, major, "orm.lock", filepath.Join(p.Dir, "orm.lock"))
		got.gen = mustRead(t, major, "generated handle",
			filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
			mustRead(t, major, "generated metadata",
				filepath.Join(p.Dir, "domain", "orm_meta.gen.go")) +
			mustRead(t, major, "generated tables",
				filepath.Join(p.Dir, "domain", "orm_tables.gen.go"))
		got.mig = mustReadOnly(t, major, filepath.Join(p.Dir, "migrations"), ".json")

		t.Logf("%s: lock=%d generated=%d migration=%d bytes",
			major, len(got.lock), len(got.gen), len(got.mig))
		// Two empty artifacts compare equal. The existence gates above are what
		// stop that, so their answer is checked here as well rather than
		// trusted: a harness that returned nothing would otherwise report five
		// identical majors.
		for _, c := range []struct {
			what string
			n    int
			min  int
		}{{"lock", len(got.lock), 50}, {"generated Go", len(got.gen), 2000},
			{"migration", len(got.mig), 2000}} {
			if c.n < c.min {
				t.Fatalf("%s: the %s is %d bytes, which is too small to be the real artifact; "+
					"comparing it across majors would prove nothing", major, c.what, c.n)
			}
		}

		if ref.major == "" {
			ref = got
			continue
		}
		if got.lock != ref.lock {
			t.Errorf("%s and %s produced different locks:\n%s",
				ref.major, major, diffText(ref.lock, got.lock))
		}
		if got.gen != ref.gen {
			t.Errorf("%s and %s produced different generated Go:\n%s",
				ref.major, major, diffText(ref.gen, got.gen))
		}
		if got.mig != ref.mig {
			t.Errorf("%s and %s produced different migrations:\n%s",
				ref.major, major, diffText(ref.mig, got.mig))
		}
	}

	// Nothing a server, a database or a machine could have contributed.
	all := ref.lock + ref.gen + ref.mig
	for _, forbidden := range []string{
		"server_version", "Canonical", "relfilenode", "populated", "relispopulated",
		"/tmp/", "/home/", "orm_test_", "localhost", "postgres://",
	} {
		if strings.Contains(all, forbidden) {
			t.Errorf("the portable artifacts contain %q", forbidden)
		}
	}
	// An OID would be a bare number, so it is looked for as the word rather
	// than as a digit that could legitimately be a type modifier.
	if strings.Contains(strings.ToLower(all), "\"oid\"") || strings.Contains(all, " oid ") {
		t.Error("the portable artifacts mention an OID")
	}
}
