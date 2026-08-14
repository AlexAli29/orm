package gendemo_test

import (
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// Fingerprints.
//
// Two claims, and the second is the one that takes work: values must never
// change a fingerprint, and structure must always change it. A fingerprint that
// failed the first would have a cardinality equal to the number of executions
// and be useless; one that failed the second would group statements PostgreSQL
// plans differently and be worse than useless.

func fp(t *testing.T, s orm.Statement) orm.Fingerprint {
	t.Helper()
	f, err := orm.FingerprintOf(s)
	if err != nil {
		t.Fatalf("FingerprintOf: %v", err)
	}
	if f.IsZero() {
		t.Fatal("the fingerprint is zero")
	}
	return f
}

func repo(t *testing.T) *gendemo.DB {
	t.Helper()
	testdb := explainDBOrSkip(t)
	return testdb
}

// explainDBOrSkip builds a database only because the builders need a repository
// to hang on; nothing in this file executes a statement.
func explainDBOrSkip(t *testing.T) *gendemo.DB {
	t.Helper()
	db, _ := explainDB(t)
	return db
}

// Only the values differ, so the fingerprint must not.
func TestFingerprint_valuesDoNotMatter(t *testing.T) {
	db := repo(t)

	cases := map[string][2]orm.Statement{
		"integers": {
			db.Users.Query().Where(gendemo.Users.ID.Eq(123)),
			db.Users.Query().Where(gendemo.Users.ID.Eq(456)),
		},
		"strings": {
			db.Users.Query().Where(gendemo.Users.Email.Eq("a@example.com")),
			db.Users.Query().Where(gendemo.Users.Email.Eq("this is a completely different string")),
		},
		"a secret and a placeholder": {
			db.Users.Query().Where(gendemo.Users.Email.Eq("hunter2")),
			db.Users.Query().Where(gendemo.Users.Email.Eq("")),
		},
		"timestamps": {
			db.Users.Query().Where(gendemo.Users.CreatedAt.Gt(time.Unix(0, 0))),
			db.Users.Query().Where(gendemo.Users.CreatedAt.Gt(time.Now())),
		},
		"json documents": {
			composedUsers(db, orm.JSONContains(orm.Of(gendemo.Users.Settings),
				orm.Val(map[string]any{"tier": "gold"}))),
			composedUsers(db, orm.JSONContains(orm.Of(gendemo.Users.Settings),
				orm.Val(map[string]any{"tier": "bronze", "secret": "token"}))),
		},
		"ranges": {
			db.Bookings.Query().Where(gendemo.Bookings.Quota.Contains(int32(5))),
			db.Bookings.Query().Where(gendemo.Bookings.Quota.Contains(int32(999))),
		},
		"network addresses": {
			composedNetworks(db, orm.ContainedBy(orm.Of(gendemo.Networks.Host),
				orm.Val(netip.MustParsePrefix("10.0.0.0/8")))),
			composedNetworks(db, orm.ContainedBy(orm.Of(gendemo.Networks.Host),
				orm.Val(netip.MustParsePrefix("192.168.1.0/24")))),
		},
		"full-text search terms": {
			composedArticles(db, orm.Matches(gendemo.Articles.Search,
				orm.PlainToTSQuery(orm.English, "postgres"))),
			composedArticles(db, orm.Matches(gendemo.Articles.Search,
				orm.PlainToTSQuery(orm.English, "an entirely different search phrase"))),
		},
		"an update's assigned value": {
			db.Users.Update().Set(gendemo.Users.Age.Set(1)).Where(gendemo.Users.ID.Eq(1)),
			db.Users.Update().Set(gendemo.Users.Age.Set(99)).Where(gendemo.Users.ID.Eq(2)),
		},
	}

	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			a, b := fp(t, pair[0]), fp(t, pair[1])
			if !a.Equal(b) {
				sqlA, _, _ := pair[0].SQL()
				sqlB, _, _ := pair[1].SQL()
				t.Errorf("the values changed the fingerprint:\n%s\n%s\n%s\n%s",
					a, sqlA, b, sqlB)
			}
		})
	}
}

