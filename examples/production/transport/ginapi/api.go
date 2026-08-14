// Package ginapi serves the same application through Gin.
//
// As with chi, the notable thing is the absence: there is no ormgin package, no
// Gin-aware ORM API, and no database hidden in the Gin context. The handlers
// are methods on a struct that holds the service, which is how the compiler
// keeps track of what a handler needs — a dependency fetched out of
// *gin.Context with a string key is one that fails at run time in production
// rather than at compile time here.
//
// Gin's *gin.Context embeds the request, and c.Request.Context() is the
// ordinary request context. That is what goes to the database. c itself
// implements context.Context too, and this package deliberately does not use it
// for that: it is a request-scoped object with its own lifecycle and its own
// mutable keyed store, and handing it to a pool as a long-lived context mixes
// two things that should not be mixed.
package ginapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"example.com/production/domain"
	"example.com/production/service"
	"example.com/production/transport/httpapi"
	"github.com/gin-gonic/gin"
)

// API serves the application over Gin.
type API struct {
	svc *service.Service
	log *slog.Logger
}

// New builds the Gin API.
func New(svc *service.Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{svc: svc, log: log}
}

// Engine returns a configured Gin engine.
func (a *API) Engine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(gin.Recovery())
	e.POST("/users", a.createUser)
	e.GET("/users/:id", a.getUser)
	e.POST("/projects", a.createProject)
	e.GET("/projects/:slug", a.getProject)
	return e
}

func (a *API) createUser(c *gin.Context) {
	var req httpapi.CreateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		a.fail(c, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	// c.Request.Context(), not c. See the package comment.
	user, err := a.svc.CreateUser(c.Request.Context(), domain.User{Email: req.Email, Name: req.Name})
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": user.ID, "email": user.Email})
}

func (a *API) getUser(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		a.fail(c, errors.Join(domain.ErrInvalid, errors.New("the id is not a number")))
		return
	}
	user, err := a.svc.User(c.Request.Context(), id)
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": user.ID, "email": user.Email})
}

func (a *API) createProject(c *gin.Context) {
	var req httpapi.CreateProjectRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		a.fail(c, errors.Join(domain.ErrInvalid, errors.New("the body is not JSON")))
		return
	}
	project, err := a.svc.CreateProject(c.Request.Context(), domain.NewProject{
		OwnerID: req.OwnerID, Slug: req.Slug, Name: req.Name, FirstTask: req.FirstTask,
	})
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"id": project.ID, "slug": project.Slug})
}

func (a *API) getProject(c *gin.Context) {
	project, err := a.svc.Project(c.Request.Context(), c.Param("slug"))
	if err != nil {
		a.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": project.ID, "slug": project.Slug, "tasks": len(project.Tasks)})
}

func (a *API) fail(c *gin.Context, err error) {
	status, message := httpapi.Classify(err)
	if status == 0 {
		c.Abort()
		return
	}
	if status >= 500 {
		a.log.ErrorContext(c.Request.Context(), "request failed",
			"path", c.Request.URL.Path, "error", err)
	}
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}
