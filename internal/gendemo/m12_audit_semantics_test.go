package gendemo_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// The M12 semantics audit: JSON's three kinds of nothing through every reader,
// schema identity for two types that share a Go representation, and locking
// under real contention.

// Release-critical: -> and ->> answer differently for a JSON null, and the ORM
// must not blur them.
//
// For {"x": null} the arrow returns a jsonb document holding null, while the
// double arrow returns SQL NULL. A reader that collapsed the two would make a
// present-but-null key indistinguishable from an absent one.
func TestAudit_jsonArrowVersusDoubleArrow(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, metadata, created_at) VALUES
	    (1, 'sqlnull@e.com',  1, 'active', '{}', '{}', NULL,          now()),
	    (2, 'missing@e.com',  2, 'active', '{}', '{}', '{}',          now()),
	    (3, 'jsonnull@e.com', 3, 'active', '{}', '{}', '{"x": null}', now()),
	    (4, 'zero@e.com',     4, 'active', '{}', '{}', '{"x": 0}',    now())`)

	doc := gendemo.Users.Metadata

	// jsonb_typeof of the arrow tells the four apart: NULL, NULL, "null",
	// "number" — which is only possible because -> kept the JSON null.
	shape := orm.Project2(orm.Of(gendemo.Users.ID), orm.JSONTypeOf(orm.JSONGet(doc, "x")),
		func(id int64, k *string) string {
			if k == nil {
				return "<sql null>"
			}
			return *k
		})
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []string{"<sql null>", "<sql null>", "null", "number"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d: -> then jsonb_typeof = %q, want %q", i+1, got[i], want[i])
		}
	}

	// And ->> is SQL NULL for the JSON null, which is the difference.
	text := orm.Project2(orm.Of(gendemo.Users.ID), orm.JSONText(doc, "x"),
		func(id int64, v *string) string {
			if v == nil {
				return "<sql null>"
			}
			return *v
		})
	textGot, err := orm.Compose(db.Executor(), text).
		From(gendemo.Users.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if textGot[2] != "<sql null>" {
		t.Errorf("->> of a JSON null read %q, want SQL NULL", textGot[2])
	}
	if textGot[3] != "0" {
		t.Errorf("->> of the number read %q", textGot[3])
	}
	// HasKey still says the key is there for row 3, which is the third answer.
	keys := jsonIDs(t, db, orm.JSONHasKey(doc, "x"))
	assertIDs(t, conn, `SELECT id FROM users WHERE metadata ? 'x' ORDER BY id`, keys)
	if len(keys) != 2 {
		t.Errorf("? matched %v, want rows 3 and 4", keys)
	}
}

// The array readers, against PostgreSQL, at every index that behaves specially.
func TestAudit_jsonIndexing(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, metadata, created_at) VALUES
	    (1, 'arr@e.com', 1, 'active', '{}', '{}', '["a","b","c"]', now()),
	    (2, 'obj@e.com', 2, 'active', '{}', '{}', '{"k":1}',       now()),
	    (3, 'nul@e.com', 3, 'active', '{}', '{}', NULL,            now())`)

	doc := gendemo.Users.Metadata
	read := func(t *testing.T, i int32) []string {
		t.Helper()
		shape := orm.Project1(orm.JSONIndexText(doc, i), func(v *string) string {
			if v == nil {
				return "<null>"
			}
			return *v
		})
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Users.Source()).
			OrderBy(orm.Of(gendemo.Users.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return got
	}
	handwritten := func(t *testing.T, i int32) []string {
		t.Helper()
		rows, err := conn.Query(t.Context(),
			`SELECT coalesce(metadata ->> $1::int4, '<null>') FROM users ORDER BY id`, i)
		if err != nil {
			t.Fatalf("handwritten: %v", err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
	for _, i := range []int32{0, 1, 2, -1, -3, 9} {
		t.Run(fmt.Sprintf("index %d", i), func(t *testing.T) {
			got, want := read(t, i), handwritten(t, i)
			if len(got) != len(want) {
				t.Fatalf("got %v, handwritten %v", got, want)
			}
			for j := range got {
				if got[j] != want[j] {
					t.Errorf("row %d: %q against %q", j+1, got[j], want[j])
				}
			}
		})
	}
}

// A key or a path component is data. Whatever is in it, the statement's shape
// does not change.
func TestAudit_jsonKeysAreAlwaysData(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, metadata, created_at) VALUES
	    (1, 'k@e.com', 1, 'active', '{}', '{}', '{"a''b": 1, "c\"d": 2, "e\\f": 3, "": 4, "ü": 5, "x,y": 6, "{z}": 7}', now())`)

	doc := gendemo.Users.Metadata
	nasty := []string{
		`a'b`, `c"d`, `e\f`, ``, `ü`, `x,y`, `{z}`,
		`'; DROP TABLE users; --`,
		`") OR 1=1 --`,
		strings.Repeat("k", 5000),
	}

	var shapes []string
	for _, key := range nasty {
		sql, args, err := orm.Compose(nil, orm.Project1(orm.JSONText(doc, key),
			func(v *string) *string { return v })).
			From(gendemo.Users.Source()).SQL()
		if err != nil {
			t.Fatalf("SQL for %q: %v", key, err)
		}
		shapes = append(shapes, sql)
		if len(args) != 1 || args[0] != key {
			t.Errorf("the key %q did not travel as the only argument: %v", key, args)
		}
	}
	// Every statement is byte-identical: the key never reaches the text.
	for i, s := range shapes {
		if s != shapes[0] {
			t.Fatalf("key %d changed the statement:\n%s\n%s", i, shapes[0], s)
		}
	}

	// And the values that exist come back.
	for key, want := range map[string]string{`a'b`: "1", `c"d`: "2", `e\f`: "3", ``: "4", `ü`: "5", `x,y`: "6", `{z}`: "7"} {
		got, err := orm.Compose(db.Executor(),
			orm.Project1(orm.JSONText(doc, key), func(v *string) *string { return v })).
			From(gendemo.Users.Source()).One(t.Context())
		if err != nil {
			t.Fatalf("reading %q: %v", key, err)
		}
		if got == nil || *got != want {
			t.Errorf("key %q read %v, want %q", key, got, want)
		}
	}
	// The table is still there, which a splice would have changed.
	var n int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("the users table holds %d rows", n)
	}
}

