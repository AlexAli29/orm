package uuidcompat_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The ways this qualification could report success without having run.
//
// Every check below is about the suite rather than about uuid, and that is the
// point: a proof is worth what its gates are worth. A suite that skips when the
// database is missing reports green on a machine with no PostgreSQL. A fixture
// that lost its unmatched row still passes the test named after it. A
// compile-negative kept in testdata proves nothing at all.
//
// Each of these is checked by reading the suite rather than by running it,
// because running it is exactly what the failure modes prevent.

func suiteSources(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	ms, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) < 8 {
		t.Fatalf("found %d test files, which is fewer than this module has; the glob "+
			"below is not seeing the suite", len(ms))
	}
	for _, m := range ms {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		out[m] = string(b)
	}
	return out
}

// A missing DSN fails. It does not skip.
func TestFalseGreen_missingDSNFails(t *testing.T) {
	src := suiteSources(t)["uuid_test.go"]
	if src == "" {
		t.Fatal("uuid_test.go is not in the suite any more")
	}
	if !strings.Contains(src, "t.Fatal") || !strings.Contains(src, "UUIDCOMPAT_DSN") {
		t.Error("the DSN helper no longer fails on a missing UUIDCOMPAT_DSN")
	}
	if skipCall.MatchString(src) {
		t.Error("uuid_test.go contains a skip; a qualification that quietly runs " +
			"nothing is the false green this module exists to prevent")
	}
}

// skipCall matches a call rather than the words, so that this file's own
// description of what it forbids does not trip it.
var skipCall = regexp.MustCompile(`(?m)^\s+t\.Skip(f|Now)?\(`)

// Nothing in the suite skips, anywhere.
func TestFalseGreen_nothingSkips(t *testing.T) {
	for name, src := range suiteSources(t) {
		if loc := skipCall.FindStringIndex(src); loc != nil {
			i := loc[0]
			line := 1 + strings.Count(src[:i], "\n")
			t.Errorf("%s:%d skips; every case here must fail rather than skip, "+
				"because a skipped case and a passing one look identical in a summary",
				name, line)
		}
	}
}

// The DSN helper is actually reached by every test that needs a database.
//
// A test that opens a pool directly from the environment would run against
// whatever was there and pass with no fixture at all.
func TestFalseGreen_databaseTestsGoThroughTheGuardedHelpers(t *testing.T) {
	for name, src := range suiteSources(t) {
		if name == "uuid_test.go" || name == "falsegreen_test.go" {
			continue // the helpers themselves
		}
		if strings.Contains(src, "os.Getenv(\"UUIDCOMPAT_DSN\")") &&
			!strings.Contains(src, "const refreshProbe") {
			t.Errorf("%s reads UUIDCOMPAT_DSN directly instead of going through dsn(t), "+
				"so it would not fail when the variable is missing", name)
		}
	}
}

// The LEFT JOIN fixture contains the rows its claim needs.
//
// Without an unmatched row there is no NULL to distinguish; without a zero-UUID
// row there is nothing the NULL could be confused with. Either omission leaves
// a test that passes and proves half of nothing.
func TestFalseGreen_leftJoinFixtureHasBothCases(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	_, unmatched, zero, _ := leftJoinFixture(t, db)

	var orders int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM orders WHERE user_id = $1`, unmatched).Scan(&orders); err != nil {
		t.Fatal(err)
	}
	if orders != 0 {
		t.Errorf("the unmatched user has %d orders; the fixture has no unmatched row "+
			"and the LEFT JOIN case is not being exercised", orders)
	}

	var zeros int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM orders WHERE user_id = $1 AND id = '00000000-0000-0000-0000-000000000000'`,
		zero).Scan(&zeros); err != nil {
		t.Fatal(err)
	}
	if zeros != 1 {
		t.Errorf("the fixture has %d rows whose order id is the zero UUID, want 1: "+
			"without one, NULL has nothing to be confused with", zeros)
	}
}

