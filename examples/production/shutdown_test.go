package production_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"example.com/production/server"
)

// Graceful shutdown, tested by doing it.
//
// This is the one place in the example's suite that opens a real TCP listener.
// Everywhere else httptest is enough, because everywhere else the question is
// what a handler returns. Here the question is about the process: whether the
// database outlives the requests that are using it, and whether a request that
// was already in flight when the signal arrived gets an answer or a broken
// connection. Neither is visible from a handler.

// startServer starts the application against the test database and stops it
// when the test ends.
func startServer(t *testing.T, e *env, timeout time.Duration) *server.Server {
	t.Helper()
	srv, err := server.Start(t.Context(), server.Config{
		DSN:             e.pool.Config().ConnString(),
		Addr:            "127.0.0.1:0", // the kernel picks; two tests can run at once
		ShutdownTimeout: timeout,
	}, e.log)
	if err != nil {
		t.Fatalf("starting the server: %v", err)
	}
	return srv
}

// The server serves, and stops when asked.
func TestShutdown_startsAndStops(t *testing.T) {
	e := newEnv(t)
	srv := startServer(t, e, 5*time.Second)

	code, body := request(t, srv.Addr(), http.MethodPost, "/users",
		`{"email":"lifecycle@example.com","name":"Lifecycle"}`)
	if code != http.StatusCreated {
		t.Fatalf("creating a user: %d %s", code, body)
	}

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("shutting down: %v", err)
	}

	// The listener is gone: the port refuses rather than hanging.
	if _, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second); err == nil {
		t.Error("the port still accepts connections after shutdown")
	}

	// And so is the pool.
	if err := srv.Pool().Ping(context.Background()); err == nil {
		t.Error("the pool is still usable after shutdown")
	}
}

// Release-critical: a request that is in flight when shutdown starts still has a
// database to talk to.
//
// The failure this guards against is the common one — closing the pool before
// the HTTP server has drained — and getting a test to distinguish it takes
// care, because the obvious test does not. A request already blocked *inside*
// PostgreSQL is holding a connection, and pgxpool.Close waits for checked-out
// connections to come back; close the pool first and that request still
// finishes. The mutation survives.
//
// What does not survive is a request that is in flight and has not acquired a
// connection yet, because that one has to acquire from a pool that is now
// closed. So this test creates exactly that: the client sends the headers and
// half the JSON body, the handler blocks decoding it, shutdown starts, and only
// then does the rest of the body arrive and the handler reach the database.
//
// If the pool were closed before the drain, the request would come back as a
// server error instead of a 201.
func TestShutdown_inFlightRequestFinishesWithADatabase(t *testing.T) {
	e := newEnv(t)
	srv := startServer(t, e, 30*time.Second)

	// A body the test writes in two pieces, so the handler is stuck in
	// json.Decode with no connection acquired.
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"http://"+srv.Addr()+"/users", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")

	type result struct {
		code int
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		tr := &http.Transport{DisableKeepAlives: true}
		defer tr.CloseIdleConnections()
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			done <- result{err: err}
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		done <- result{resp.StatusCode, string(out), nil}
	}()

	if _, err := pw.Write([]byte(`{"email":"inflight-body@example.com",`)); err != nil {
		t.Fatal(err)
	}
	// The server has the headers and part of the body; the handler is inside
	// Decode. There is no event to wait on for that, so this is a pause — but
	// the assertion below turns a pause that was too short into a failure
	// rather than a false pass: if the request were not in flight, Shutdown
	// would have returned immediately.
	time.Sleep(300 * time.Millisecond)

	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown() }()
	time.Sleep(300 * time.Millisecond)

	select {
	case err := <-shutdownDone:
		t.Fatalf("shutdown finished while a request was still in flight: %v", err)
	default:
	}

	// Now the handler gets its body, and goes to the database.
	if _, err := pw.Write([]byte(`"name":"In Flight"}`)); err != nil {
		t.Fatal(err)
	}
	if err := pw.Close(); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("the in-flight request failed during shutdown: %v", r.err)
	}
	if r.code != http.StatusCreated {
		t.Fatalf("the in-flight request got %d during shutdown: %s", r.code, r.body)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown reported: %v", err)
	}

	var n int
	if err := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE email = 'inflight-body@example.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the in-flight request's write left %d rows, want 1", n)
	}
}

// And a request already inside PostgreSQL when the signal arrives finishes too.
//
// This is the weaker of the two — pgxpool.Close waits for a checked-out
// connection, so it would pass even with the ordering reversed — but it is the
// scenario an operator actually pictures, and it proves the drain waits for a
// slow query rather than cutting it off at the listener.
func TestShutdown_requestBlockedInPostgresFinishes(t *testing.T) {
	e := newEnv(t)
	srv := startServer(t, e, 30*time.Second)

	// Hold the table so the request cannot proceed.
	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		code int
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, body, err := requestErr(srv.Addr(), http.MethodPost, "/users",
			`{"email":"inflight@example.com","name":"In Flight"}`, 30*time.Second)
		done <- result{code, body, err}
	}()

	// Wait until the request really is stuck on the lock, rather than guessing
	// with a sleep. This is the difference between testing the ordering and
	// testing the scheduler.
	waitForLockWait(t, e, "users")

	// The signal arrives now.
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- srv.Shutdown() }()

	// Give shutdown a moment to get into its drain, then let the request go.
	// A pool closed too early would already have broken it by this point.
	time.Sleep(300 * time.Millisecond)
	select {
	case r := <-done:
		t.Fatalf("the blocked request returned during the drain: %d %s (%v)", r.code, r.body, r.err)
	default:
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("the in-flight request failed during shutdown: %v", r.err)
	}
	if r.code != http.StatusCreated {
		t.Fatalf("the in-flight request got %d during shutdown: %s", r.code, r.body)
	}

	if err := <-shutdownDone; err != nil {
		t.Fatalf("shutdown reported: %v", err)
	}

	// The row is committed: the request was not merely answered, its work
	// landed.
	var n int
	if err := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE email = 'inflight@example.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the in-flight request's write left %d rows, want 1", n)
	}
}

