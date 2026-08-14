package gendemo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Writes and errors, audited.
//
// The guarantees here are the ones whose failure is silent: a zero value the
// tool decided not to write, a WHERE clause it decided not to require, a
// PostgreSQL error somebody has to parse a string to recognise. None of those
// look like failures until somebody's data is wrong.

// ------------------------------------------- the query is what SQL would do

// A typed query and the SQL somebody would have written by hand return the same
// rows. Comparing generated SQL text would only prove the compiler is
// consistent with itself; this compares behaviour, which is the claim.
func TestAudit_queriesMatchHandwrittenSQL(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed+`
		INSERT INTO users (id, email, age, active, state, nickname, score, tags, settings, bio, visits, created_at) VALUES
		  (4, 'kim@example.com',   17, true,  'active',  'kim', 3.5,  '{go}',  '{}', NULL,  0, '2024-04-01T09:00:00Z'),
		  (5, 'lee@example.com',   62, false, 'pending', NULL,  NULL, '{}',    '{}', 'x',   9, '2024-05-01T09:00:00Z'),
		  (6, 'ash@example.com',   30, true,  'banned',  'ash', 1.5,  '{ops}', '{}', NULL,  3, '2024-08-01T09:00:00Z');
		SELECT setval(pg_get_serial_sequence('users','id'), 1000);`)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	db := gendemo.New(conn)

	handwritten := func(where string, args ...any) []int64 {
		t.Helper()
		rows, err := conn.Query(t.Context(), "SELECT id FROM users WHERE "+where+" ORDER BY id", args...)
		if err != nil {
			t.Fatalf("handwritten %q: %v", where, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scanning: %v", err)
			}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("handwritten %q: %v", where, err)
		}
		return out
	}

	for _, tt := range []struct {
		name  string
		pred  orm.Predicate[gendemo.User]
		where string
		args  []any
	}{
		{"equality", gendemo.Users.Age.Eq(30), "age = $1", []any{30}},
		{"inequality", gendemo.Users.Age.Ne(30), "age <> $1", []any{30}},
		{"greater", gendemo.Users.Age.Gt(30), "age > $1", []any{30}},
		{"greater or equal", gendemo.Users.Age.Gte(30), "age >= $1", []any{30}},
		{"less", gendemo.Users.Age.Lt(30), "age < $1", []any{30}},
		{"less or equal", gendemo.Users.Age.Lte(30), "age <= $1", []any{30}},
		{"between", gendemo.Users.Age.Between(18, 45), "age BETWEEN $1 AND $2", []any{18, 45}},
		{"like", gendemo.Users.Email.Like("%a%"), "email LIKE $1", []any{"%a%"}},
		{"case-insensitive like", gendemo.Users.Email.ILike("%A%"), "email ILIKE $1", []any{"%A%"}},
		{"in", gendemo.Users.Age.In(17, 30), "age = ANY($1)", []any{[]int32{17, 30}}},
		{"in with one value", gendemo.Users.Age.In(17), "age = ANY($1)", []any{[]int32{17}}},
		{"is null", gendemo.Users.Nickname.IsNull(), "nickname IS NULL", nil},
		{"is not null", gendemo.Users.Nickname.IsNotNull(), "nickname IS NOT NULL", nil},
		{"a nullable comparison", gendemo.Users.Score.Gt(2.0), "score > $1", []any{2.0}},
		{"an enum", gendemo.Users.State.Eq(gendemo.UserStateActive), "state = $1", []any{"active"}},
		{"a boolean", gendemo.Users.Active.Eq(false), "active = $1", []any{false}},
		{"and", orm.And(gendemo.Users.Age.Gt(18), gendemo.Users.Active.Eq(true)),
			"(age > $1 AND active = $2)", []any{18, true}},
		{"or", orm.Or(gendemo.Users.Age.Lt(18), gendemo.Users.Age.Gt(60)),
			"(age < $1 OR age > $2)", []any{18, 60}},
		{"not", orm.Not(gendemo.Users.Active.Eq(true)), "NOT (active = $1)", []any{true}},
		{"a nest of all three", orm.And(
			orm.Or(gendemo.Users.Age.Lt(18), gendemo.Users.Age.Gt(60)),
			orm.Not(gendemo.Users.Nickname.IsNull()),
		), "((age < $1 OR age > $2) AND NOT (nickname IS NULL))", []any{18, 60}},
		{"a semi-join", gendemo.Users.Posts.Any(),
			"EXISTS (SELECT 1 FROM posts p WHERE p.author_id = users.id)", nil},
		{"a filtered semi-join", gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true)),
			"EXISTS (SELECT 1 FROM posts p WHERE p.author_id = users.id AND p.published)", nil},
		{"a negative semi-join", gendemo.Users.Posts.None(),
			"NOT EXISTS (SELECT 1 FROM posts p WHERE p.author_id = users.id)", nil},
		{"a raw fragment", orm.Expr[gendemo.User]("char_length(email) > $1", 16),
			"char_length(email) > $1", []any{16}},
	} {
		got, err := db.Users.Query().Where(tt.pred).OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("%s: All: %v", tt.name, err)
		}
		want := handwritten(tt.where, tt.args...)
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			sql, _, _ := db.Users.Query().Where(tt.pred).SQL()
			t.Errorf("%s:\n    orm         = %v\n    handwritten = %v\n    %s", tt.name, ids(got), want, sql)
		}
		if len(want) == 0 {
			t.Errorf("%s matched nothing, so it proves nothing", tt.name)
		}
	}

	// Pagination against the same handwritten query.
	for _, page := range []struct{ limit, offset int }{{2, 0}, {2, 2}, {1, 5}, {10, 0}, {3, 4}} {
		got, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).
			Limit(page.limit).Offset(page.offset).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		rows, err := conn.Query(t.Context(),
			fmt.Sprintf("SELECT id FROM users ORDER BY id LIMIT %d OFFSET %d", page.limit, page.offset))
		if err != nil {
			t.Fatalf("handwritten: %v", err)
		}
		var want []int64
		for rows.Next() {
			var id int64
			_ = rows.Scan(&id)
			want = append(want, id)
		}
		rows.Close()
		if fmt.Sprint(ids(got)) != fmt.Sprint(want) {
			t.Errorf("limit %d offset %d: orm = %v, handwritten = %v", page.limit, page.offset, ids(got), want)
		}
	}
}

