// Command blog is a small HTTP service over the ORM.
//
// It is here to be read and run rather than to be a framework. Every handler is
// a few lines of stdlib net/http around one query, so what stands out is the
// query — which is the part worth looking at.
//
//	docker compose up -d
//	psql "$BLOG_DSN" -f schema.sql
//	go run .
//
// See README.md for the whole sequence.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/examples/blog/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		slog.Error("blog", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	dsn := os.Getenv("BLOG_DSN")
	if dsn == "" {
		return errors.New("set BLOG_DSN")
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parsing BLOG_DSN: %w", err)
	}
	// Every connection is taught this package's PostgreSQL types once, when it
	// is opened. Without it the post_status enum arrives as bytes pgx has no
	// codec for.
	cfg.AfterConnect = domain.RegisterTypes

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("opening the pool: %w", err)
	}
	defer pool.Close()

	srv := &http.Server{
		Addr:              addr(),
		Handler:           routes(&server{db: domain.New(pool)}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	slog.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func addr() string {
	if a := os.Getenv("BLOG_ADDR"); a != "" {
		return a
	}
	return ":8080"
}

type server struct {
	db *domain.DB
}

func routes(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", s.createUser)
	mux.HandleFunc("GET /users", s.listUsers)
	mux.HandleFunc("GET /users/{id}", s.getUser)
	mux.HandleFunc("PATCH /users/{id}", s.updateUser)
	mux.HandleFunc("POST /posts", s.createPost)
	mux.HandleFunc("GET /posts", s.listPosts)
	mux.HandleFunc("GET /posts/{id}", s.getPost)
	mux.HandleFunc("POST /posts/{id}/comments", s.createComment)
	mux.HandleFunc("GET /search/users", s.searchUsers)
	mux.HandleFunc("GET /reports/authors", s.authorReport)
	return mux
}

// listUsers is the query this ORM exists for.
//
// The filters come from the request, so the set of conditions is not known
// until run time — and it is still typed: Users.Email.ILike takes a string
// because email is text, and Users.Posts.Any takes a Predicate[Post] because
// posts is what the relation points at. Neither is checked by a linter or by
// PostgreSQL. They are checked by the Go compiler.
func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	var predicates []orm.Predicate[domain.User]
	if email := q.Get("email"); email != "" {
		predicates = append(predicates, domain.Users.Email.ILike("%"+email+"%"))
	}
	if q.Get("active") != "" {
		predicates = append(predicates, domain.Users.Active.Eq(q.Get("active") == "true"))
	}
	if q.Get("published") == "true" {
		// A relation used as a filter, not as a thing to load: this compiles to
		// a correlated EXISTS in the same statement and returns each user once.
		predicates = append(predicates, domain.Users.Posts.Any(
			domain.Posts.Status.Eq(domain.PostPublished),
		))
	}

	users, err := s.db.Users.Query().
		Where(orm.And(predicates...)).
		OrderBy(domain.Users.CreatedAt.Desc()).
		Limit(50).
		All(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, users)
}

// getUser loads a user and the tree hanging off them, which costs one statement
// per relation and not one per row.
func (s *server) getUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	user, err := s.db.Users.Query().
		Where(domain.Users.ID.Eq(id)).
		With(
			domain.Users.Profile,
			domain.Users.Posts.
				Where(domain.Posts.Status.Eq(domain.PostPublished)).
				OrderBy(domain.Posts.CreatedAt.Desc()).
				Limit(5).
				With(
					domain.Posts.Category,
					domain.Posts.Comments.
						OrderBy(domain.Comments.CreatedAt.Desc()).
						Limit(3).
						With(domain.Comments.Author),
				),
		).
		One(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, user)
}

// createUser shows the write layer's one rule: a Go zero value is a value.
//
// active is false in the struct and false is what is stored, because the ORM
// has no way to tell "the caller wants false" from "the caller left it alone" —
// and guessing is how a row ends up with a value nobody chose. Asking for the
// column's default is a separate, explicit thing.
func (s *server) createUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email      string `json:"email"`
		Name       string `json:"name"`
		UseDefault bool   `json:"use_default_active"`
	}
	if !decode(w, r, &in) {
		return
	}

	user := domain.User{Email: in.Email, Name: in.Name, Active: false}
	opts := []orm.InsertOpt[domain.User]{orm.Default(domain.Users.CreatedAt)}
	if in.UseDefault {
		opts = append(opts, orm.Default(domain.Users.Active))
	}

	created, err := s.db.Users.Insert(r.Context(), user, opts...)
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, created)
}