// The structure differs, so the fingerprint must.
func TestFingerprint_structureMatters(t *testing.T) {
	db := repo(t)

	shape1 := orm.Project1(gendemo.Users.Email, func(s string) string { return s })
	shape2 := orm.Project1(gendemo.Users.Nickname, func(s *string) string { return "" })

	cases := map[string][2]orm.Statement{
		"a different column": {
			db.Users.Query().Where(gendemo.Users.ID.Eq(1)),
			db.Users.Query().Where(gendemo.Users.Age.Eq(1)),
		},
		"a different operator": {
			db.Users.Query().Where(gendemo.Users.Age.Eq(1)),
			db.Users.Query().Where(gendemo.Users.Age.Gt(1)),
		},
		"ascending and descending": {
			db.Users.Query().OrderBy(gendemo.Users.CreatedAt.Asc()),
			db.Users.Query().OrderBy(gendemo.Users.CreatedAt.Desc()),
		},
		"a limit and no limit": {
			db.Users.Query().Limit(10),
			db.Users.Query(),
		},
		"an offset and no offset": {
			db.Users.Query().Limit(10),
			db.Users.Query().Limit(10).Offset(5),
		},
		"a different projection": {
			orm.Select(db.Users, shape1),
			orm.Select(db.Users, shape2),
		},
		"one predicate and two": {
			db.Users.Query().Where(gendemo.Users.ID.Eq(1)),
			db.Users.Query().Where(gendemo.Users.ID.Eq(1), gendemo.Users.Age.Eq(2)),
		},
		"AND and OR": {
			db.Users.Query().Where(orm.And(gendemo.Users.ID.Eq(1), gendemo.Users.Age.Eq(2))),
			db.Users.Query().Where(orm.Or(gendemo.Users.ID.Eq(1), gendemo.Users.Age.Eq(2))),
		},
		"a lock and no lock": {
			db.Users.Query(),
			db.Users.Query().ForUpdate(),
		},
		"two lock strengths": {
			db.Users.Query().Lock(orm.ForUpdateStrong),
			db.Users.Query().Lock(orm.ForShare),
		},
		"a lock option": {
			db.Users.Query().Lock(orm.ForUpdateStrong),
			db.Users.Query().Lock(orm.ForUpdateStrong, orm.SkipLocked),
		},
		"a different table": {
			db.Users.Query().Where(gendemo.Users.ID.Eq(1)),
			db.Posts.Query().Where(gendemo.Posts.ID.Eq(1)),
		},
		"an update and a delete": {
			db.Users.Update().Set(gendemo.Users.Age.Set(1)).Where(gendemo.Users.ID.Eq(1)),
			db.Users.Delete().Where(gendemo.Users.ID.Eq(1)),
		},
		"a different JSON operator": {
			composedUsers(db, orm.JSONContains(orm.Of(gendemo.Users.Settings),
				orm.Val(map[string]any{"a": 1}))),
			composedUsers(db, orm.JSONContainedBy(orm.Of(gendemo.Users.Settings),
				orm.Val(map[string]any{"a": 1}))),
		},
		"a different range operator": {
			db.Bookings.Query().Where(gendemo.Bookings.Quota.Contains(int32(5))),
			db.Bookings.Query().Where(gendemo.Bookings.Quota.Overlaps(orm.ClosedOpen[int32](1, 5))),
		},
		"two IN lists of different lengths": {
			db.Users.Query().Where(gendemo.Users.ID.In(1, 2)),
			db.Users.Query().Where(gendemo.Users.ID.In(1, 2, 3)),
		},
		// The compiler renders LIMIT and OFFSET as literals rather than
		// parameters, so they are part of the statement PostgreSQL plans — and
		// it plans LIMIT 1 differently from LIMIT 1000000. Grouping them
		// together would merge statements whose plans differ, which is the one
		// thing a fingerprint is for keeping apart.
		"two limits": {
			db.Users.Query().Limit(10),
			db.Users.Query().Limit(1000),
		},
		"two offsets": {
			db.Users.Query().Limit(10).Offset(0),
			db.Users.Query().Limit(10).Offset(500),
		},
	}

	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			a, b := fp(t, pair[0]), fp(t, pair[1])
			if a.Equal(b) {
				sqlA, _, _ := pair[0].SQL()
				t.Errorf("two different structures share a fingerprint %s:\n%s", a, sqlA)
			}
		})
	}
}

// A join's kind is structure.
func TestFingerprint_joinKind(t *testing.T) {
	db := repo(t)
	shape := orm.Project1(orm.Of(gendemo.Users.Email), func(s string) string { return s })

	inner := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		Join(gendemo.Posts.Source(), orm.Eq(orm.Of(gendemo.Posts.AuthorID), orm.Of(gendemo.Users.ID)))
	left := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(orm.Of(gendemo.Posts.AuthorID), orm.Of(gendemo.Users.ID)))

	if fp(t, inner).Equal(fp(t, left)) {
		t.Error("an inner join and a left join share a fingerprint")
	}
}

// An alias is structure: two occurrences of one table are not one source.
func TestFingerprint_aliases(t *testing.T) {
	db := repo(t)
	shape := orm.Project1(orm.Of(gendemo.Users.Email), func(s string) string { return s })
	manager := gendemo.Users.As("manager")

	plain := orm.Compose(db.Executor(), shape).From(gendemo.Users.Source())
	aliased := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		Join(manager.Source(), orm.Eq(orm.Of(manager.ID), orm.Of(gendemo.Users.ManagerID)))

	if fp(t, plain).Equal(fp(t, aliased)) {
		t.Error("a query with a self-join shares a fingerprint with one without")
	}
}

