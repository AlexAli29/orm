package gendemo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// M12.1: COPY.
//
// The claims are that this is PostgreSQL's copy protocol rather than a large
// INSERT wearing its name, that the row path reads the generated accessors, and
// that the semantics COPY does not have — RETURNING, ON CONFLICT, per-row
// errors — are absent rather than approximated.

// Release-critical: the wire protocol is COPY.
//
// pg_stat_statements is not available in a plain image, so the proof is taken
// from the server's own view of what it executed: a COPY leaves a statement
// beginning with COPY in pg_stat_activity's query field while it runs, and the
// only way to see that is from another connection during the copy. Simpler and
// just as conclusive: pgx's CopyFrom is the only path this code takes, and the
// executor it is handed records what it was asked to do.
func TestCopy_usesThePostgreSQLCopyProtocol(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	// A recording executor sits between the repository and pgx and reports
	// which pgx call was made. Query is never used by a COPY.
	rec := &recordingCopyExecutor{inner: conn}
	repo := gendemo.New(rec).Categories

	n, err := repo.CopyFrom(t.Context(), []gendemo.Category{{Name: "a"}, {Name: "b"}})
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != 2 {
		t.Errorf("CopyFrom reported %d rows, want 2", n)
	}
	if rec.copies != 1 {
		t.Errorf("pgx CopyFrom was called %d times, want 1", rec.copies)
	}
	if rec.queries != 0 {
		t.Errorf("the COPY sent %d statements through Query; it must send none", rec.queries)
	}
	if rec.table.Sanitize() != `"public"."categories"` {
		t.Errorf("COPY targeted %s", rec.table.Sanitize())
	}
	// The identity column is not mentioned: PostgreSQL supplies it.
	if got := strings.Join(rec.columns, ","); got != "name" {
		t.Errorf("COPY named columns %q, want just the writable ones", got)
	}

	// And the rows are really there, with identities PostgreSQL assigned.
	var count, ids int64
	if err := conn.QueryRow(t.Context(),
		`SELECT count(*), count(DISTINCT id) FROM categories`).Scan(&count, &ids); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 2 || ids != 2 {
		t.Errorf("the table holds %d rows with %d distinct ids", count, ids)
	}
	_ = db
}

// The whole-entity path copies every writable column and nothing else.
func TestCopy_entityRoundTrip(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	nick := "copied"
	score := 2.5
	when := time.Date(2024, 3, 1, 9, 0, 0, 0, time.UTC)
	users := []gendemo.User{
		{
			Email: "c1@example.com", Age: 41, Active: true,
			State: gendemo.UserStateActive, Nickname: &nick, Score: &score,
			Tags: []string{"go", "sql"}, Settings: map[string]any{"tier": "gold"},
			CreatedAt: when,
		},
		{
			// Every nullable field left nil, and every non-nullable one at its
			// Go zero — which is a value, not an absence. The identity is not
			// among them: COPY does not mention it and PostgreSQL assigns it.
			Email: "", Age: 0, Active: false,
			State: gendemo.UserStatePending, Tags: []string{}, Settings: map[string]any{},
			CreatedAt: when,
		},
	}
	n, err := db.Users.CopyFrom(t.Context(), users)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != 2 {
		t.Fatalf("copied %d rows, want 2", n)
	}

	got, err := db.Users.Query().
		Where(gendemo.Users.Email.In("c1@example.com", "")).
		OrderBy(gendemo.Users.Email.Desc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read back %d rows", len(got))
	}
	if got[0].Email != "c1@example.com" || got[0].Nickname == nil || *got[0].Nickname != "copied" {
		t.Errorf("row 1 = %+v", got[0])
	}
	if got[0].Settings["tier"] != "gold" || len(got[0].Tags) != 2 {
		t.Errorf("a jsonb and an array did not round-trip: %+v", got[0])
	}
	// The zero row kept its zeros and its NULLs, and the two stayed apart.
	if got[1].Email != "" || got[1].Age != 0 || got[1].Active {
		t.Errorf("row 2 lost its zero values: %+v", got[1])
	}
	if got[1].Nickname != nil || got[1].Score != nil {
		t.Errorf("row 2 turned a nil into a value: %+v", got[1])
	}
	// The generated column PostgreSQL computes was not copied and is right.
	var slug *string
	if err := conn.QueryRow(t.Context(),
		`SELECT slug FROM users WHERE email = 'c1@example.com'`).Scan(&slug); err != nil {
		t.Fatalf("reading the generated column: %v", err)
	}
	if slug == nil || *slug != "c1@example.com" {
		t.Errorf("the generated column = %v", slug)
	}
}

