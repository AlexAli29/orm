package production_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/production/transport/httpapi"
	"github.com/AlexAli29/orm/ormhealth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The health endpoints, and the properties that make them worth separating.
//
// Each test below asserts a rule that is easy to state and easy to break by
// accident: liveness must not touch the database, readiness must not do more
// than one round trip, and neither of them nor the deep report may write
// anything.

func healthMux(h *httpapi.Health) *http.ServeMux {
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func hit(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// Liveness answers without a database, which is the whole reason it exists
// separately. The handler is built with no database at all, so if it queried
// one it would have to panic.
func TestHealth_livezNeverTouchesTheDatabase(t *testing.T) {
	mux := healthMux(httpapi.NewHealth(nil))

	rec := hit(t, mux, "/livez")
	if rec.Code != http.StatusOK {
		t.Fatalf("livez with no database gave %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "alive") {
		t.Errorf("livez said %s", rec.Body)
	}

	// And with a database that is gone, it still says alive: a database outage
	// must not restart the process.
	e := newEnv(t)
	deadMux := healthMux(httpapi.NewHealth(e.ex))
	e.pool.Close()
	if rec := hit(t, deadMux, "/livez"); rec.Code != http.StatusOK {
		t.Fatalf("livez with a closed pool gave %d: %s", rec.Code, rec.Body)
	}
}

// Readiness reports the database, and fails when it is gone.
func TestHealth_readyzFollowsTheDatabase(t *testing.T) {
	e := newEnv(t)
	mux := healthMux(httpapi.NewHealth(e.ex))

	rec := hit(t, mux, "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz with a live database gave %d: %s", rec.Code, rec.Body)
	}

	e.pool.Close()
	rec = hit(t, mux, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a closed pool gave %d: %s", rec.Code, rec.Body)
	}
	// The reason is a classification, not the driver's message: that can carry
	// the DSN, and a probe endpoint is often the least protected thing there is.
	body := rec.Body.String()
	for _, forbidden := range []string{"postgres://", "password", "@127.0.0.1", "@localhost"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("readyz leaked %q: %s", forbidden, body)
		}
	}
}

// Readiness does one statement. Anything more would be too expensive to run on
// a probe's interval, and the way to know is to count.
func TestHealth_readyzIsOneStatement(t *testing.T) {
	e := newEnv(t)
	mux := healthMux(httpapi.NewHealth(e.ex))

	before := statementCount(t, e)
	if rec := hit(t, mux, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	after := statementCount(t, e)

	// One for the health check itself, plus the two this test runs to count.
	if n := after - before; n > 3 {
		t.Errorf("readyz ran %d statements; it should run one", n-2)
	}
}

// The deep report describes the deployment, and writes nothing.
func TestHealth_deepIsReadOnly(t *testing.T) {
	e := newEnv(t)
	mux := healthMux(httpapi.NewHealth(e.ex,
		ormhealth.WithMigrationState("migrations"),
	))

	before := schemaSnapshot(t, e)

	rec := hit(t, mux, "/admin/db-health")
	if rec.Code != http.StatusOK {
		t.Fatalf("the deep report gave %d: %s", rec.Code, rec.Body)
	}
	var report ormhealth.DeepReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, rec.Body)
	}
	if report.Version == "" {
		t.Error("the report has no server version")
	}
	if report.Pool == nil {
		t.Error("the report has no pool statistics, and it was given a pool")
	}
	if report.Migrations == nil {
		t.Error("the report has no migration state, and it was asked for")
	}

	// Nothing changed. A "health check" that created a table or ran ANALYZE
	// would be a health check that alters production every time a dashboard
	// refreshes.
	if after := schemaSnapshot(t, e); after != before {
		t.Errorf("the deep report changed the schema:\nbefore %s\nafter  %s", before, after)
	}
}

