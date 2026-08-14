// Package fiberapi serves the same application through Fiber, and is the one
// transport where the context needs thought.
//
// # Why this package does not use c.Context()
//
// Fiber is built on fasthttp, and `c.Context()` returns a *fasthttp.RequestCtx.
// That type happens to satisfy context.Context — it has Deadline, Done, Err and
// Value — which makes it look like the obvious thing to hand to the database.
// It is not, for one reason: fasthttp pools RequestCtx values and reuses them
// for later requests once the handler returns. Anything still holding one is
// holding an object that now belongs to a different request. A pgx operation
// that outlived the handler — a stream still being read, a query whose
// cancellation is watched — would be watching the wrong request's lifecycle.
//
// # What this package does instead
//
// A middleware derives an ordinary context from the server's base context,
// gives it the request's deadline, and stores it with SetUserContext. Handlers
// pass c.UserContext() to the service, and that is what reaches pgx.
//
// It is a real context.Context with an ordinary lifetime, so:
//
//	shutdown cancels it        because it descends from the base context
//	a slow request cancels it  because it carries a deadline
//	the handler returning       does not invalidate it
//
// What it does not do is cancel when the client disconnects. Fiber v2 does not
// surface that as a context cancellation, and this example does not pretend
// otherwise — inventing a goroutine to watch for it would be inventing a
// behaviour Fiber does not have. The deadline is the bound that exists, and it
// is documented rather than implied.
//
// Everything else here is the same as the other transports: the same service,
// the same error classification, no Fiber-specific ORM API.
package fiberapi

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"example.com/production/domain"
	"example.com/production/service"
	"example.com/production/transport/httpapi"
	"github.com/gofiber/fiber/v2"
)

// API serves the application over Fiber.
type API struct {
	svc *service.Service
	log *slog.Logger
	// base is the context every request's context descends from, so that
	// cancelling it at shutdown cancels the database work in flight.
	base context.Context
	// timeout bounds one request's database work.
	timeout time.Duration
}

// New builds the Fiber API.
//
// base is the application's context: cancel it and every in-flight request's
// database work is cancelled with it.
func New(base context.Context, svc *service.Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	if base == nil {
		base = context.Background()
	}
	return &API{svc: svc, log: log, base: base, timeout: 30 * time.Second}
}

// App returns a configured Fiber app.
func (a *API) App() *fiber.App {
	app := fiber.New(fiber.Config{DisableStartupMessage: true})
	app.Use(a.withContext)
	app.Post("/users", a.createUser)
	app.Get("/users/:id", a.getUser)
	app.Post("/projects", a.createProject)
	app.Get("/projects/:slug", a.getProject)
	return app
}

// withContext gives every request a real context.Context to work with.
//
// This is the whole Fiber-specific part of the example. The context is derived
// from the application's base context so shutdown reaches it, carries a
// deadline so one request cannot hold a connection forever, and is cancelled
// when the handler returns so nothing outlives the request by accident.
func (a *API) withContext(c *fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(a.base, a.timeout)
	defer cancel()
	c.SetUserContext(ctx)
	return c.Next()
}

func (a *API) createUser(c *fiber.Ctx) error {
	var req httpapi.CreateUserRequest
	if err := c.BodyParser(&req); err != nil {
		return a.fail(c, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
	}
	// c.UserContext(), never c.Context(). See the package comment.
	user, err := a.svc.CreateUser(c.UserContext(), domain.User{Email: req.Email, Name: req.Name})
	if err != nil {
		return a.fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": user.ID, "email": user.Email})
}

func (a *API) getUser(c *fiber.Ctx) error {
	id, err := strconv.ParseInt(c.Params("id"), 10, 64)
	if err != nil {
		return a.fail(c, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
	}
	user, err := a.svc.User(c.UserContext(), id)
	if err != nil {
		return a.fail(c, err)
	}
	return c.JSON(fiber.Map{"id": user.ID, "email": user.Email})
}

func (a *API) createProject(c *fiber.Ctx) error {
	var req httpapi.CreateProjectRequest
	if err := c.BodyParser(&req); err != nil {
		return a.fail(c, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
	}
	project, err := a.svc.CreateProject(c.UserContext(), domain.NewProject{
		OwnerID: req.OwnerID, Slug: req.Slug, Name: req.Name, FirstTask: req.FirstTask,
	})
	if err != nil {
		return a.fail(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{"id": project.ID, "slug": project.Slug})
}

func (a *API) getProject(c *fiber.Ctx) error {
	project, err := a.svc.Project(c.UserContext(), c.Params("slug"))
	if err != nil {
		return a.fail(c, err)
	}
	return c.JSON(fiber.Map{
		"id": project.ID, "slug": project.Slug, "tasks": len(project.Tasks),
	})
}

func (a *API) fail(c *fiber.Ctx, err error) error {
	status, message := httpapi.Classify(err)
	if status == 0 {
		// The client is gone. Fiber wants a status anyway; 499 is the
		// convention for it and nothing is read from the body.
		return c.SendStatus(499)
	}
	if status >= 500 {
		a.log.ErrorContext(c.UserContext(), "request failed", "path", c.Path(), "error", err)
	}
	return c.Status(status).JSON(fiber.Map{"error": message})
}