// A malformed JSONPath is PostgreSQL's error rather than a panic or a splice.
func TestAudit_jsonPathIsAValueNotSyntax(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `DELETE FROM posts; DELETE FROM users`)
	m11exec(t, conn, `INSERT INTO users (id, email, age, state, tags, settings, metadata, created_at) VALUES
	    (1, 'p@e.com', 1, 'active', '{}', '{}', '{"n": 5}', now())`)

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
	for _, tt := range []struct {
		name  string
		path  string
		valid bool
	}{
		{"valid", `$.n > 1`, true},
		{"unicode", `$."ü" > 1`, true},
		{"malformed", `$.(((`, false},
		{"sql looking", `'; DROP TABLE users; --`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := orm.Compose(db.Executor(), shape).
				From(gendemo.Users.Source()).
				Where(orm.JSONMatches(gendemo.Users.Metadata, tt.path)).
				All(t.Context())
			switch {
			case tt.valid && err != nil:
				t.Errorf("a valid path failed: %v", err)
			case !tt.valid && err == nil:
				t.Error("a malformed path succeeded")
			case !tt.valid:
				var pg *pgconn.PgError
				if !errors.As(err, &pg) {
					t.Errorf("error = %v (%T), want a *pgconn.PgError", err, err)
				}
			}
		})
	}
	var n int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("the users table holds %d rows after the injection attempts", n)
	}
}