// An empty IN is a predicate that matches nothing, which is what SQL says and
// what a filter built from an empty request has to mean.
func TestAudit_emptyIn(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)

	n, err := db.Users.Query().Where(gendemo.Users.Age.In()).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("an empty IN matched %d rows", n)
	}
	// And it stays an annihilator inside a group rather than disappearing.
	n, err = db.Users.Query().
		Where(orm.And(gendemo.Users.Age.In(), gendemo.Users.Age.Gt(0))).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("an empty IN inside And matched %d rows", n)
	}
	// An OR with an empty IN keeps the other side.
	n, err = db.Users.Query().
		Where(orm.Or(gendemo.Users.Age.In(), gendemo.Users.Age.Gt(0))).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("an empty IN inside Or matched %d rows, want 3", n)
	}
}

// ------------------------------------------------------------------ writes

// A Go zero value is a value. Every one of them survives a round trip, and none
// of them is quietly replaced by the column's default.
func TestAudit_zeroValuesAreWritten(t *testing.T) {
	testdb.AdminDSN(t)
	db := emptyDB(t)

	// active and state both have defaults; writing the zero value must beat
	// them, and the empty string, the zero number and the empty slice must all
	// arrive as themselves.
	zero := newUser("zero@example.com")
	zero.Age = 0
	zero.Active = false
	zero.State = gendemo.UserStatePending
	zero.Avatar = ptr([]byte{})
	got, err := db.Users.Insert(t.Context(), zero)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got.Age != 0 || got.Active {
		t.Errorf("a zero value was replaced by a default: %+v", got)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("an empty slice became %v", got.Tags)
	}
	if got.Avatar == nil || len(*got.Avatar) != 0 {
		t.Errorf("an empty byte slice became %v", got.Avatar)
	}
	if got.Nickname != nil {
		t.Errorf("a nil pointer became %v", got.Nickname)
	}

	// Asking for the default is the other, explicit thing.
	withDefault, err := db.Users.Insert(t.Context(), newUser("default@example.com"),
		orm.Default(gendemo.Users.Active), orm.Default(gendemo.Users.State))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !withDefault.Active || withDefault.State != gendemo.UserStatePending {
		t.Errorf("Default did not take the database's value: %+v", withDefault)
	}

	// The entity handed in is not the entity handed back.
	in := newUser("untouched@example.com")
	out, err := db.Users.Insert(t.Context(), in)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if in.ID != 0 {
		t.Errorf("Insert mutated the entity it was given: %+v", in)
	}
	// The identity and the generated column came from the database. created_at
	// did not, and must not have: the entity carried the zero time and a zero
	// value is a value, so PostgreSQL was told to store exactly that.
	if out.ID == 0 || out.Slug == nil || *out.Slug != "untouched@example.com" {
		t.Errorf("the returned entity is missing what the database computed: %+v", out)
	}
	if !out.CreatedAt.IsZero() {
		t.Errorf("created_at = %v; the entity carried the zero time and a zero value is a value", out.CreatedAt)
	}
}

