package gendemo_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// M12.5: row locking.
//
// The claims are that the four strengths and the two waiting policies are
// PostgreSQL's own, that SKIP LOCKED really does give two workers disjoint
// rows under real concurrency, and that NOWAIT fails immediately with
// PostgreSQL's error rather than with something invented here.

func lockSQL(t *testing.T, q interface {
	SQL() (string, []any, error)
}) string {
	t.Helper()
	sql, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	return sql
}

// Each strength and policy compiles to the clause PostgreSQL names, in the
// position its grammar requires: after ORDER BY, LIMIT and OFFSET.
func TestLock_clauses(t *testing.T) {
	base := func() *orm.Query[gendemo.User] {
		return gendemo.New(nil).Users.Query().
			OrderBy(gendemo.Users.ID.Asc()).
			Limit(10)
	}
	for _, tt := range []struct {
		name string
		q    *orm.Query[gendemo.User]
		want string
	}{
		{"for update", base().Lock(orm.ForUpdateStrong), "FOR UPDATE"},
		{"for no key update", base().Lock(orm.ForNoKeyUpdate), "FOR NO KEY UPDATE"},
		{"for share", base().Lock(orm.ForShare), "FOR SHARE"},
		{"for key share", base().Lock(orm.ForKeyShare), "FOR KEY SHARE"},
		{"nowait", base().Lock(orm.ForUpdateStrong, orm.NoWait), "FOR UPDATE NOWAIT"},
		{"skip locked", base().Lock(orm.ForUpdateStrong, orm.SkipLocked), "FOR UPDATE SKIP LOCKED"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sql := lockSQL(t, tt.q)
			if !strings.HasSuffix(sql, tt.want) {
				t.Errorf("SQL = %s\nwant it to end with %s", sql, tt.want)
			}
			// The clause is last: PostgreSQL's grammar puts it after LIMIT.
			if !strings.Contains(sql, "LIMIT 10 "+tt.want) {
				t.Errorf("the locking clause is not after LIMIT: %s", sql)
			}
		})
	}
}

// Two waiting policies are two answers to one question, so a statement takes
// one of them.
func TestLock_refusesConflictingPolicies(t *testing.T) {
	_, _, err := gendemo.New(nil).Users.Query().
		Lock(orm.ForUpdateStrong, orm.NoWait, orm.SkipLocked).
		SQL()
	if err == nil {
		t.Fatal("NOWAIT and SKIP LOCKED were accepted together")
	}
	if !strings.Contains(err.Error(), "one of them") {
		t.Errorf("error = %v", err)
	}
}

// A locking clause names sources the statement selects from, and nothing else.
func TestLock_targets(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })

	t.Run("naming a joined table", func(t *testing.T) {
		sql, _, err := orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
			Lock(orm.ForUpdateStrong, orm.LockOf(gendemo.Users.Source())).
			SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.HasSuffix(sql, `FOR UPDATE OF "users"`) {
			t.Errorf("SQL = %s", sql)
		}
	})
	t.Run("a source the statement does not select from", func(t *testing.T) {
		_, _, err := orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			Lock(orm.ForUpdateStrong, orm.LockOf(gendemo.Posts.Source())).
			SQL()
		if err == nil {
			t.Fatal("a lock named a source the statement does not read")
		}
		if !strings.Contains(err.Error(), "scope error") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a derived table has no rows to lock", func(t *testing.T) {
		sub := orm.Sub("s", orm.Rows(orm.Named("id", orm.Of(gendemo.Users.ID))).
			From(gendemo.Users.Source()))
		_, _, err := orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			Lock(orm.ForUpdateStrong, orm.LockOf(sub)).
			SQL()
		if err == nil {
			t.Fatal("a derived table was accepted as a lock target")
		}
		if !strings.Contains(err.Error(), "only a table can be locked") {
			t.Errorf("error = %v", err)
		}
	})
}

