package gendemo_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// M16: the error contract, frozen.
//
// Two properties have to survive every wrapper this ORM adds, on every path
// that can fail, because they are the only machine-readable things a caller has:
//
//	errors.As(err, &pgErr)                  what PostgreSQL refused, and why
//	errors.Is(err, context.Canceled)        who ended the operation
//
// Error *strings* are explicitly not API — docs/compatibility.md says so — which
// is exactly why these two must hold. A caller who cannot match on the type has
// nothing left but the text.
//
// Every case below goes through a different code path, because a wrapper that
// forgets %w is forgotten in one place at a time.

// Release-critical: a PostgreSQL failure stays reachable as *pgconn.PgError.
func TestErrors_pgErrorSurvivesEveryPath(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)
	ctx := t.Context()
	advanceIdentity(t, db)

	// A row that will collide, so the unique index has something to refuse.
	seed, err := db.Users.Insert(ctx, gendemo.User{
		ID: 9001, Email: "m16-contract@example.com", Nickname: ptr("m16"),
		State: gendemo.UserStateActive, Tags: []string{"m16"}, Settings: map[string]any{},
	})
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for _, c := range []struct {
		what      string
		run       func() error
		wantState string
	}{
		{"an insert that violates a unique index", func() error {
			_, err := db.Users.Insert(ctx, gendemo.User{
				ID: 9002, Email: "m16-contract@example.com", State: gendemo.UserStateActive,
				Tags: []string{}, Settings: map[string]any{},
			})
			return err
		}, "23505"},
		{"an update that violates a unique index", func() error {
			second, err := db.Users.Insert(ctx, gendemo.User{
				ID: 9003, Email: "m16-other@example.com", State: gendemo.UserStateActive,
				Tags: []string{}, Settings: map[string]any{},
			})
			if err != nil {
				return err
			}
			_, err = db.Users.Update().
				Set(gendemo.Users.Email.Set("m16-contract@example.com")).
				Where(gendemo.Users.ID.Eq(second.ID)).
				Exec(ctx)
			return err
		}, "23505"},
		{"a delete that a foreign key refuses", func() error {
			if _, err := db.Posts.Insert(ctx, gendemo.Post{
				ID: 9001, AuthorID: &seed.ID, Title: "held",
			}); err != nil {
				return err
			}
			_, err := db.Users.Delete().Where(gendemo.Users.ID.Eq(seed.ID)).Exec(ctx)
			return err
		}, "23503"},
		{"a raw statement the server rejects", func() error {
			_, err := orm.Raw(db.Users, `SELECT * FROM no_such_table_m16`).All(ctx)
			return err
		}, "42P01"},
		{"a query against a column that is not there", func() error {
			_, err := orm.Raw(db.Users, `SELECT no_such_column FROM users`).All(ctx)
			return err
		}, "42703"},
	} {
		t.Run(c.what, func(t *testing.T) {
			err := c.run()
			if err == nil {
				t.Fatal("the operation succeeded; this test proves nothing")
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Fatalf("errors.As did not reach *pgconn.PgError, so a caller has only the text: %v", err)
			}
			if pgErr.Code != c.wantState {
				t.Errorf("SQLSTATE = %s, want %s", pgErr.Code, c.wantState)
			}
		})
	}
}

