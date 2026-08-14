package gendemo_test

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/observe"
	"github.com/jackc/pgx/v5"
)

func testdbAdmin(t *testing.T) { t.Helper(); testdb.AdminDSN(t) }

func testdbNew(t *testing.T) string { t.Helper(); return testdb.Create(t, schema(t)+seed) }

// The M14 final audit: attacks rather than demonstrations.
//
// Each of these was written to fail. What they go after is the class of defect
// a green suite hides — a privacy guarantee that holds for the rendering and
// not for the encoding, arithmetic that survives one plan and not another, and
// state that a second call inherits from the first.

const auditSecret = "m14-super-secret-843721"

// Item 21, and the one the specification calls out as important: a report can
// be JSON-encoded, and the raw plan inside it is PostgreSQL's own output.
//
// The rendering withholds the plan's condition strings. The question this asks
// is whether encoding the report to JSON — the obvious thing to do with a
// structured report — walks straight past that and serialises the values the
// rendering was careful about.
func TestAudit_reportSerializationPrivacy(t *testing.T) {
	db, _ := reportDB(t)

	r, err := db.Users.Query().
		Where(gendemo.Users.Email.Eq(auditSecret + "@example.com")).
		PerformanceReport(t.Context())
	if err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}
	if r.Plan == nil {
		t.Fatal("no plan")
	}
	if !strings.Contains(string(r.Plan.JSON()), auditSecret) {
		t.Skip("this server did not inline the parameter into the plan")
	}

	// The rendering is safe, which is the guarantee that already exists.
	if strings.Contains(r.String(), auditSecret) {
		t.Fatalf("the rendering leaked the value:\n%s", r)
	}

	// Encoding is the attack. Whatever the answer, it has to be one the
	// documentation states rather than one a caller discovers.
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("encoding the report: %v", err)
	}
	if strings.Contains(string(encoded), auditSecret) {
		t.Errorf("encoding the report serialised the value the rendering withheld;\n"+
			"a caller who logs a structured report gets what a caller who logs the rendering does not.\n"+
			"encoded: %s", truncate(string(encoded), 400))
	}
}

// Item 24: the report runs an EXPLAIN, which is a traced operation. If the plan
// or its conditions reach a trace event, the tracing guarantee is bypassed by
// the report rather than by tracing.
func TestAudit_reportTracingPrivacy(t *testing.T) {
	rec := &recorder{}
	db, _ := tracedReportDB(t, rec)

	if _, err := db.Users.Query().
		Where(gendemo.Users.Email.Eq(auditSecret + "@example.com")).
		PerformanceReport(t.Context()); err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}

	// The report explains, so the declared explain op must actually fire —
	// otherwise the observability model advertises an event nothing emits.
	var sawExplain bool
	for _, op := range rec.ops() {
		if op == observe.OpExplain {
			sawExplain = true
		}
		if op == observe.OpExplainAnalyze {
			t.Error("the default report emitted an explain_analyze event")
		}
	}
	if !sawExplain {
		t.Errorf("the report emitted no explain event; ops were %v", rec.ops())
	}

	text := rec.text()
	if strings.Contains(text, auditSecret) {
		t.Errorf("a trace event carries the bind value or the plan's rendering of it:\n%s", text)
	}
	// The plan JSON must not be smuggled into an event either.
	if strings.Contains(text, `"Node Type"`) || strings.Contains(text, "Index Cond") {
		t.Errorf("a trace event carries plan JSON:\n%s", text)
	}
	if len(rec.ops()) == 0 {
		t.Error("the report produced no trace events, so this proves nothing")
	}
}

