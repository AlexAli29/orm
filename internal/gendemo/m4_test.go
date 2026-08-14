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
)

// The write layer, against real PostgreSQL. These tests start from an empty
// table rather than the read fixtures, because a write test that shares rows
// with its neighbours is a test that fails for someone else's reason.

func emptyDB(t *testing.T) *gendemo.DB {
	t.Helper()
	dsn := testdb.Create(t, schema(t))
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return gendemo.New(conn)
}

// newUser is a complete entity with no database-supplied values filled in.
func newUser(email string) gendemo.User {
	return gendemo.User{
		Email:    email,
		Age:      30,
		Active:   false,
		State:    gendemo.UserStateActive,
		Tags:     []string{},
		Settings: map[string]any{},
	}
}

// TestInsert_zeroValuesAreValues is the regression test for the whole
// no-zero-value-magic principle. active defaults to true in the schema; the
// entity says false, and false is what must be stored.
func TestInsert_zeroValuesAreValues(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	created, err := d.Users.Insert(t.Context(), newUser("x@example.com"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if created.Active {
		t.Error("Active came back true; a Go zero value was read as an absence and the column took its default")
	}

	// And it is false in the table, not merely in the returned entity.
	got, err := d.Users.Query().Where(gendemo.Users.Email.Eq("x@example.com")).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Active {
		t.Error("the stored row has active = true")
	}
}

// TestInsert_defaultIsTheOtherThing proves false and "let the database decide"
// are different requests over the same column.
func TestInsert_defaultIsTheOtherThing(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	created, err := d.Users.Insert(t.Context(), newUser("y@example.com"), orm.Default(gendemo.Users.Active))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if !created.Active {
		t.Error("Active came back false; Default did not let the column take its default of true")
	}
}

func TestInsert_returnsWhatTheDatabaseComputed(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	input := newUser("z@example.com")
	before := input

	created, err := d.Users.Insert(t.Context(), input, orm.Default(gendemo.Users.CreatedAt))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The identity column was never written and came back filled in.
	if created.ID == 0 {
		t.Error("the generated key did not come back")
	}
	if created.CreatedAt.IsZero() {
		t.Error("the defaulted timestamp did not come back")
	}
	// The generated column is computed by PostgreSQL, never written, and still
	// read back.
	if created.Slug == nil || *created.Slug != "z@example.com" {
		t.Errorf("the generated column came back as %v", created.Slug)
	}

	// The entity handed in is untouched. Generated values belong to the
	// returned copy alone.
	if input.ID != before.ID || !input.CreatedAt.Equal(before.CreatedAt) || input.Slug != before.Slug {
		t.Errorf("Insert modified the entity it was given: %+v, was %+v", input, before)
	}
	if input.ID != 0 {
		t.Errorf("the input entity acquired the generated key %d", input.ID)
	}
}

func TestInsert_nullIsNotDefault(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	// nickname is nullable with no default, so a nil field writes NULL.
	created, err := d.Users.Insert(t.Context(), newUser("n@example.com"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if created.Nickname != nil {
		t.Errorf("Nickname = %v, want NULL", created.Nickname)
	}

	// A value writes the value.
	u := newUser("m@example.com")
	nick := "alex"
	u.Nickname = &nick
	created, err = d.Users.Insert(t.Context(), u)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if created.Nickname == nil || *created.Nickname != "alex" {
		t.Errorf("Nickname = %v, want alex", created.Nickname)
	}
}

func TestInsert_generatedColumnIsNeverWritten(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	// Setting the field makes no difference: the column is not in the write
	// list at all, so PostgreSQL computes it as it always would.
	u := newUser("g@example.com")
	lie := "not-the-slug"
	u.Slug = &lie

	created, err := d.Users.Insert(t.Context(), u)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if created.Slug == nil || *created.Slug != "g@example.com" {
		t.Errorf("Slug = %v, want the computed value", created.Slug)
	}

	sql, _, err := d.Users.InsertSQL([]gendemo.User{u})
	if err != nil {
		t.Fatalf("InsertSQL: %v", err)
	}
	insertList, _, _ := strings.Cut(sql, " RETURNING ")
	if strings.Contains(insertList, `"slug"`) {
		t.Errorf("the generated column reached the write list: %s", insertList)
	}
	if !strings.Contains(sql, `RETURNING "id"`) || !strings.Contains(sql, `"slug"`) {
		t.Errorf("the generated column is missing from RETURNING: %s", sql)
	}
	if strings.Contains(insertList, `"id"`) {
		t.Errorf("the identity column reached the write list: %s", insertList)
	}
}

func TestInsertMany_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	want := make([]gendemo.User, 25)
	for i := range want {
		want[i] = newUser(fmt.Sprintf("u%02d@example.com", i))
		want[i].Age = int32(i)
	}

	created, err := d.Users.InsertMany(t.Context(), want, orm.Default(gendemo.Users.CreatedAt))
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if len(created) != len(want) {
		t.Fatalf("inserted %d rows, want %d", len(created), len(want))
	}
	// RETURNING follows the VALUES order, which is the order given.
	for i, c := range created {
		if c.Email != want[i].Email || c.Age != want[i].Age {
			t.Fatalf("row %d came back as %s (age %d), want %s (age %d)", i, c.Email, c.Age, want[i].Email, want[i].Age)
		}
		if c.ID == 0 {
			t.Errorf("row %d has no generated key", i)
		}
	}
	// The keys are distinct.
	seen := make(map[int64]bool, len(created))
	for _, c := range created {
		if seen[c.ID] {
			t.Errorf("key %d was handed out twice", c.ID)
		}
		seen[c.ID] = true
	}

	n, err := d.Users.Query().Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != int64(len(want)) {
		t.Errorf("the table holds %d rows, want %d", n, len(want))
	}
}

func TestInsertMany_empty(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	for _, in := range [][]gendemo.User{nil, {}} {
		out, err := d.Users.InsertMany(t.Context(), in)
		if err != nil {
			t.Fatalf("InsertMany: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("returned %d rows", len(out))
		}
	}
	if n, _ := d.Users.Query().Count(t.Context()); n != 0 {
		t.Errorf("an empty insert wrote %d rows", n)
	}
}

func TestUpdate_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	created, err := d.Users.InsertMany(t.Context(), []gendemo.User{
		newUser("a@example.com"), newUser("b@example.com"), newUser("c@example.com"),
	})
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}

	when := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	affected, err := d.Users.Update().
		Set(
			gendemo.Users.Nickname.Set("alexander"),
			gendemo.Users.Active.Set(true),
			gendemo.Users.DeletedAt.Set(when),
		).
		Where(gendemo.Users.ID.Eq(created[0].ID)).
		Exec(t.Context())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if affected != 1 {
		t.Errorf("updated %d rows, want 1", affected)
	}

	got, err := d.Users.Query().Where(gendemo.Users.ID.Eq(created[0].ID)).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Nickname == nil || *got.Nickname != "alexander" || !got.Active {
		t.Errorf("the row is %+v", got)
	}
	if got.DeletedAt == nil || !got.DeletedAt.Equal(when) {
		t.Errorf("DeletedAt = %v", got.DeletedAt)
	}

	// The others are untouched.
	n, err := d.Users.Query().Where(gendemo.Users.Active.Eq(false)).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("%d rows are still inactive, want 2", n)
	}
}

func TestUpdate_setNull(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	u := newUser("a@example.com")
	nick := "alex"
	u.Nickname = &nick
	created, err := d.Users.Insert(t.Context(), u)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	affected, err := d.Users.Update().
		Set(gendemo.Users.Nickname.SetNull()).
		Where(gendemo.Users.ID.Eq(created.ID)).
		Exec(t.Context())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if affected != 1 {
		t.Errorf("updated %d rows", affected)
	}

	got, err := d.Users.Query().Where(gendemo.Users.ID.Eq(created.ID)).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got.Nickname != nil {
		t.Errorf("Nickname = %v, want NULL", got.Nickname)
	}
}

func TestUpdate_matchingNothing(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	affected, err := d.Users.Update().
		Set(gendemo.Users.Active.Set(true)).
		Where(gendemo.Users.ID.Eq(9999)).
		Exec(t.Context())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if affected != 0 {
		t.Errorf("updated %d rows, want none", affected)
	}
}

func TestDelete_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	created, err := d.Users.InsertMany(t.Context(), []gendemo.User{
		newUser("a@example.com"), newUser("b@example.com"), newUser("c@example.com"),
	})
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}

	affected, err := d.Users.Delete().Where(gendemo.Users.ID.Eq(created[1].ID)).Exec(t.Context())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if affected != 1 {
		t.Errorf("deleted %d rows, want 1", affected)
	}
	if n, _ := d.Users.Query().Count(t.Context()); n != 2 {
		t.Errorf("%d rows remain, want 2", n)
	}
}