// Copying a subset leaves every other column to the table's own defaults.
func TestCopy_selectedColumnsLeaveTheRestToTheTable(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	n, err := orm.CopyColumns(t.Context(), db.Posts,
		[]gendemo.Post{
			{Title: "copied-a", AuthorID: ptr64(1)},
			{Title: "copied-b"},
		},
		gendemo.Posts.Title, gendemo.Posts.AuthorID)
	if err != nil {
		t.Fatalf("CopyColumns: %v", err)
	}
	if n != 2 {
		t.Fatalf("copied %d rows", n)
	}

	// published, score and created_at were not mentioned, so PostgreSQL's
	// defaults apply — false, 0 and now() — rather than Go's zeros being sent.
	rows, err := conn.Query(t.Context(),
		`SELECT title, published, score, created_at IS NOT NULL, author_id
		 FROM posts WHERE title IN ('copied-a','copied-b') ORDER BY title`)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var title string
		var published, hasCreated bool
		var score int32
		var author *int64
		if err := rows.Scan(&title, &published, &score, &hasCreated, &author); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if published || score != 0 || !hasCreated {
			t.Errorf("%s did not take the table's defaults: published=%v score=%d created=%v",
				title, published, score, hasCreated)
		}
		if title == "copied-b" && author != nil {
			t.Errorf("an omitted nullable value became %v", *author)
		}
		seen++
	}
	if seen != 2 {
		t.Errorf("read back %d rows", seen)
	}
}

// A column COPY cannot supply is refused before the connection is touched.
func TestCopy_refusesColumnsItCannotSupply(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)

	for _, tt := range []struct {
		name string
		run  func() (int64, error)
		want string
	}{
		{"a generated column", func() (int64, error) {
			return orm.CopyColumns(t.Context(), db.Users, nil, gendemo.Users.Slug)
		}, "COPY cannot supply a value"},
		{"an identity column", func() (int64, error) {
			return orm.CopyColumns(t.Context(), db.Categories, nil, gendemo.Categories.ID)
		}, "COPY cannot supply a value"},
		{"the same column twice", func() (int64, error) {
			return orm.CopyColumns(t.Context(), db.Posts, nil,
				gendemo.Posts.Title, gendemo.Posts.Title)
		}, "copied twice"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.run()
			if err == nil {
				t.Fatal("the copy was accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// Streaming pulls one row at a time rather than materialising the source.
func TestCopy_streamsWithoutMaterialising(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	const rows = 20000
	live := 0
	peak := 0
	seq := func(yield func(gendemo.Category, error) bool) {
		for i := range rows {
			live++
			if live > peak {
				peak = live
			}
			ok := yield(gendemo.Category{Name: fmt.Sprintf("c%d", i)}, nil)
			live--
			if !ok {
				return
			}
		}
	}
	n, err := db.Categories.CopyFromSeq(t.Context(), seq)
	if err != nil {
		t.Fatalf("CopyFromSeq: %v", err)
	}
	if n != rows {
		t.Fatalf("copied %d rows, want %d", n, rows)
	}
	// Only one row is ever in flight: the source is pulled, not drained.
	if peak != 1 {
		t.Errorf("%d rows were live at once; the source was materialised", peak)
	}
	var count int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM categories`).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != rows {
		t.Errorf("the table holds %d rows", count)
	}
}

// An error from the source stops the COPY and is what the caller sees.
func TestCopy_sourceErrorStopsIt(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	boom := errors.New("the source gave up")
	seq := func(yield func(gendemo.Category, error) bool) {
		for i := range 100 {
			if i == 50 {
				yield(gendemo.Category{}, boom)
				return
			}
			if !yield(gendemo.Category{Name: fmt.Sprintf("c%d", i)}, nil) {
				return
			}
		}
	}
	_, err := db.Categories.CopyFromSeq(t.Context(), seq)
	if err == nil {
		t.Fatal("a failing source produced no error")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap the source's", err)
	}
	// The COPY failed as one statement, so none of the rows it had already
	// sent are in the table.
	var count int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM categories`).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 0 {
		t.Errorf("%d rows survived a failed COPY", count)
	}
}

// PostgreSQL's own failure survives with its error intact, and without a row
// index this package would have had to invent.
func TestCopy_serverErrorIsPostgreSQLs(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)

	// Two rows with the same unique email.
	_, err := db.Users.CopyFrom(t.Context(), []gendemo.User{
		{Email: "dup@example.com", State: gendemo.UserStatePending, Tags: []string{},
			Settings: map[string]any{}, CreatedAt: t0},
		{Email: "dup@example.com", State: gendemo.UserStatePending, Tags: []string{},
			Settings: map[string]any{}, CreatedAt: t0},
	})
	if err == nil {
		t.Fatal("a duplicate key was copied")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
	}
	if pg.Code != "23505" {
		t.Errorf("SQLSTATE = %s, want 23505", pg.Code)
	}
	if !strings.Contains(err.Error(), "public.users") {
		t.Errorf("error = %v, want it to name the table", err)
	}
}

// A COPY on a transaction-bound repository uses that transaction.
func TestCopy_insideATransaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	sentinel := errors.New("roll back")
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		n, err := tx.Categories.CopyFrom(t.Context(), []gendemo.Category{{Name: "in-tx"}})
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("copied %d rows", n)
		}
		// Visible inside the transaction, through the ordinary query path.
		got, err := tx.Categories.Query().All(t.Context())
		if err != nil {
			return err
		}
		if len(got) != 1 || got[0].Name != "in-tx" {
			t.Errorf("inside the transaction the copied rows are %+v", got)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx = %v", err)
	}
	// Rolled back, so nothing persists.
	after, err := db.Categories.Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("%d copied rows survived the rollback", len(after))
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections still acquired", n)
	}
}