// The COPY path is executed rather than described.
//
// A test that named CopyFrom and then asserted nothing about the database would
// pass whether or not the call did anything, so this runs one and watches the
// row count move.
func TestFalseGreen_copyPathRunsAndWritesRows(t *testing.T) {
	if src := suiteSources(t)["uuid_test.go"]; !strings.Contains(src, "CopyFrom") {
		t.Fatal("no COPY call is made anywhere in the suite")
	}

	pool, db := open(t)
	reset(t, pool)
	before := countRows(t, pool, "users")

	rows := make([]domain.User, 0, 8)
	for range 8 {
		rows = append(rows, domain.User{
			ID: uuid.New(), Email: uuid.NewString(),
			ExternalID: uuid.New(), Tags: []uuid.UUID{},
		})
	}
	copied, err := db.Users.CopyFrom(t.Context(), rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if copied != int64(len(rows)) {
		t.Errorf("COPY reported %d rows, want %d", copied, len(rows))
	}
	if after := countRows(t, pool, "users"); after != before+len(rows) {
		t.Errorf("the row count went from %d to %d; COPY reported success without "+
			"writing", before, after)
	}
}

// The concurrent-refresh evidence actually sends SQL.
//
// The staleness tests count statements, and a probe that never ran would count
// zero for every case — which is the answer one of them expects. So the
// success case, where the count must be one, is what proves the counter works.
func TestFalseGreen_refreshEvidenceCountsRealStatements(t *testing.T) {
	src := suiteSources(t)["staleness_test.go"]
	if !strings.Contains(src, "want exactly 1") {
		t.Error("no case asserts that a successful refresh sends exactly one " +
			"statement; without it, a probe that never ran would satisfy every " +
			"other assertion by counting zero")
	}
	if !strings.Contains(src, "55000") {
		t.Error("no case pins PostgreSQL's SQLSTATE for the stale-positive refusal")
	}
}

// The compile-negatives are compiled.
func TestFalseGreen_compileNegativesAreCompiled(t *testing.T) {
	src := suiteSources(t)["compile_test.go"]
	if !strings.Contains(src, `exec.Command("go", "build"`) {
		t.Error("the compile-negative fixtures are not handed to a compiler; a snippet " +
			"nobody builds proves that somebody wrote a snippet")
	}
	if !strings.Contains(src, "this compiled, and it must not") {
		t.Error("nothing fails when a negative fixture builds")
	}
	if len(compileNegatives) == 0 {
		t.Error("there are no compile-negative fixtures")
	}
}

// The cross-major capture refuses an artifact that is missing or empty.
func TestFalseGreen_crossMajorCaptureRefusesEmptyArtifacts(t *testing.T) {
	src := suiteSources(t)["crossmajor_test.go"]
	if src == "" {
		t.Fatal("the cross-major capture is not in the suite")
	}
	for _, want := range []string{"is empty", "missing"} {
		if !strings.Contains(src, want) {
			t.Errorf("the capture does not refuse an artifact that %q; a comparison "+
				"between two absent files is identical and means nothing", want)
		}
	}
}

// Every major has to be configured, and the matrix refuses to run on fewer.
func TestFalseGreen_theFiveMajorMatrixCannotRunShort(t *testing.T) {
	src := suiteSources(t)["crossmajor_test.go"]
	for _, v := range []string{"PG14", "PG15", "PG16", "PG17", "PG18"} {
		if !strings.Contains(src, v) {
			t.Errorf("the matrix does not name %s", v)
		}
	}
	if !strings.Contains(src, "t.Fatal") {
		t.Error("the matrix does not fail when a server is missing")
	}
}

// The ORM module still does not depend on google/uuid.
//
// This is the boundary the whole configured-mapping design exists to keep, and
// it is checked here as well as in CI so that a developer who never pushes
// still finds out.
func TestFalseGreen_theORMDoesNotDependOnGoogleUUID(t *testing.T) {
	for _, f := range []string{"../go.mod", "../go.sum"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "google/uuid") {
			t.Errorf("%s mentions google/uuid; every project importing the ORM would "+
				"acquire it, which is the thing configured mappings exist to avoid", f)
		}
	}
	// And this module does depend on it, or the qualification is not qualifying
	// a third-party uuid type at all.
	b, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "github.com/google/uuid") {
		t.Error("this module no longer depends on google/uuid")
	}
}

// The suite is not empty, and the count is stated so that a collapse is visible.
func TestFalseGreen_theSuiteRunsWhatItClaims(t *testing.T) {
	re := regexp.MustCompile(`(?m)^func (Test\w+)\(`)
	names := map[string]bool{}
	for _, src := range suiteSources(t) {
		for _, m := range re.FindAllStringSubmatch(src, -1) {
			names[m[1]] = true
		}
	}
	const atLeast = 30
	if len(names) < atLeast {
		t.Errorf("the suite defines %d tests, want at least %d: a qualification that "+
			"shrank without anybody noticing is the same failure as one that skipped",
			len(names), atLeast)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}

// goBuildWorks is a sanity check that the negative-fixture machinery can build
// something correct, so that "it did not build" is evidence about the fixture.
func TestFalseGreen_theCompilerHarnessCanBuildAValidFixture(t *testing.T) {
	dir := filepath.Join("negativeprobe", "sanity")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll("negativeprobe") })
	src := `package sanity

import (
	"example.com/uuidcompat/domain"
	"github.com/google/uuid"
)

func valid() { _ = domain.Users.ID.Eq(uuid.New()) }
`
	if err := os.WriteFile(filepath.Join(dir, "sanity.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("go", "build", "./"+filepath.ToSlash(dir)).CombinedOutput()
	if err != nil {
		t.Fatalf("the harness cannot build a correct fixture, so every negative result "+
			"it produces is unattributable:\n%s", out)
	}
}
