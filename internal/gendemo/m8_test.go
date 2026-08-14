package gendemo_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Transactions, the raw escape hatch, and type registration, against real
// PostgreSQL. These are the parts of the library whose whole value is in what
// happens when something goes wrong, so most of what is tested here is failure.

const txSeed = `
INSERT INTO users (id, email, age, active, state, tags, settings) VALUES
  (1, 'a@example.com', 40, true, 'active', '{}', '{}'),
  (2, 'b@example.com', 30, true, 'active', '{}', '{}');
SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users));
`

func txDB(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	db, _, conn := tracedDB(t, txSeed)
	return db, conn
}

func countUsers(t *testing.T, db *gendemo.DB) int64 {
	t.Helper()
	n, err := db.Users.Query().Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return n
}

// txUser is a complete entity, so an insert of it needs a default only for the
// timestamp the database owns.
func txUser(email string) gendemo.User {
	return gendemo.User{
		Email:    email,
		Age:      20,
		State:    gendemo.UserStateActive,
		Tags:     []string{},
		Settings: map[string]any{},
	}
}

func TestTx_commits(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		u, err := tx.Users.Insert(t.Context(), txUser("tx@example.com"),
			orm.Default(gendemo.Users.CreatedAt))
		if err != nil {
			return err
		}
		_, err = tx.Profiles.Insert(t.Context(), gendemo.Profile{UserID: u.ID})
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if got := countUsers(t, db); got != 3 {
		t.Errorf("users = %d, want the committed row to be there", got)
	}
}

// The callback's error comes back unchanged, so a caller's own sentinel still
// matches through it, and nothing it wrote survives.
func TestTx_rollsBackOnError(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	sentinel := errors.New("the caller's own error")
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		if _, err := tx.Users.Insert(t.Context(), txUser("doomed@example.com"),
			orm.Default(gendemo.Users.CreatedAt)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want the callback's own error", err)
	}
	if got := countUsers(t, db); got != 2 {
		t.Errorf("users = %d, want the transaction rolled back", got)
	}
}

// A panic is a bug. It rolls the transaction back and then continues, because
// turning it into an error would hide where it came from.
func TestTx_panicRollsBackAndRepanics(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	func() {
		defer func() {
			p := recover()
			if p == nil {
				t.Fatal("the panic did not reach the caller")
			}
			if s, _ := p.(string); s != "boom" {
				t.Errorf("recovered %v, want the original panic value", p)
			}
		}()
		_ = db.Tx(t.Context(), func(tx *gendemo.DB) error {
			if _, err := tx.Users.Insert(t.Context(), txUser("panic@example.com"),
				orm.Default(gendemo.Users.CreatedAt)); err != nil {
				t.Fatalf("Insert: %v", err)
			}
			panic("boom")
		})
	}()

	if got := countUsers(t, db); got != 2 {
		t.Errorf("users = %d, want the transaction rolled back before the panic resumed", got)
	}
	// The connection is usable afterwards, which is what makes the rollback
	// worth doing rather than leaving to the pool.
	if _, err := db.Users.Query().All(t.Context()); err != nil {
		t.Errorf("the connection is unusable after a panicking transaction: %v", err)
	}
}

// A nested transaction is a savepoint. Rolling one back leaves the outer
// transaction alive and able to continue.
func TestTx_nestedSavepoint(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	inner := errors.New("the nested callback failed")
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		if _, err := tx.Users.Insert(t.Context(), txUser("outer@example.com"),
			orm.Default(gendemo.Users.CreatedAt)); err != nil {
			return err
		}
		// This one commits.
		if err := tx.Tx(t.Context(), func(nested *gendemo.DB) error {
			_, err := nested.Users.Insert(t.Context(), txUser("nested-kept@example.com"),
				orm.Default(gendemo.Users.CreatedAt))
			return err
		}); err != nil {
			return err
		}
		// This one does not, and the outer transaction carries on.
		if err := tx.Tx(t.Context(), func(nested *gendemo.DB) error {
			if _, err := nested.Users.Insert(t.Context(), txUser("nested-lost@example.com"),
				orm.Default(gendemo.Users.CreatedAt)); err != nil {
				return err
			}
			return inner
		}); !errors.Is(err, inner) {
			return err
		}
		_, err := tx.Users.Insert(t.Context(), txUser("after@example.com"),
			orm.Default(gendemo.Users.CreatedAt))
		return err
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}

	emails, err := db.Users.Query().OrderBy(gendemo.Users.Email.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	var got []string
	for _, u := range emails {
		got = append(got, u.Email)
	}
	want := []string{"a@example.com", "after@example.com", "b@example.com", "nested-kept@example.com", "outer@example.com"}
	if !equal(got, want) {
		t.Errorf("emails = %v, want %v; the failed savepoint took only its own row", got, want)
	}
}

