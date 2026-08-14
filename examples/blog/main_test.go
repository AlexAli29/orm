package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/examples/blog/internal/domain"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The example is code people copy, so it is tested like code people copy.
//
// It runs against a real database through the real handlers, which is what
// stops it from quietly rotting: a change to the ORM that broke any of these
// calls would break this test rather than somebody's afternoon.

func newServer(t *testing.T) *server {
	t.Helper()
	testdb.AdminDSN(t)

	schema, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatalf("reading schema.sql: %v", err)
	}
	dsn := testdb.Create(t, string(schema))

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.AfterConnect = domain.RegisterTypes

	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return &server{db: domain.New(pool)}
}

// do runs one request through the mux and decodes the response.
func do(t *testing.T, s *server, method, target, body string, into any) int {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	w := httptest.NewRecorder()
	routes(s).ServeHTTP(w, r)

	if into != nil && w.Code < 300 {
		if err := json.NewDecoder(w.Body).Decode(into); err != nil {
			t.Fatalf("%s %s: decoding the response: %v\nbody: %s", method, target, err, w.Body.String())
		}
	}
	return w.Code
}

func TestBlog(t *testing.T) {
	s := newServer(t)

	// A user, then the same user again: the unique constraint is PostgreSQL's
	// and the handler turns it into a 409 by matching the SQLSTATE rather than
	// the message.
	var author domain.User
	if code := do(t, s, "POST", "/users", `{"email":"ada@example.com","name":"Ada"}`, &author); code != http.StatusCreated {
		t.Fatalf("creating a user: %d", code)
	}
	if author.ID == 0 || author.CreatedAt.IsZero() {
		t.Errorf("the created user came back without database-supplied values: %+v", author)
	}
	// The struct said false and false is what was stored, because a Go zero
	// value is a value.
	if author.Active {
		t.Error("active came back true; the column's default overrode a value the caller set")
	}
	if code := do(t, s, "POST", "/users", `{"email":"ada@example.com","name":"Ada again"}`, nil); code != http.StatusConflict {
		t.Errorf("the duplicate email returned %d, want 409", code)
	}

	// Asking for the default is explicit and separate.
	var defaulted domain.User
	if code := do(t, s, "POST", "/users",
		`{"email":"grace@example.com","name":"Grace","use_default_active":true}`, &defaulted); code != http.StatusCreated {
		t.Fatalf("creating a user with defaults: %d", code)
	}
	if !defaulted.Active {
		t.Error("active came back false; the requested default was not applied")
	}

	// A post and two comments in one transaction.
	var post domain.Post
	body := `{"author_id":` + itoa(author.ID) + `,"title":"Notes","body":"...","comments":["first","second"]}`
	if code := do(t, s, "POST", "/posts", body, &post); code != http.StatusCreated {
		t.Fatalf("creating a post: %d", code)
	}
	if post.Status != domain.PostDraft {
		t.Errorf("status = %q, want the column's default", post.Status)
	}

	// A transaction that cannot finish leaves nothing behind: the author does
	// not exist, so the post that was already inserted goes with it.
	before := countPosts(t, s)
	if code := do(t, s, "POST", "/posts", `{"author_id":999999,"title":"Doomed","body":"..."}`, nil); code != http.StatusBadRequest {
		t.Errorf("a post by a missing author returned %d, want 400", code)
	}
	if after := countPosts(t, s); after != before {
		t.Errorf("posts went from %d to %d; the failed transaction left a row", before, after)
	}

	// The nested read: the post, its author, its comments and their authors.
	var loaded domain.Post
	if code := do(t, s, "GET", "/posts/"+itoa(post.ID), "", &loaded); code != http.StatusOK {
		t.Fatalf("reading the post: %d", code)
	}
	comments, ok := loaded.Comments.Get()
	if !ok || len(comments) != 2 {
		t.Fatalf("comments = %v, %v; want the two the transaction wrote", comments, ok)
	}
	for _, c := range comments {
		a, ok := c.Author.Get()
		if !ok || a == nil || a.ID != author.ID {
			t.Errorf("comment %d author = %v, %v", c.ID, a, ok)
		}
	}

	// The dynamic filter, with and without the relation predicate.
	var users []domain.User
	if code := do(t, s, "GET", "/users?email=example.com", "", &users); code != http.StatusOK {
		t.Fatalf("listing users: %d", code)
	}
	if len(users) != 2 {
		t.Errorf("listed %d users, want both", len(users))
	}
	users = nil
	if code := do(t, s, "GET", "/users?published=true", "", &users); code != http.StatusOK {
		t.Fatalf("listing users by relation: %d", code)
	}
	if len(users) != 0 {
		t.Errorf("listed %d users with a published post, want none; the post is a draft", len(users))
	}

	// A partial update assigns only what was sent.
	var updated domain.User
	if code := do(t, s, "PATCH", "/users/"+itoa(author.ID), `{"active":true}`, &updated); code != http.StatusOK {
		t.Fatalf("updating: %d", code)
	}
	if !updated.Active || updated.Name != "Ada" {
		t.Errorf("update changed the wrong thing: %+v", updated)
	}

	// The escape hatch returns the same entity type as everything else.
	var found []domain.User
	if code := do(t, s, "GET", "/search/users?q=ada", "", &found); code != http.StatusOK {
		t.Fatalf("searching: %d", code)
	}
	if len(found) != 1 || found[0].Email != "ada@example.com" {
		t.Errorf("search returned %+v", found)
	}

	// The composed report: one statement over a CTE, a joined aggregate
	// subquery and a LATERAL, read into a shape bound at compile time.
	var report []authorRow
	if code := do(t, s, "GET", "/reports/authors", "", &report); code != http.StatusOK {
		t.Fatalf("reporting: %d", code)
	}
	if len(report) == 0 {
		t.Fatal("the report is empty, so it proves nothing about the values")
	}
	for _, row := range report {
		// An author with no published post is still an author: the LEFT JOIN
		// keeps the row and the count arrives as NULL, which the shape reads
		// as zero rather than dropping.
		if row.Posts > 0 && row.Latest == nil {
			t.Errorf("%s has %d posts and no latest title", row.Name, row.Posts)
		}
		if row.Posts == 0 && row.Label != "quiet" {
			t.Errorf("%s has no posts and is labelled %q", row.Name, row.Label)
		}
	}

	// A read of something that is not there is ErrNotFound, mapped to 404.
	if code := do(t, s, "GET", "/posts/999999", "", nil); code != http.StatusNotFound {
		t.Errorf("a missing post returned %d, want 404", code)
	}
}

func countPosts(t *testing.T, s *server) int64 {
	t.Helper()
	n, err := s.db.Posts.Query().Count(context.Background())
	if err != nil {
		t.Fatalf("counting posts: %v", err)
	}
	return n
}

func itoa(n int64) string {
	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(n); err != nil {
		panic(err)
	}
	return strings.TrimSpace(b.String())
}
