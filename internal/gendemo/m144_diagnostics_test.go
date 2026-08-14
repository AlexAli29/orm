package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/diagnostics"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// M14.4: query diagnostics.
//
// The claims are that the static half reads the query's own structure rather
// than its SQL, that it never carries a bind value, that it answers the N+1
// question with a number instead of a warning, and that the plan half reports
// only what PostgreSQL actually said.

// find returns the diagnostic with the given code, and whether there was one.
func find(ds []diagnostics.Diagnostic, code string) (diagnostics.Diagnostic, bool) {
	for _, d := range ds {
		if d.Code == code {
			return d, true
		}
	}
	return diagnostics.Diagnostic{}, false
}

func evidence(d diagnostics.Diagnostic, name string) (string, bool) {
	for _, e := range d.Evidence {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// The shape is read from the tree: sources, joins, ordering and limit all
// arrive without anybody parsing SQL.
func TestDiagnostics_staticShape(t *testing.T) {
	db := gendemo.New(nil)

	q := db.Users.Query().
		Where(gendemo.Users.Age.Gt(int32(30)), gendemo.Users.Active.Eq(true)).
		OrderBy(gendemo.Users.CreatedAt.Desc()).
		Limit(20)

	s, err := q.Shape()
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}
	if s.Kind != diagnostics.KindSelect {
		t.Errorf("kind = %s", s.Kind)
	}
	if s.Root.Table != "users" {
		t.Errorf("root = %+v", s.Root)
	}
	if !s.Analyzable {
		t.Error("a built query is not analyzable")
	}
	if s.FilterCount != 2 {
		t.Errorf("filter count = %d, want 2", s.FilterCount)
	}
	if got := strings.Join(s.FilterColumns, ","); got != "users.age,users.active" {
		t.Errorf("filter columns = %q", got)
	}
	if len(s.OrderBy) != 1 || !s.OrderBy[0].Desc || s.OrderBy[0].Expr != "users.created_at" {
		t.Errorf("order by = %+v", s.OrderBy)
	}
	if !s.HasLimit || s.Limit != 20 {
		t.Errorf("limit = %v/%d", s.HasLimit, s.Limit)
	}

	report, err := q.Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	d, ok := find(report.Static, diagnostics.CodeShape)
	if !ok {
		t.Fatalf("no shape finding in %v", report)
	}
	if d.Severity != diagnostics.Info {
		t.Errorf("the shape finding is %s, want info", d.Severity)
	}
	// A LIMIT is present, so the unbounded note is suppressed.
	if _, ok := find(report.Static, diagnostics.CodeUnbounded); ok {
		t.Error("a query with a LIMIT was told it has no LIMIT")
	}
}

// Release-critical: no bind value reaches a shape or a report, whatever the
// query filtered on.
func TestDiagnostics_carryNoValues(t *testing.T) {
	db := gendemo.New(nil)

	const (
		secretEmail = "person@example.com"
		secretNick  = "s3cr3t-token-abcdef"
	)
	q := db.Users.Query().
		Where(
			gendemo.Users.Email.Eq(secretEmail),
			gendemo.Users.Nickname.Eq(secretNick),
			gendemo.Users.Age.In(41, 42, 43),
		).
		OrderBy(gendemo.Users.Email.Asc()).
		Limit(7)

	report, err := q.Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	rendered := report.String()
	for _, secret := range []string{secretEmail, secretNick, "s3cr3t", "41", "43"} {
		if strings.Contains(rendered, secret) {
			t.Errorf("the report contains %q:\n%s", secret, rendered)
		}
	}
	// The identifiers are there, which is what makes the report useful.
	for _, want := range []string{"users.email", "users.nickname", "users.age"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the report does not mention %q:\n%s", want, rendered)
		}
	}
	// The limit is structure rather than a bind value — PostgreSQL plans
	// differently for one — so it is reported, and 7 is not a secret.
	if !strings.Contains(rendered, "limit: 7") {
		t.Errorf("the limit is missing:\n%s", rendered)
	}

	// The same for the debug rendering.
	debug := orm.DebugSQL(q)
	for _, secret := range []string{secretEmail, secretNick} {
		if strings.Contains(debug, secret) {
			t.Errorf("DebugSQL contains %q:\n%s", secret, debug)
		}
	}
	if !strings.Contains(debug, "$1") || !strings.Contains(debug, "fingerprint:") {
		t.Errorf("DebugSQL = %s", debug)
	}
	if !strings.Contains(debug, "values are not shown") {
		t.Errorf("DebugSQL does not say the values are withheld:\n%s", debug)
	}
}

