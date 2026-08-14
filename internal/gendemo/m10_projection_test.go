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
	"github.com/jackc/pgx/v5/pgxpool"
)

// Typed projections against real PostgreSQL.
//
// The claim under test is that the expressions selected, their NULL semantics
// and the Go shape they land in agree by construction. Most of that is proved
// by the code compiling at all — a nullable column bound to a non-pointer
// destination does not build, which is what the compile-fail suite asserts.
// What is left for a database is that the SQL means what the types promised.

// UserSummary is the shape most of these tests read.
type UserSummary struct {
	ID    int64
	Email string
}

// userSummaries is a result shape built once and used by many queries, which is
// the point of it being a value.
var userSummaries = orm.Project2(
	gendemo.Users.ID, gendemo.Users.Email,
	func(id int64, email string) UserSummary { return UserSummary{ID: id, Email: email} },
)

func summaryIDs(rows []UserSummary) []int64 {
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func TestProjection_selectsOnlyWhatWasAskedFor(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	sql, args, err := orm.Select(db.Users, userSummaries).
		Where(gendemo.Users.Active.Eq(true)).
		OrderBy(gendemo.Users.CreatedAt.Desc()).
		Limit(50).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`WHERE "users"."active" = $1 ORDER BY "users"."created_at" DESC LIMIT 50`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 || args[0] != true {
		t.Errorf("args = %v", args)
	}
	// Never SELECT *, and never a column nobody asked for.
	if strings.Contains(sql, "*") || strings.Contains(sql, "nickname") {
		t.Errorf("the statement selects more than the projection: %s", sql)
	}

	got, err := orm.Select(db.Users, userSummaries).
		Where(gendemo.Users.Active.Eq(true)).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if fmt.Sprint(summaryIDs(got)) != "[1 2]" {
		t.Errorf("ids = %v", summaryIDs(got))
	}
	if got[0].Email != "alex@example.com" {
		t.Errorf("row = %+v", got[0])
	}
}

// A nullable column projects as a pointer, and SQL NULL arrives as nil rather
// than as the type's zero value.
func TestProjection_nullableColumnsStayNullable(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	type row struct {
		ID       int64
		Nickname *string
		Deleted  *time.Time
		Score    *float64
	}
	shape := orm.Project4(
		gendemo.Users.ID, gendemo.Users.Nickname, gendemo.Users.DeletedAt, gendemo.Users.Score,
		func(id int64, nick *string, deleted *time.Time, score *float64) row {
			return row{ID: id, Nickname: nick, Deleted: deleted, Score: score}
		},
	)
	got, err := orm.Select(db.Users, shape).OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows", len(got))
	}
	// User 2 has no nickname and no score; user 1 has both and no deleted_at.
	if got[0].Nickname == nil || *got[0].Nickname != "alex" {
		t.Errorf("row 1 nickname = %v", got[0].Nickname)
	}
	if got[1].Nickname != nil {
		t.Errorf("a SQL NULL became %q", *got[1].Nickname)
	}
	if got[1].Score != nil {
		t.Errorf("a NULL double became %v", *got[1].Score)
	}
	if got[0].Deleted != nil {
		t.Errorf("row 1 deleted_at = %v", got[0].Deleted)
	}
	if got[2].Deleted == nil {
		t.Error("row 3 has a deleted_at and came back nil")
	}
}

// A result alias names a column of the result. It is not a table alias, and
// aliasing a generated descriptor must not change that descriptor anywhere.
func TestProjection_resultAliases(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	shape := orm.Project2(
		gendemo.Users.ID.As("user_id"), gendemo.Users.Email.As("address"),
		func(id int64, email string) UserSummary { return UserSummary{ID: id, Email: email} },
	)
	sql, _, err := orm.Select(db.Users, shape).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id" AS "user_id", "users"."email" AS "address" FROM "public"."users"`
	if sql != want {
		t.Errorf("SQL = %s", sql)
	}
	// The descriptor is unchanged: the unaliased projection still renders bare.
	plain, _, err := orm.Select(db.Users, userSummaries).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(plain, "user_id") {
		t.Errorf("aliasing mutated the shared descriptor: %s", plain)
	}
	if _, err := orm.Select(db.Users, shape).All(t.Context()); err != nil {
		t.Fatalf("All: %v", err)
	}
}

