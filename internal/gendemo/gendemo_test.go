package gendemo_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// These tests run against real PostgreSQL through code this project generated.
// Nothing here is hand-written plumbing: the descriptors, the metadata and the
// scanner all came out of orm generate, and the rows come back through pgx.

func schema(t *testing.T) string {
	t.Helper()
	path := filepath.Join("schema.sql")
	ddl, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(ddl)
}

// t0 is the created_at of the first seeded user.
var t0 = time.Date(2024, 1, 1, 9, 0, 0, 0, time.UTC)

// seed inserts fixture rows with plain SQL, so that what the ORM reads was not
// written by the thing under test.
const seed = `
INSERT INTO users (id, email, age, active, state, nickname, score, tags, settings, avatar, bio, visits, deleted_at, created_at) VALUES
  (1, 'alex@example.com',  30, true,  'active',  'alex', 1.5,  '{go,sql}', '{"tier":"gold"}', '\x0102', 'hello', 7,    NULL,                   '2024-01-01T09:00:00Z'),
  (2, 'sam@example.com',   17, true,  'pending', NULL,   NULL, '{}',       '{}',              NULL,     NULL,    NULL, NULL,                   '2024-06-01T09:00:00Z'),
  (3, 'robin@example.com', 45, false, 'banned',  'rob',  9.25, '{ops}',    '{"tier":"none"}', NULL,     'hi',    2,    '2024-07-01T09:00:00Z', '2024-09-01T09:00:00Z');

INSERT INTO posts (id, author_id, title, published, created_at) VALUES
  (1, 1, 'first',  true,  '2024-02-01T09:00:00Z'),
  (2, 1, 'second', false, '2024-03-01T09:00:00Z'),
  (3, 3, 'third',  true,  '2024-10-01T09:00:00Z');
`

// db creates a database, seeds it and returns a generated handle over it.
func db(t *testing.T) *gendemo.DB {
	t.Helper()
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return gendemo.New(conn)
}

func emails(users []gendemo.User) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		out = append(out, u.Email)
	}
	return out
}

func TestSelect_all(t *testing.T) {
	testdb.AdminDSN(t)
	users, err := db(t).Users.Query().OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := strings.Join(emails(users), ","); got != "alex@example.com,sam@example.com,robin@example.com" {
		t.Errorf("emails = %q", got)
	}
}

func TestSelect_scansEveryMappedType(t *testing.T) {
	testdb.AdminDSN(t)
	users, err := db(t).Users.Query().Where(gendemo.Users.ID.Eq(1)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("got %d users, want 1", len(users))
	}
	u := users[0]

	if u.ID != 1 || u.Email != "alex@example.com" || u.Age != 30 || !u.Active {
		t.Errorf("scalars = %+v", u)
	}
	if u.State != gendemo.UserStateActive {
		t.Errorf("State = %q, want the enum constant", u.State)
	}
	if u.Nickname == nil || *u.Nickname != "alex" {
		t.Errorf("Nickname = %v", u.Nickname)
	}
	if u.Score == nil || *u.Score != 1.5 {
		t.Errorf("Score = %v", u.Score)
	}
	if strings.Join(u.Tags, ",") != "go,sql" {
		t.Errorf("Tags = %v", u.Tags)
	}
	if u.Settings["tier"] != "gold" {
		t.Errorf("Settings = %v", u.Settings)
	}
	if u.Avatar == nil || len(*u.Avatar) != 2 || (*u.Avatar)[0] != 1 {
		t.Errorf("Avatar = %v", u.Avatar)
	}
	if u.DeletedAt != nil {
		t.Errorf("DeletedAt = %v, want nil", u.DeletedAt)
	}
	if !u.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want %v", u.CreatedAt, t0)
	}
	// database/sql's null wrappers carry NULL as well as a pointer does, and
	// the descriptor compares against the value behind them.
	if !u.Bio.Valid || u.Bio.V != "hello" {
		t.Errorf("Bio = %+v, want the string hello", u.Bio)
	}
	if !u.Visits.Valid || u.Visits.Int64 != 7 {
		t.Errorf("Visits = %+v, want 7", u.Visits)
	}
}