// TestWriteGuards_integration proves the guards stop the statement rather than
// merely reporting after it ran.
func TestWriteGuards_integration(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	if _, err := d.Users.InsertMany(t.Context(), []gendemo.User{
		newUser("a@example.com"), newUser("b@example.com"),
	}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}

	t.Run("an update with no WHERE changes nothing", func(t *testing.T) {
		_, err := d.Users.Update().Set(gendemo.Users.Active.Set(true)).Exec(t.Context())
		if !errors.Is(err, orm.ErrMissingWhere) {
			t.Fatalf("error = %v, want ErrMissingWhere", err)
		}
		n, err := d.Users.Query().Where(gendemo.Users.Active.Eq(true)).Count(t.Context())
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		if n != 0 {
			t.Errorf("%d rows were activated by a refused update", n)
		}
	})

	t.Run("All makes the same update run", func(t *testing.T) {
		affected, err := d.Users.Update().Set(gendemo.Users.Active.Set(true)).All().Exec(t.Context())
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if affected != 2 {
			t.Errorf("updated %d rows, want 2", affected)
		}
		n, _ := d.Users.Query().Where(gendemo.Users.Active.Eq(true)).Count(t.Context())
		if n != 2 {
			t.Errorf("%d rows are active, want 2", n)
		}
	})

	t.Run("a delete with no WHERE removes nothing", func(t *testing.T) {
		_, err := d.Users.Delete().Exec(t.Context())
		if !errors.Is(err, orm.ErrMissingWhere) {
			t.Fatalf("error = %v, want ErrMissingWhere", err)
		}
		if n, _ := d.Users.Query().Count(t.Context()); n != 2 {
			t.Errorf("%d rows remain after a refused delete, want 2", n)
		}
	})

	t.Run("All makes the same delete run", func(t *testing.T) {
		affected, err := d.Users.Delete().All().Exec(t.Context())
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if affected != 2 {
			t.Errorf("deleted %d rows, want 2", affected)
		}
		if n, _ := d.Users.Query().Count(t.Context()); n != 0 {
			t.Errorf("%d rows remain", n)
		}
	})
}

