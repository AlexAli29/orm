package uuidcompat_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Generated concurrent-refresh eligibility, and the two ways it goes stale.
//
// REFRESH MATERIALIZED VIEW CONCURRENTLY needs a unique index over a non-partial
// set of plain columns. The generator works out whether one exists and writes
// the answer into the descriptor, so the answer is a fact about the schema at
// generation time and the schema keeps moving afterwards. Two things can then be
// true at once, and they fail in opposite directions:
//
//	stale-negative  the database gained a qualifying index; the descriptor has
//	                not been regenerated, so the code refuses locally and sends
//	                nothing. Nothing is broken; something is unavailable.
//
//	stale-positive  the database lost it; the descriptor still says yes, so the
//	                statement goes to PostgreSQL and PostgreSQL refuses it. The
//	                error has to arrive as PostgreSQL's own, not as something
//	                rewritten on the way out.
//
// The index here is over a uuid column, which is the whole reason this is in
// the uuid qualification: eligibility is decided from the index's shape, and a
// uuid key has to qualify exactly as any other plain column would. If uuid ever
// fell out of the eligible set, the stale-negative case would become permanent
// and look exactly like a correctly cautious refusal.
//
// Both halves run against a copy of this module's own declarations in a database
// of their own, because the lifecycle moves the schema underneath the generated
// code and the qualification database has to stay converged.

// The line that gives the materialized view its unique uuid index. Removing it
// from the declarations is how the fixture reaches a state with no qualifying
// index, and it is matched exactly so that renaming the index in entities.go
// fails here rather than silently making both phases identical.
const uuidIndexDirective = "//orm:index user_summaries_key (UserID) unique"

var (
	ormBinOnce sync.Once
	ormBinPath string
	ormBinErr  error
)

// ormBin builds the CLI from the ORM module, once per test binary.
//
// It is built rather than found so that the lifecycle is driven by the code in
// this tree, and it is built in the ORM module because that is where the
// generator's own build dependencies live.
func ormBin(t *testing.T) string {
	t.Helper()
	ormBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "ormbin")
		if err != nil {
			ormBinErr = err
			return
		}
		ormBinPath = filepath.Join(dir, "orm")
		cmd := exec.Command("go", "build", "-o", ormBinPath, "./cmd/orm")
		cmd.Dir = ".."
		if out, err := cmd.CombinedOutput(); err != nil {
			ormBinErr = err
			ormBinPath = string(out)
		}
	})
	if ormBinErr != nil {
		t.Fatalf("building the CLI: %v\n%s", ormBinErr, ormBinPath)
	}
	return ormBinPath
}

// staged is a copy of this module, pointed at a database of its own.
type staged struct {
	dir string
	dsn string
	t   *testing.T
}

// stage copies the module's declarations somewhere writable and prepares an
// empty database for them.
//
// The copy is of the permanent declarations rather than a fixture written for
// the occasion: what goes stale has to be the thing being qualified.
func stage(t *testing.T, dbname string) *staged {
	t.Helper()
	return stageAt(t, dsn(t), dbname)
}