// Item 12: EXPLAIN must not leave anything on the query. A second compile has
// to produce what the first one did.
func TestAudit_explainLeavesNoResidue(t *testing.T) {
	db, _ := reportDB(t)
	q := db.Users.Query().
		Where(gendemo.Users.Age.Gt(int32(20))).
		OrderBy(gendemo.Users.ID.Asc()).
		Limit(3)

	sql0, args0, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	fp0, err := q.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	rows0, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// Every M14 entry point, in an order chosen to be inconvenient.
	if _, err := q.Explain(t.Context()); err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if _, err := q.PerformanceReport(t.Context()); err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}
	_ = orm.DebugSQL(q)
	if _, err := q.Diagnostics(); err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if _, err := q.ExplainAnalyze(t.Context()); err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	if _, err := q.Explain(t.Context()); err != nil {
		t.Fatalf("second Explain: %v", err)
	}

	sql1, args1, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL again: %v", err)
	}
	if sql1 != sql0 {
		t.Errorf("the SQL changed after M14 calls:\n%s\n%s", sql0, sql1)
	}
	if len(args1) != len(args0) {
		t.Errorf("the argument count changed: %d -> %d", len(args0), len(args1))
	}
	if strings.Contains(strings.ToUpper(sql1), "EXPLAIN") {
		t.Errorf("an EXPLAIN prefix stuck to the query: %s", sql1)
	}
	fp1, err := q.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint again: %v", err)
	}
	if !fp1.Equal(fp0) {
		t.Errorf("the fingerprint changed: %s -> %s", fp0, fp1)
	}
	rows1, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All again: %v", err)
	}
	if len(rows1) != len(rows0) {
		t.Errorf("the query returned %d rows and then %d", len(rows0), len(rows1))
	}
}

// Item 13 and 86: one query shape, many goroutines, every M14 entry point.
func TestAudit_m14IsConcurrencySafe(t *testing.T) {
	// A pool, not a bare connection: pgx documents a *pgx.Conn as usable by one
	// goroutine at a time, so sharing one would be measuring pgx's contract
	// rather than the ORM's.
	testdbAdmin(t)
	db := gendemo.New(poolFor(t, testdbNew(t)))
	q := db.Users.Query().Where(gendemo.Users.Age.Gt(int32(1))).Limit(5)

	baseline, err := q.Fingerprint()
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	const workers = 64
	var wg sync.WaitGroup
	errs := make(chan error, workers*4)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fp, err := q.Fingerprint()
			if err != nil {
				errs <- err
				return
			}
			if !fp.Equal(baseline) {
				errs <- errDiff{"fingerprint differed under concurrency"}
				return
			}
			if _, err := q.Diagnostics(); err != nil {
				errs <- err
				return
			}
			if _, err := q.Explain(t.Context()); err != nil {
				errs <- err
				return
			}
			if _, err := q.PerformanceReport(t.Context()); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a worker failed: %v", err)
	}
}

type errDiff struct{ s string }

func (e errDiff) Error() string { return e.s }

// Item 88 and 89: a thousand plans and reports leave the pool where they found
// it.
func TestAudit_m14ReleasesConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("resource loop")
	}
	testdbAdmin(t)
	pool := poolFor(t, testdbNew(t))
	db := gendemo.New(pool)
	q := db.Users.Query().Limit(1)

	for range 500 {
		if _, err := q.Explain(t.Context()); err != nil {
			t.Fatalf("Explain: %v", err)
		}
	}
	if n := settledAcquired(t, pool); n != 0 {
		t.Errorf("%d connections acquired after 500 explains", n)
	}
	for range 200 {
		if _, err := q.PerformanceReport(t.Context()); err != nil {
			t.Fatalf("PerformanceReport: %v", err)
		}
	}
	if n := settledAcquired(t, pool); n != 0 {
		t.Errorf("%d connections acquired after 200 reports", n)
	}
}

// Item 15: the same concrete value in several positions stays several
// placeholders. Deduplicating by value would change which parameter a plan is
// built for.
func TestAudit_duplicateValuesAreNotDeduplicated(t *testing.T) {
	db, _ := reportDB(t)
	const same = "duplicate@example.com"

	q := db.Users.Query().Where(
		gendemo.Users.Email.Ne(same),
		gendemo.Users.Email.Ne(same),
		gendemo.Users.Email.Ne(same),
	)
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 3 {
		t.Errorf("args = %d, want 3: identical values are still three parameters", len(args))
	}
	for _, ph := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(sql, ph) {
			t.Errorf("%s is missing from %s", ph, sql)
		}
	}
	// And EXPLAIN sees the same statement.
	if _, err := q.Explain(t.Context()); err != nil {
		t.Fatalf("Explain: %v", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tracedReportDB builds a database whose executor is wrapped in a tracer.
func tracedReportDB(t *testing.T, tr observe.Tracer) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	db, conn := reportDB(t)
	return gendemo.New(orm.Traced(db.Executor(), tr)), conn
}