// A projection query answers the same "which rows" question an entity query
// does, so its answers have to agree with handwritten SQL.
func TestProjection_matchesHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	db := gendemo.New(conn)

	handwritten := func(sql string, args ...any) []int64 {
		t.Helper()
		rows, err := conn.Query(t.Context(), sql, args...)
		if err != nil {
			t.Fatalf("handwritten: %v", err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			var ignored string
			if err := rows.Scan(&id, &ignored); err != nil {
				t.Fatalf("scanning: %v", err)
			}
			out = append(out, id)
		}
		return out
	}

	for _, tt := range []struct {
		name string
		q    *orm.SelectQuery[gendemo.User, UserSummary]
		sql  string
		args []any
	}{
		{"everything", orm.Select(db.Users, userSummaries).OrderBy(gendemo.Users.ID.Asc()),
			"SELECT id, email FROM users ORDER BY id", nil},
		{"filtered", orm.Select(db.Users, userSummaries).
			Where(gendemo.Users.Age.Gt(20)).OrderBy(gendemo.Users.ID.Asc()),
			"SELECT id, email FROM users WHERE age > $1 ORDER BY id", []any{20}},
		{"paginated", orm.Select(db.Users, userSummaries).
			OrderBy(gendemo.Users.ID.Asc()).Limit(2).Offset(1),
			"SELECT id, email FROM users ORDER BY id LIMIT 2 OFFSET 1", nil},
		{"a semi-join", orm.Select(db.Users, userSummaries).
			Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).
			OrderBy(gendemo.Users.ID.Asc()),
			"SELECT id, email FROM users u WHERE EXISTS (SELECT 1 FROM posts p WHERE p.author_id = u.id AND p.published) ORDER BY id", nil},
	} {
		got, err := tt.q.All(t.Context())
		if err != nil {
			t.Fatalf("%s: All: %v", tt.name, err)
		}
		want := handwritten(tt.sql, tt.args...)
		if fmt.Sprint(summaryIDs(got)) != fmt.Sprint(want) {
			t.Errorf("%s: orm = %v, handwritten = %v", tt.name, summaryIDs(got), want)
		}
		if len(want) == 0 {
			t.Errorf("%s matched nothing, so it proves nothing", tt.name)
		}
	}
}

func TestProjection_distinct(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	// Three users, two distinct activity states.
	shape := orm.Project1(gendemo.Users.Active, func(a bool) bool { return a })
	got, err := orm.Select(db.Users, shape).Distinct().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("DISTINCT returned %v, want two distinct values", got)
	}
	sql, _, err := orm.Select(db.Users, shape).Distinct().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, "SELECT DISTINCT ") {
		t.Errorf("SQL = %s", sql)
	}
	// Counting a DISTINCT query counts the distinct rows, not the table.
	n, err := orm.Select(db.Users, shape).Distinct().Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestProjection_oneSemantics(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	for _, tt := range []struct {
		name string
		q    *orm.SelectQuery[gendemo.User, UserSummary]
		want error
	}{
		{"no rows", orm.Select(db.Users, userSummaries).Where(gendemo.Users.ID.Lt(0)), orm.ErrNotFound},
		{"one row", orm.Select(db.Users, userSummaries).Where(gendemo.Users.ID.Eq(1)), nil},
		{"many rows", orm.Select(db.Users, userSummaries), orm.ErrMultipleRows},
		{"limit 0", orm.Select(db.Users, userSummaries).Limit(0), orm.ErrNotFound},
		{"limit 1 over many", orm.Select(db.Users, userSummaries).Limit(1), nil},
	} {
		got, err := tt.q.One(t.Context())
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: One = %v, want %v", tt.name, err, tt.want)
		}
		if err == nil && got.ID == 0 {
			t.Errorf("%s: One returned %+v", tt.name, got)
		}
	}

	// One does not narrow the builder it was called on.
	q := orm.Select(db.Users, userSummaries)
	if _, err := q.One(t.Context()); !errors.Is(err, orm.ErrMultipleRows) {
		t.Fatalf("One = %v", err)
	}
	all, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("All after One returned %d rows, want 3", len(all))
	}
}

