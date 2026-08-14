// Package chiapi serves the same application through chi.
//
// The point of this package is how little there is in it. There is no ORM
// middleware, no chi-specific ORM API, and no second copy of the business
// logic — it builds a chi router, pulls the URL parameters chi's way, and calls
// the same [service.Service] the stdlib transport calls, mapping errors with
// the same [httpapi.Classify].
//
// chi routes ordinary http.Handlers and its request carries an ordinary
// *http.Request, so r.Context() is the request's context and reaches the
// database unchanged. There is nothing to be careful about here, which is the
// finding.
package chiapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"example.com/production/domain"
	"example.com/production/service"
	"example.com/production/transport/httpapi"
	"github.com/go-chi/chi/v5"
)

// API serves the application over chi.
type API struct {
	svc *service.Service
	log *slog.Logger
}

// New builds the chi API. Dependencies are arguments, not globals, and not
// values stashed in the router's context by middleware — a handler that fetched
// its database out of the context would be one the compiler cannot check.
func New(svc *service.Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{svc: svc, log: log}
}

// Router returns the routes.
func (a *API) Router() chi.Router {
	r := chi.NewRouter()
	r.Post("/users", a.createUser)
	r.Get("/users/{id}", a.getUser)
	r.Post("/projects", a.createProject)
	r.Get("/projects/{slug}", a.getProject)
	return r
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var req httpapi.CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	user, err := a.svc.CreateUser(r.Context(), domain.User{Email: req.Email, Name: req.Name})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": user.ID, "email": user.Email})
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
		return
	}
	user, err := a.svc.User(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "email": user.Email})
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var req httpapi.CreateProjectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	project, err := a.svc.CreateProject(r.Context(), domain.NewProject{
		OwnerID: req.OwnerID, Slug: req.Slug, Name: req.Name, FirstTask: req.FirstTask,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": project.ID, "slug": project.Slug})
}

func (a *API) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := a.svc.Project(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": project.ID, "slug": project.Slug, "tasks": len(project.Tasks),
	})
}

// fail uses the shared classification, so a not-found is 404 here for the same
// reason it is 404 in the stdlib transport.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := httpapi.Classify(err)
	if status == 0 {
		return
	}
	if status >= 500 {
		a.log.ErrorContext(r.Context(), "request failed", "path", r.URL.Path, "error", err)
	}
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