// Cancelling a large COPY returns, and the pool recovers.
func TestCopy_cancellationReleasesTheConnection(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	for range 5 {
		ctx, cancel := context.WithCancel(t.Context())
		seq := func(yield func(gendemo.Category, error) bool) {
			for i := range 1000000 {
				if i == 500 {
					cancel()
				}
				if !yield(gendemo.Category{Name: "x"}, nil) {
					return
				}
			}
		}
		_, err := db.Categories.CopyFromSeq(ctx, seq)
		cancel()
		if err == nil {
			t.Fatal("a cancelled COPY succeeded")
		}
		if !errors.Is(err, context.Canceled) {
			t.Logf("cancelled COPY returned %v", err)
		}
	}
	// The pool is usable afterwards and holds nothing.
	if _, err := db.Categories.Query().All(t.Context()); err != nil {
		t.Fatalf("the pool is unusable after cancelled copies: %v", err)
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections still acquired after five cancelled copies", n)
	}
}

// An executor that cannot COPY says so rather than falling back to an INSERT.
func TestCopy_refusesAnExecutorThatCannotCopy(t *testing.T) {
	repo := gendemo.New(queryOnlyExecutor{}).Categories
	_, err := repo.CopyFrom(context.Background(), []gendemo.Category{{Name: "x"}})
	if err == nil {
		t.Fatal("a query-only executor accepted a COPY")
	}
	if !strings.Contains(err.Error(), "cannot COPY") {
		t.Errorf("error = %v", err)
	}
}

// recordingCopyExecutor reports which pgx call the repository made.
type recordingCopyExecutor struct {
	inner   *pgx.Conn
	copies  int
	queries int
	table   pgx.Identifier
	columns []string
}

func (r *recordingCopyExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.queries++
	return r.inner.Query(ctx, sql, args...)
}

func (r *recordingCopyExecutor) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	r.copies++
	r.table = table
	r.columns = columns
	return r.inner.CopyFrom(ctx, table, columns, src)
}

type queryOnlyExecutor struct{}

func (queryOnlyExecutor) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("not implemented")
}

func ptr64(v int64) *int64 { return &v }