// Release-critical: a cancelled context stays reachable through the wrappers.
//
// A caller distinguishes "the client went away" from "the database is broken"
// with errors.Is and nothing else. Getting this wrong turns every disconnect
// into a logged outage.
func TestErrors_contextCancellationSurvivesEveryPath(t *testing.T) {
	testdb.AdminDSN(t)

	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		return ctx
	}

	for _, c := range []struct {
		what string
		run  func(context.Context, *gendemo.DB) error
	}{
		{"a query", func(ctx context.Context, db *gendemo.DB) error {
			_, err := db.Users.Query().All(ctx)
			return err
		}},
		{"a single-row read", func(ctx context.Context, db *gendemo.DB) error {
			_, err := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).One(ctx)
			return err
		}},
		{"an insert", func(ctx context.Context, db *gendemo.DB) error {
			_, err := db.Users.Insert(ctx, gendemo.User{
				ID: 9100, Email: "cancelled@example.com", State: gendemo.UserStateActive,
				Tags: []string{}, Settings: map[string]any{},
			})
			return err
		}},
		{"an update", func(ctx context.Context, db *gendemo.DB) error {
			_, err := db.Users.Update().
				Set(gendemo.Users.Nickname.Set("x")).
				Where(gendemo.Users.ID.Eq(1)).Exec(ctx)
			return err
		}},
		{"a delete", func(ctx context.Context, db *gendemo.DB) error {
			_, err := db.Users.Delete().Where(gendemo.Users.ID.Eq(1)).Exec(ctx)
			return err
		}},
		{"a raw statement", func(ctx context.Context, db *gendemo.DB) error {
			_, err := orm.Raw(db.Users, `SELECT * FROM users`).All(ctx)
			return err
		}},
		{"a transaction", func(ctx context.Context, db *gendemo.DB) error {
			return orm.RunTx(ctx, db.Executor(), func(ex orm.Executor) error {
				_, err := gendemo.New(ex).Users.Query().All(ctx)
				return err
			})
		}},
		{"an explain", func(ctx context.Context, db *gendemo.DB) error {
			_, err := db.Users.Query().Explain(ctx)
			return err
		}},
	} {
		t.Run(c.what, func(t *testing.T) {
			// A cancelled query leaves the connection closed, so each case
			// gets its own rather than inheriting the previous one's corpse.
			err := c.run(cancelled(), db(t))
			if err == nil {
				t.Fatal("the operation succeeded with a cancelled context")
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("errors.Is did not reach context.Canceled: %v", err)
			}
		})
	}

	// A deadline that has already passed is the same contract with a different
	// sentinel, and callers branch on them separately: one is a client going
	// away, the other is this service being too slow.
	t.Run("an expired deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
		defer cancel()
		time.Sleep(time.Millisecond)
		_, err := db(t).Users.Query().All(ctx)
		if err == nil {
			t.Fatal("the query succeeded with an expired deadline")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is did not reach context.DeadlineExceeded: %v", err)
		}
	})
}

// The sentinels the ORM defines are reachable and mean what they say.
func TestErrors_sentinelsAreReachable(t *testing.T) {
	testdb.AdminDSN(t)
	db := db(t)
	ctx := t.Context()
	advanceIdentity(t, db)

	if _, err := db.Users.Query().Where(gendemo.Users.ID.Eq(-1)).One(ctx); !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("a One that matched nothing gave %v, want orm.ErrNotFound", err)
	}

	for i := range 2 {
		if _, err := db.Users.Insert(ctx, gendemo.User{
			ID: int64(9200 + i), Email: "sentinel@example.com",
			State: gendemo.UserStateActive, Tags: []string{}, Settings: map[string]any{},
		}); err != nil {
			// The second insert collides, which is not what this case is about.
			break
		}
	}
	if _, err := db.Users.Query().One(ctx); !errors.Is(err, orm.ErrMultipleRows) {
		t.Errorf("a One that matched several rows gave %v, want orm.ErrMultipleRows", err)
	}
}

// advanceIdentity moves the users and posts identity sequences past the seeded
// rows.
//
// The fixture inserts explicit ids, which does not advance the sequence, so the
// next generated id collides with row one. This is a property of the fixture,
// not of the ORM.
func advanceIdentity(t *testing.T, db *gendemo.DB) {
	t.Helper()
	for _, table := range []string{"users", "posts"} {
		rows, err := db.Executor().Query(t.Context(),
			`SELECT setval(pg_get_serial_sequence($1, 'id'), 9000)`, table)
		if err != nil {
			t.Fatalf("advancing the %s sequence: %v", table, err)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("advancing the %s sequence: %v", table, err)
		}
	}
}