func TestSelect_nullableColumns(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	// A NULL scans as nil rather than as a zero value, which is the whole
	// reason the field is a pointer.
	users, err := d.Users.Query().Where(gendemo.Users.ID.Eq(2)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	u := users[0]
	if u.Nickname != nil || u.Score != nil || u.Avatar != nil || u.DeletedAt != nil {
		t.Errorf("a row of NULLs scanned as %+v", u)
	}
	if u.Bio.Valid || u.Visits.Valid {
		t.Errorf("a NULL scanned into a sql.Null wrapper as valid: %+v %+v", u.Bio, u.Visits)
	}

	// IsNull and IsNotNull select on the distinction.
	nulls, err := d.Users.Query().
		Where(gendemo.Users.Nickname.IsNull()).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := strings.Join(emails(nulls), ","); got != "sam@example.com" {
		t.Errorf("IsNull matched %q", got)
	}

	notNull, err := d.Users.Query().
		Where(gendemo.Users.DeletedAt.IsNotNull()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := strings.Join(emails(notNull), ","); got != "robin@example.com" {
		t.Errorf("IsNotNull matched %q", got)
	}
}

func TestSelect_operators(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	tests := []struct {
		name  string
		preds []orm.Predicate[gendemo.User]
		want  string
	}{
		{name: "Eq", preds: p(gendemo.Users.Active.Eq(true)), want: "alex@example.com,sam@example.com"},
		{name: "Ne", preds: p(gendemo.Users.Active.Ne(true)), want: "robin@example.com"},
		{name: "Gt", preds: p(gendemo.Users.Age.Gt(int32(30))), want: "robin@example.com"},
		{name: "Gte", preds: p(gendemo.Users.Age.Gte(int32(30))), want: "alex@example.com,robin@example.com"},
		{name: "Lt", preds: p(gendemo.Users.Age.Lt(int32(30))), want: "sam@example.com"},
		{name: "Lte", preds: p(gendemo.Users.Age.Lte(int32(30))), want: "alex@example.com,sam@example.com"},
		{name: "Between", preds: p(gendemo.Users.Age.Between(18, 40)), want: "alex@example.com"},
		{name: "Like", preds: p(gendemo.Users.Email.Like("%example.com")), want: "alex@example.com,sam@example.com,robin@example.com"},
		{name: "Like is case sensitive", preds: p(gendemo.Users.Email.Like("ALEX%")), want: ""},
		{name: "ILike is not", preds: p(gendemo.Users.Email.ILike("ALEX%")), want: "alex@example.com"},
		{name: "In", preds: p(gendemo.Users.ID.In(1, 3)), want: "alex@example.com,robin@example.com"},
		{name: "In over nothing", preds: p(gendemo.Users.ID.In()), want: ""},
		{name: "an enum compares as its own type", preds: p(gendemo.Users.State.Eq(gendemo.UserStateBanned)), want: "robin@example.com"},
		{name: "an enum orders by its declared order", preds: p(gendemo.Users.State.Gt(gendemo.UserStatePending)), want: "alex@example.com,robin@example.com"},
		// A sql.Null column compares against its value type, and tests for
		// NULL separately.
		{name: "a sql.Null column compares by value", preds: p(gendemo.Users.Bio.Eq("hello")), want: "alex@example.com"},
		{name: "a sql.Null column tests for null", preds: p(gendemo.Users.Visits.IsNull()), want: "sam@example.com"},
		{name: "a sql.Null column orders by magnitude", preds: p(gendemo.Users.Visits.Gte(int64(7))), want: "alex@example.com"},
		{
			name:  "And",
			preds: p(orm.And(gendemo.Users.Active.Eq(true), gendemo.Users.Age.Gte(int32(18)))),
			want:  "alex@example.com",
		},
		{
			name:  "Or",
			preds: p(orm.Or(gendemo.Users.Age.Lt(int32(18)), gendemo.Users.Age.Gt(int32(40)))),
			want:  "sam@example.com,robin@example.com",
		},
		{
			name:  "Not",
			preds: p(orm.Not(gendemo.Users.Active.Eq(true))),
			want:  "robin@example.com",
		},
		{
			name:  "several predicates are AND",
			preds: p(gendemo.Users.Active.Eq(true), gendemo.Users.Age.Gte(int32(18))),
			want:  "alex@example.com",
		},
		{
			name:  "no predicates at all",
			preds: p(orm.And[gendemo.User]()),
			want:  "alex@example.com,sam@example.com,robin@example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users, err := d.Users.Query().
				Where(tt.preds...).
				OrderBy(gendemo.Users.ID.Asc()).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if got := strings.Join(emails(users), ","); got != tt.want {
				t.Errorf("matched %q, want %q", got, tt.want)
			}
		})
	}
}

func p(ps ...orm.Predicate[gendemo.User]) []orm.Predicate[gendemo.User] { return ps }

func TestSelect_orderAndLimit(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	desc, err := d.Users.Query().OrderBy(gendemo.Users.CreatedAt.Desc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := strings.Join(emails(desc), ","); got != "robin@example.com,sam@example.com,alex@example.com" {
		t.Errorf("descending = %q", got)
	}

	limited, err := d.Users.Query().OrderBy(gendemo.Users.CreatedAt.Desc()).Limit(2).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(limited) != 2 || limited[0].Email != "robin@example.com" {
		t.Errorf("limited = %q", emails(limited))
	}

	none, err := d.Users.Query().Limit(0).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("LIMIT 0 returned %d rows", len(none))
	}
}

// TestSelect_dynamicFilters is the flagship case: a filter set assembled at run
// time, where every combination — including none at all — has to produce valid
// SQL.
func TestSelect_dynamicFilters(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	type filter struct {
		Email  string
		MinAge *int32
		States []gendemo.UserState
	}

	search := func(f filter) ([]gendemo.User, string, error) {
		predicates := []orm.Predicate[gendemo.User]{}

		if f.Email != "" {
			predicates = append(predicates, gendemo.Users.Email.ILike("%"+f.Email+"%"))
		}
		if f.MinAge != nil {
			predicates = append(predicates, gendemo.Users.Age.Gte(*f.MinAge))
		}
		if len(f.States) > 0 {
			predicates = append(predicates, gendemo.Users.State.In(f.States...))
		}

		query := d.Users.Query().
			Where(orm.And(predicates...)).
			OrderBy(gendemo.Users.CreatedAt.Desc()).
			Limit(50)

		sql, _, err := query.SQL()
		if err != nil {
			return nil, "", err
		}
		users, err := query.All(t.Context())
		return users, sql, err
	}

	tests := []struct {
		name      string
		filter    filter
		want      string
		wantWhere bool
	}{
		{
			name: "no filters at all",
			want: "robin@example.com,sam@example.com,alex@example.com",
		},
		{
			name:      "email only",
			filter:    filter{Email: "ALEX"},
			want:      "alex@example.com",
			wantWhere: true,
		},
		{
			name:      "age only",
			filter:    filter{MinAge: ptr(int32(30))},
			want:      "robin@example.com,alex@example.com",
			wantWhere: true,
		},
		{
			name:      "states only",
			filter:    filter{States: []gendemo.UserState{gendemo.UserStateActive, gendemo.UserStateBanned}},
			want:      "robin@example.com,alex@example.com",
			wantWhere: true,
		},
		{
			name:      "everything at once",
			filter:    filter{Email: "example.com", MinAge: ptr(int32(18)), States: []gendemo.UserState{gendemo.UserStateActive}},
			want:      "alex@example.com",
			wantWhere: true,
		},
		{
			name:      "a filter that matches nothing",
			filter:    filter{Email: "nobody"},
			want:      "",
			wantWhere: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			users, sql, err := search(tt.filter)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			if got := strings.Join(emails(users), ","); got != tt.want {
				t.Errorf("matched %q, want %q", got, tt.want)
			}
			if hasWhere := strings.Contains(sql, " WHERE "); hasWhere != tt.wantWhere {
				t.Errorf("WHERE present = %v, want %v:\n%s", hasWhere, tt.wantWhere, sql)
			}
		})
	}
}