// Asking for a default the column does not have is refused before the statement
// is built, and so is asking for one on a column the database always writes.
func TestAudit_defaultIsChecked(t *testing.T) {
	testdb.AdminDSN(t)
	db := emptyDB(t)

	for _, tt := range []struct {
		name string
		opt  orm.InsertOpt[gendemo.User]
	}{
		{"a column with no default", orm.Default(gendemo.Users.Email)},
		{"the same column twice", orm.Default(gendemo.Users.Active, gendemo.Users.Active)},
	} {
		_, err := db.Users.Insert(t.Context(), newUser("d"+tt.name+"@example.com"), tt.opt)
		if !errors.Is(err, orm.ErrInvalidDefault) {
			t.Errorf("%s: err = %v, want ErrInvalidDefault", tt.name, err)
		}
	}

	// A generated column is accepted, because asking PostgreSQL to supply the
	// value is exactly what a generated column does. It is already left out of
	// the statement, so the option changes nothing — and refusing it would mean
	// refusing a true statement about the column.
	if _, err := db.Users.Insert(t.Context(), newUser("gen@example.com"),
		orm.Default(gendemo.Users.Slug)); err != nil {
		t.Errorf("Default on a generated column: %v", err)
	}
}

// InsertMany chunks so that one statement never exceeds what the protocol can
// carry, and the rows come back in the order they were given whether it chunked
// or not.
func TestAudit_insertManyChunking(t *testing.T) {
	testdb.AdminDSN(t)
	db := emptyDB(t)

	if got, err := db.Users.InsertMany(t.Context(), nil); err != nil || len(got) != 0 {
		t.Errorf("InsertMany(nil) = %v, %v", got, err)
	}
	if got, err := db.Users.InsertMany(t.Context(), []gendemo.User{}); err != nil || len(got) != 0 {
		t.Errorf("InsertMany(empty) = %v, %v", got, err)
	}

	// Enough rows that the parameter count of one statement would be far past
	// the 65535 the wire protocol allows: users has fifteen writable columns,
	// so 6000 rows is around 90000 parameters.
	const n = 6000
	rows := make([]gendemo.User, 0, n)
	for i := range n {
		u := newUser(fmt.Sprintf("bulk%d@example.com", i))
		u.Age = int32(i % 90)
		rows = append(rows, u)
	}
	got, err := db.Users.InsertMany(t.Context(), rows)
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if len(got) != n {
		t.Fatalf("InsertMany returned %d rows, want %d", len(got), n)
	}
	for i, u := range got {
		if u.Email != rows[i].Email {
			t.Fatalf("row %d came back as %s, want %s", i, u.Email, rows[i].Email)
		}
		if u.ID == 0 {
			t.Fatalf("row %d came back without its identity: %+v", i, u)
		}
	}

	// A failure part way through is still a failure: nothing claims success.
	dup := []gendemo.User{newUser("dup@example.com"), newUser("dup@example.com")}
	if _, err := db.Users.InsertMany(t.Context(), dup); err == nil {
		t.Error("two rows with one unique email were accepted")
	} else {
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23505" {
			t.Errorf("err = %v, want a unique violation", err)
		}
	}
}

// ------------------------------------------------------------ PgError

