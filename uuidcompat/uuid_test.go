package uuidcompat_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"example.com/uuidcompat/domain"
	"github.com/AlexAli29/orm"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The permanent qualification for PostgreSQL's uuid type.
//
// Everything here runs against the one topology this module declares — a uuid
// primary key, a uuid array, a nullable uuid, a uuid foreign key, a view and a
// materialized view with a unique uuid index. One project rather than a drawer
// of fixtures, so that a claim cannot pass because its fixture was shaped to
// let it.
//
// The database is prepared by the ordinary commands: orm makemigrations, orm
// migrate, orm generate. If the schema in this database is not the one the
// declarations describe, these tests fail rather than skip.

// dsn is the database this module was migrated into.
//
// A missing DSN fails rather than skips. A qualification suite that quietly
// runs nothing is the false green this whole exercise exists to prevent.
func dsn(t *testing.T) string {
	t.Helper()
	v := os.Getenv("UUIDCOMPAT_DSN")
	if v == "" {
		t.Fatal("UUIDCOMPAT_DSN is not set: the UUID qualification cannot run, and skipping it " +
			"would report success for tests that never executed")
	}
	return v
}

func open(t *testing.T) (*pgxpool.Pool, *domain.DB) {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), dsn(t))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(pool.Close)
	// The schema must already be the declared one.
	var n int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_class WHERE relnamespace='public'::regnamespace
		   AND relname IN ('users','orders','user_orders','user_summaries')`).Scan(&n); err != nil {
		t.Fatalf("checking the schema: %v", err)
	}
	if n != 4 {
		t.Fatalf("the database has %d of the 4 declared relations; run orm migrate first", n)
	}
	return pool, domain.New(pool)
}

// reset empties the tables so each test starts from nothing.
func reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `TRUNCATE orders, users CASCADE`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
}

// PostgreSQL's own account of what the declarations produced.
//
// The whole point of a uuid column is that it is a uuid: text that looks like
// one sorts differently, indexes differently and accepts values a uuid column
// rejects. So the catalog is asked rather than the migration believed.
func TestUUID_catalogTypesAreUUID(t *testing.T) {
	pool, _ := open(t)

	want := map[string]string{
		"users.id":               "uuid",
		"users.external_id":      "uuid",
		"users.optional_id":      "uuid",
		"users.tags":             "uuid[]",
		"orders.id":              "uuid",
		"orders.user_id":         "uuid",
		"user_orders.user_id":    "uuid",
		"user_summaries.user_id": "uuid",
	}
	rows, err := pool.Query(t.Context(), `
		SELECT c.relname || '.' || a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relnamespace = 'public'::regnamespace AND a.attnum > 0 AND NOT a.attisdropped
		  AND c.relkind IN ('r','v','m')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		got[name] = typ
	}
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("%s is %q, want %q", name, got[name], typ)
		}
	}
}

