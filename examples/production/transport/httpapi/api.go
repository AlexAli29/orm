// Package httpapi is the canonical transport: net/http and nothing else.
//
// It is the reference the other three transports are measured against. What it
// demonstrates is the whole request lifecycle — the request's context reaching
// the database, an error becoming a status code without leaking what the
// database said, and the health endpoints being three different questions
// rather than one.
//
// The ORM appears nowhere in this package. It talks to a service and to the
// domain's error values, which is what makes swapping chi, Gin or Fiber in
// beside it a matter of writing routes.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"example.com/production/domain"
	"example.com/production/service"
)

// API turns HTTP requests into service calls.
//
// It holds its dependencies rather than reaching for globals: the service and
// the logger are constructor arguments, so a test builds one with whatever it
// wants and the program builds one with what it configured.
type API struct {
	svc *service.Service
	log *slog.Logger
}

// New builds the API.
func New(svc *service.Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{svc: svc, log: log}
}

// Routes returns the application's handlers on a stdlib mux.
//
// Go 1.22's patterns carry the method and the wildcard, so there is no router
// dependency here and nothing to explain about one.
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", a.createUser)
	mux.HandleFunc("GET /users/{id}", a.getUser)
	mux.HandleFunc("POST /projects", a.createProject)
	mux.HandleFunc("GET /projects/{slug}", a.getProject)
	mux.HandleFunc("GET /users/{id}/projects", a.listProjects)
	return mux
}

// CreateUserRequest and the rest are the wire shapes, declared here rather than
// reused from the domain: the domain type is what the application means and
// this is what a client sends, and letting one be the other is how an internal
// field becomes a public API by accident.
type CreateUserRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type CreateProjectRequest struct {
	OwnerID   int64  `json:"owner_id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	FirstTask string `json:"first_task"`
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	// r.Context() and nothing else. A handler that reached for
	// context.Background() here would keep the query running after the client
	// had gone, which is the bug this line exists to not have.
	user, err := a.svc.CreateUser(r.Context(), domain.User{Email: req.Email, Name: req.Name})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, userResponse(user))
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
		return
	}
	user, err := a.svc.User(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, userResponse(user))
}

func (a *API) createProject(w http.ResponseWriter, r *http.Request) {
	var req CreateProjectRequest
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
	writeJSON(w, http.StatusCreated, projectResponse(project))
}

func (a *API) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := a.svc.Project(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(project))
}

func (a *API) listProjects(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
		return
	}
	projects, err := a.svc.ProjectsForOwner(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, projectResponse(p))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

// fail is the whole error-mapping layer, and it is deliberately this small.
//
// It maps the domain's three error values onto status codes and sends a fixed
// message. What it does not do is put err.Error() in the response: a PostgreSQL
// error's message quotes the row that caused it, so a unique violation carries
// the email address that collided, and a handler that echoed it would publish
// one user's data to whoever guessed it. The whole error goes to the log, where
// the trust boundary is documented; the client gets a sentence.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := Classify(err)

	switch {
	case status == 0:
		// The client went away. There is nobody to answer and writing a
		// response to a closed connection is noise; the log line is the record.
		a.log.DebugContext(r.Context(), "request cancelled",
			"method", r.Method, "path", r.URL.Path, "error", err)
		return
	case status >= 500:
		// The detail is logged and not sent. This is the only branch where the
		// two differ, and it is the branch where the difference matters.
		a.log.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
	default:
		a.log.InfoContext(r.Context(), "request rejected",
			"method", r.Method, "path", r.URL.Path, "status", status, "error", err)
	}
	writeJSON(w, status, map[string]string{"error": message})
}

// Classify maps an error to a status code and the message a client is told.
//
// A status of zero means the client is gone and there is nothing to write. The
// messages are constants: they say what kind of thing went wrong and never
// carry a value from the request or from the database.
//
// It is exported because the other three transports use it. That is the point
// of the example — the error model belongs to the application, and each
// transport only has to know how to send a status code.
func Classify(err error) (status int, message string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, context.Canceled):
		return 0, ""
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "the request took too long"
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, "that already exists"
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest, "the request was not valid"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

func userResponse(u domain.User) map[string]any {
	return map[string]any{"id": u.ID, "email": u.Email, "name": u.Name}
}

func projectResponse(p domain.Project) map[string]any {
	out := map[string]any{"id": p.ID, "owner_id": p.OwnerID, "slug": p.Slug, "name": p.Name}
	if p.Tasks != nil {
		tasks := make([]any, 0, len(p.Tasks))
		for _, t := range p.Tasks {
			tasks = append(tasks, map[string]any{"id": t.ID, "title": t.Title, "done": t.Done})
		}
		out["tasks"] = tasks
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// PathSlug is used by the framework transports, which extract the parameter
// their own way and then call the same service.
func PathSlug(s string) string { return strings.TrimSpace(s) }
