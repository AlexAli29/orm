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

// Richer writes against real PostgreSQL.
//
// The value of an expression assignment is that the new value is computed from
// the old one without reading it first, so the tests that matter are the ones
// where reading it first would give a different answer. The value of RETURNING
// is that a delete can be observed at all.

// Scenario F: an atomic increment.
func TestWriteExpr_arithmetic(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	sql, args, err := db.Users.Update().
		Set(gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1))).
		Where(gendemo.Users.ID.Eq(1)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `UPDATE "public"."users" SET "age" = "users"."age" + $1 WHERE "users"."id" = $2`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	// The literal is a parameter, not text.
	if len(args) != 2 || args[0] != int32(1) || args[1] != int64(1) {
		t.Errorf("args = %v", args)
	}

	n, err := db.Users.Update().
		Set(gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1))).
		Where(gendemo.Users.ID.Eq(1)).
		Exec(t.Context())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if n != 1 {
		t.Errorf("updated %d rows", n)
	}
	var age int32
	if err := conn.QueryRow(t.Context(), "SELECT age FROM users WHERE id = 1").Scan(&age); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if age != 31 {
		t.Errorf("age = %d, want 31", age)
	}

	// Every arithmetic form, against the handwritten equivalent.
	for _, tt := range []struct {
		name string
		set  orm.Assign[gendemo.User]
		sql  string
		// perRow marks a case whose result depends on the row it ran against,
		// so the two sides are each checked against their own row instead of
		// against each other.
		perRow bool
	}{
		{"subtract", gendemo.Users.Age.SetExpr(gendemo.Users.Age.Sub(1)), "age = age - 1", false},
		{"multiply", gendemo.Users.Age.SetExpr(gendemo.Users.Age.Mul(2)), "age = age * 2", false},
		{"divide", gendemo.Users.Age.SetExpr(gendemo.Users.Age.Div(2)), "age = age / 2", false},
		{"another column", gendemo.Users.Age.SetExpr(gendemo.Users.Age.AddCol(gendemo.Users.Age)),
			"age = age + age", false},
		// This one writes each row's own email, so the two rows differ by
		// construction: what is compared is that each took its own.
		{"a column into a nullable one",
			gendemo.Users.Nickname.SetExpr(orm.Nullable(gendemo.Users.Email)), "nickname = email", true},
	} {
		if _, err := conn.Exec(t.Context(), "UPDATE users SET age = 30, nickname = 'x' WHERE id = 2"); err != nil {
			t.Fatalf("resetting: %v", err)
		}
		if _, err := db.Users.Update().Set(tt.set).Where(gendemo.Users.ID.Eq(2)).Exec(t.Context()); err != nil {
			t.Fatalf("%s: Exec: %v", tt.name, err)
		}
		var gotAge int32
		var gotName *string
		if err := conn.QueryRow(t.Context(), "SELECT age, nickname FROM users WHERE id = 2").
			Scan(&gotAge, &gotName); err != nil {
			t.Fatalf("%s: reading back: %v", tt.name, err)
		}

		if _, err := conn.Exec(t.Context(), "UPDATE users SET age = 30, nickname = 'x' WHERE id = 3"); err != nil {
			t.Fatalf("resetting: %v", err)
		}
		if _, err := conn.Exec(t.Context(), "UPDATE users SET "+tt.sql+" WHERE id = 3"); err != nil {
			t.Fatalf("%s: handwritten: %v", tt.name, err)
		}
		var wantAge int32
		var wantName *string
		if err := conn.QueryRow(t.Context(), "SELECT age, nickname FROM users WHERE id = 3").
			Scan(&wantAge, &wantName); err != nil {
			t.Fatalf("%s: reading back: %v", tt.name, err)
		}
		if tt.perRow {
			if text(gotName) != "b@example.com" || text(wantName) != "c@example.com" {
				t.Errorf("%s: orm = %s, handwritten = %s; each should have taken its own row's email",
					tt.name, text(gotName), text(wantName))
			}
			continue
		}
		if gotAge != wantAge || text(gotName) != text(wantName) {
			t.Errorf("%s: orm = (%d, %s), handwritten = (%d, %s)",
				tt.name, gotAge, text(gotName), wantAge, text(wantName))
		}
	}
}

