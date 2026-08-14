// Package httpin is the driving adapter: it turns HTTP requests into use-case
// calls and use-case results into responses.
//
// It knows the core and net/http. It does not know the ORM, PostgreSQL or
// pgx — which is why it can classify an error at all. It is handed one of three
// sentinels, and three sentinels map onto status codes without anyone deciding
// what a SQLSTATE means at the edge of the system.
package httpin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"example.com/hexagonal/core/app"
	"example.com/hexagonal/core/domain"
)

// API serves the application over HTTP.
type API struct {
	svc *app.Service
	log *slog.Logger
}

// New builds the adapter.
func New(svc *app.Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{svc: svc, log: log}
}

// Routes returns the handlers.
func (a *API) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", a.register)
	mux.HandleFunc("GET /users/{id}", a.user)
	mux.HandleFunc("GET /users/{id}/projects", a.projects)
	mux.HandleFunc("POST /projects", a.startProject)
	mux.HandleFunc("GET /projects/{slug}", a.project)
	return mux
}

type registerRequest struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

type projectRequest struct {
	OwnerID int64  `json:"owner_id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
}

func (a *API) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	u, err := a.svc.Register(r.Context(), domain.User{Email: req.Email, Name: req.Name})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, map[string]any{"id": u.ID, "email": u.Email, "name": u.Name})
}

func (a *API) user(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
		return
	}
	u, err := a.svc.User(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"id": u.ID, "email": u.Email, "name": u.Name})
}

func (a *API) projects(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
		return
	}
	ps, err := a.svc.Projects(r.Context(), id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Name})
	}
	write(w, http.StatusOK, map[string]any{"projects": out})
}

func (a *API) startProject(w http.ResponseWriter, r *http.Request) {
	var req projectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.fail(w, r, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	p, err := a.svc.StartProject(r.Context(), domain.Project{
		OwnerID: req.OwnerID, Slug: req.Slug, Name: req.Name,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	write(w, http.StatusCreated, map[string]any{"id": p.ID, "slug": p.Slug})
}

func (a *API) project(w http.ResponseWriter, r *http.Request) {
	p, err := a.svc.ProjectBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	write(w, http.StatusOK, map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Name})
}

// Classify maps a domain failure onto a status and the sentence the client
// gets.
//
// The sentence is fixed per category. It is not derived from the error, because
// an error's text is written for whoever reads the log and the two audiences
// are not the same.
func Classify(err error) (int, string) {
	switch {
	case err == nil:
		return http.StatusOK, ""
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not found"
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, "that already exists"
	case errors.Is(err, domain.ErrInvalid):
		return http.StatusBadRequest, "the request is not valid"
	default:
		return http.StatusInternalServerError, "internal error"
	}
}

// fail logs what happened and tells the client only its category.
func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, message := Classify(err)
	if status >= 500 {
		a.log.ErrorContext(r.Context(), "request failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
	} else {
		a.log.InfoContext(r.Context(), "request rejected",
			"method", r.Method, "path", r.URL.Path, "status", status)
	}
	write(w, status, map[string]any{"error": message})
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