func TestSelect_sqlIsVisibleBeforeItRuns(t *testing.T) {
	testdb.AdminDSN(t)
	d := db(t)

	query := d.Users.Query().
		Where(gendemo.Users.Active.Eq(true), gendemo.Users.Age.Gte(int32(18))).
		OrderBy(gendemo.Users.CreatedAt.Desc()).
		Limit(50)

	sql, args, err := query.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, `SELECT "users"."id", "users"."email"`) {
		t.Errorf("sql = %s", sql)
	}
	if !strings.Contains(sql, `WHERE "users"."active" = $1 AND "users"."age" >= $2`) {
		t.Errorf("sql = %s", sql)
	}
	if !strings.HasSuffix(sql, `ORDER BY "users"."created_at" DESC LIMIT 50`) {
		t.Errorf("sql = %s", sql)
	}
	if len(args) != 2 || args[0] != true || args[1] != int32(18) {
		t.Errorf("args = %#v", args)
	}

	// The statement it showed is the statement it runs.
	if _, err := query.All(t.Context()); err != nil {
		t.Fatalf("All: %v", err)
	}
}

func TestSelect_secondEntityInTheSamePackage(t *testing.T) {
	testdb.AdminDSN(t)
	posts, err := db(t).Posts.Query().
		Where(gendemo.Posts.Published.Eq(true)).
		OrderBy(gendemo.Posts.CreatedAt.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(posts) != 2 || posts[0].Title != "first" || posts[1].Title != "third" {
		t.Errorf("posts = %+v", posts)
	}
	if !posts[0].CreatedAt.After(t0) {
		t.Errorf("CreatedAt = %v", posts[0].CreatedAt)
	}
}

func TestSelect_relationFieldsAreIgnored(t *testing.T) {
	testdb.AdminDSN(t)
	// User has an orm.Many[Post] and Post an orm.One[User]. Neither is a
	// column, so neither appears in the SELECT list, and both come back
	// unloaded.
	users, err := db(t).Users.Query().Where(gendemo.Users.ID.Eq(1)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	sql, _, err := db(t).Users.Query().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "posts") {
		t.Errorf("the relation reached the SELECT list: %s", sql)
	}
	if !users[0].Posts.IsZero() {
		t.Error("a relation came back loaded from a query that never asked for it")
	}
}

func TestSelect_valuesAreParameters(t *testing.T) {
	testdb.AdminDSN(t)
	// A value that reads as SQL is data. If it were interpolated, this would
	// drop the table rather than return no rows.
	const nasty = `'; DROP TABLE users; --`
	d := db(t)
	users, err := d.Users.Query().Where(gendemo.Users.Email.Eq(nasty)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("matched %d rows", len(users))
	}
	// The table is still there.
	all, err := d.Users.Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("%d rows survived, want 3", len(all))
	}
}

func TestSelect_databaseErrorsSurface(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	d := gendemo.New(conn)
	_ = conn.Close(context.WithoutCancel(t.Context()))

	if _, err := d.Users.Query().All(t.Context()); err == nil {
		t.Error("All succeeded on a closed connection")
	} else if !strings.Contains(err.Error(), "querying public.users") {
		t.Errorf("error = %v, want it to name the table", err)
	}
}

func ptr[T any](v T) *T { return &v }
