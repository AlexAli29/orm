package production_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"example.com/production/domain"
	"example.com/production/transport/chiapi"
	"example.com/production/transport/fiberapi"
	"example.com/production/transport/ginapi"
	"example.com/production/transport/httpapi"
	"github.com/gofiber/fiber/v2"
)

// The same application, through four routers.
//
// The point of running the identical scenarios against every transport is that
// the answers should not depend on which one is in front: the same request
// succeeds, the same conflict is a 409, and the same failure says the same
// non-specific sentence. A framework that changed any of those would be
// changing the application, which is exactly what the layering is supposed to
// prevent.

// transport is one router under test, reduced to the one thing a test needs:
// the ability to answer a request.
type transport struct {
	name string
	// do runs a request and returns the status and the body. Fiber does not
	// implement http.Handler, so this is the smallest interface all four share.
	do func(t *testing.T, e *env, method, path, body string) (int, string)
}

func handlerTransport(name string, build func(*env) http.Handler) transport {
	return transport{name: name, do: func(t *testing.T, e *env, method, path, body string) (int, string) {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req := httptest.NewRequestWithContext(t.Context(), method, path, r)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		build(e).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}}
}

// transports lists every router the example ships. Adding one here is what
// makes it subject to all the rules below at once.
func transports() []transport {
	return []transport{
		handlerTransport("net/http", func(e *env) http.Handler {
			return httpapi.New(e.svc, e.log).Routes()
		}),
		handlerTransport("chi", func(e *env) http.Handler {
			return chiapi.New(e.svc, e.log).Router()
		}),
		handlerTransport("gin", func(e *env) http.Handler {
			return ginapi.New(e.svc, e.log).Engine()
		}),
		{name: "fiber", do: func(t *testing.T, e *env, method, path, body string) (int, string) {
			t.Helper()
			app := fiberapi.New(t.Context(), e.svc, e.log).App()
			var r io.Reader
			if body != "" {
				r = strings.NewReader(body)
			}
			req := httptest.NewRequest(method, path, r)
			req.Header.Set("Content-Type", "application/json")
			// Fiber's own test driver: it runs the request through the real
			// fasthttp adapter rather than around it, which is the only way the
			// context question this transport documents is actually exercised.
			resp, err := app.Test(req, 10_000)
			if err != nil {
				t.Fatalf("fiber: %v", err)
			}
			defer resp.Body.Close()
			out, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("reading the fiber response: %v", err)
			}
			return resp.StatusCode, string(out)
		}},
	}
}

// Every transport serves the whole scenario the same way.
func TestTransports_sameApplicationThroughEveryRouter(t *testing.T) {
	for _, tr := range transports() {
		t.Run(tr.name, func(t *testing.T) {
			e := newEnv(t)

			// A success, and the id it hands back is real.
			code, body := tr.do(t, e, http.MethodPost, "/users",
				`{"email":"through-`+tr.name+`@example.com","name":"Router"}`)
			if code != http.StatusCreated {
				t.Fatalf("creating a user: %d %s", code, body)
			}
			var created struct {
				ID    int64  `json:"id"`
				Email string `json:"email"`
			}
			if err := json.Unmarshal([]byte(body), &created); err != nil {
				t.Fatalf("the response is not JSON: %v (%s)", err, body)
			}
			if created.ID == 0 {
				t.Fatalf("the created user has no id: %s", body)
			}

			// A read of what was written, through the same router.
			code, body = tr.do(t, e, http.MethodGet, fmt.Sprintf("/users/%d", created.ID), "")
			if code != http.StatusOK {
				t.Fatalf("reading the user back: %d %s", code, body)
			}
			if !strings.Contains(body, "through-"+tr.name+"@example.com") {
				t.Errorf("the user read back is not the one written: %s", body)
			}

			// A failure the database decides: the unique index on email.
			code, body = tr.do(t, e, http.MethodPost, "/users",
				`{"email":"through-`+tr.name+`@example.com","name":"Again"}`)
			if code != http.StatusConflict {
				t.Errorf("a duplicate email gave %d, want 409: %s", code, body)
			}
			assertNoLeak(t, body)

			// A failure the domain decides.
			code, body = tr.do(t, e, http.MethodPost, "/users", `{"email":"","name":"Nameless"}`)
			if code != http.StatusBadRequest {
				t.Errorf("an empty email gave %d, want 400: %s", code, body)
			}

			// A body that is not JSON at all.
			code, body = tr.do(t, e, http.MethodPost, "/users", `not json`)
			if code != http.StatusBadRequest {
				t.Errorf("a malformed body gave %d, want 400: %s", code, body)
			}
			assertNoLeak(t, body)

			// Something that is not there.
			code, body = tr.do(t, e, http.MethodGet, "/users/999999", "")
			if code != http.StatusNotFound {
				t.Errorf("a missing user gave %d, want 404: %s", code, body)
			}
			assertNoLeak(t, body)
		})
	}
}