// stageAt is stage against a nominated server, which is what the five-major
// matrix needs: the same declarations, one database per major.
func stageAt(t *testing.T, admin, dbname string) *staged {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	copyModule(t, ".", dir)

	// The module's replace is relative to its place in the tree, and the copy
	// is not in that place.
	gomod := filepath.Join(dir, "go.mod")
	b, err := os.ReadFile(gomod)
	if err != nil {
		t.Fatal(err)
	}
	fixed := strings.Replace(string(b),
		"replace github.com/AlexAli29/orm => ..",
		"replace github.com/AlexAli29/orm => "+root, 1)
	if fixed == string(b) {
		t.Fatal("the copied go.mod has no replace to rewrite; the staged module would " +
			"resolve the ORM from the proxy rather than from this tree")
	}
	if err := os.WriteFile(gomod, []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}

	// The copy takes the declarations, not the history. This module's committed
	// migrations describe a project that already has the index, and the
	// lifecycle here has to start before it — so the staged copy writes its own
	// migrations from the declarations it is given at each phase.
	migrations, err := filepath.Glob(filepath.Join(dir, "migrations", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("the copy has no migrations to clear, which means the layout moved " +
			"and this staged project may not be starting from nothing")
	}
	for _, m := range migrations {
		if err := os.Remove(m); err != nil {
			t.Fatal(err)
		}
	}

	s := &staged{dir: dir, t: t}
	bootstrap := exec.Command("go", "run", "./bootstrap", admin, dbname)
	bootstrap.Dir = dir
	if out, err := bootstrap.CombinedOutput(); err != nil {
		t.Fatalf("preparing %s: %v\n%s", dbname, err, out)
	}
	s.dsn = swapDatabase(admin, dbname)
	return s
}

// serverMajor asks the server which major it is.
func (s *staged) serverMajor() string {
	s.t.Helper()
	return s.runProbe("majorprobe", majorProbe)
}

// workload exercises the uuid surface through the staged module's generated
// code: writes, reads, an array, a nullable, a foreign key, an outer join, a
// projection, the view, the materialized view and a concurrent refresh.
//
// It is a program rather than a set of assertions here because it has to run
// against a database this test process is not otherwise connected to, and
// because the point is that the generated code works — not that a catalog query
// answers.
func (s *staged) workload() {
	s.t.Helper()
	if out := s.runProbe("workloadprobe", workloadProbe); out != "ok" {
		s.t.Fatalf("the workload did not complete: %s", out)
	}
}

// runProbe writes a program into the staged module, runs it, and returns its
// trimmed stdout.
func (s *staged) runProbe(name, src string) string {
	s.t.Helper()
	dir := filepath.Join(s.dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./"+name)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.t.Fatalf("running %s: %v\n%s", name, err, out)
	}
	return strings.TrimSpace(string(out))
}

var dbPath = regexp.MustCompile(`(://[^/?]+)/[^?]*`)

func swapDatabase(dsn, dbname string) string {
	return dbPath.ReplaceAllString(dsn, "${1}/"+dbname)
}

// copyModule copies the Go sources, the config and the migrations. The test
// files are left behind: the staged copy is built and run, never tested, and
// copying them would make it depend on the DSN this suite uses.
func copyModule(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		if strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		switch filepath.Ext(rel) {
		case ".go", ".mod", ".sum", ".yaml", ".json":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), b, 0o644)
	})
	if err != nil {
		t.Fatalf("copying the module: %v", err)
	}
}

