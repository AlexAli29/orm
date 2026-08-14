package hexagonal_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"example.com/hexagonal/adapter/httpin"
	"example.com/hexagonal/adapter/ormstore"
	"example.com/hexagonal/core/app"
	"example.com/hexagonal/core/domain"
	"example.com/hexagonal/core/port"
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/ormtest"
	ormpg "github.com/AlexAli29/orm/ormtest/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The hexagon, running.
//
// The boundary tests prove the arrangement; these prove it works. Both are
// needed: an architecture that is clean and broken is not better than one that
// is neither.

// fixedClock is the port that a test supplies instead of the wall clock, which
// is the point of having the port.
type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

type env struct {
	pool *pgxpool.Pool
	svc  *app.Service
	api  *httpin.API
	when time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pg := ormpg.Run(t, postgresImage()...)

	conn, err := pgx.Connect(t.Context(), pg.DSN)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer conn.Close(context.Background())
	ormtest.Migrate(t, conn, "migrations")

	pool := pg.Pool()
	if _, err := pool.Exec(t.Context(),
		`TRUNCATE projects, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("clearing: %v", err)
	}

	when := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	var ex orm.Executor = pool
	store := ormstore.NewStore()
	svc, err := app.New(store, store, ormstore.NewWork(ex), fixedClock{when})
	if err != nil {
		t.Fatalf("wiring: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &env{pool: pool, svc: svc, api: httpin.New(svc, log), when: when}
}

func do(t *testing.T, e *env, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, r)
	rec := httptest.NewRecorder()
	e.api.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// The whole path works: HTTP in, use case, adapter, PostgreSQL, and back.
func TestHexagonal_endToEnd(t *testing.T) {
	e := newEnv(t)

	code, body := do(t, e, http.MethodPost, "/users",
		`{"email":"hex@example.com","name":"Hex"}`)
	if code != http.StatusCreated {
		t.Fatalf("registering: %d %s", code, body)
	}
	var user struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &user); err != nil {
		t.Fatal(err)
	}

	code, body = do(t, e, http.MethodPost, "/projects",
		`{"owner_id":`+itoa(user.ID)+`,"slug":"first-project","name":"First"}`)
	if code != http.StatusCreated {
		t.Fatalf("starting a project: %d %s", code, body)
	}

	code, body = do(t, e, http.MethodGet, "/users/"+itoa(user.ID)+"/projects", "")
	if code != http.StatusOK {
		t.Fatalf("listing: %d %s", code, body)
	}
	if !strings.Contains(body, "first-project") {
		t.Errorf("the project is not in the list: %s", body)
	}

	// The clock is the port's, not the machine's, which is what makes a
	// timestamp assertable.
	var created time.Time
	if err := e.pool.QueryRow(t.Context(),
		`SELECT created_at FROM users WHERE id = $1`, user.ID).Scan(&created); err != nil {
		t.Fatal(err)
	}
	if !created.UTC().Equal(e.when) {
		t.Errorf("created_at = %s, want the test clock's %s", created.UTC(), e.when)
	}
}

// Each failure category arrives at the edge as its own status, and the ORM's
// vocabulary does not.
func TestHexagonal_failuresTranslate(t *testing.T) {
	e := newEnv(t)

	code, body := do(t, e, http.MethodPost, "/users",
		`{"email":"dup@example.com","name":"First"}`)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}

	cases := []struct {
		what   string
		method string
		path   string
		body   string
		want   int
	}{
		{"a duplicate email", http.MethodPost, "/users", `{"email":"dup@example.com","name":"Second"}`, http.StatusConflict},
		{"an empty name", http.MethodPost, "/users", `{"email":"ok@example.com","name":""}`, http.StatusBadRequest},
		{"a malformed body", http.MethodPost, "/users", `{`, http.StatusBadRequest},
		{"a missing user", http.MethodGet, "/users/999999", "", http.StatusNotFound},
		{"a bad slug", http.MethodPost, "/projects", `{"owner_id":1,"slug":"Not A Slug","name":"X"}`, http.StatusBadRequest},
		{"an owner who is not there", http.MethodPost, "/projects", `{"owner_id":999999,"slug":"orphan","name":"X"}`, http.StatusBadRequest},
		{"a missing project", http.MethodGet, "/projects/nothing-here", "", http.StatusNotFound},
	}
	for _, c := range cases {
		code, body := do(t, e, c.method, c.path, c.body)
		if code != c.want {
			t.Errorf("%s gave %d, want %d: %s", c.what, code, c.want, body)
		}
		for _, forbidden := range []string{
			"SQLSTATE", "23505", "23503", "users_email_key", "pgconn", "pgx",
			"INSERT", "SELECT ", "postgres://",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s leaked %q: %s", c.what, forbidden, body)
			}
		}
	}
}

// The unit of work is real: a use case that fails part-way leaves nothing.
//
// StartProject reads the owner and writes the project in one transaction. The
// failure here is forced by a constraint, so the rollback is PostgreSQL's own.
func TestHexagonal_unitOfWorkIsAtomic(t *testing.T) {
	e := newEnv(t)

	code, body := do(t, e, http.MethodPost, "/users",
		`{"email":"atomic@example.com","name":"Atomic"}`)
	if code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	var user struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &user)

	if code, body := do(t, e, http.MethodPost, "/projects",
		`{"owner_id":`+itoa(user.ID)+`,"slug":"taken","name":"First"}`); code != http.StatusCreated {
		t.Fatalf("%d %s", code, body)
	}
	// The same slug again: the unique index refuses.
	if code, body := do(t, e, http.MethodPost, "/projects",
		`{"owner_id":`+itoa(user.ID)+`,"slug":"taken","name":"Second"}`); code != http.StatusConflict {
		t.Fatalf("a duplicate slug gave %d: %s", code, body)
	}

	var n int
	if err := e.pool.QueryRow(t.Context(),
		`SELECT count(*) FROM projects WHERE slug = 'taken'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("there are %d projects with that slug, want 1", n)
	}
}

// The core runs without a database, which is the payoff the arrangement is for.
//
// This test wires the use cases to repositories that are maps. If the core had
// acquired a dependency on storage, this would not compile — which makes it a
// second, independent check on the same boundary the import test states.
func TestHexagonal_coreRunsWithoutADatabase(t *testing.T) {
	mem := newMemory()
	svc, err := app.New(mem, mem, mem, fixedClock{time.Unix(0, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}

	u, err := svc.Register(context.Background(), domain.User{Email: "mem@example.com", Name: "Mem"})
	if err != nil {
		t.Fatalf("registering in memory: %v", err)
	}
	if _, err := svc.StartProject(context.Background(), domain.Project{
		OwnerID: u.ID, Slug: "in-memory", Name: "In Memory",
	}); err != nil {
		t.Fatalf("starting a project in memory: %v", err)
	}
	ps, err := svc.Projects(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 {
		t.Fatalf("got %d projects", len(ps))
	}

	// And the rules still hold, because they live in the core rather than in
	// the database's constraints.
	if _, err := svc.Register(context.Background(), domain.User{Email: "nope", Name: "Bad"}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an invalid email gave %v", err)
	}
	if _, err := svc.StartProject(context.Background(), domain.Project{
		OwnerID: 999, Slug: "orphan", Name: "Orphan",
	}); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("an absent owner gave %v", err)
	}
}

// memory is an in-memory implementation of every port, which exists only to
// prove the core does not need PostgreSQL.
type memory struct {
	users    map[int64]domain.User
	projects map[string]domain.Project
	nextID   int64
}

func newMemory() *memory {
	return &memory{users: map[int64]domain.User{}, projects: map[string]domain.Project{}, nextID: 1}
}

func (m *memory) Do(ctx context.Context, fn func(context.Context, port.Tx) error) error {
	return fn(ctx, m)
}

func (m *memory) CreateUser(_ context.Context, _ port.Tx, u domain.User) (domain.User, error) {
	for _, existing := range m.users {
		if existing.Email == u.Email {
			return domain.User{}, domain.ErrConflict
		}
	}
	u.ID = m.nextID
	m.nextID++
	m.users[u.ID] = u
	return u, nil
}

func (m *memory) UserByID(_ context.Context, _ port.Tx, id int64) (domain.User, error) {
	u, ok := m.users[id]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (m *memory) UserByEmail(_ context.Context, _ port.Tx, email string) (domain.User, error) {
	for _, u := range m.users {
		if u.Email == email {
			return u, nil
		}
	}
	return domain.User{}, domain.ErrNotFound
}

func (m *memory) CreateProject(_ context.Context, _ port.Tx, p domain.Project) (domain.Project, error) {
	if _, taken := m.projects[p.Slug]; taken {
		return domain.Project{}, domain.ErrConflict
	}
	p.ID = m.nextID
	m.nextID++
	m.projects[p.Slug] = p
	return p, nil
}

func (m *memory) ProjectBySlug(_ context.Context, _ port.Tx, slug string) (domain.Project, error) {
	p, ok := m.projects[slug]
	if !ok {
		return domain.Project{}, domain.ErrNotFound
	}
	return p, nil
}

func (m *memory) ProjectsByOwner(_ context.Context, _ port.Tx, ownerID int64) ([]domain.Project, error) {
	var out []domain.Project
	for _, p := range m.projects {
		if p.OwnerID == ownerID {
			out = append(out, p)
		}
	}
	return out, nil
}

// itoa is strconv.FormatInt, named for how it reads at the call sites above.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// postgresImage lets CI choose which PostgreSQL this example is tested against,
// so the workflow's version matrix means something.
func postgresImage() []ormpg.Option {
	if ref := os.Getenv("ORM_TEST_POSTGRES_IMAGE"); ref != "" {
		return []ormpg.Option{ormpg.Image(ref)}
	}
	return nil
}

// The unit of work is a real transaction, not a passthrough.
//
// The atomicity test above passes without one: a single failing insert leaves
// nothing behind whether or not there was a transaction around it. This is the
// check that distinguishes them — two writes inside one Do, the second refused
// by the database, and the first must be gone.
//
// It goes through the port rather than around it, so what is being tested is
// the thing the core is handed.
func TestHexagonal_unitOfWorkIsATransaction(t *testing.T) {
	e := newEnv(t)

	store := ormstore.NewStore()
	work := ormstore.NewWork(e.pool)

	err := work.Do(t.Context(), func(ctx context.Context, tx port.Tx) error {
		u, err := store.CreateUser(ctx, tx, domain.User{
			Email: "rolled-back@example.com", Name: "Rolled Back", CreatedAt: e.when,
		})
		if err != nil {
			return err
		}
		if _, err := store.CreateProject(ctx, tx, domain.Project{
			OwnerID: u.ID, Slug: "will-not-exist", Name: "Doomed", CreatedAt: e.when,
		}); err != nil {
			return err
		}
		// The second write fails: an owner that is not there.
		_, err = store.CreateProject(ctx, tx, domain.Project{
			OwnerID: 999999, Slug: "orphan", Name: "Orphan", CreatedAt: e.when,
		})
		return err
	})
	if err == nil {
		t.Fatal("the unit of work succeeded with a foreign key violation in it")
	}
	if !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("the failure is %v, want the core's vocabulary", err)
	}

	// Everything the callback wrote before the failure is gone. Without a
	// transaction the user and the first project would both have committed.
	for _, q := range []struct {
		what  string
		query string
	}{
		{"the user", `SELECT count(*) FROM users WHERE email = 'rolled-back@example.com'`},
		{"the first project", `SELECT count(*) FROM projects WHERE slug = 'will-not-exist'`},
	} {
		var n int
		if err := e.pool.QueryRow(t.Context(), q.query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s survived a failed unit of work (%d rows), so Do is not a transaction", q.what, n)
		}
	}

	// And the two statements really shared one transaction, which is the same
	// fact stated the other way: PostgreSQL gives one transaction id.
	var first, second uint64
	if err := work.Do(t.Context(), func(ctx context.Context, tx port.Tx) error {
		ex, ok := tx.(interface {
			QueryRow(context.Context, string, ...any) pgx.Row
		})
		if !ok {
			t.Fatalf("the unit of work is a %T, which cannot run a statement", tx)
		}
		if err := ex.QueryRow(ctx, `SELECT txid_current()`).Scan(&first); err != nil {
			return err
		}
		return ex.QueryRow(ctx, `SELECT txid_current()`).Scan(&second)
	}); err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("two statements in one unit of work got transaction ids %d and %d", first, second)
	}
}