func (s *server) updateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var in struct {
		Name   *string `json:"name"`
		Active *bool   `json:"active"`
	}
	if !decode(w, r, &in) {
		return
	}

	// Only what the request actually sent is assigned. An update with nothing
	// to set, or with no WHERE, is refused rather than run.
	q := s.db.Users.Update().Where(domain.Users.ID.Eq(id))
	if in.Name != nil {
		q = q.Set(domain.Users.Name.Set(*in.Name))
	}
	if in.Active != nil {
		q = q.Set(domain.Users.Active.Set(*in.Active))
	}
	n, err := q.Exec(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	if n == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	user, err := s.db.Users.Query().Where(domain.Users.ID.Eq(id)).One(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, user)
}

// createPost writes a post and its first comments together, so either both
// exist or neither does.
func (s *server) createPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AuthorID   int64    `json:"author_id"`
		CategoryID *int64   `json:"category_id"`
		Title      string   `json:"title"`
		Body       string   `json:"body"`
		Comments   []string `json:"comments"`
	}
	if !decode(w, r, &in) {
		return
	}

	var created domain.Post
	err := s.db.Tx(r.Context(), func(tx *domain.DB) error {
		post, err := tx.Posts.Insert(r.Context(), domain.Post{
			AuthorID:   in.AuthorID,
			CategoryID: in.CategoryID,
			Title:      in.Title,
			Body:       in.Body,
		}, orm.Default(domain.Posts.Status), orm.Default(domain.Posts.CreatedAt))
		if err != nil {
			return err
		}
		for _, body := range in.Comments {
			if _, err := tx.Comments.Insert(r.Context(), domain.Comment{
				PostID:   post.ID,
				AuthorID: in.AuthorID,
				Body:     body,
			}, orm.Default(domain.Comments.CreatedAt)); err != nil {
				return err
			}
		}
		created = post
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, created)
}

func (s *server) listPosts(w http.ResponseWriter, r *http.Request) {
	posts, err := s.db.Posts.Query().
		With(
			domain.Posts.Author,
			domain.Posts.Category,
			domain.Posts.Comments.
				OrderBy(domain.Comments.CreatedAt.Desc()).
				Limit(10).
				With(domain.Comments.Author),
		).
		OrderBy(domain.Posts.CreatedAt.Desc()).
		Limit(20).
		All(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, posts)
}

func (s *server) getPost(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	post, err := s.db.Posts.Query().
		Where(domain.Posts.ID.Eq(id)).
		With(domain.Posts.Author, domain.Posts.Category, domain.Posts.Comments.With(domain.Comments.Author)).
		One(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, post)
}

func (s *server) createComment(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var in struct {
		AuthorID int64  `json:"author_id"`
		Body     string `json:"body"`
	}
	if !decode(w, r, &in) {
		return
	}

	created, err := s.db.Comments.Insert(r.Context(), domain.Comment{
		PostID:   id,
		AuthorID: in.AuthorID,
		Body:     in.Body,
	}, orm.Default(domain.Comments.CreatedAt))
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusCreated, created)
}

// searchUsers is what the escape hatch is for.
//
// Ranking by similarity is a PostgreSQL feature with no typed equivalent here,
// and inventing one would mean inventing an API for every extension anybody
// installs. Raw takes the statement and keeps the generated scanner, so the
// result is still []domain.User with nothing hand-written to maintain.
//
// The search term is a parameter. It is never formatted into the text.
func (s *server) searchUsers(w http.ResponseWriter, r *http.Request) {
	term := r.URL.Query().Get("q")
	if term == "" {
		http.Error(w, "q is required", http.StatusBadRequest)
		return
	}

	users, err := orm.Raw(s.db.Users, `
		SELECT id, email, name, active, created_at
		FROM users
		WHERE email ILIKE '%' || $1 || '%' OR name ILIKE '%' || $1 || '%'
		ORDER BY length(name), name
		LIMIT 20
	`, term).All(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, users)
}