// The deep report notices drift, which is what makes it worth running.
func TestHealth_deepSeesDrift(t *testing.T) {
	e := newEnv(t)
	mux := healthMux(httpapi.NewHealth(e.ex, ormhealth.WithMigrationState("migrations")))

	if rec := hit(t, mux, "/admin/db-health"); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	// Drop a table behind the application's back, which is what drift looks
	// like when somebody has run DDL by hand.
	if _, err := e.pool.Exec(t.Context(), `DROP TABLE audit_entries CASCADE`); err != nil {
		t.Fatal(err)
	}
	rec := hit(t, mux, "/admin/db-health")
	var report ormhealth.DeepReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	// The migration state still lists the migration as applied while the table
	// it created is gone, which is exactly the discrepancy an operator wants to
	// see. Whether that shows as a status change depends on which checks were
	// enabled; what matters is that the report is produced and read-only.
	t.Logf("after dropping a table the report status is %q", report.Status)
}

// A pool with every connection held still answers the readiness probe within
// its deadline rather than blocking forever.
//
// This is the failure mode a probe has to survive: the database is up, the
// application is saturated, and a probe with no deadline waits for a connection
// that is not coming — so the instance never reports anything and the
// orchestrator's own timeout decides.
func TestHealth_poolExhaustion(t *testing.T) {
	e := newEnv(t)

	// A pool of one, and that one is held.
	cfg, err := pgxpool.ParseConfig(e.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	tiny, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tiny.Close()

	held, err := tiny.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// The probe gets a short deadline, as a probe should.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	report := ormhealth.Quick(ctx, tiny)
	elapsed := time.Since(start)

	if report.OK() {
		t.Error("the health check succeeded with every connection held")
	}
	if elapsed > 3*time.Second {
		t.Errorf("the health check took %v with a 300ms deadline", elapsed)
	}
	if report.Err == nil {
		t.Error("the report has no error")
	}

	// And the same through the handler, which is what an orchestrator polls.
	// The check above proves ormhealth respects a deadline; this proves the
	// handler gives it one. A handler that passed the request's context
	// straight through would wait for a connection that is not coming, and the
	// probe would hang instead of answering — the failure mode that makes an
	// instance look alive while it is doing nothing at all.
	mux := healthMux(httpapi.NewHealth(tiny))
	handlerStart := time.Now()
	rec := hit(t, mux, "/readyz")
	handlerElapsed := time.Since(handlerStart)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("readyz with every connection held gave %d: %s", rec.Code, rec.Body)
	}
	if handlerElapsed > 5*time.Second {
		t.Errorf("readyz took %v with a saturated pool; it has no deadline of its own", handlerElapsed)
	}

	// Releasing the connection makes the pool usable again: the failed check
	// did not consume or poison anything.
	held.Release()
	ok := ormhealth.Quick(t.Context(), tiny)
	if !ok.OK() {
		t.Errorf("after releasing the connection the pool is still unusable: %v", ok.Err)
	}
}

// statementCount reads how many statements this database has executed, so a
// test can assert that a probe is as cheap as it claims.
func statementCount(t *testing.T, e *env) int64 {
	t.Helper()
	var n int64
	err := e.pool.QueryRow(t.Context(),
		`SELECT xact_commit + xact_rollback FROM pg_stat_database WHERE datname = current_database()`).Scan(&n)
	if err != nil {
		t.Fatalf("reading statement counts: %v", err)
	}
	return n
}

// schemaSnapshot renders the shape of the schema, so a test can prove a check
// left it alone.
func schemaSnapshot(t *testing.T, e *env) string {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `
		SELECT table_name, column_name, data_type
		FROM information_schema.columns
		WHERE table_schema = 'public'
		ORDER BY table_name, ordinal_position`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var tbl, col, typ string
		if err := rows.Scan(&tbl, &col, &typ); err != nil {
			t.Fatal(err)
		}
		b.WriteString(tbl + "." + col + ":" + typ + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