func (s *staged) orm(args ...string) (string, error) {
	s.t.Helper()
	cmd := exec.Command(ormBin(s.t), append(args, "--config", "orm.yaml")...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ormConfig runs the CLI against a config other than the module's own.
func (s *staged) ormConfig(config string, args ...string) (string, error) {
	s.t.Helper()
	cmd := exec.Command(ormBin(s.t), append(args, "--config", config)...)
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// exec runs one statement against the staged database.
func (s *staged) exec(sql string) {
	s.t.Helper()
	dir := filepath.Join(s.dir, "execprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	src := strings.Replace(execProbe, "__SQL__", sql, 1)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./execprobe")
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	if out, err := cmd.CombinedOutput(); err != nil {
		s.t.Fatalf("executing %q: %v\n%s", sql, err, out)
	}
}

func (s *staged) mustORM(args ...string) string {
	s.t.Helper()
	out, err := s.orm(args...)
	if err != nil {
		s.t.Fatalf("orm %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// declareIndex adds or removes the unique uuid index on the materialized view.
func (s *staged) declareIndex(present bool) {
	s.t.Helper()
	p := filepath.Join(s.dir, "domain", "entities.go")
	b, err := os.ReadFile(p)
	if err != nil {
		s.t.Fatal(err)
	}
	src := string(b)
	has := strings.Contains(src, uuidIndexDirective)
	switch {
	case present && !has:
		src = strings.Replace(src, "//orm:depends-on public.user_orders",
			"//orm:depends-on public.user_orders\n"+uuidIndexDirective, 1)
	case !present && has:
		src = strings.Replace(src, uuidIndexDirective+"\n", "", 1)
	default:
		return
	}
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// refreshResult is what the staged program reports about one refresh attempt.
type refreshResult struct {
	Err        string `json:"err"`
	Statements int    `json:"statements"`
	SQLState   string `json:"sqlstate"`
	IsPgError  bool   `json:"is_pg_error"`
}

// refresh runs a concurrent refresh through the staged module's own generated
// code and reports what happened, including how many statements reached the
// server.
//
// Counting statements is the point of doing this through a program rather than
// through SQL: "refused locally" and "refused by PostgreSQL" produce an error
// either way, and the only thing that tells them apart from outside is whether
// anything was sent.
func (s *staged) refresh() refreshResult {
	s.t.Helper()
	dir := filepath.Join(s.dir, "refreshprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(refreshProbe), 0o644); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./refreshprobe")
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("running the refresh probe: %v\n%s", err, out)
	}
	var r refreshResult
	if err := json.Unmarshal(out, &r); err != nil {
		s.t.Fatalf("reading the probe's answer: %v\n%s", err, out)
	}
	return r
}

// The descriptor is behind the database: the index exists, the generated code
// does not know, and nothing is sent.
func TestUUID_staleNegativeConcurrentRefreshEligibility(t *testing.T) {
	dsn(t) // fail rather than skip when there is no server
	s := stage(t, "uuidstale_negative")

	// Phase 1: no unique index on the materialized view at all.
	s.declareIndex(false)
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")

	if got := s.refresh(); got.Err == "" {
		t.Fatal("a concurrent refresh succeeded with no unique index declared anywhere")
	} else if got.Statements != 0 {
		t.Errorf("with no qualifying index the descriptor sent %d statements, want 0",
			got.Statements)
	}

	// Phase 2: the database gains the index. The generated code does not.
	s.declareIndex(true)
	s.mustORM("makemigrations")
	s.mustORM("migrate")

	// PostgreSQL would now allow it.
	if !s.serverAllowsConcurrentRefresh() {
		t.Fatal("the migration did not produce an index PostgreSQL accepts for a " +
			"concurrent refresh; the rest of this test would prove nothing")
	}

	got := s.refresh()
	if got.Err == "" {
		t.Fatal("the stale descriptor allowed a concurrent refresh")
	}
	if got.Statements != 0 {
		t.Errorf("the stale descriptor sent %d statements, want 0: it must refuse "+
			"locally rather than ask PostgreSQL", got.Statements)
	}
	if got.IsPgError {
		t.Errorf("the refusal came from PostgreSQL (%s); it should not have got there",
			got.SQLState)
	}

	// check --generated says so, and says it before anything is regenerated.
	out, err := s.orm("check", "--generated")
	if err == nil {
		t.Errorf("check --generated passed while the generated code was behind the "+
			"schema:\n%s", out)
	}
	if !strings.Contains(out, "concurrent") && !strings.Contains(out, "Generated") {
		t.Errorf("check --generated does not report the generated code as stale:\n%s", out)
	}

	// Phase 3: regenerate, and it works.
	before := s.descriptorIndex()
	s.mustORM("generate")
	after := s.descriptorIndex()
	if before == after {
		t.Errorf("the generated descriptor still names %q after regeneration; "+
			"the eligibility change did not reach it", after)
	}
	if after != "user_summaries_key" {
		t.Errorf("the regenerated descriptor names %q as the concurrent-refresh "+
			"index, want user_summaries_key", after)
	}
	s.mustORM("check", "--generated")

	if got := s.refresh(); got.Err != "" {
		t.Errorf("the regenerated descriptor still refuses: %s", got.Err)
	} else if got.Statements != 1 {
		t.Errorf("the successful refresh sent %d statements, want exactly 1",
			got.Statements)
	}
}

// The descriptor is ahead of the database: it says the index qualifies, the
// index is gone, and PostgreSQL's own refusal is what comes back.
func TestUUID_stalePositiveConcurrentRefreshEligibility(t *testing.T) {
	dsn(t)
	s := stage(t, "uuidstale_positive")

	// Start converged, with the index.
	s.declareIndex(true)
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	s.mustORM("generate")
	s.mustORM("check", "--generated")
	if got := s.refresh(); got.Err != "" {
		t.Fatalf("the converged project cannot refresh concurrently: %s", got.Err)
	}

	// The index goes away. The generated code is not told.
	s.declareIndex(false)
	s.mustORM("makemigrations")
	s.mustORM("migrate")
	if s.serverAllowsConcurrentRefresh() {
		t.Fatal("the index is still there after the migration that removed it")
	}

	got := s.refresh()
	if got.Err == "" {
		t.Fatal("a concurrent refresh succeeded with no unique index in the database")
	}
	if got.Statements != 1 {
		t.Errorf("the optimistic descriptor sent %d statements, want 1: it believes "+
			"the index is there, so it must ask", got.Statements)
	}
	if !got.IsPgError {
		t.Errorf("the error is not PostgreSQL's own (%s); a rewritten error loses "+
			"the SQLSTATE and everything a caller could branch on", got.Err)
	}
	if got.SQLState != "55000" {
		t.Errorf("SQLSTATE is %q, want 55000 (object not in prerequisite state): %s",
			got.SQLState, got.Err)
	}

	// And check --generated says the generated code is behind.
	out, err := s.orm("check", "--generated")
	if err == nil {
		t.Errorf("check --generated passed while the descriptor claimed an index "+
			"that no longer exists:\n%s", out)
	}

	// Regenerate: now the refusal is local and nothing is sent.
	s.mustORM("generate")
	s.mustORM("check", "--generated")
	got = s.refresh()
	if got.Err == "" {
		t.Fatal("the regenerated descriptor allowed a concurrent refresh with no index")
	}
	if got.Statements != 0 {
		t.Errorf("the regenerated descriptor sent %d statements, want 0", got.Statements)
	}
	if got.IsPgError {
		t.Errorf("the regenerated descriptor still asked PostgreSQL (%s)", got.SQLState)
	}
}

// serverAllowsConcurrentRefresh asks PostgreSQL rather than the descriptor.
func (s *staged) serverAllowsConcurrentRefresh() bool {
	s.t.Helper()
	dir := filepath.Join(s.dir, "eligprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(eligibilityProbe), 0o644); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./eligprobe")
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("running the eligibility probe: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) == "yes"
}

// descriptorIndex reads the concurrent-refresh index out of the generated code.
func (s *staged) descriptorIndex() string {
	s.t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "domain", "orm_db.gen.go"))
	if err != nil {
		s.t.Fatal(err)
	}
	m := regexp.MustCompile(`NewMaterializedViewRepo\(ex, &userSummaryMeta, "([^"]*)"\)`).
		FindStringSubmatch(string(b))
	if m == nil {
		s.t.Fatalf("the generated code does not construct the materialized view repo "+
			"in the shape this test reads:\n%s", b)
	}
	return m[1]
}

// addPackage appends a package to the staged project's config.
func (s *staged) addPackage(path string) {
	s.t.Helper()
	p := filepath.Join(s.dir, "orm.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		s.t.Fatal(err)
	}
	src := string(b)
	if !strings.Contains(src, "  - path: ./domain\n") {
		s.t.Fatalf("the staged config does not list ./domain in the shape this "+
			"appends to:\n%s", src)
	}
	src = strings.Replace(src, "  - path: ./domain\n",
		"  - path: "+path+"\n    output: same\n  - path: ./domain\n", 1)
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

// columnType asks the staged database what a column's type is.
func (s *staged) columnType(relation, column string) string {
	s.t.Helper()
	dir := filepath.Join(s.dir, "typeprobe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		s.t.Fatal(err)
	}
	src := strings.NewReplacer("__REL__", relation, "__COL__", column).Replace(typeProbe)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./typeprobe")
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(), "UUIDCOMPAT_DSN="+s.dsn)
	out, err := cmd.Output()
	if err != nil {
		s.t.Fatalf("running the type probe: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// appliedCount is how many migrations the staged database records as applied.
//
// check's exit status is ignored on purpose: after a migration PostgreSQL
// refused, the declarations and the database legitimately disagree, and the
// count is exactly what has to be read in that state.
func (s *staged) appliedCount() int {
	s.t.Helper()
	out, _ := s.orm("check")
	m := regexp.MustCompile(`Migrations\s+(\d+) applied`).FindStringSubmatch(out)
	if m == nil {
		s.t.Fatalf("check does not report an applied count in the shape this reads:\n%s", out)
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n
}