// authorReport is what SQL composition is for.
//
// The question is one report: every active author, how many posts they have,
// the newest one, and a label. Written as separate queries it is three round
// trips and a join in Go; written as one statement it is a CTE, an aggregate
// subquery joined in, and a correlated LATERAL — which PostgreSQL answers in one
// pass and this package builds without giving up a result type.
//
// The line worth reading is the third selected expression. post_count is
// count(*) and can never be NULL, but it is read through a LEFT JOIN that may
// match nothing — so it is nullable *here*, OptRef says so, and the result type
// is *int64. Reading it with Ref would not compile against this destination, and
// a select list that claimed non-null would be refused before PostgreSQL saw it.
func (s *server) authorReport(w http.ResponseWriter, r *http.Request) {
	// The active authors, named once and used as a source.
	authorID := orm.Named("id", orm.Of(domain.Users.ID))
	authorName := orm.Named("name", orm.Of(domain.Users.Name))
	authors := orm.CTE("authors", orm.Rows(authorID, authorName).
		From(domain.Users.Source()).
		Where(orm.Cond(domain.Users.Active.Eq(true))))

	// How many published posts each author has.
	statsAuthor := orm.Named("author_id", orm.Of(domain.Posts.AuthorID))
	statsCount := orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))
	stats := orm.Sub("stats", orm.Rows(statsAuthor, statsCount).
		From(domain.Posts.Source()).
		Where(orm.Cond(domain.Posts.Status.Eq(domain.PostPublished))).
		GroupBy(orm.Of(domain.Posts.AuthorID)))

	// The newest post per author: one subquery per left-hand row, ordered and
	// limited on its own terms. That is what LATERAL is for, and it is why the
	// subquery may name the CTE beside it.
	latestTitle := orm.Named("title", orm.Of(domain.Posts.Title))
	latest := orm.Sub("latest", orm.Rows(latestTitle).
		From(domain.Posts.Source()).
		Where(orm.Eq(domain.Posts.AuthorID, orm.Ref(authors, authorID))).
		OrderBy(orm.Of(domain.Posts.CreatedAt).Desc()).
		Limit(1))

	posts := orm.OptRef(stats, statsCount)
	report := orm.Project4(
		orm.Ref(authors, authorName),
		posts,
		orm.OptRef(latest, latestTitle),
		orm.Case(posts.Gt(nil), orm.Val("prolific")).Else(orm.Val("quiet")),
		func(name string, posts *int64, latest *string, label string) authorRow {
			row := authorRow{Name: name, Latest: latest, Label: label}
			if posts != nil {
				row.Posts = *posts
			}
			return row
		},
	)

	rows, err := orm.Compose(s.db.Executor(), report).
		With(authors).
		From(authors).
		LeftJoin(stats, orm.Eq(orm.Ref(stats, statsAuthor), orm.Ref(authors, authorID))).
		LeftJoinLateral(latest).
		OrderBy(orm.Ref(authors, authorName).Asc()).
		All(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	write(w, http.StatusOK, rows)
}

// authorRow is the report's shape. Nothing infers it: the projection binds four
// expressions to a function of exactly these four types.
type authorRow struct {
	Name   string  `json:"name"`
	Posts  int64   `json:"posts"`
	Latest *string `json:"latest,omitempty"`
	Label  string  `json:"label"`
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id must be a number")
	}
	return id, nil
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(into); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return false
	}
	return true
}

func write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("writing the response", "err", err)
	}
}

// fail turns an ORM error into a status code.
//
// The sentinels are matched with errors.Is and PostgreSQL's own error with
// errors.As, through every layer of context the ORM added. Nothing here parses
// an error message.
func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orm.ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
		return
	case errors.Is(err, orm.ErrMissingWhere), errors.Is(err, orm.ErrMissingSet):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation
			http.Error(w, "already exists", http.StatusConflict)
			return
		case "23503": // foreign_key_violation
			http.Error(w, "referenced row does not exist", http.StatusBadRequest)
			return
		}
	}

	slog.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