// A savepoint cannot have its own isolation level, so asking for one is refused
// rather than reported as done.
func TestTx_nestedOptions(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		return tx.TxOptions(t.Context(), pgx.TxOptions{IsoLevel: pgx.Serializable}, func(*gendemo.DB) error {
			t.Error("the nested callback ran under options that cannot be applied")
			return nil
		})
	})
	if !errors.Is(err, orm.ErrNestedTxOptions) {
		t.Errorf("error = %v, want ErrNestedTxOptions", err)
	}

	// Zero options ask for nothing in particular, so they nest like Tx.
	err = db.Tx(t.Context(), func(tx *gendemo.DB) error {
		return tx.TxOptions(t.Context(), pgx.TxOptions{}, func(nested *gendemo.DB) error {
			_, err := nested.Users.Query().Count(t.Context())
			return err
		})
	})
	if err != nil {
		t.Errorf("zero nested options: %v", err)
	}
}

// Options at the top are pgx's own, not SQL this package writes.
func TestTx_options(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	opts := pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}

	err := db.TxOptions(t.Context(), opts, func(tx *gendemo.DB) error {
		n, err := tx.Users.Query().Count(t.Context())
		if err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("count inside the transaction = %d, want 2", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("TxOptions: %v", err)
	}

	// Read-only means read-only, and PostgreSQL is the one enforcing it: the
	// options reached the server rather than being emulated here. The write
	// aborts the transaction, so it gets one of its own.
	var writeErr error
	err = db.TxOptions(t.Context(), opts, func(tx *gendemo.DB) error {
		_, writeErr = tx.Users.Insert(t.Context(), txUser("readonly@example.com"),
			orm.Default(gendemo.Users.CreatedAt))
		return writeErr
	})
	if err == nil {
		t.Fatal("a write succeeded in a read-only transaction")
	}
	var pgErr *pgconn.PgError
	if !errors.As(writeErr, &pgErr) {
		t.Errorf("error = %v, want PostgreSQL's own to survive the wrapping", writeErr)
	}
	if pgErr != nil && pgErr.Code != "25006" {
		t.Errorf("SQLSTATE = %s, want 25006 (read-only transaction)", pgErr.Code)
	}
}

// The receiver's repositories keep their own executor, so a transaction is
// something a caller opts into rather than something that happens to them.
func TestTx_doesNotRebindTheOriginal(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	before := db.Executor()
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		if tx.Executor() == before {
			t.Error("the callback got the same executor the pool uses")
		}
		if _, ok := tx.Executor().(pgx.Tx); !ok {
			t.Errorf("the callback's executor is %T, want the transaction", tx.Executor())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if db.Executor() != before {
		t.Error("the receiver's executor changed")
	}
}

// An executor that cannot begin a transaction says so rather than running the
// callback outside one.
func TestTx_executorCannotBegin(t *testing.T) {
	db := gendemo.New(nil)
	err := db.Tx(context.Background(), func(*gendemo.DB) error {
		t.Error("the callback ran without a transaction")
		return nil
	})
	if !errors.Is(err, orm.ErrNoTransaction) {
		t.Errorf("error = %v, want ErrNoTransaction", err)
	}
}

// A unique violation has to arrive as PostgreSQL's own error, through every
// layer of context this package adds, or a caller cannot tell it from anything
// else that went wrong.
func TestTx_postgresErrorsSurvive(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	assertUnique := func(err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("error = %v, want a *pgconn.PgError", err)
		}
		if pgErr.Code != "23505" {
			t.Errorf("SQLSTATE = %s, want 23505", pgErr.Code)
		}
		if !strings.Contains(err.Error(), "users") {
			t.Errorf("error = %v, want it to name what failed", err)
		}
	}

	_, err := db.Users.Insert(t.Context(), txUser("a@example.com"),
		orm.Default(gendemo.Users.CreatedAt))
	assertUnique(err)

	err = db.Tx(t.Context(), func(tx *gendemo.DB) error {
		_, err := tx.Users.Insert(t.Context(), txUser("a@example.com"),
			orm.Default(gendemo.Users.CreatedAt))
		return err
	})
	assertUnique(err)

	// The connection is still usable after the rollback.
	if got := countUsers(t, db); got != 2 {
		t.Errorf("users = %d, want the failed transaction to have left nothing", got)
	}
}

func TestRaw_all(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	users, err := orm.Raw(db.Users, `
		SELECT id, email, age, active, state, nickname, score, tags, settings,
		       avatar, manager_id, bio, visits, deleted_at, created_at, slug, metadata
		FROM users
		WHERE lower(email) LIKE lower($1)
		ORDER BY id
	`, "%EXAMPLE.COM").All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1, 2}) {
		t.Errorf("returned %v, want both users", got)
	}
	if users[0].Email != "a@example.com" || users[0].Age != 40 {
		t.Errorf("scanned %+v, want the generated destinations to have been used", users[0])
	}
}

