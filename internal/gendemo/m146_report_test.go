package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// M14.6: the performance report.
//
// The release-critical claim is the first one below: the default report plans
// the statement and does not run it. A convenience method that quietly executed
// the UPDATE it was asked to describe would be the most dangerous thing in the
// package, so the test counts rows either side of one.

func reportDB(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(t.Context()) })
	if err := gendemo.RegisterTypes(t.Context(), conn); err != nil {
		t.Fatalf("registering types: %v", err)
	}
	return gendemo.New(conn), conn
}

func reportUserCount(t *testing.T, conn *pgx.Conn) int64 {
	t.Helper()
	var n int64
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	return n
}

func reportNickname(t *testing.T, conn *pgx.Conn, id int64) *string {
	t.Helper()
	var s *string
	if err := conn.QueryRow(t.Context(), "SELECT nickname FROM users WHERE id = $1", id).Scan(&s); err != nil {
		t.Fatalf("reading the nickname: %v", err)
	}
	return s
}

// Release-critical: the default report never executes the statement it
// describes, for any statement kind.
func TestReport_defaultNeverExecutes(t *testing.T) {
	db, conn := reportDB(t)

	before := reportUserCount(t, conn)
	beforeNick := reportNickname(t, conn, 1)

	t.Run("UPDATE", func(t *testing.T) {
		r, err := db.Users.Update().
			Set(gendemo.Users.Nickname.Set("changed-by-the-report")).
			Where(gendemo.Users.ID.Eq(int64(1))).
			PerformanceReport(t.Context())
		if err != nil {
			t.Fatalf("PerformanceReport: %v", err)
		}
		if r.Analyzed {
			t.Error("the default report produced an analysed plan")
		}
		if r.Plan == nil {
			t.Fatal("no plan")
		}
		got := reportNickname(t, conn, 1)
		switch {
		case (got == nil) != (beforeNick == nil):
			t.Errorf("the report changed the row: %v -> %v", beforeNick, got)
		case got != nil && *got != *beforeNick:
			t.Errorf("the report changed the nickname to %q", *got)
		}
	})

	t.Run("DELETE", func(t *testing.T) {
		if _, err := db.Users.Delete().
			Where(gendemo.Users.ID.Eq(int64(1))).
			PerformanceReport(t.Context()); err != nil {
			t.Fatalf("PerformanceReport: %v", err)
		}
		if n := reportUserCount(t, conn); n != before {
			t.Errorf("the report deleted rows: %d -> %d", before, n)
		}
	})

	t.Run("SELECT", func(t *testing.T) {
		if _, err := db.Users.Query().PerformanceReport(t.Context()); err != nil {
			t.Fatalf("PerformanceReport: %v", err)
		}
		if n := reportUserCount(t, conn); n != before {
			t.Errorf("row count changed: %d -> %d", before, n)
		}
	})

	// And the plan really is the unanalysed one, which is what the finding says.
	r, err := db.Users.Query().PerformanceReport(t.Context())
	if err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}
	if r.Plan.Analyzed {
		t.Error("the default report's plan carries measurements")
	}
	if !strings.Contains(r.String(), "the statement was not executed") {
		t.Errorf("the rendering does not say the statement was not executed:\n%s", r)
	}
}

// The analyzing form does execute, and its name says so.
func TestReport_analyzeExecutes(t *testing.T) {
	db, conn := reportDB(t)

	shape, err := db.Users.Update().
		Set(gendemo.Users.Nickname.Set("changed")).
		Where(gendemo.Users.ID.Eq(int64(1))).
		Shape()
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}
	stmt := db.Users.Update().
		Set(gendemo.Users.Nickname.Set("changed")).
		Where(gendemo.Users.ID.Eq(int64(1)))

	r, err := orm.PerformanceReportAnalyze(t.Context(), db.Executor(), stmt, shape)
	if err != nil {
		t.Fatalf("PerformanceReportAnalyze: %v", err)
	}
	if !r.Analyzed {
		t.Error("the analysing report produced an unanalysed plan")
	}
	got := reportNickname(t, conn, 1)
	if got == nil || *got != "changed" {
		t.Errorf("nickname = %v, want the write to have happened", got)
	}
	if !strings.Contains(r.String(), string(orm.FromActual)) {
		t.Errorf("the rendering does not mark the plan as measured:\n%s", r)
	}
}