// Scenario M, release-critical: two workers claim jobs concurrently and get
// disjoint sets, without either waiting for the other.
func TestLock_skipLockedGivesWorkersDisjointRows(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	// Twenty jobs; each worker claims ten.
	jobs := make([]gendemo.Category, 20)
	for i := range jobs {
		jobs[i] = gendemo.Category{Name: "job"}
	}
	if _, err := db.Categories.CopyFrom(t.Context(), jobs); err != nil {
		t.Fatalf("seeding jobs: %v", err)
	}

	claim := func(hold chan struct{}) ([]int64, error) {
		var ids []int64
		err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
			got, err := tx.Categories.Query().
				OrderBy(gendemo.Categories.ID.Asc()).
				Limit(10).
				Lock(orm.ForUpdateStrong, orm.SkipLocked).
				All(t.Context())
			if err != nil {
				return err
			}
			for _, c := range got {
				ids = append(ids, c.ID)
			}
			// Hold the locks until the other worker has run.
			<-hold
			return nil
		})
		return ids, err
	}

	holdA := make(chan struct{})
	var (
		wg           sync.WaitGroup
		idsA, idsB   []int64
		errA, errB   error
		startedFirst = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		idsA, errA = claim(holdA)
	}()

	// Give A time to take its locks, then run B while A still holds them.
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(startedFirst)
	}()
	<-startedFirst

	done := make(chan struct{})
	go func() {
		defer close(done)
		idsB, errB = claim(closedChan())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the second worker blocked; SKIP LOCKED did not skip")
	}
	close(holdA)
	wg.Wait()

	if errA != nil || errB != nil {
		t.Fatalf("worker errors: %v / %v", errA, errB)
	}
	if len(idsA) != 10 {
		t.Errorf("worker A claimed %d jobs, want 10", len(idsA))
	}
	if len(idsB) == 0 {
		t.Fatal("worker B claimed nothing; SKIP LOCKED returned no rows at all")
	}
	seen := map[int64]bool{}
	for _, id := range idsA {
		seen[id] = true
	}
	for _, id := range idsB {
		if seen[id] {
			t.Errorf("job %d was claimed by both workers", id)
		}
	}
}

// Scenario N: NOWAIT fails immediately, with PostgreSQL's own error.
func TestLock_noWaitFailsImmediately(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	held := make(chan struct{})
	release := make(chan struct{})
	var holderErr error
	go func() {
		holderErr = db.Tx(t.Context(), func(tx *gendemo.DB) error {
			if _, err := tx.Users.Query().
				Where(gendemo.Users.ID.Eq(int64(1))).
				Lock(orm.ForUpdateStrong).
				All(t.Context()); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	start := time.Now()
	err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		_, err := tx.Users.Query().
			Where(gendemo.Users.ID.Eq(int64(1))).
			Lock(orm.ForUpdateStrong, orm.NoWait).
			All(t.Context())
		return err
	})
	elapsed := time.Since(start)
	close(release)

	if err == nil {
		t.Fatal("NOWAIT succeeded against a locked row")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
	}
	if pg.Code != "55P03" {
		t.Errorf("SQLSTATE = %s, want 55P03 (lock_not_available)", pg.Code)
	}
	if elapsed > 3*time.Second {
		t.Errorf("NOWAIT waited %v", elapsed)
	}
	if holderErr != nil {
		t.Errorf("the holding transaction failed: %v", holderErr)
	}
}

// FOR SHARE and FOR UPDATE conflict; two FOR SHARE locks do not. PostgreSQL
// decides, and this only checks that the strengths reach it.
func TestLock_strengthsConflictAsPostgreSQLSays(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	holdShare := func(t *testing.T, held, release chan struct{}) {
		t.Helper()
		go func() {
			_ = db.Tx(t.Context(), func(tx *gendemo.DB) error {
				if _, err := tx.Users.Query().
					Where(gendemo.Users.ID.Eq(int64(1))).
					Lock(orm.ForShare).
					All(t.Context()); err != nil {
					return err
				}
				close(held)
				<-release
				return nil
			})
		}()
		<-held
	}

	t.Run("share does not block share", func(t *testing.T) {
		held, release := make(chan struct{}), make(chan struct{})
		holdShare(t, held, release)
		defer close(release)

		err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
			_, err := tx.Users.Query().
				Where(gendemo.Users.ID.Eq(int64(1))).
				Lock(orm.ForShare, orm.NoWait).
				All(t.Context())
			return err
		})
		if err != nil {
			t.Errorf("a second FOR SHARE was refused: %v", err)
		}
	})

	t.Run("share blocks update", func(t *testing.T) {
		held, release := make(chan struct{}), make(chan struct{})
		holdShare(t, held, release)
		defer close(release)

		err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
			_, err := tx.Users.Query().
				Where(gendemo.Users.ID.Eq(int64(1))).
				Lock(orm.ForUpdateStrong, orm.NoWait).
				All(t.Context())
			return err
		})
		if err == nil {
			t.Fatal("FOR UPDATE took a row held under FOR SHARE")
		}
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "55P03" {
			t.Errorf("error = %v", err)
		}
	})
}

// The M5 spelling still means what it did.
func TestLock_forUpdateIsUnchanged(t *testing.T) {
	sql := lockSQL(t, gendemo.New(nil).Users.Query().ForUpdate())
	if !strings.HasSuffix(sql, "FOR UPDATE") {
		t.Errorf("SQL = %s", sql)
	}
	// Count and Exists still drop the clause, because neither returns rows to
	// lock — the contract M3 set.
	q := gendemo.New(nil).Users.Query().Lock(orm.ForUpdateStrong, orm.SkipLocked)
	if _, _, err := q.SQL(); err != nil {
		t.Fatalf("SQL: %v", err)
	}
}

func closedChan() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}