func TestUpsert_doUpdate(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	first, err := d.Users.Insert(t.Context(), newUser("dup@example.com"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	second := newUser("dup@example.com")
	second.Age = 99
	second.Active = true
	nick := "updated"
	second.Nickname = &nick

	got, err := d.Users.Insert(t.Context(), second,
		orm.OnConflict(gendemo.Users.Email).DoUpdate(gendemo.Users.Age, gendemo.Users.Nickname))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The same row, updated in the named columns only.
	if got.ID != first.ID {
		t.Errorf("the upsert made a new row: %d, was %d", got.ID, first.ID)
	}
	if got.Age != 99 {
		t.Errorf("Age = %d, want the new value", got.Age)
	}
	if got.Nickname == nil || *got.Nickname != "updated" {
		t.Errorf("Nickname = %v, want updated", got.Nickname)
	}
	// Active was not named, so it keeps the existing row's value.
	if got.Active {
		t.Error("Active changed, but DoUpdate did not name it")
	}
	if n, _ := d.Users.Query().Count(t.Context()); n != 1 {
		t.Errorf("the table holds %d rows, want 1", n)
	}
}

func TestUpsert_doNothing(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	first, err := d.Users.Insert(t.Context(), newUser("dup@example.com"))
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	second := newUser("dup@example.com")
	second.Age = 99

	got, err := d.Users.Insert(t.Context(), second,
		orm.OnConflict(gendemo.Users.Email).DoNothing())
	// PostgreSQL returns no row for an insert it discarded, so there is no
	// entity to hand back and none is invented.
	if !errors.Is(err, orm.ErrConflictIgnored) {
		t.Fatalf("error = %v, want ErrConflictIgnored", err)
	}
	if got.ID != 0 || got.Email != "" {
		t.Errorf("a discarded insert returned %+v, want the zero entity", got)
	}

	// The existing row is untouched, and no second query went looking for it.
	stored, err := d.Users.Query().Where(gendemo.Users.Email.Eq("dup@example.com")).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if stored.ID != first.ID || stored.Age != first.Age {
		t.Errorf("the existing row changed: %+v", stored)
	}

	// A non-conflicting insert under the same clause still returns its row.
	fresh, err := d.Users.Insert(t.Context(), newUser("new@example.com"),
		orm.OnConflict(gendemo.Users.Email).DoNothing())
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if fresh.ID == 0 {
		t.Error("a successful insert under DoNothing returned no row")
	}
}

func TestUpsert_manyWithDoNothingReturnsWhatLanded(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	if _, err := d.Users.Insert(t.Context(), newUser("a@example.com")); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	out, err := d.Users.InsertMany(t.Context(),
		[]gendemo.User{newUser("a@example.com"), newUser("b@example.com")},
		orm.OnConflict(gendemo.Users.Email).DoNothing())
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	// One conflicted and was discarded, so the result is shorter than the
	// input rather than padded with a row that was never written.
	if len(out) != 1 || out[0].Email != "b@example.com" {
		t.Errorf("InsertMany returned %+v, want only the row that landed", out)
	}
}

func TestWrites_insideATransaction(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
	}

	// The repository writes through whatever executor it holds, so a caller
	// who needs several statements to succeed together supplies a transaction.
	// Nothing here opens one on its own.
	inTx := gendemo.New(tx)
	if _, err := inTx.Users.InsertMany(t.Context(), []gendemo.User{
		newUser("a@example.com"), newUser("b@example.com"),
	}); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if _, err := inTx.Users.Update().
		Set(gendemo.Users.Active.Set(true)).
		All().
		Exec(t.Context()); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatalf("rolling back: %v", err)
	}
	if n, err := gendemo.New(conn).Users.Query().Count(t.Context()); err != nil || n != 0 {
		t.Errorf("%d rows survived the rollback (err %v), want none", n, err)
	}
}

func TestWrites_areInspectable(t *testing.T) {
	testdb.AdminDSN(t)
	d := emptyDB(t)

	sql, args, err := d.Users.Update().
		Set(gendemo.Users.Nickname.Set("alex"), gendemo.Users.Active.Set(true)).
		Where(gendemo.Users.ID.Eq(1)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const wantUpdate = `UPDATE "public"."users" SET "nickname" = $1, "active" = $2 WHERE "users"."id" = $3`
	if sql != wantUpdate {
		t.Errorf("update = %s\nwant %s", sql, wantUpdate)
	}
	if len(args) != 3 || args[0] != "alex" || args[1] != true || args[2] != int64(1) {
		t.Errorf("args = %#v", args)
	}

	sql, args, err = d.Users.Delete().Where(gendemo.Users.ID.Eq(1)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const wantDelete = `DELETE FROM "public"."users" WHERE "users"."id" = $1`
	if sql != wantDelete {
		t.Errorf("delete = %s\nwant %s", sql, wantDelete)
	}
	if len(args) != 1 {
		t.Errorf("args = %#v", args)
	}
}