// The relation plan is answered with an exact statement count rather than an
// N+1 warning, because the loader batches and the count is knowable.
func TestDiagnostics_relationPlan(t *testing.T) {
	db := gendemo.New(nil)

	report, err := db.Users.Query().
		With(gendemo.Users.Posts.With(gendemo.Posts.Comments)).
		Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	d, ok := find(report.Static, diagnostics.CodeRelationPlan)
	if !ok {
		t.Fatalf("no relation finding in %v", report)
	}
	if got, _ := evidence(d, "statements"); got != "3" {
		t.Errorf("statements = %s, want 3 (root, posts, comments)", got)
	}
	if got, _ := evidence(d, "relation steps"); got != "2" {
		t.Errorf("relation steps = %s, want 2", got)
	}
	if got, _ := evidence(d, "depth"); got != "2" {
		t.Errorf("depth = %s, want 2", got)
	}
	if !strings.Contains(d.Note, "not an N+1") {
		t.Errorf("the note does not answer the N+1 question: %s", d.Note)
	}
	if d.Severity != diagnostics.Info {
		t.Errorf("severity = %s, want info: batched relation loading is not a problem", d.Severity)
	}

	// And the buffering reason names the relation loader rather than a generic
	// "this buffers".
	b, ok := find(report.Static, diagnostics.CodeBuffered)
	if !ok {
		t.Fatal("no buffering finding")
	}
	if r, _ := evidence(b, "reason"); !strings.Contains(r, "relation") {
		t.Errorf("buffering reason = %q", r)
	}
}

// A to-one relation folds into the root statement and costs no statement of its
// own, which the count has to reflect.
func TestDiagnostics_foldedRelationCostsNoStatement(t *testing.T) {
	db := gendemo.New(nil)
	report, err := db.Posts.Query().With(gendemo.Posts.Author).Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	d, _ := find(report.Static, diagnostics.CodeRelationPlan)
	if got, _ := evidence(d, "statements"); got != "1" {
		t.Errorf("statements = %s, want 1: a folded to-one relation joins into the root", got)
	}
}

// Raw says what it cannot say, rather than inventing structure from a string.
func TestDiagnostics_rawBoundary(t *testing.T) {
	db := gendemo.New(nil)
	q := orm.Raw(db.Users, "SELECT id, email FROM users WHERE email = $1", "x@example.com")

	s, err := q.Shape()
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}
	if s.Analyzable {
		t.Error("a raw statement claims to be analyzable")
	}
	report, err := q.Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	d, ok := find(report.Static, diagnostics.CodeRawBoundary)
	if !ok {
		t.Fatalf("no raw boundary finding in %v", report)
	}
	if !strings.Contains(d.Note, "does not parse SQL") {
		t.Errorf("the note does not say why: %s", d.Note)
	}
	// And nothing was fabricated: no sources, no filters.
	if len(report.Static) != 1 {
		t.Errorf("a raw statement produced %d findings, want only the boundary one", len(report.Static))
	}
	if s.Root.Table != "" || len(s.FilterColumns) != 0 {
		t.Errorf("structure was invented from the SQL: %+v", s)
	}
}

// A join with no condition is information, not a bug report.
func TestDiagnostics_crossJoinIsInformational(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	q := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		CrossJoin(gendemo.Categories.Source())

	report, err := q.Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	d, ok := find(report.Static, diagnostics.CodeCrossJoin)
	if !ok {
		t.Fatalf("no cross join finding in %v", report)
	}
	if d.Severity != diagnostics.Info {
		t.Errorf("a cross join is reported as %s; it is often deliberate", d.Severity)
	}
	if !strings.Contains(d.Note, "often deliberate") {
		t.Errorf("the note reads as a bug report: %s", d.Note)
	}
}

// The unbounded note appears only where reading everything is not the point.
func TestDiagnostics_unboundedIsQuiet(t *testing.T) {
	db := gendemo.New(nil)

	t.Run("a plain select with no limit gets a low-confidence note", func(t *testing.T) {
		report, err := db.Users.Query().Diagnostics()
		if err != nil {
			t.Fatalf("Diagnostics: %v", err)
		}
		d, ok := find(report.Static, diagnostics.CodeUnbounded)
		if !ok {
			t.Fatalf("no unbounded finding in %v", report)
		}
		if d.Severity != diagnostics.Info || d.Confidence != diagnostics.Low {
			t.Errorf("severity/confidence = %s/%s, want info/low", d.Severity, d.Confidence)
		}
	})

	for _, tt := range []struct {
		name  string
		build func() (diagnostics.Report, error)
	}{
		{"a limit", func() (diagnostics.Report, error) {
			return db.Users.Query().Limit(10).Diagnostics()
		}},
		{"an aggregate", func() (diagnostics.Report, error) {
			return orm.Select(db.Users, orm.Project1(
				orm.Count[gendemo.User](), func(n int64) int64 { return n })).Diagnostics()
		}},
		{"a group by", func() (diagnostics.Report, error) {
			return orm.Select(db.Users, orm.Project1(
				gendemo.Users.State.Value(), func(s gendemo.UserState) gendemo.UserState { return s })).
				GroupBy(gendemo.Users.State).Diagnostics()
		}},
	} {
		t.Run(tt.name+" suppresses it", func(t *testing.T) {
			report, err := tt.build()
			if err != nil {
				t.Fatalf("Diagnostics: %v", err)
			}
			if _, ok := find(report.Static, diagnostics.CodeUnbounded); ok {
				t.Errorf("the unbounded note appeared anyway:\n%v", report)
			}
		})
	}
}