// A clone with the same structure fingerprints identically, and one that then
// changes does not.
func TestFingerprint_clone(t *testing.T) {
	db := repo(t)
	base := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).OrderBy(gendemo.Users.Age.Desc()).Limit(5)

	same := base.Clone()
	if !fp(t, base).Equal(fp(t, same)) {
		t.Error("a clone fingerprints differently")
	}

	// Different values, same structure.
	other := base.Clone()
	if !fp(t, base).Equal(fp(t, other)) {
		t.Error("a clone with the same structure fingerprints differently")
	}

	// A clone that gains a predicate is a different structure.
	changed := base.Clone().Where(gendemo.Users.Active.Eq(true))
	if fp(t, base).Equal(fp(t, changed)) {
		t.Error("a clone with an extra predicate shares the original's fingerprint")
	}
}

// The same statement fingerprints the same way every time, in this process and
// in any other. A seeded map hash would not.
func TestFingerprint_deterministic(t *testing.T) {
	db := repo(t)
	build := func() orm.Statement {
		return db.Users.Query().
			Where(gendemo.Users.ID.Eq(1), gendemo.Users.Active.Eq(true)).
			OrderBy(gendemo.Users.CreatedAt.Desc()).
			Limit(20)
	}
	first := fp(t, build())
	for range 20 {
		if got := fp(t, build()); !got.Equal(first) {
			t.Fatalf("the same query fingerprinted as %s and %s", first, got)
		}
	}
	// And the rendering is stable and versioned.
	if !strings.HasPrefix(first.String(), "v1:") {
		t.Errorf("the fingerprint is rendered as %q", first)
	}
	if len(first.Full()) != len("v1:")+64 {
		t.Errorf("the full fingerprint is %q", first.Full())
	}
}

// The fingerprint's text contains no value, which is what makes it safe to put
// in a log line.
func TestFingerprint_leaksNoValues(t *testing.T) {
	db := repo(t)
	secrets := []string{"hunter2", "bearer-abc123", "person@example.com"}
	for _, secret := range secrets {
		f := fp(t, db.Users.Query().Where(gendemo.Users.Email.Eq(secret)))
		for _, form := range []string{f.String(), f.Full()} {
			if strings.Contains(form, secret) {
				t.Errorf("the fingerprint %q contains %q", form, secret)
			}
			// A digest is hex after the version prefix, so nothing readable can
			// be in it at all.
			body := strings.TrimPrefix(strings.TrimPrefix(form, "v1:"), "v1:")
			for _, r := range body {
				if !strings.ContainsRune("0123456789abcdef", r) {
					t.Errorf("the fingerprint body %q is not hex", body)
					break
				}
			}
		}
	}
}

// A COPY has no SQL, and still has an identity worth grouping by.
func TestFingerprint_copy(t *testing.T) {
	base := orm.CopyFingerprint("public", "users", []string{"id", "email", "age"})

	if base.IsZero() {
		t.Fatal("a COPY has no fingerprint")
	}
	if !base.Equal(orm.CopyFingerprint("public", "users", []string{"id", "email", "age"})) {
		t.Error("the same COPY fingerprints differently")
	}
	// A different column order is a different operation on the wire.
	if base.Equal(orm.CopyFingerprint("public", "users", []string{"email", "id", "age"})) {
		t.Error("two column orders share a fingerprint")
	}
	// A different column set, a different table and a different schema all
	// differ.
	for name, other := range map[string]orm.Fingerprint{
		"fewer columns":      orm.CopyFingerprint("public", "users", []string{"id", "email"}),
		"a different table":  orm.CopyFingerprint("public", "posts", []string{"id", "email", "age"}),
		"a different schema": orm.CopyFingerprint("other", "users", []string{"id", "email", "age"}),
	} {
		if base.Equal(other) {
			t.Errorf("%s shares the base fingerprint", name)
		}
	}
	// And a COPY never collides with a SELECT of the same shape.
	db := repo(t)
	if base.Equal(fp(t, db.Users.Query())) {
		t.Error("a COPY and a SELECT share a fingerprint")
	}
}