func TestRaw_one(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	const stmt = `SELECT id, email, age, active, state, nickname, score, tags, settings,
	       avatar, manager_id, bio, visits, deleted_at, created_at, slug, metadata
	FROM users WHERE email = $1`

	u, err := orm.Raw(db.Users, stmt, "a@example.com").One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if u.ID != 1 {
		t.Errorf("One returned user %d, want 1", u.ID)
	}
	if _, err := orm.Raw(db.Users, stmt, "nobody@example.com").One(t.Context()); !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if _, err := orm.Raw(db.Users, `SELECT id, email, age, active, state, nickname, score, tags, settings,
	       avatar, manager_id, bio, visits, deleted_at, created_at, slug, metadata FROM users`).One(t.Context()); !errors.Is(err, orm.ErrMultipleRows) {
		t.Errorf("error = %v, want ErrMultipleRows", err)
	}
}

// Raw scans by position, so a statement selecting a different shape is reported
// before any row is read rather than as a scan failure naming a column the
// caller never wrote.
func TestRaw_columnMismatch(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	_, err := orm.Raw(db.Users, `SELECT id, email FROM users`).All(t.Context())
	if !errors.Is(err, orm.ErrRawColumns) {
		t.Fatalf("error = %v, want ErrRawColumns", err)
	}
	if !strings.Contains(err.Error(), "2 columns") {
		t.Errorf("error = %v, want it to say what the statement returned", err)
	}
}

// The placeholders are PostgreSQL's own and are passed through untouched, since
// Raw owns the whole statement and has nothing to renumber into.
func TestRaw_placeholdersArePassedThrough(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	const stmt = `SELECT id, email, age, active, state, nickname, score, tags, settings,
	       avatar, manager_id, bio, visits, deleted_at, created_at, slug, metadata
	FROM users WHERE age >= $2 AND email LIKE $1 ORDER BY id`

	sql, args, err := orm.Raw(db.Users, stmt, "%example.com", int32(35)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if sql != stmt {
		t.Error("the statement was rewritten")
	}
	if len(args) != 2 {
		t.Fatalf("args = %v", args)
	}
	users, err := orm.Raw(db.Users, stmt, "%example.com", int32(35)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1}) {
		t.Errorf("returned %v, want the one user over 35", got)
	}
}

// A raw query built from a repository inside a transaction runs inside that
// transaction, because the repository is what decides the executor.
func TestRaw_insideATransaction(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	const stmt = `SELECT id, email, age, active, state, nickname, score, tags, settings,
	       avatar, manager_id, bio, visits, deleted_at, created_at, slug, metadata
	FROM users ORDER BY id`

	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		if _, err := tx.Users.Insert(t.Context(), txUser("raw-tx@example.com"),
			orm.Default(gendemo.Users.CreatedAt)); err != nil {
			return err
		}
		users, err := orm.Raw(tx.Users, stmt).All(t.Context())
		if err != nil {
			return err
		}
		// The uncommitted row is visible, which is only true if the raw query
		// ran on the transaction.
		if len(users) != 3 {
			t.Errorf("the raw query saw %d users, want the transaction's own view", len(users))
		}
		return errors.New("roll it back")
	})
	if err == nil {
		t.Fatal("Tx succeeded")
	}
	if got := countUsers(t, db); got != 2 {
		t.Errorf("users = %d, want the rollback to have taken the row", got)
	}
}

// Stopping a stream early has to release the result set, or the connection is
// held until something else notices.
func TestRaw_rowsEarlyStop(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	const stmt = `SELECT id, email, age, active, state, nickname, score, tags, settings,
	       avatar, manager_id, bio, visits, deleted_at, created_at, slug, metadata
	FROM users ORDER BY id`

	seen := 0
	for u, err := range orm.Raw(db.Users, stmt).Rows(t.Context()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		seen++
		if u.ID == 1 {
			break
		}
	}
	if seen != 1 {
		t.Errorf("read %d rows, want the loop to have stopped at the first", seen)
	}
	// The connection works again immediately, which it would not if the rows
	// were still open.
	if got := countUsers(t, db); got != 2 {
		t.Errorf("the connection is unusable after an early stop: count = %d", got)
	}
}

// The same claim for the typed query's stream.
func TestRows_earlyStopReleasesTheConnection(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := txDB(t)

	seen := 0
	for _, err := range db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Rows(t.Context()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		seen++
		break
	}
	if seen != 1 {
		t.Errorf("read %d rows, want one", seen)
	}
	if got := countUsers(t, db); got != 2 {
		t.Errorf("the connection is unusable after an early stop: count = %d", got)
	}
}

// The registration hook is what a pool is configured with, so the test
// configures one the same way an application would.
func TestRegisterTypes_throughAPool(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+txSeed)

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
	users, err := db.Users.Query().Where(gendemo.Users.State.Eq(gendemo.UserStateActive)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 2 {
		t.Errorf("returned %d users, want both", len(users))
	}
	// The enum registered above is what makes this comparison the enum's own
	// rather than a text one.
	if users[0].State != gendemo.UserStateActive {
		t.Errorf("state = %q, want the enum value", users[0].State)
	}
}