// A locking clause is reported with what it locks and what it does about
// contention, because both change what the statement does to other sessions.
func TestDiagnostics_locking(t *testing.T) {
	db := gendemo.New(nil)
	report, err := db.Users.Query().
		Limit(10).
		Lock(orm.ForUpdateStrong, orm.SkipLocked).
		Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	d, ok := find(report.Static, diagnostics.CodeLocking)
	if !ok {
		t.Fatalf("no locking finding in %v", report)
	}
	if d.Severity != diagnostics.Notice {
		t.Errorf("severity = %s", d.Severity)
	}
	if got, _ := evidence(d, "strength"); got != "FOR UPDATE" {
		t.Errorf("strength = %q", got)
	}
	if got, _ := evidence(d, "waiting policy"); got != "SKIP LOCKED" {
		t.Errorf("waiting policy = %q", got)
	}
	if got, _ := evidence(d, "locks"); !strings.Contains(got, "every source") {
		t.Errorf("the finding does not say what is locked: %q", got)
	}
}

// CTEs, derived tables and correlated subqueries are all read from the tree.
func TestDiagnostics_compositionIsVisible(t *testing.T) {
	ids := orm.Named("id", orm.Of(gendemo.Users.ID))
	cte := orm.CTE("adults", orm.Rows(ids).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Age.Gte(int32(18)))))

	sub := orm.Sub("recent", orm.Rows(orm.Named("aid", orm.Of(gendemo.Posts.AuthorID))).
		From(gendemo.Posts.Source()))

	q := orm.Compose(nil, orm.Project1(orm.Ref(cte, ids), func(id int64) int64 { return id })).
		With(cte).
		From(cte).
		LeftJoin(sub, orm.Eq(orm.Ref(cte, ids), orm.Ref(sub, orm.Named("aid", orm.Of(gendemo.Posts.AuthorID)))))

	s, err := q.Shape()
	if err != nil {
		// The join condition above references an Out built separately, which
		// the compiler may refuse; the CTE half is what this test is about.
		q = orm.Compose(nil, orm.Project1(orm.Ref(cte, ids), func(id int64) int64 { return id })).
			With(cte).From(cte)
		s, err = q.Shape()
		if err != nil {
			t.Fatalf("Shape: %v", err)
		}
	}
	if len(s.CTEs) != 1 || s.CTEs[0].Name != "adults" {
		t.Errorf("CTEs = %+v", s.CTEs)
	}
	if s.Root.Kind != diagnostics.SourceCTE {
		t.Errorf("root kind = %s, want a CTE", s.Root.Kind)
	}
}

// A correlated subquery is counted, and an uncorrelated one is not.
func TestDiagnostics_correlation(t *testing.T) {
	db := gendemo.New(nil)

	correlated := db.Users.Query().Where(orm.Exists[gendemo.User](
		orm.Rows(orm.Named("one", orm.Of(gendemo.Posts.ID))).
			From(gendemo.Posts.Source()).
			Where(orm.Eq(orm.Of(gendemo.Posts.AuthorID), orm.Of(gendemo.Users.ID)))))

	s, err := correlated.Shape()
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}
	if s.Correlated != 1 {
		t.Errorf("correlated = %d, want 1", s.Correlated)
	}
	report, err := correlated.Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if _, ok := find(report.Static, diagnostics.CodeCorrelated); !ok {
		t.Errorf("no correlation finding:\n%v", report)
	}

	uncorrelated := db.Users.Query().Where(orm.Exists[gendemo.User](
		orm.Rows(orm.Named("one", orm.Of(gendemo.Posts.ID))).
			From(gendemo.Posts.Source())))
	s2, err := uncorrelated.Shape()
	if err != nil {
		t.Fatalf("Shape: %v", err)
	}
	if s2.Correlated != 0 {
		t.Errorf("an uncorrelated subquery counted as %d", s2.Correlated)
	}
}