// A cancelled request does not leave a row behind, whichever router carried it.
//
// This matters more than it looks: the transaction is owned by the service, so
// if a router quietly substituted a context that ignores cancellation, the
// write would commit after the client had gone.
func TestTransports_cancellationRollsBackEverywhere(t *testing.T) {
	// Fiber is excluded here and tested separately below: its context comes
	// from the middleware's deadline rather than from the client's request, and
	// that difference is documented rather than papered over.
	for _, tr := range transports()[:3] {
		t.Run(tr.name, func(t *testing.T) {
			e := newEnv(t)

			ctx, cancel := context.WithCancel(t.Context())
			cancel()

			var h http.Handler
			switch tr.name {
			case "net/http":
				h = httpapi.New(e.svc, e.log).Routes()
			case "chi":
				h = chiapi.New(e.svc, e.log).Router()
			case "gin":
				h = ginapi.New(e.svc, e.log).Engine()
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/users",
				strings.NewReader(`{"email":"cancelled-`+tr.name+`@example.com","name":"Gone"}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code == http.StatusCreated {
				t.Fatalf("a cancelled request created a user: %s", rec.Body)
			}
			var n int
			if err := e.pool.QueryRow(t.Context(),
				`SELECT count(*) FROM users WHERE email = $1`,
				"cancelled-"+tr.name+"@example.com").Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("a cancelled request left %d rows behind", n)
			}
		})
	}
}

// Fiber: the context that reaches the database is a real one, and it outlives
// nothing.
//
// The claim in the package comment is that handlers pass c.UserContext() and
// never c.Context(), because fasthttp pools the latter and reuses it for a
// later request. This test states the two halves that can be checked from
// outside: the context the middleware installs is not the fasthttp one, and it
// stays valid for the whole of the handler's database work.
func TestFiber_contextIsNotThePooledRequestCtx(t *testing.T) {
	e := newEnv(t)
	api := fiberapi.New(t.Context(), e.svc, e.log)
	app := api.App()

	// A probe route added to the same app, so it goes through the same
	// middleware the real handlers do.
	var (
		userType  string
		fiberType string
		stillOK   error
		deadline  bool
	)
	app.Get("/probe-context", func(c *fiber.Ctx) error {
		uc := c.UserContext()
		userType = fmt.Sprintf("%T", uc)
		fiberType = fmt.Sprintf("%T", c.Context())
		_, deadline = uc.Deadline()

		// The context works for real database work, which is what a handler
		// does with it.
		if _, err := e.svc.CreateUser(uc, domainUser("fiber-probe@example.com", "Probe")); err != nil {
			stillOK = err
		}
		return c.SendString("ok")
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/probe-context", nil), 10_000)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the probe returned %d", resp.StatusCode)
	}

	if stillOK != nil {
		t.Errorf("database work with c.UserContext() failed: %v", stillOK)
	}
	if userType == fiberType {
		t.Errorf("c.UserContext() is the same type as c.Context() (%s); the middleware did not install one", userType)
	}
	if strings.Contains(userType, "fasthttp") {
		t.Errorf("the context handed to the database is %s, which fasthttp pools and reuses", userType)
	}
	if !deadline {
		t.Error("the request context has no deadline, so one request could hold a connection forever")
	}

	// The row is there, which is the other half of "the context stayed valid":
	// a context cancelled underneath the operation would have rolled it back.
	var n int
	if err := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE email = 'fiber-probe@example.com'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("the probe's write left %d rows, want 1", n)
	}
}

// Fiber: cancelling the application's base context cancels the real handlers'
// database work.
//
// This is the property the middleware exists for, and it is also the test that
// tells c.UserContext() from c.Context(). The base context is cancelled before
// the request is served, and then a *real* route — the one the application
// ships, not a probe added by the test — is called. A handler using
// c.UserContext() is holding a context descended from the cancelled base, so
// the write never happens. A handler using c.Context() is holding fasthttp's
// pooled request context, which knows nothing about the application's lifetime,
// and the write lands.
//
// It is also why the base context is a constructor parameter rather than
// context.Background().
func TestFiber_baseContextCancellationReachesTheDatabase(t *testing.T) {
	e := newEnv(t)

	base, cancel := context.WithCancel(t.Context())
	api := fiberapi.New(base, e.svc, e.log)
	app := api.App()

	cancel() // as shutdown would, before the request is served

	req := httptest.NewRequest(http.MethodPost, "/users",
		strings.NewReader(`{"email":"never@example.com","name":"Never"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, terr := app.Test(req, 10_000)
	if terr != nil {
		t.Fatal(terr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusCreated {
		t.Fatalf("the handler created a user after the base context was cancelled: %s", body)
	}

	var n int
	if qerr := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM users WHERE email = 'never@example.com'`).Scan(&n); qerr != nil {
		t.Fatal(qerr)
	}
	if n != 0 {
		t.Errorf("a cancelled write left %d rows behind; the handler's context is not the one the middleware installed", n)
	}
}

// domainUser is the shorthand these tests use to call the service directly.
func domainUser(email, name string) domain.User {
	return domain.User{Email: email, Name: name}
}

// isCancellation reports whether an error is the context being cancelled,
// through whatever wrapping the driver and the service put around it.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(err.Error(), "context canceled")
}

// assertNoLeak states the rule every error response obeys, whichever router
// produced it: a client learns what went wrong in categories, and nothing about
// the database, the schema or another user.
func assertNoLeak(t *testing.T, body string) {
	t.Helper()
	for _, forbidden := range []string{
		"SQLSTATE", "23505", "23503", "42P01",
		"users_email_key", "projects_slug_key",
		"INSERT", "SELECT ", "UPDATE ",
		"postgres://", "password",
		"pgx", "pgconn",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the response contains %q:\n%s", forbidden, body)
		}
	}
}
