package production_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"example.com/production/domain"
)

// The canonical example's integration tests.
//
// They exercise the HTTP surface with httptest rather than a real listener,
// because a real listener proves nothing about a handler and costs a port. The
// two tests that are genuinely about the network — graceful shutdown — use one
// deliberately, and say so.

func post(t *testing.T, e *env, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, &buf)
	rec := httptest.NewRecorder()
	e.api.Routes().ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, e *env, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	e.api.Routes().ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return out
}

// The success path: a user, then a project with its first task, then reading it
// back with the task loaded through the relation.
func TestAPI_success(t *testing.T) {
	e := newEnv(t)

	rec := post(t, e, "/users", map[string]any{"email": "ada@example.com", "name": "Ada"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a user: %d %s", rec.Code, rec.Body)
	}
	user := decode(t, rec)
	ownerID := int64(user["id"].(float64))

	rec = post(t, e, "/projects", map[string]any{
		"owner_id": ownerID, "slug": "analytics", "name": "Analytics",
		"first_task": "write the schema",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("creating a project: %d %s", rec.Code, rec.Body)
	}

	rec = get(t, e, "/projects/analytics")
	if rec.Code != http.StatusOK {
		t.Fatalf("reading the project: %d %s", rec.Code, rec.Body)
	}
	project := decode(t, rec)
	tasks, ok := project["tasks"].([]any)
	if !ok || len(tasks) != 1 {
		t.Fatalf("the project came back with %v tasks", project["tasks"])
	}
	if got := tasks[0].(map[string]any)["title"]; got != "write the schema" {
		t.Errorf("the first task is %v", got)
	}

	// And the owner's list.
	rec = get(t, e, "/users/"+itoa(ownerID)+"/projects")
	if rec.Code != http.StatusOK {
		t.Fatalf("listing projects: %d %s", rec.Code, rec.Body)
	}
}

// The database-backed failures, each becoming the status a client should see.
func TestAPI_databaseErrors(t *testing.T) {
	e := newEnv(t)

	rec := post(t, e, "/users", map[string]any{"email": "dup@example.com", "name": "First"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	ownerID := int64(decode(t, rec)["id"].(float64))

	t.Run("a unique violation is 409", func(t *testing.T) {
		rec := post(t, e, "/users", map[string]any{"email": "dup@example.com", "name": "Second"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("a duplicate email gave %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a foreign key violation is 400", func(t *testing.T) {
		rec := post(t, e, "/projects", map[string]any{
			"owner_id": 999999, "slug": "orphan", "name": "Orphan", "first_task": "x",
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("an unknown owner gave %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("a missing row is 404", func(t *testing.T) {
		if rec := get(t, e, "/projects/nothing-here"); rec.Code != http.StatusNotFound {
			t.Fatalf("a missing project gave %d: %s", rec.Code, rec.Body)
		}
		if rec := get(t, e, "/users/999999"); rec.Code != http.StatusNotFound {
			t.Fatalf("a missing user gave %d: %s", rec.Code, rec.Body)
		}
	})

	t.Run("an invalid request is 400", func(t *testing.T) {
		if rec := post(t, e, "/projects", map[string]any{"owner_id": ownerID}); rec.Code != http.StatusBadRequest {
			t.Fatalf("an incomplete project gave %d: %s", rec.Code, rec.Body)
		}
	})
}

// The response a client gets must not contain what the database said.
//
// PostgreSQL's unique-violation message quotes the key — "Key
// (email)=(secret@example.com) already exists" — which is one user's data. A
// handler that echoed err.Error() would publish it to whoever guessed the
// address.
func TestAPI_errorsDoNotLeak(t *testing.T) {
	e := newEnv(t)

	const secret = "sentinel-b8f21c7a@example.com"
	if rec := post(t, e, "/users", map[string]any{"email": secret, "name": "First"}); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	rec := post(t, e, "/users", map[string]any{"email": secret, "name": "Second"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, forbidden := range []string{
		secret,            // the value that collided
		"users_email_key", // the constraint, which names the schema
		"SQLSTATE",        // the driver's rendering
		"23505",           // the code
		"INSERT",          // any SQL at all
		"postgres://",     // any hint of a DSN
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the response contains %q:\n%s", forbidden, body)
		}
	}
	// What it does contain is a sentence.
	if got := decode(t, rec)["error"]; got != "that already exists" {
		t.Errorf("the response says %q", got)
	}
}

// The multi-step operation is atomic: when the third step fails, the first two
// are gone.
//
// The failure is forced by a real constraint rather than by a mock, because the
// claim is about PostgreSQL's transaction and a mock would be testing the mock.
func TestAPI_transactionRollsBack(t *testing.T) {
	e := newEnv(t)

	rec := post(t, e, "/users", map[string]any{"email": "tx@example.com", "name": "Tx"})
	ownerID := int64(decode(t, rec)["id"].(float64))

	// A project that succeeds, so there is something to compare against.
	if rec := post(t, e, "/projects", map[string]any{
		"owner_id": ownerID, "slug": "first", "name": "First", "first_task": "a",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	auditBefore := count(t, e, `SELECT count(*) FROM audit_entries`)
	projectsBefore := count(t, e, `SELECT count(*) FROM projects`)
	tasksBefore := count(t, e, `SELECT count(*) FROM tasks`)

	// Now one whose first step succeeds and whose later steps cannot: the slug
	// is unique, so re-using it fails the project insert; to fail *later* than
	// the first step, break the audit table instead.
	// NOT VALID so the constraint applies to new rows only: the successful
	// operation above already wrote an audit row, and the point here is to fail
	// the *next* one at its third step.
	if _, err := e.pool.Exec(t.Context(),
		`ALTER TABLE audit_entries ADD CONSTRAINT audit_action_check
		 CHECK (action <> 'project.created') NOT VALID`); err != nil {
		t.Fatal(err)
	}

	rec = post(t, e, "/projects", map[string]any{
		"owner_id": ownerID, "slug": "second", "name": "Second", "first_task": "b",
	})
	if rec.Code < 400 {
		t.Fatalf("the failing operation returned %d: %s", rec.Code, rec.Body)
	}

	// The assertion is on the database, not on the error. All three steps are
	// gone, including the two that had succeeded.
	if got := count(t, e, `SELECT count(*) FROM projects`); got != projectsBefore {
		t.Errorf("%d projects survived a rolled-back operation (was %d)", got, projectsBefore)
	}
	if got := count(t, e, `SELECT count(*) FROM tasks`); got != tasksBefore {
		t.Errorf("%d tasks survived a rolled-back operation (was %d)", got, tasksBefore)
	}
	if got := count(t, e, `SELECT count(*) FROM audit_entries`); got != auditBefore {
		t.Errorf("%d audit rows survived (was %d)", got, auditBefore)
	}
	if got := count(t, e, `SELECT count(*) FROM projects WHERE slug = 'second'`); got != 0 {
		t.Error("the project from the failed operation is still there")
	}
}

// Cancelling the request cancels the transaction, and nothing partial survives.
func TestAPI_cancellationRollsBack(t *testing.T) {
	e := newEnv(t)

	rec := post(t, e, "/users", map[string]any{"email": "cancel@example.com", "name": "C"})
	ownerID := int64(decode(t, rec)["id"].(float64))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := e.svc.CreateProject(ctx, domain.NewProject{
		OwnerID: ownerID, Slug: "cancelled", Name: "Cancelled", FirstTask: "never",
	})
	if err == nil {
		t.Fatal("the operation succeeded with a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Logf("the error was %v", err)
	}
	if got := count(t, e, `SELECT count(*) FROM projects WHERE slug = 'cancelled'`); got != 0 {
		t.Error("a cancelled operation left a project behind")
	}
	// And the pool still works.
	if _, err := e.svc.User(t.Context(), ownerID); err != nil {
		t.Fatalf("after cancellation the pool is unusable: %v", err)
	}
}

// A cancelled request produces no response body and no panic.
func TestAPI_cancelledRequest(t *testing.T) {
	e := newEnv(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/projects/anything", nil)
	rec := httptest.NewRecorder()
	e.api.Routes().ServeHTTP(rec, req)

	// The handler returns without writing, so the recorder keeps its default.
	if rec.Body.Len() != 0 {
		t.Errorf("a cancelled request produced a body: %s", rec.Body)
	}
}

// Concurrent requests share the service and the store, which hold no mutable
// state. The race detector is the assertion.
func TestAPI_concurrentRequests(t *testing.T) {
	e := newEnv(t)

	rec := post(t, e, "/users", map[string]any{"email": "conc@example.com", "name": "C"})
	ownerID := int64(decode(t, rec)["id"].(float64))

	done := make(chan error, 32)
	for i := range 32 {
		go func(i int) {
			slug := "p" + itoa(int64(i))
			rec := post(t, e, "/projects", map[string]any{
				"owner_id": ownerID, "slug": slug, "name": slug, "first_task": "t",
			})
			if rec.Code != http.StatusCreated {
				done <- errors.New(slug + ": " + rec.Body.String())
				return
			}
			done <- nil
		}(i)
	}
	for range 32 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if got := count(t, e, `SELECT count(*) FROM projects`); got != 32 {
		t.Errorf("%d projects, want 32", got)
	}
}

func count(t *testing.T, e *env, sql string) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

var _ = time.Second