// Writes describe themselves, including whether RETURNING makes them buffer.
func TestDiagnostics_writes(t *testing.T) {
	db := gendemo.New(nil)

	u := db.Users.Update().
		Set(gendemo.Users.Nickname.Set("x")).
		Where(gendemo.Users.ID.Eq(int64(1)))
	su, err := u.Shape()
	if err != nil {
		t.Fatalf("update Shape: %v", err)
	}
	if su.Kind != diagnostics.KindUpdate || su.Root.Table != "users" {
		t.Errorf("update shape = %+v", su)
	}
	if su.FilterCount != 1 || su.Projected != 1 {
		t.Errorf("update filters/sets = %d/%d", su.FilterCount, su.Projected)
	}

	d := db.Users.Delete().Where(gendemo.Users.ID.Eq(int64(1)))
	sd, err := d.Shape()
	if err != nil {
		t.Fatalf("delete Shape: %v", err)
	}
	if sd.Kind != diagnostics.KindDelete {
		t.Errorf("delete kind = %s", sd.Kind)
	}
}

// COPY has no statement, so its shape is the target and the column order — the
// two things the wire format actually depends on.
func TestDiagnostics_copyShape(t *testing.T) {
	db := gendemo.New(nil)
	s := orm.CopyShape(db.Users, gendemo.Users.Email, gendemo.Users.Age)
	if s.Kind != diagnostics.KindCopy {
		t.Errorf("kind = %s", s.Kind)
	}
	if s.Table == "" || !strings.Contains(s.Table, "users") {
		t.Errorf("table = %q", s.Table)
	}
	if strings.Join(s.Columns, ",") != "email,age" {
		t.Errorf("columns = %v, want the caller's order", s.Columns)
	}
	if !s.Streams {
		t.Error("COPY is reported as buffering")
	}
	for _, d := range diagnostics.Static(s) {
		if strings.Contains(d.String(), "@") {
			t.Errorf("the COPY finding carries a value: %s", d)
		}
	}
}

// The plan half, against a plan PostgreSQL actually produced.
//
// What is asserted is the plumbing rather than a particular finding: the fixture
// is small, so the thresholds correctly keep almost everything quiet. That the
// findings are quiet on a small table is itself the property worth freezing.
func TestDiagnostics_realPlan(t *testing.T) {
	db := db(t)

	q := db.Users.Query().
		Where(gendemo.Users.Age.Gt(int32(18))).
		OrderBy(gendemo.Users.CreatedAt.Desc()).
		Limit(5)

	p, err := q.Explain(t.Context())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	ds := orm.DiagnosePlan(p)

	// A plain EXPLAIN carries estimates, and the report says so rather than
	// treating them as measurements.
	d, ok := find(ds, diagnostics.CodeNotAnalyzed)
	if !ok {
		t.Fatalf("a plain EXPLAIN did not report that it carries estimates:\n%v", ds)
	}
	if !strings.Contains(d.Note, "executes the statement") {
		t.Errorf("the note does not warn that ANALYZE runs the query: %s", d.Note)
	}

	// The three-row fixture is far below every threshold, so nothing else fires.
	for _, code := range []string{
		diagnostics.CodeRowsRemoved, diagnostics.CodeSortSpill,
		diagnostics.CodeHashSpill, diagnostics.CodeLoops,
	} {
		if got, ok := find(ds, code); ok {
			t.Errorf("a three-row table produced %s: %s", code, got)
		}
	}

	// Analysed, the estimates become measurements and the note disappears.
	ap, err := q.ExplainAnalyze(t.Context())
	if err != nil {
		t.Fatalf("ExplainAnalyze: %v", err)
	}
	if !ap.Analyzed {
		t.Fatal("ExplainAnalyze produced an unanalysed plan")
	}
	if _, ok := find(orm.DiagnosePlan(ap), diagnostics.CodeNotAnalyzed); ok {
		t.Error("an analysed plan was reported as carrying estimates")
	}

	// Every plan finding names a node that is really in the plan.
	for _, d := range orm.DiagnosePlan(ap) {
		if d.NodePath == "" {
			continue
		}
		if _, ok := ap.Node(d.NodeID); !ok {
			t.Errorf("%s names node %d, which is not in the plan", d.Code, d.NodeID)
		}
	}
}

// A whole report: the static half needs no database, the plan half needs one,
// and neither carries a value.
func TestDiagnostics_bothHalves(t *testing.T) {
	db := db(t)
	q := db.Users.Query().Where(gendemo.Users.Email.Eq("alex@example.com"))

	report, err := q.Diagnostics()
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if len(report.Static) == 0 {
		t.Fatal("no static findings")
	}
	if len(report.Plan) != 0 {
		t.Error("Diagnostics returned plan findings without being given a plan")
	}

	p, err := q.Explain(t.Context())
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	report.Plan = orm.DiagnosePlan(p)

	rendered := report.String()
	if strings.Contains(rendered, "alex@example.com") {
		t.Errorf("the report carries the bind value:\n%s", rendered)
	}
	if !strings.Contains(rendered, "users.email") {
		t.Errorf("the report does not name the filtered column:\n%s", rendered)
	}
}