// A request that arrives after shutdown has begun is refused, not half-served.
func TestShutdown_refusesNewRequests(t *testing.T) {
	e := newEnv(t)
	srv := startServer(t, e, 10*time.Second)

	if err := srv.Shutdown(); err != nil {
		t.Fatalf("shutting down: %v", err)
	}

	_, _, err := requestErr(srv.Addr(), http.MethodPost, "/users",
		`{"email":"too-late@example.com","name":"Too Late"}`, 3*time.Second)
	if err == nil {
		t.Fatal("a request after shutdown was served")
	}

	var n int
	if qerr := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE email = 'too-late@example.com'`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 0 {
		t.Errorf("a refused request wrote %d rows", n)
	}
}

// Starting and stopping the application leaves no goroutines behind.
//
// A leak here is the kind that does not show up until a process has been
// reloading configuration for a week, so it is worth one test that starts the
// whole thing several times and counts.
func TestShutdown_leavesNoGoroutines(t *testing.T) {
	e := newEnv(t)

	// One cycle first, so that whatever is initialised lazily — the driver's
	// type registry, the resolver's caches — is already up before counting.
	srv := startServer(t, e, 5*time.Second)
	if code, body := request(t, srv.Addr(), http.MethodPost, "/users",
		`{"email":"warmup@example.com","name":"Warmup"}`); code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	if err := srv.Shutdown(); err != nil {
		t.Fatal(err)
	}
	settle()
	before := runtime.NumGoroutine()

	for i := range 3 {
		srv := startServer(t, e, 5*time.Second)
		code, body := request(t, srv.Addr(), http.MethodGet, "/users/999999", "")
		if code != http.StatusNotFound {
			t.Fatalf("cycle %d: %d %s", i, code, body)
		}
		if err := srv.Shutdown(); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	settle()
	after := runtime.NumGoroutine()

	// An exact equality here would be flaky: the runtime's own goroutines come
	// and go. A leak of one per cycle would show as three.
	if after-before > 2 {
		buf := make([]byte, 1<<16)
		buf = buf[:runtime.Stack(buf, true)]
		t.Errorf("goroutines went from %d to %d over three start/stop cycles:\n%s", before, after, buf)
	}
}

// Shutdown reports the timeout rather than waiting forever, and still closes the
// database.
//
// A shutdown that hangs is worse than one that gives up: the orchestrator kills
// the process anyway, and the log says nothing about why.
func TestShutdown_timesOutAndStillClosesThePool(t *testing.T) {
	e := newEnv(t)
	srv := startServer(t, e, 500*time.Millisecond)

	blocker, err := e.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(t.Context(), `LOCK TABLE users IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())

	go func() {
		_, _, _ = requestErr(srv.Addr(), http.MethodPost, "/users",
			`{"email":"stuck@example.com","name":"Stuck"}`, 30*time.Second)
	}()
	waitForLockWait(t, e, "users")

	start := time.Now()
	err = srv.Shutdown()
	elapsed := time.Since(start)

	if err == nil {
		t.Error("a shutdown that could not drain reported success")
	} else if !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("the error does not say what failed: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("shutdown took %v with a 500ms timeout", elapsed)
	}
	// Whatever happened, the database handle is not left open.
	if perr := srv.Pool().Ping(context.Background()); perr == nil {
		t.Error("a timed-out shutdown left the pool open")
	}
}

// request runs one HTTP request against a real listener and fails the test if
// the transport itself failed.
func request(t *testing.T, addr, method, path, body string) (int, string) {
	t.Helper()
	code, out, err := requestErr(addr, method, path, body, 15*time.Second)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return code, out
}

// requestErr runs one HTTP request and returns the transport error rather than
// failing, because several tests here are about what that error is.
func requestErr(addr, method, path, body string, timeout time.Duration) (int, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+addr+path, r)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")

	// A fresh transport per request, so a pooled keep-alive connection from an
	// earlier test cannot make a shut-down server look reachable.
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()

	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(out), nil
}

// waitForLockWait blocks until some backend is waiting on a lock for the table,
// which is how these tests know a request is genuinely in flight rather than
// merely started.
func waitForLockWait(t *testing.T, e *env, table string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := e.pool.QueryRow(t.Context(), `
			SELECT EXISTS (
			  SELECT 1 FROM pg_stat_activity a
			  JOIN pg_locks l ON l.pid = a.pid
			  WHERE a.datname = current_database()
			    AND a.wait_event_type = 'Lock'
			    AND NOT l.granted
			    AND l.relation = to_regclass($1)
			)`, table).Scan(&waiting)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting for the lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no request ever blocked on the lock")
}

// settle gives finished goroutines a chance to be reaped before they are
// counted.
func settle() {
	for range 5 {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
}