// Rows streams, and stopping early gives the connection back.
func TestProjection_rowsStreamAndRelease(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+`
		INSERT INTO users (id, email, age, tags, settings)
		  SELECT g, 'u'||g||'@example.com', 30, '{}', '{}' FROM generate_series(1, 3000) g;`)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.MaxConns = 2
	cfg.AfterConnect = gendemo.RegisterTypes
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	defer pool.Close()
	db := gendemo.New(pool)

	for i := range 40 {
		switch i % 4 {
		case 0: // break after one row
			n := 0
			for _, err := range orm.Select(db.Users, userSummaries).Rows(t.Context()) {
				if err != nil {
					t.Fatalf("Rows: %v", err)
				}
				n++
				break
			}
			if n != 1 {
				t.Fatalf("read %d rows before breaking", n)
			}
		case 1: // read all of it
			n := 0
			for _, err := range orm.Select(db.Users, userSummaries).Rows(t.Context()) {
				if err != nil {
					t.Fatalf("Rows: %v", err)
				}
				n++
			}
			if n != 3000 {
				t.Fatalf("streamed %d rows", n)
			}
		case 2: // cancel part way through
			ctx, cancel := context.WithCancel(t.Context())
			seen := 0
			for _, err := range orm.Select(db.Users, userSummaries).Rows(ctx) {
				if err != nil {
					break
				}
				seen++
				if seen == 3 {
					cancel()
				}
			}
			cancel()
		case 3: // a statement the server refuses
			for _, err := range orm.Select(db.Users, orm.Project1(
				orm.RawValue[gendemo.User, string]("no_such_column"),
				func(s string) string { return s },
			)).Rows(t.Context()) {
				if err == nil {
					t.Fatal("a broken projection yielded a row")
				}
				break
			}
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conns := make([]*pgxpool.Conn, 0, 2)
	for range 2 {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("the pool never recovered: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	if used := pool.Stat().AcquiredConns(); used != 0 {
		t.Errorf("%d connections are still checked out", used)
	}
}

// A projection over an occurrence the query does not select from is refused
// before PostgreSQL sees it.
func TestProjection_scopeIsChecked(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	manager := gendemo.Users.As("manager")
	shape := orm.Project2(
		manager.ID, manager.Email,
		func(id int64, email string) UserSummary { return UserSummary{ID: id, Email: email} },
	)
	// The query selects from users, not from the manager occurrence.
	if _, _, err := orm.Select(db.Users, shape).SQL(); err == nil {
		t.Fatal("a projection over an unattached occurrence compiled")
	} else if !strings.Contains(err.Error(), "scope error") {
		t.Errorf("err = %v, want a scope error", err)
	}

	// Selecting from that occurrence makes it legal.
	got, err := orm.SelectFrom(db.Users, manager.Source(), shape).
		OrderBy(manager.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("SelectFrom: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d rows", len(got))
	}
}

// Builder mistakes accumulate and are reported together, and none of them
// reaches the database.
func TestProjection_builderErrors(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	q := orm.Select(db.Users, userSummaries).Limit(-1).Offset(-2)
	if _, err := q.All(t.Context()); err == nil {
		t.Fatal("a negative limit ran")
	} else if !strings.Contains(err.Error(), "limit") || !strings.Contains(err.Error(), "offset") {
		t.Errorf("err = %v, want both mistakes", err)
	}
	if ex.n != 0 {
		t.Errorf("a refused projection sent %d statements", ex.n)
	}
}

// PostgreSQL's own errors stay reachable through the projection path.
func TestProjection_postgresErrorsSurvive(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	shape := orm.Project1(
		orm.RawValue[gendemo.User, string]("no_such_function(email)"),
		func(s string) string { return s },
	)
	_, err := orm.Select(db.Users, shape).All(t.Context())
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("err = %v, want a *pgconn.PgError", err)
	}
	if pg.Code != "42883" {
		t.Errorf("SQLSTATE = %s", pg.Code)
	}
}

// The same builder produces the same statement every time.
func TestProjection_isDeterministic(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	build := func() (string, []any, error) {
		return orm.Select(db.Users, userSummaries).
			Where(gendemo.Users.Active.Eq(true), gendemo.Users.Age.Gt(18)).
			OrderBy(gendemo.Users.CreatedAt.Desc(), gendemo.Users.ID.Asc()).
			Distinct().Limit(10).Offset(2).SQL()
	}
	first, firstArgs, err := build()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for range 20 {
		got, args, err := build()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if got != first || fmt.Sprint(args) != fmt.Sprint(firstArgs) {
			t.Fatalf("two builds differ:\n  %s %v\n  %s %v", first, firstArgs, got, args)
		}
	}
}

// Cloning a projection query gives an independent builder.
func TestProjection_clone(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	base := orm.Select(db.Users, userSummaries).Where(gendemo.Users.Age.Gt(0))
	active := base.Clone().Where(gendemo.Users.Active.Eq(true))
	all := base.Clone()

	activeRows, err := active.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	allRows, err := all.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(activeRows) != 2 || len(allRows) != 3 {
		t.Errorf("clone shared state: %d active, %d all", len(activeRows), len(allRows))
	}
}

// A projection inside a transaction reads the transaction's own uncommitted
// rows, and nothing outside it does.
func TestProjection_insideATransaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed+
		"\nSELECT setval(pg_get_serial_sequence('users','id'), 1000);")
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	cfg.AfterConnect = gendemo.RegisterTypes
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	defer pool.Close()
	db := gendemo.New(pool)

	want := errors.New("roll back")
	err = db.Tx(t.Context(), func(tx *gendemo.DB) error {
		if _, err := tx.Users.Insert(t.Context(), newUser("tx@example.com")); err != nil {
			return err
		}
		inside, err := orm.Select(tx.Users, userSummaries).
			Where(gendemo.Users.Email.Eq("tx@example.com")).All(t.Context())
		if err != nil {
			return err
		}
		if len(inside) != 1 {
			t.Errorf("the transaction cannot see its own row: %v", inside)
		}
		outside, err := orm.Select(db.Users, userSummaries).
			Where(gendemo.Users.Email.Eq("tx@example.com")).All(t.Context())
		if err != nil {
			return err
		}
		if len(outside) != 0 {
			t.Errorf("an uncommitted row was visible outside the transaction: %v", outside)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Tx = %v", err)
	}
	after, err := orm.Select(db.Users, userSummaries).
		Where(gendemo.Users.Email.Eq("tx@example.com")).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("the rolled-back row survived: %v", after)
	}
}