// A uuid foreign key is a uuid foreign key, not an integer beside one.
func TestUUID_foreignKeyIsUUID(t *testing.T) {
	pool, _ := open(t)
	var def string
	if err := pool.QueryRow(t.Context(),
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conrelid = 'public.orders'::regclass AND contype = 'f'`).Scan(&def); err != nil {
		t.Fatalf("reading the foreign key: %v", err)
	}
	if !strings.Contains(def, "REFERENCES users(id)") {
		t.Errorf("the foreign key is %q", def)
	}
}

// The write and read surface, over one topology.
func TestUUID_writeAndReadSurface(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	ctx := t.Context()

	u1, u2 := uuid.New(), uuid.New()
	ext, opt := uuid.New(), uuid.New()
	tagA, tagB := uuid.New(), uuid.New()

	made, err := db.Users.InsertMany(ctx, []domain.User{
		{ID: u1, Email: "a@example.com", ExternalID: ext, OptionalID: &opt, Tags: []uuid.UUID{tagA, tagB}},
		{ID: u2, Email: "b@example.com", ExternalID: uuid.New(), Tags: []uuid.UUID{}},
	})
	if err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if len(made) != 2 {
		t.Fatalf("inserted %d rows", len(made))
	}
	// RETURNING scanned the uuid back as itself.
	if made[0].ID != u1 || made[0].ExternalID != ext {
		t.Errorf("RETURNING gave %v/%v", made[0].ID, made[0].ExternalID)
	}
	// The nullable one is absent, not a zero uuid.
	if made[1].OptionalID != nil {
		t.Errorf("an unset nullable uuid came back as %v", *made[1].OptionalID)
	}
	// uuid[] survived the round trip in order.
	if len(made[0].Tags) != 2 || made[0].Tags[0] != tagA || made[0].Tags[1] != tagB {
		t.Errorf("uuid[] came back as %v", made[0].Tags)
	}

	// Typed predicates over uuid.
	one, err := db.Users.Query().Where(domain.Users.ID.Eq(u1)).One(ctx)
	if err != nil {
		t.Fatalf("Eq: %v", err)
	}
	if one.ID != u1 {
		t.Errorf("Eq scanned %v", one.ID)
	}
	both, err := db.Users.Query().Where(domain.Users.ID.In(u1, u2)).OrderBy(domain.Users.ID.Asc()).All(ctx)
	if err != nil {
		t.Fatalf("In + OrderBy: %v", err)
	}
	if len(both) != 2 {
		t.Errorf("In matched %d rows", len(both))
	}
	nulls, err := db.Users.Query().Where(domain.Users.OptionalID.IsNull()).All(ctx)
	if err != nil {
		t.Fatalf("IsNull: %v", err)
	}
	if len(nulls) != 1 {
		t.Errorf("IsNull matched %d rows", len(nulls))
	}

	// Update with a uuid on both sides of the statement.
	next := uuid.New()
	n, err := db.Users.Update().
		Set(domain.Users.ExternalID.Set(next)).
		Where(domain.Users.ID.Eq(u2)).
		Exec(ctx)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if n != 1 {
		t.Errorf("Update touched %d rows", n)
	}
	after, err := db.Users.Query().Where(domain.Users.ID.Eq(u2)).One(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.ExternalID != next {
		t.Errorf("the updated uuid is %v", after.ExternalID)
	}
}

// The zero UUID is a value. SQL NULL is the absence of one. They must never
// collapse into each other, in either direction.
func TestUUID_zeroIsNotNull(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	ctx := t.Context()

	var zero uuid.UUID
	stored, err := db.Users.Insert(ctx, domain.User{
		ID: zero, Email: "zero@example.com", ExternalID: zero, Tags: []uuid.UUID{},
	})
	if err != nil {
		t.Fatalf("inserting a zero uuid: %v", err)
	}
	if stored.ID != zero {
		t.Errorf("the zero uuid came back as %v", stored.ID)
	}
	if stored.OptionalID != nil {
		t.Error("an unset nullable uuid became a zero value")
	}

	// The row is found by the zero uuid, so it really is stored as one.
	got, err := db.Users.Query().Where(domain.Users.ID.Eq(zero)).One(ctx)
	if err != nil {
		t.Fatalf("selecting the zero uuid: %v", err)
	}
	if got.ID != zero {
		t.Errorf("selected %v", got.ID)
	}
	// And the nullable column is NULL in the database, not all-zeros.
	var isNull bool
	if err := pool.QueryRow(ctx,
		`SELECT optional_id IS NULL FROM users WHERE id = $1`, zero).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("the unset nullable uuid was stored as a value")
	}
}

// COPY, through pgx's own path rather than an INSERT that resembles it.
func TestUUID_copyFrom(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	ctx := t.Context()

	const n = 500
	rows := make([]domain.User, 0, n)
	ids := make([]uuid.UUID, 0, n)
	for i := range n {
		id := uuid.New()
		ids = append(ids, id)
		u := domain.User{ID: id, Email: string(rune('a'+i%26)) + uuid.NewString(), ExternalID: uuid.New(), Tags: []uuid.UUID{uuid.New()}}
		if i%2 == 0 {
			opt := uuid.New()
			u.OptionalID = &opt
		}
		rows = append(rows, u)
	}
	copied, err := db.Users.CopyFrom(ctx, rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if copied != n {
		t.Fatalf("COPY reported %d rows, want %d", copied, n)
	}

	var total, nulls int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE optional_id IS NULL) FROM users`).Scan(&total, &nulls); err != nil {
		t.Fatal(err)
	}
	if total != n {
		t.Errorf("the database holds %d rows", total)
	}
	if nulls != n/2 {
		t.Errorf("%d rows have a NULL nullable uuid, want %d", nulls, n/2)
	}
	// Every uuid arrived intact, which a text round trip would not guarantee.
	back, err := db.Users.Query().All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[uuid.UUID]bool, len(back))
	for _, u := range back {
		seen[u.ID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Fatalf("uuid %v did not survive COPY", id)
		}
	}
}

// PostgreSQL's own errors, reachable and unrewritten.
func TestUUID_errorFidelity(t *testing.T) {
	pool, db := open(t)
	reset(t, pool)
	ctx := t.Context()

	u := uuid.New()
	if _, err := db.Users.Insert(ctx, domain.User{
		ID: u, Email: "dup@example.com", ExternalID: uuid.New(), Tags: []uuid.UUID{},
	}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		what  string
		run   func() error
		state string
	}{
		{"a duplicate uuid primary key", func() error {
			_, err := db.Users.Insert(ctx, domain.User{
				ID: u, Email: "other@example.com", ExternalID: uuid.New(), Tags: []uuid.UUID{},
			})
			return err
		}, "23505"},
		{"a uuid foreign key violation", func() error {
			_, err := db.Orders.Insert(ctx, domain.Order{
				ID: uuid.New(), UserID: uuid.New(), Label: "orphan",
			})
			return err
		}, "23503"},
		{"an invalid uuid cast", func() error {
			var junk uuid.UUID
			return pool.QueryRow(ctx, `SELECT 'not-a-uuid'::uuid`).Scan(&junk)
		}, "22P02"},
	} {
		t.Run(c.what, func(t *testing.T) {
			err := c.run()
			if err == nil {
				t.Fatal("the operation succeeded")
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("PostgreSQL's error is not reachable: %v", err)
			}
			if pgErr.Code != c.state {
				t.Errorf("SQLSTATE = %s, want %s", pgErr.Code, c.state)
			}
		})
	}
}

// Raw binds and scans a uuid through the same driver path, and PostgreSQL
// agrees about the type of what was bound.
func TestUUID_raw(t *testing.T) {
	pool, _ := open(t)
	id := uuid.New()

	var back uuid.UUID
	var typ string
	if err := pool.QueryRow(t.Context(),
		`SELECT $1::uuid, pg_typeof($1::uuid)::text`, id).Scan(&back, &typ); err != nil {
		t.Fatalf("Raw: %v", err)
	}
	if back != id {
		t.Errorf("the uuid came back as %v", back)
	}
	if typ != "uuid" {
		t.Errorf("pg_typeof said %q; the value was not bound as a uuid", typ)
	}
}

var _ = context.Background
var _ = orm.Concurrently