// The transaction pattern: analyse inside an explicit transaction and roll it
// back, which undoes PostgreSQL's own effects and is documented as no more
// than that.
func TestReport_analyzeInsideRolledBackTransaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	var reportSeen bool
	sentinel := errRollback{}
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		u := tx.Users.Update().
			Set(gendemo.Users.Nickname.Set("only-inside-the-tx")).
			Where(gendemo.Users.ID.Eq(int64(1)))
		shape, err := u.Shape()
		if err != nil {
			return err
		}
		r, err := orm.PerformanceReportAnalyze(t.Context(), tx.Executor(), u, shape)
		if err != nil {
			return err
		}
		reportSeen = r.Analyzed

		// The write is visible inside the transaction.
		got, err := tx.Users.Query().Where(gendemo.Users.ID.Eq(int64(1))).One(t.Context())
		if err != nil {
			return err
		}
		if got.Nickname == nil || *got.Nickname != "only-inside-the-tx" {
			t.Errorf("inside the transaction the nickname is %v", got.Nickname)
		}
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("Tx returned %v", err)
	}
	if !reportSeen {
		t.Error("the report inside the transaction was not analysed")
	}

	// And the rollback undid it.
	after, err := db.Users.Query().Where(gendemo.Users.ID.Eq(int64(1))).One(t.Context())
	if err != nil {
		t.Fatalf("reading after the rollback: %v", err)
	}
	if after.Nickname != nil && *after.Nickname == "only-inside-the-tx" {
		t.Error("the rollback did not undo the analysed write")
	}
}

// A report carries no bind values, in any of its sections or its rendering.
func TestReport_carriesNoValues(t *testing.T) {
	db, _ := reportDB(t)

	const secret = "sentinel-9f3a-secret@example.com"
	r, err := db.Users.Query().
		Where(gendemo.Users.Email.Eq(secret)).
		Limit(5).
		PerformanceReport(t.Context())
	if err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}

	rendered := r.String()
	if strings.Contains(rendered, secret) || strings.Contains(rendered, "sentinel-9f3a") {
		t.Errorf("the report carries the bind value:\n%s", rendered)
	}
	if strings.Contains(r.SQL, secret) {
		t.Errorf("the SQL carries the value: %s", r.SQL)
	}
	if !strings.Contains(r.SQL, "$1") {
		t.Errorf("the SQL lost its placeholder: %s", r.SQL)
	}
	if r.Args != 1 {
		t.Errorf("args = %d, want 1", r.Args)
	}
	if strings.Contains(r.Fingerprint.String(), secret) {
		t.Error("the fingerprint carries the value")
	}
	// The plan is a different matter, and the boundary is worth freezing
	// exactly rather than wishing away. PostgreSQL plans a parameterised
	// statement with the values it was given and writes them into the
	// conditions it reports, so the raw plan does carry the value. Removing it
	// would mean parsing SQL, which this package does not do.
	if r.Plan == nil {
		t.Fatal("no plan")
	}
	if !strings.Contains(string(r.Plan.JSON()), secret) {
		t.Skip("this server did not inline the parameter into the plan, so the boundary below is untestable here")
	}
	// Given that it does, the rendering is where the guarantee lives: the thing
	// that reaches a log has the condition withheld.
	if strings.Contains(rendered, secret) {
		t.Errorf("the rendering carries the value PostgreSQL put in the plan:\n%s", rendered)
	}
	if !strings.Contains(rendered, "withheld") {
		t.Errorf("the rendering does not say a condition was withheld:\n%s", rendered)
	}
	// And opting in brings it back, for a reader who knows what is in it.
	if withCond := r.Render(orm.WithConditions()); !strings.Contains(withCond, secret) {
		t.Error("WithConditions did not render the condition")
	}
	// No finding quotes the condition either.
	for _, d := range r.PlanFindings {
		if strings.Contains(d.String(), secret) {
			t.Errorf("a plan finding carries the value: %s", d)
		}
	}
}