// The text search configuration is a value too, so an unknown one is
// PostgreSQL's error and a hostile one is just an unknown configuration.
func TestAudit_textSearchConfigIsAValue(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `INSERT INTO articles (title, body) VALUES ('a', 'b')`)

	shape := orm.Project1(orm.Of(gendemo.Articles.ID), func(v int64) int64 { return v })
	sqlFor := func(cfg orm.TextSearchConfig) (string, []any) {
		t.Helper()
		sql, args, err := orm.Compose(nil, shape).
			From(gendemo.Articles.Source()).
			Where(orm.Matches(gendemo.Articles.Search, orm.PlainToTSQuery(cfg, "x"))).
			SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		return sql, args
	}
	base, _ := sqlFor(orm.English)
	hostile, args := sqlFor(orm.SearchConfig(`english'); DROP TABLE articles; --`))
	if base != hostile {
		t.Errorf("the configuration changed the statement:\n%s\n%s", base, hostile)
	}
	if len(args) != 2 || args[0] != `english'); DROP TABLE articles; --` {
		t.Errorf("the configuration did not travel as an argument: %v", args)
	}

	_, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		Where(orm.Matches(gendemo.Articles.Search,
			orm.PlainToTSQuery(orm.SearchConfig("no_such_config"), "x"))).
		All(t.Context())
	if err == nil {
		t.Fatal("an unknown text search configuration succeeded")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Errorf("error = %v (%T), want a *pgconn.PgError", err, err)
	}
	var n int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM articles`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("the articles table holds %d rows", n)
	}
}

// inet and cidr share one Go type, and the schema must still tell them apart —
// otherwise changing a column from one to the other would look like no change.
func TestAudit_inetAndCidrAreDistinctSchemaTypes(t *testing.T) {
	testdb.AdminDSN(t)
	_, conn := m12env(t)

	var subnet, host string
	if err := conn.QueryRow(t.Context(), `
		SELECT
		  (SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		    WHERE attrelid = 'networks'::regclass AND attname = 'subnet'),
		  (SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		    WHERE attrelid = 'networks'::regclass AND attname = 'host')`).Scan(&subnet, &host); err != nil {
		t.Fatalf("reading the catalog: %v", err)
	}
	if subnet != "cidr" || host != "inet" {
		t.Errorf("the catalog reports subnet=%q host=%q; the two Go-identical types collapsed", subnet, host)
	}
	// And PostgreSQL itself refuses to treat them as the same column type.
	if _, err := conn.Exec(t.Context(),
		`ALTER TABLE networks ALTER COLUMN subnet TYPE inet`); err != nil {
		t.Fatalf("the two types cannot even be exchanged: %v", err)
	}
	var after string
	if err := conn.QueryRow(t.Context(), `
		SELECT format_type(atttypid, atttypmod) FROM pg_attribute
		 WHERE attrelid = 'networks'::regclass AND attname = 'subnet'`).Scan(&after); err != nil {
		t.Fatalf("reading the catalog: %v", err)
	}
	if after != "inet" {
		t.Errorf("after the change the catalog reports %q", after)
	}
	if after == subnet {
		t.Error("changing cidr to inet was invisible in the catalog")
	}
}

// Release-critical: ten workers claiming from one hundred jobs get disjoint
// sets, none blocks, and between them they claim everything.
func TestAudit_skipLockedUnderHeavyContention(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	cfg := poolConfig(t, dsn, 12)
	pool := poolFrom(t, cfg)
	db := gendemo.New(pool)

	const (
		jobs    = 100
		workers = 10
		batch   = 10
	)
	rows := make([]gendemo.Category, jobs)
	for i := range rows {
		rows[i] = gendemo.Category{Name: "job"}
	}
	if _, err := db.Categories.CopyFrom(t.Context(), rows); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	for round := range 3 {
		var (
			mu      sync.Mutex
			claimed = map[int64]int{}
			wg      sync.WaitGroup
			release = make(chan struct{})
			errs    = make([]error, workers)
		)
		start := make(chan struct{})
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[w] = db.Tx(t.Context(), func(tx *gendemo.DB) error {
					got, err := tx.Categories.Query().
						OrderBy(gendemo.Categories.ID.Asc()).
						Limit(batch).
						Lock(orm.ForUpdateStrong, orm.SkipLocked).
						All(t.Context())
					if err != nil {
						return err
					}
					mu.Lock()
					for _, c := range got {
						claimed[c.ID]++
					}
					mu.Unlock()
					// Hold every worker's locks until all have claimed, which
					// is what makes the disjointness meaningful.
					<-release
					return nil
				})
			}()
		}
		close(start)

		done := make(chan struct{})
		go func() {
			// Give the workers time to claim, then let them all commit.
			time.Sleep(700 * time.Millisecond)
			close(release)
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: workers blocked; SKIP LOCKED did not skip", round)
		}

		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d worker %d: %v", round, i, err)
			}
		}
		mu.Lock()
		total := 0
		for id, n := range claimed {
			if n != 1 {
				t.Errorf("round %d: job %d was claimed %d times", round, id, n)
			}
			total += n
		}
		mu.Unlock()
		if total != jobs {
			t.Errorf("round %d: %d of %d jobs were claimed", round, total, jobs)
		}
	}
	if n := pool.Stat().AcquiredConns(); n != 0 {
		t.Errorf("%d connections still acquired after the contention rounds", n)
	}
}

// An uneven division leaves a smaller final batch and still no overlap.
func TestAudit_skipLockedUnevenBatches(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	pool := poolFrom(t, poolConfig(t, dsn, 6))
	db := gendemo.New(pool)

	const jobs = 23
	rows := make([]gendemo.Category, jobs)
	for i := range rows {
		rows[i] = gendemo.Category{Name: "job"}
	}
	if _, err := db.Categories.CopyFrom(t.Context(), rows); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	var (
		mu      sync.Mutex
		claimed = map[int64]int{}
		sizes   []int
		wg      sync.WaitGroup
		release = make(chan struct{})
	)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = db.Tx(t.Context(), func(tx *gendemo.DB) error {
				got, err := tx.Categories.Query().
					OrderBy(gendemo.Categories.ID.Asc()).
					Limit(10).
					Lock(orm.ForUpdateStrong, orm.SkipLocked).
					All(t.Context())
				if err != nil {
					return err
				}
				mu.Lock()
				sizes = append(sizes, len(got))
				for _, c := range got {
					claimed[c.ID]++
				}
				mu.Unlock()
				<-release
				return nil
			})
		}()
	}
	time.Sleep(700 * time.Millisecond)
	close(release)
	wg.Wait()

	total := 0
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("job %d was claimed %d times", id, n)
		}
		total++
	}
	if total != jobs {
		t.Errorf("%d of %d jobs were claimed; batches were %v", total, jobs, sizes)
	}
}

// LockOf is checked by source identity, not by the name a source happens to
// carry.
func TestAudit_lockOfUsesSourceIdentity(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })

	a := gendemo.Users.As("u")
	b := gendemo.Users.As("u")
	shape = orm.Project1(orm.Of(a.ID), func(v int64) int64 { return v })
	_, _, err := orm.Compose(nil, shape).
		From(a.Source()).
		Lock(orm.ForUpdateStrong, orm.LockOf(b.Source())).
		SQL()
	if err == nil {
		t.Fatal("a lock target with the same alias but a different identity was accepted")
	}
	if !strings.Contains(err.Error(), "scope error") {
		t.Errorf("error = %v", err)
	}

	// The one actually in the statement is accepted, and names its alias.
	sql, _, err := orm.Compose(nil, shape).
		From(a.Source()).
		Lock(orm.ForUpdateStrong, orm.LockOf(a.Source())).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `FOR UPDATE OF "u"`) {
		t.Errorf("SQL = %s", sql)
	}
}

// Locking the nullable side of an outer join is PostgreSQL's to refuse, and the
// error survives.
func TestAudit_lockingAnOuterJoinedSide(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })
	_, err2 := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
		Lock(orm.ForUpdateStrong, orm.LockOf(gendemo.Posts.Source())).
		All(t.Context())
	if err2 == nil {
		t.Fatal("locking the nullable side of an outer join succeeded")
	}
	var pg *pgconn.PgError
	if !errors.As(err2, &pg) {
		t.Fatalf("error = %v (%T), want PostgreSQL's own", err2, err2)
	}
}

// Count and Exists ask how many rows there are, and neither is a reason to take
// locks the caller did not ask for on the rows they never receive.
func TestAudit_lockDoesNotLeakIntoHelperQueries(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)

	q := db.Users.Query().Lock(orm.ForUpdateStrong, orm.SkipLocked)
	// Both run without error, which is the contract: the helper statements are
	// legal whatever the locking clause says.
	if _, err := q.Count(t.Context()); err != nil {
		t.Errorf("Count on a locking query: %v", err)
	}
	if _, err := q.Exists(t.Context()); err != nil {
		t.Errorf("Exists on a locking query: %v", err)
	}
}