// Every path that reaches PostgreSQL keeps PostgreSQL's own error recoverable.
// Wrapping may add context; it may not destroy the thing a caller matches on.
func TestAudit_postgresErrorsSurviveEveryPath(t *testing.T) {
	testdb.AdminDSN(t)
	db := emptyDB(t)
	ctx := t.Context()

	// One row to conflict with, and one post to be referenced.
	existing, err := db.Users.Insert(ctx, newUser("alex@example.com"))
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	if _, err := db.Posts.Insert(ctx, gendemo.Post{AuthorID: &existing.ID, Title: "p"}); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	cases := []struct {
		name string
		code string
		run  func() error
	}{
		{"a unique violation on insert", "23505", func() error {
			_, err := db.Users.Insert(ctx, newUser("alex@example.com"))
			return err
		}},
		{"a unique violation in a batch", "23505", func() error {
			_, err := db.Users.InsertMany(ctx, []gendemo.User{newUser("alex@example.com")})
			return err
		}},
		{"a foreign key violation", "23503", func() error {
			_, err := db.Posts.Insert(ctx, gendemo.Post{AuthorID: ptr(int64(999999)), Title: "x"})
			return err
		}},
		{"a write in a read-only transaction", "25006", func() error {
			return db.TxOptions(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx *gendemo.DB) error {
				_, err := tx.Users.Update().Set(gendemo.Users.Age.Set(1)).
					Where(gendemo.Users.ID.Eq(existing.ID)).Exec(ctx)
				return err
			})
		}},
		{"a runtime error inside an expression", "22012", func() error {
			_, err := db.Users.Query().
				Where(orm.Expr[gendemo.User]("age / (age - age) > 0")).All(ctx)
			return err
		}},
		{"an invalid enum label", "22P02", func() error {
			_, err := db.Users.Update().Set(gendemo.Users.State.Set(gendemo.UserState("nope"))).
				Where(gendemo.Users.ID.Eq(existing.ID)).Exec(ctx)
			return err
		}},
		{"a broken raw statement", "42703", func() error {
			_, err := orm.Raw[gendemo.User](db.Users, "SELECT nope FROM users").All(ctx)
			return err
		}},
		{"a broken raw statement, streamed", "42703", func() error {
			for _, err := range orm.Raw[gendemo.User](db.Users, "SELECT nope FROM users").Rows(ctx) {
				if err != nil {
					return err
				}
			}
			return nil
		}},
		{"a raw expression the server refuses", "42883", func() error {
			_, err := db.Users.Query().Where(orm.Expr[gendemo.User]("no_such_function(email)")).All(ctx)
			return err
		}},
		{"a delete blocked by a foreign key", "23503", func() error {
			_, err := db.Users.Delete().Where(gendemo.Users.ID.Eq(existing.ID)).Exec(ctx)
			return err
		}},
		{"a failure inside a transaction", "23505", func() error {
			return db.Tx(ctx, func(tx *gendemo.DB) error {
				_, err := tx.Users.Insert(ctx, newUser("alex@example.com"))
				return err
			})
		}},
	}

	for _, tt := range cases {
		err := tt.run()
		if err == nil {
			t.Errorf("%s: no error", tt.name)
			continue
		}
		var pg *pgconn.PgError
		if !errors.As(err, &pg) {
			t.Errorf("%s: %v is not a *pgconn.PgError", tt.name, err)
			continue
		}
		if pg.Code != tt.code {
			t.Errorf("%s: SQLSTATE %s, want %s (%v)", tt.name, pg.Code, tt.code, err)
		}
		// The wrapping still says which operation it was, or the code would be
		// the only thing a reader had.
		if !strings.Contains(err.Error(), pg.Message) {
			t.Errorf("%s: the wrapped error lost PostgreSQL's message: %v", tt.name, err)
		}
	}
}

// ------------------------------------------------------------- the guards

// A write with no WHERE is refused before it reaches PostgreSQL, in every form
// the builder can take.
func TestAudit_writeGuardsNeverReachTheDatabase(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	count := func() int64 {
		t.Helper()
		var n int64
		if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM users").Scan(&n); err != nil {
			t.Fatalf("counting: %v", err)
		}
		return n
	}
	before := count()

	for _, tt := range []struct {
		name string
		run  func() error
		want error
	}{
		{"an update with no WHERE", func() error {
			_, err := db.Users.Update().Set(gendemo.Users.Age.Set(1)).Exec(t.Context())
			return err
		}, orm.ErrMissingWhere},
		{"a delete with no WHERE", func() error {
			_, err := db.Users.Delete().Exec(t.Context())
			return err
		}, orm.ErrMissingWhere},
		{"an update that assigns nothing", func() error {
			_, err := db.Users.Update().Where(gendemo.Users.ID.Eq(1)).Exec(t.Context())
			return err
		}, orm.ErrMissingSet},
		{"an update assigning one column twice", func() error {
			_, err := db.Users.Update().
				Set(gendemo.Users.Age.Set(1), gendemo.Users.Age.Set(2)).
				Where(gendemo.Users.ID.Eq(1)).Exec(t.Context())
			return err
		}, orm.ErrDuplicateAssignment},
	} {
		if err := tt.run(); !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}

	// Nothing above ran a statement, and nothing above changed a row.
	if ex.n != 0 {
		t.Errorf("a refused write sent %d statements:\n%s", ex.n, strings.Join(ex.sql, "\n"))
	}
	if after := count(); after != before {
		t.Errorf("the row count changed from %d to %d", before, after)
	}

	// All() is the deliberate form, and it works.
	n, err := db.Users.Update().Set(gendemo.Users.Age.Set(41)).All().Exec(t.Context())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n != before {
		t.Errorf("All() updated %d rows, want %d", n, before)
	}
	// Asking for both is a contradiction rather than a preference.
	if _, err := db.Users.Update().Set(gendemo.Users.Age.Set(1)).
		Where(gendemo.Users.ID.Eq(1)).All().Exec(t.Context()); err == nil {
		t.Error("a write that is both filtered and unfiltered was accepted")
	}
}