// Release-critical for M14's scope: a report contains no advice.
func TestReport_containsNoAdvice(t *testing.T) {
	db, _ := reportDB(t)
	r, err := db.Users.Query().
		Where(gendemo.Users.Age.Gt(int32(1))).
		OrderBy(gendemo.Users.CreatedAt.Desc()).
		PerformanceReport(t.Context())
	if err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}
	rendered := strings.ToLower(r.String())
	for _, phrase := range []string{
		"create index", "add an index", "recommend", "suggested index",
		"you should", "advisor", "optimize", "auto-tune", "increase work_mem",
	} {
		if strings.Contains(rendered, phrase) {
			t.Errorf("the report contains advice (%q):\n%s", phrase, r)
		}
	}
	// And it says so explicitly, so that a reader knows the omission is
	// deliberate rather than a gap.
	if !strings.Contains(rendered, "no index or configuration advice") {
		t.Errorf("the report does not state its own scope:\n%s", r)
	}
}

// The report is structured first: everything the rendering shows is reachable
// as a field, so a tool never has to parse the text.
func TestReport_isStructuredFirst(t *testing.T) {
	db, _ := reportDB(t)
	r, err := db.Users.Query().Limit(3).PerformanceReport(t.Context())
	if err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}

	if r.Fingerprint.IsZero() {
		t.Error("no fingerprint")
	}
	if r.SQL == "" {
		t.Error("no SQL")
	}
	if !r.Shape.Analyzable || r.Shape.Root.Table != "users" {
		t.Errorf("shape = %+v", r.Shape)
	}
	if len(r.Static) == 0 {
		t.Error("no static findings")
	}
	if r.Plan == nil || r.PlanSummary == nil {
		t.Error("no plan or summary")
	}
	if r.PlanSummary.Nodes == 0 {
		t.Error("the summary counted no nodes")
	}
	// The two halves are reachable together, with their provenance intact.
	d := r.Diagnostics()
	if len(d.Static) != len(r.Static) || len(d.Plan) != len(r.PlanFindings) {
		t.Errorf("Diagnostics() lost findings: %d/%d vs %d/%d",
			len(d.Static), len(d.Plan), len(r.Static), len(r.PlanFindings))
	}
	// The rendering names each section's provenance.
	rendered := r.String()
	for _, want := range []string{string(orm.FromORM), string(orm.FromEstimate), string(orm.FromDerived)} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendering does not mark %q:\n%s", want, rendered)
		}
	}
}

// A report built from a plan obtained earlier contacts nothing.
func TestReport_fromAnEarlierPlan(t *testing.T) {
	db, _ := reportDB(t)
	q := db.Users.Query().Limit(2)

	p, err := q.ExplainAnalyze(t.Context())
	if err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	shape, err := q.Shape()
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}

	r, err := orm.ReportFromPlan(q, shape, p)
	if err != nil {
		t.Fatalf("ReportFromPlan: %v", err)
	}
	if !r.Analyzed {
		t.Error("a report from an analysed plan is not marked analysed")
	}
	if r.Plan != p {
		t.Error("the report did not keep the plan it was given")
	}
	if !strings.Contains(r.String(), string(orm.FromActual)) {
		t.Error("the rendering does not mark the plan as measured")
	}
}

// Raw reports what it can: no structure, but a fingerprint, a plan and plan
// findings.
func TestReport_raw(t *testing.T) {
	db, _ := reportDB(t)
	q := orm.Raw(db.Users, "SELECT id, email FROM users WHERE age > $1", int32(1))

	r, err := q.PerformanceReport(t.Context())
	if err != nil {
		t.Fatalf("PerformanceReport: %v", err)
	}
	if r.Shape.Analyzable {
		t.Error("a raw statement claims structural analysis")
	}
	if r.Fingerprint.IsZero() {
		t.Error("a raw statement has no fingerprint")
	}
	if r.Plan == nil {
		t.Error("a raw statement produced no plan")
	}
	if len(r.Static) != 1 {
		t.Errorf("a raw statement produced %d static findings, want only the boundary one", len(r.Static))
	}
}