// text renders a nullable string by value rather than by address.
func text(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// Scenario G: an update that reports what it changed, with no second statement.
func TestWriteReturning_update(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	sql, _, err := orm.UpdateReturning(
		db.Users.Update().Set(gendemo.Users.Active.Set(true)).Where(gendemo.Users.ID.Eq(3)),
		userSummaries,
	).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `UPDATE "public"."users" SET "active" = $1 WHERE "users"."id" = $2 ` +
		`RETURNING "users"."id", "users"."email"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}

	ex.reset()
	got, err := orm.UpdateReturning(
		db.Users.Update().Set(gendemo.Users.Active.Set(true)).Where(gendemo.Users.ID.Eq(3)),
		userSummaries,
	).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0].ID != 3 || got[0].Email != "robin@example.com" {
		t.Fatalf("returned %+v", got)
	}
	// One statement: the values came from the UPDATE, not from a SELECT.
	if ex.n != 1 {
		t.Errorf("update returning ran %d statements, want 1", ex.n)
	}

	// The whole entity, through the generated scanner.
	entities, err := orm.UpdateReturningEntity(
		db.Users.Update().Set(gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1))).
			Where(gendemo.Users.ID.Lt(3)),
	).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("returned %d entities", len(entities))
	}
	for _, u := range entities {
		if u.Email == "" || u.CreatedAt.IsZero() {
			t.Errorf("entity came back incomplete: %+v", u)
		}
	}
	// RETURNING is explicit, never a star.
	sql, _, err = orm.UpdateReturningEntity(
		db.Users.Update().Set(gendemo.Users.Active.Set(true)).All(),
	).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "*") || !strings.Contains(sql, `RETURNING "id", "email"`) {
		t.Errorf("SQL = %s", sql)
	}
}

// One on a write runs the whole write and then reports the count. The contract
// is that the mutation is never narrowed to make the terminal convenient.
func TestWriteReturning_oneDoesNotNarrowTheWrite(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	// Two rows match. One reports ErrMultipleRows — and both rows are changed,
	// because the UPDATE was the one the builder described.
	_, err := orm.UpdateReturning(
		db.Users.Update().Set(gendemo.Users.Active.Set(false)).Where(gendemo.Users.ID.Lt(3)),
		userSummaries,
	).One(t.Context())
	if !errors.Is(err, orm.ErrMultipleRows) {
		t.Fatalf("One = %v, want ErrMultipleRows", err)
	}
	var changed int
	if err := conn.QueryRow(t.Context(),
		"SELECT count(*) FROM users WHERE id < 3 AND active = false").Scan(&changed); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if changed != 2 {
		t.Errorf("%d rows were changed; One must not narrow the write", changed)
	}

	// A WHERE that identifies one row is the case One is for.
	got, err := orm.UpdateReturning(
		db.Users.Update().Set(gendemo.Users.Active.Set(true)).Where(gendemo.Users.ID.Eq(1)),
		userSummaries,
	).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.ID != 1 {
		t.Errorf("returned %+v", got)
	}

	// Nothing matched is ErrNotFound.
	if _, err := orm.UpdateReturning(
		db.Users.Update().Set(gendemo.Users.Active.Set(true)).Where(gendemo.Users.ID.Lt(0)),
		userSummaries,
	).One(t.Context()); !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("One = %v, want ErrNotFound", err)
	}
}

// Scenario H: a delete that reports what it removed. There is no other way to
// see it afterwards.
func TestWriteReturning_delete(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	ex.reset()
	got, err := orm.DeleteReturning(
		db.Posts.Delete().Where(gendemo.Posts.Published.Eq(false)),
		orm.Project2(gendemo.Posts.ID, gendemo.Posts.Title,
			func(id int64, title string) string { return fmt.Sprintf("%d:%s", id, title) }),
	).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0] != "2:second" {
		t.Fatalf("returned %v", got)
	}
	// No pre-delete SELECT.
	if ex.n != 1 {
		t.Errorf("delete returning ran %d statements, want 1", ex.n)
	}
	var left int
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM posts").Scan(&left); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if left != 2 {
		t.Errorf("%d posts remain, want 2", left)
	}

	// The whole entity of a deleted row.
	entities, err := orm.DeleteReturningEntity(
		db.Posts.Delete().Where(gendemo.Posts.ID.Eq(1)),
	).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(entities) != 1 || entities[0].Title != "first" {
		t.Errorf("returned %+v", entities)
	}
}

// Scenario I: an upsert that computes its new values.
func TestWriteConflict_expressions(t *testing.T) {
	testdb.AdminDSN(t)
	db := emptyDB(t)

	base := newUser("upsert@example.com")
	base.Age = 5
	first, err := db.Users.Insert(t.Context(), base)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The conflicting insert takes the new name from EXCLUDED and increments a
	// counter from the row that was already there.
	again := newUser("upsert@example.com")
	renamed := "renamed"
	again.Nickname = &renamed
	again.Age = 100
	got, err := db.Users.Insert(t.Context(), again,
		orm.OnConflict(gendemo.Users.Email).DoUpdateSet(
			gendemo.Users.Nickname.SetExpr(orm.Excluded(gendemo.Users.Nickname)),
			gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1)),
		),
	)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("the upsert made a new row: %d and %d", first.ID, got.ID)
	}
	if got.Nickname == nil || *got.Nickname != "renamed" {
		t.Errorf("nickname = %v, want the excluded value", got.Nickname)
	}
	if got.Age != 6 {
		t.Errorf("age = %d, want the existing value plus one", got.Age)
	}

	// The SQL says so, and EXCLUDED is a node rather than a string.
	sql, _, err := db.Users.InsertSQL([]gendemo.User{again},
		orm.OnConflict(gendemo.Users.Email).DoUpdateSet(
			gendemo.Users.Nickname.SetExpr(orm.Excluded(gendemo.Users.Nickname)),
			gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1)),
		),
	)
	if err != nil {
		t.Fatalf("InsertSQL: %v", err)
	}
	if !strings.Contains(sql, `DO UPDATE SET "nickname" = EXCLUDED."nickname", "age" = "users"."age" + $`) {
		t.Errorf("SQL = %s", sql)
	}

	// DoUpdate and DoUpdateSet combine, and a conflict WHERE restricts which
	// conflicting rows are touched.
	third := newUser("upsert@example.com")
	third.Age = 1
	_, err = db.Users.Insert(t.Context(), third,
		orm.OnConflict(gendemo.Users.Email).
			Where(gendemo.Users.Age.Lt(0)).
			DoUpdate(gendemo.Users.Nickname),
	)
	// The condition matches nothing, so PostgreSQL updates nothing and returns
	// no row — which is the same outcome DO NOTHING has.
	if !errors.Is(err, orm.ErrConflictIgnored) {
		t.Errorf("Insert = %v, want ErrConflictIgnored", err)
	}
}

// EXCLUDED exists only inside a conflict clause, and using it elsewhere is
// refused before the statement runs.
func TestWriteConflict_excludedIsScoped(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	_, err = db.Users.Update().
		Set(gendemo.Users.Nickname.SetExpr(orm.Excluded(gendemo.Users.Nickname))).
		Where(gendemo.Users.ID.Eq(1)).
		Exec(t.Context())
	if err == nil {
		t.Fatal("EXCLUDED was accepted in an ordinary update")
	}
	if !strings.Contains(err.Error(), "only exists inside ON CONFLICT DO UPDATE") {
		t.Errorf("err = %v", err)
	}
	if ex.n != 0 {
		t.Errorf("the refused update sent %d statements", ex.n)
	}
}

// The M4 guards are unchanged: an expression assignment is an assignment, and a
// write with no WHERE is still refused.
func TestWriteExpr_guardsAreUnchanged(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	for _, tt := range []struct {
		name string
		run  func() error
		want error
	}{
		{"an expression update with no WHERE", func() error {
			_, err := db.Users.Update().
				Set(gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1))).Exec(t.Context())
			return err
		}, orm.ErrMissingWhere},
		{"a returning update with no WHERE", func() error {
			_, err := orm.UpdateReturning(
				db.Users.Update().Set(gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1))),
				userSummaries,
			).All(t.Context())
			return err
		}, orm.ErrMissingWhere},
		{"a returning delete with no WHERE", func() error {
			_, err := orm.DeleteReturning(db.Users.Delete(), userSummaries).All(t.Context())
			return err
		}, orm.ErrMissingWhere},
		{"a column assigned as both a literal and an expression", func() error {
			_, err := db.Users.Update().Set(
				gendemo.Users.Age.Set(1),
				gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(1)),
			).Where(gendemo.Users.ID.Eq(1)).Exec(t.Context())
			return err
		}, orm.ErrDuplicateAssignment},
		{"an expression assigned to a generated column", func() error {
			_, err := db.Users.Update().
				Set(gendemo.Users.Slug.SetExpr(orm.Nullable(gendemo.Users.Email))).
				Where(gendemo.Users.ID.Eq(1)).Exec(t.Context())
			return err
		}, nil},
	} {
		err := tt.run()
		if err == nil {
			t.Errorf("%s was accepted", tt.name)
			continue
		}
		if tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("%s: err = %v, want %v", tt.name, err, tt.want)
		}
	}
	if ex.n != 0 {
		t.Errorf("a refused write sent %d statements", ex.n)
	}
}

// Placeholders stay in statement order across every clause that carries one.
func TestWriteExpr_placeholderOrdering(t *testing.T) {
	testdb.AdminDSN(t)
	db := emptyDB(t)

	sql, args, err := orm.UpdateReturning(
		db.Users.Update().
			Set(
				gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(7)),
				gendemo.Users.Bio.Set("second"),
			).
			Where(gendemo.Users.ID.Eq(11), gendemo.Users.Email.Like("%third%")),
		orm.Project2(
			gendemo.Users.ID,
			orm.RawValue[gendemo.User, string]("upper($1)", "fourth"),
			func(id int64, s string) string { return s },
		),
	).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	// Set, then Where, then Returning — the order the clauses appear in.
	if fmt.Sprint(args) != "[7 second 11 %third% fourth]" {
		t.Errorf("args = %v", args)
	}
	for i, want := range []string{"$1", "$2", "$3", "$4", "$5"} {
		if !strings.Contains(sql, want) {
			t.Errorf("placeholder %d missing from %s", i+1, sql)
		}
	}
	if strings.Index(sql, "$1") > strings.Index(sql, "$3") {
		t.Errorf("placeholders are not in statement order: %s", sql)
	}
}

// PostgreSQL's own errors survive the new paths.
func TestWriteExpr_postgresErrors(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := aggDB(t)

	// int4 overflow, raised by the arithmetic itself.
	_, err := db.Posts.Update().
		Set(gendemo.Posts.Score.SetExpr(gendemo.Posts.Score.Mul(2147483647))).
		Where(gendemo.Posts.ID.Eq(1)).
		Exec(t.Context())
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("err = %v, want a *pgconn.PgError", err)
	}
	if pg.Code != "22003" {
		t.Errorf("SQLSTATE = %s, want 22003", pg.Code)
	}

	// A foreign key violation through a RETURNING delete.
	_, err = orm.DeleteReturning(
		db.Users.Delete().Where(gendemo.Users.ID.Eq(1)),
		userSummaries,
	).All(t.Context())
	if !errors.As(err, &pg) {
		t.Fatalf("err = %v, want a *pgconn.PgError", err)
	}
	if pg.Code != "23503" {
		t.Errorf("SQLSTATE = %s, want 23503", pg.Code)
	}
}

// Scenario J: projection, aggregate and RETURNING in one transaction, rolled
// back together.
func TestWrite_insideATransaction(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := aggDB(t)

	want := errors.New("roll back")
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		updated, err := orm.UpdateReturning(
			tx.Users.Update().Set(gendemo.Users.Age.SetExpr(gendemo.Users.Age.Add(100))).
				Where(gendemo.Users.ID.Eq(1)),
			orm.Project2(gendemo.Users.ID, gendemo.Users.Age,
				func(id int64, age int32) int32 { return age }),
		).One(t.Context())
		if err != nil {
			return err
		}
		if updated != 130 {
			t.Errorf("age = %d, want 130", updated)
		}

		// An aggregate inside the transaction sees the uncommitted value.
		shape := orm.Project1(orm.SumInt32(gendemo.Users.Age), func(v *int64) *int64 { return v })
		sum, err := orm.Select(tx.Users, shape).One(t.Context())
		if err != nil {
			return err
		}
		if sum == nil || *sum != 220 {
			t.Errorf("sum inside the transaction = %v, want 220", sum)
		}

		// A delete returning inside a savepoint, rolled back on its own.
		inner := errors.New("inner")
		if err := tx.Tx(t.Context(), func(sp *gendemo.DB) error {
			gone, err := orm.DeleteReturning(
				sp.Posts.Delete().Where(gendemo.Posts.AuthorID.IsNull()),
				orm.Project1(gendemo.Posts.ID, func(id int64) int64 { return id }),
			).All(t.Context())
			if err != nil {
				return err
			}
			if len(gone) != 1 {
				t.Errorf("deleted %v", gone)
			}
			return inner
		}); !errors.Is(err, inner) {
			return err
		}
		// The savepoint rolled back, so the post is still there.
		var posts int
		if err := conn.QueryRow(context.Background(), "SELECT 1").Scan(&posts); err != nil {
			return err
		}
		n, err := tx.Posts.Query().Count(t.Context())
		if err != nil {
			return err
		}
		if n != 5 {
			t.Errorf("posts = %d after the savepoint rolled back, want 5", n)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Tx = %v", err)
	}

	// Nothing survived the outer rollback.
	var age int32
	if err := conn.QueryRow(t.Context(), "SELECT age FROM users WHERE id = 1").Scan(&age); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if age != 30 {
		t.Errorf("age = %d after the rollback, want 30", age)
	}
}