// A statement that does not compile has no fingerprint, and the error is the one
// the statement would have failed with.
func TestFingerprint_invalidStatement(t *testing.T) {
	db := repo(t)
	// A predicate over a source the query does not name.
	bad := orm.Compose(db.Executor(),
		orm.Project1(orm.Of(gendemo.Users.Email), func(s string) string { return s })).
		From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Users.ID.Eq(1)))

	if _, err := orm.FingerprintOf(bad); err == nil {
		t.Fatal("a query that does not compile produced a fingerprint")
	}
	if _, err := orm.FingerprintOf(nil); err == nil {
		t.Fatal("a nil statement produced a fingerprint")
	}
	var zero orm.Fingerprint
	if zero.String() != "" || zero.Full() != "" || !zero.IsZero() {
		t.Error("the zero fingerprint renders as something")
	}
}

// Fingerprinting a shared query descriptor from many goroutines is safe: it
// reads the builder and writes nothing.
func TestFingerprint_concurrent(t *testing.T) {
	db := repo(t)
	q := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).OrderBy(gendemo.Users.Age.Asc())

	want := fp(t, q)
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine builds its own statement from the shared
			// descriptors, which is what an application does.
			got, err := db.Users.Query().
				Where(gendemo.Users.ID.Eq(2)).
				OrderBy(gendemo.Users.Age.Asc()).
				Fingerprint()
			if err != nil {
				errs <- err.Error()
				return
			}
			if !got.Equal(want) {
				errs <- "a concurrent fingerprint differs: " + got.String()
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// Raw SQL fingerprints by the text the caller wrote, and its arguments do not
// change it.
func TestFingerprint_raw(t *testing.T) {
	db := repo(t)
	a := orm.Raw(db.Users, `SELECT * FROM users WHERE email = $1`, "one@example.com")
	b := orm.Raw(db.Users, `SELECT * FROM users WHERE email = $1`, "two@example.com")
	c := orm.Raw(db.Users, `SELECT * FROM users WHERE id = $1`, 1)

	if !fp(t, a).Equal(fp(t, b)) {
		t.Error("two raw statements with the same text fingerprint differently")
	}
	if fp(t, a).Equal(fp(t, c)) {
		t.Error("two different raw statements share a fingerprint")
	}
	// Whitespace is part of the text, because nothing parses it. That is the
	// honest answer for SQL this package did not build.
	d := orm.Raw(db.Users, `SELECT *  FROM users WHERE email = $1`, "x")
	if fp(t, a).Equal(fp(t, d)) {
		t.Log("whitespace does not change the raw fingerprint")
	}
}

// Every builder can be fingerprinted through its own method as well as through
// the free function, and the two agree.
func TestFingerprint_methodsAgree(t *testing.T) {
	db := repo(t)
	shape := orm.Project1(gendemo.Users.Email, func(s string) string { return s })

	type pair struct {
		method func() (orm.Fingerprint, error)
		stmt   orm.Statement
	}
	q := db.Users.Query()
	sq := orm.Select(db.Users, shape)
	cq := orm.Compose(db.Executor(),
		orm.Project1(orm.Of(gendemo.Users.Email), func(s string) string { return s })).
		From(gendemo.Users.Source())
	up := db.Users.Update().Set(gendemo.Users.Age.Set(1)).Where(gendemo.Users.ID.Eq(1))
	del := db.Users.Delete().Where(gendemo.Users.ID.Eq(1))
	raw := orm.Raw(db.Users, `SELECT 1`)

	for name, p := range map[string]pair{
		"entity query":   {q.Fingerprint, q},
		"select query":   {sq.Fingerprint, sq},
		"composed query": {cq.Fingerprint, cq},
		"update":         {up.Fingerprint, up},
		"delete":         {del.Fingerprint, del},
		"raw":            {raw.Fingerprint, raw},
	} {
		t.Run(name, func(t *testing.T) {
			viaMethod, err := p.method()
			if err != nil {
				t.Fatalf("the method: %v", err)
			}
			if !viaMethod.Equal(fp(t, p.stmt)) {
				t.Error("the method and the function disagree")
			}
		})
	}
}

// The composed-query helpers, for the predicates that relate expressions rather
// than hanging off one entity's descriptor.

func composedUsers(db *gendemo.DB, p orm.Predicate[orm.Composed]) orm.Statement {
	return orm.Compose(db.Executor(),
		orm.Project1(orm.Of(gendemo.Users.Email), func(s string) string { return s })).
		From(gendemo.Users.Source()).
		Where(p)
}

func composedNetworks(db *gendemo.DB, p orm.Predicate[orm.Composed]) orm.Statement {
	return orm.Compose(db.Executor(),
		orm.Project1(orm.Of(gendemo.Networks.Label), func(s string) string { return s })).
		From(gendemo.Networks.Source()).
		Where(p)
}

func composedArticles(db *gendemo.DB, p orm.Predicate[orm.Composed]) orm.Statement {
	return orm.Compose(db.Executor(),
		orm.Project1(orm.Of(gendemo.Articles.Title), func(s string) string { return s })).
		From(gendemo.Articles.Source()).
		Where(p)
}
