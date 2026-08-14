package gendemo_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/tracelog"
)

// Relation loading, against real PostgreSQL.
//
// The tests that matter most here are the ones about shape rather than values:
// that asking for a relation does not change which root rows come back, and
// that the number of statements does not grow with the number of rows.

// counter records every statement a connection runs, so a test can assert the
// exact number rather than "not too many".
type counter struct {
	mu         sync.Mutex
	statements []string
}

func (c *counter) Log(ctx context.Context, level tracelog.LogLevel, msg string, data map[string]any) {
	if msg != "Query" {
		return
	}
	sql, _ := data["sql"].(string)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = append(c.statements, sql)
}

func (c *counter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statements = nil
}

func (c *counter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.statements)
}

func (c *counter) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.statements...)
}

// tracedDB builds a database from the given DDL and returns a handle whose
// statements are counted, alongside the connection behind it so that a test can
// seed further rows without going through the ORM.
func tracedDB(t *testing.T, ddl string) (*gendemo.DB, *counter, *pgx.Conn) {
	t.Helper()
	dsn := testdb.Create(t, schema(t)+ddl)

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	c := &counter{}
	cfg.Tracer = &tracelog.TraceLog{Logger: c, LogLevel: tracelog.LogLevelTrace}

	conn, err := pgx.ConnectConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	c.reset()
	return gendemo.New(conn), c, conn
}

// execRaw runs a statement outside the ORM, for arranging data a test needs.
func execRaw(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(t.Context(), sql); err != nil {
		t.Fatalf("arranging data: %v", err)
	}
}

// relSeed is a small world covering every relation shape at once.
const relSeed = `
INSERT INTO users (id, email, age, active, state, manager_id, tags, settings) VALUES
  (1, 'a@example.com', 40, true, 'active', NULL, '{}', '{}'),
  (2, 'b@example.com', 30, true, 'active', 1,    '{}', '{}'),
  (3, 'c@example.com', 20, true, 'active', 1,    '{}', '{}');

INSERT INTO profiles (id, user_id, bio) VALUES
  (1, 1, 'the first bio'),
  (2, 3, NULL);

INSERT INTO posts (id, author_id, title, published) VALUES
  (1, 1, 'A', true),
  (2, 1, 'B', true),
  (3, 1, 'C', false),
  (4, 3, 'D', true),
  (5, 3, 'E', true);

INSERT INTO documents (id, title, creator_id, editor_id) VALUES
  (1, 'doc one', 1, 2),
  (2, 'doc two', 2, NULL);

INSERT INTO tenants (region, code, name) VALUES
  ('eu', 'acme', 'Acme Europe'),
  ('us', 'acme', 'Acme America');

INSERT INTO branches (id, branch_region, branch_code, label) VALUES
  (1, 'eu', 'acme', 'Berlin'),
  (2, 'eu', 'acme', 'Paris'),
  (3, 'us', 'acme', 'Austin');
`

func relDB(t *testing.T) (*gendemo.DB, *counter) {
	t.Helper()
	db, c, _ := tracedDB(t, relSeed)
	return db, c
}

func TestWith_belongsTo(t *testing.T) {
	testdb.AdminDSN(t)
	// posts 1..3 belong to user 1 and 4..5 to user 3, so several rows point at
	// the same target — which a belongs-to must handle, and which is why
	// nothing here requires the local key to be unique.
	d, c := relDB(t)

	posts, err := d.Posts.Query().
		With(gendemo.Posts.Author).
		OrderBy(gendemo.Posts.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(posts) != 5 {
		t.Fatalf("got %d posts, want 5", len(posts))
	}

	want := []string{"a@example.com", "a@example.com", "a@example.com", "c@example.com", "c@example.com"}
	for i, p := range posts {
		author, loaded := p.Author.Get()
		if !loaded {
			t.Fatalf("post %d has an unloaded author", p.ID)
		}
		if author == nil {
			t.Fatalf("post %d has no author, want %s", p.ID, want[i])
		}
		if author.Email != want[i] {
			t.Errorf("post %d belongs to %s, want %s", p.ID, author.Email, want[i])
		}
	}
	// Each relation gets its own value: nothing is shared between the three
	// posts that point at the same user.
	first, _ := posts[0].Author.Get()
	second, _ := posts[1].Author.Get()
	if first == second {
		t.Error("two posts share one author value")
	}
	// A to-one relation folds into the root statement.
	if c.count() != 1 {
		t.Errorf("ran %d statements, want 1:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
}

func TestWith_belongsToAbsent(t *testing.T) {
	testdb.AdminDSN(t)
	d, _, _ := tracedDB(t, relSeed+`
INSERT INTO posts (id, author_id, title, published) VALUES (6, NULL, 'orphan', true);`)

	post, err := d.Posts.Query().With(gendemo.Posts.Author).Where(gendemo.Posts.ID.Eq(6)).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	author, loaded := post.Author.Get()
	// Requested and absent is a different fact from never requested, and both
	// have to be distinguishable.
	if !loaded {
		t.Fatal("the relation is unloaded, but it was asked for")
	}
	if author != nil {
		t.Errorf("the orphan post has author %+v", author)
	}
}

func TestWith_hasOne(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	users, err := d.Users.Query().
		With(gendemo.Users.Profile).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}

	first, loaded := users[0].Profile.Get()
	if !loaded || first == nil {
		t.Fatalf("user 1 has profile %v (loaded %v), want one", first, loaded)
	}
	if first.Bio == nil || *first.Bio != "the first bio" {
		t.Errorf("user 1 profile bio = %v", first.Bio)
	}

	// User 2 has no profile row at all.
	second, loaded := users[1].Profile.Get()
	if !loaded {
		t.Fatal("user 2 has an unloaded profile")
	}
	if second != nil {
		t.Errorf("user 2 has profile %+v, want absent", second)
	}

	// User 3 has a profile whose only nullable column is NULL. That must not
	// be mistaken for an absent row — which is why presence is decided by the
	// primary key rather than by whichever column happens to be handy.
	third, loaded := users[2].Profile.Get()
	if !loaded || third == nil {
		t.Fatalf("user 3 has profile %v, want a present row with a NULL bio", third)
	}
	if third.Bio != nil {
		t.Errorf("user 3 profile bio = %v, want NULL", third.Bio)
	}

	if c.count() != 1 {
		t.Errorf("ran %d statements, want 1:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
}

func TestWith_hasMany(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	users, err := d.Users.Query().
		With(gendemo.Users.Posts).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	want := []int{3, 0, 2}
	for i, u := range users {
		posts, loaded := u.Posts.Get()
		if !loaded {
			t.Fatalf("user %d has unloaded posts", u.ID)
		}
		if len(posts) != want[i] {
			t.Errorf("user %d has %d posts, want %d", u.ID, len(posts), want[i])
		}
	}
	// A relation that was asked for and matched nothing is loaded and empty,
	// which is a different fact from one nobody asked for.
	empty, loaded := users[1].Posts.Get()
	if !loaded || empty == nil || len(empty) != 0 {
		t.Errorf("user 2 posts = %v (loaded %v), want a loaded empty slice", empty, loaded)
	}

	// One statement for the roots and one for the relation, whatever the row
	// count.
	if c.count() != 2 {
		t.Errorf("ran %d statements, want 2:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
}

func TestWith_statementCountIsIndependentOfRowCount(t *testing.T) {
	testdb.AdminDSN(t)

	for _, n := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("%d users", n), func(t *testing.T) {
			var seed strings.Builder
			for i := 1; i <= n; i++ {
				fmt.Fprintf(&seed, "INSERT INTO users (id, email, age, active, state, tags, settings) VALUES (%d, 'u%d@example.com', 20, true, 'active', '{}', '{}');\n", i, i)
				fmt.Fprintf(&seed, "INSERT INTO posts (author_id, title, published) VALUES (%d, 'p%d', true);\n", i, i)
			}
			d, c, _ := tracedDB(t, seed.String())

			users, err := d.Users.Query().With(gendemo.Users.Posts).All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if len(users) != n {
				t.Fatalf("got %d users, want %d", len(users), n)
			}
			for _, u := range users {
				posts, loaded := u.Posts.Get()
				if !loaded || len(posts) != 1 {
					t.Fatalf("user %d has %d posts (loaded %v), want 1", u.ID, len(posts), loaded)
				}
			}
			// This is the whole point of batching. One statement per relation,
			// never one per row.
			if c.count() != 2 {
				t.Errorf("%d users cost %d statements, want 2", n, c.count())
			}
		})
	}
}

func TestWith_paginationIsUnchanged(t *testing.T) {
	testdb.AdminDSN(t)

	var seed strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&seed, "INSERT INTO users (id, email, age, active, state, tags, settings) VALUES (%d, 'u%02d@example.com', 20, true, 'active', '{}', '{}');\n", i, i)
		for j := range i % 3 {
			fmt.Fprintf(&seed, "INSERT INTO posts (author_id, title, published) VALUES (%d, 'p%d-%d', true);\n", i, i, j)
		}
	}
	d, _, _ := tracedDB(t, seed.String())

	ids := func(users []gendemo.User) []int64 {
		out := make([]int64, len(users))
		for i, u := range users {
			out[i] = u.ID
		}
		return out
	}

	plain, err := d.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Limit(20).Offset(10).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Asking for a to-many relation must not turn a limit on parents into a
	// limit on parent-child pairs.
	withPosts, err := d.Users.Query().
		With(gendemo.Users.Posts).
		OrderBy(gendemo.Users.ID.Asc()).
		Limit(20).
		Offset(10).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// And neither must a to-one relation, even though it joins.
	withProfile, err := d.Users.Query().
		With(gendemo.Users.Profile).
		OrderBy(gendemo.Users.ID.Asc()).
		Limit(20).
		Offset(10).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	if len(plain) != 20 {
		t.Fatalf("the plain page holds %d users, want 20", len(plain))
	}
	if got, want := fmt.Sprint(ids(withPosts)), fmt.Sprint(ids(plain)); got != want {
		t.Errorf("with a to-many relation the page is %s, want %s", got, want)
	}
	if got, want := fmt.Sprint(ids(withProfile)), fmt.Sprint(ids(plain)); got != want {
		t.Errorf("with a to-one relation the page is %s, want %s", got, want)
	}
}

func TestWith_selfRelation(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	users, err := d.Users.Query().
		With(gendemo.Users.Manager, gendemo.Users.Reports).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// User 1 manages 2 and 3 and reports to nobody.
	manager, loaded := users[0].Manager.Get()
	if !loaded || manager != nil {
		t.Errorf("user 1 manager = %v (loaded %v), want absent", manager, loaded)
	}
	reports, loaded := users[0].Reports.Get()
	if !loaded || len(reports) != 2 {
		t.Fatalf("user 1 has %d reports (loaded %v), want 2", len(reports), loaded)
	}

	// Users 2 and 3 report to user 1 and manage nobody.
	for _, i := range []int{1, 2} {
		m, loaded := users[i].Manager.Get()
		if !loaded || m == nil {
			t.Fatalf("user %d has manager %v, want user 1", users[i].ID, m)
		}
		if m.ID != 1 {
			t.Errorf("user %d reports to %d, want 1", users[i].ID, m.ID)
		}
		r, loaded := users[i].Reports.Get()
		if !loaded || len(r) != 0 {
			t.Errorf("user %d has %d reports, want none", users[i].ID, len(r))
		}
	}

	// The folded side joins a second occurrence of users, so the statement has
	// to name them apart.
	if c.count() != 2 {
		t.Errorf("ran %d statements, want 2:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
	if !strings.Contains(c.all()[0], `AS "_r0"`) {
		t.Errorf("the self-join has no alias:\n%s", c.all()[0])
	}
}

func TestWith_severalRelationsToOneTable(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	docs, err := d.Documents.Query().
		With(gendemo.Documents.Creator, gendemo.Documents.Editor).
		OrderBy(gendemo.Documents.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d documents, want 2", len(docs))
	}

	creator, _ := docs[0].Creator.Get()
	editor, _ := docs[0].Editor.Get()
	if creator == nil || creator.ID != 1 {
		t.Errorf("doc 1 creator = %v, want user 1", creator)
	}
	if editor == nil || editor.ID != 2 {
		t.Errorf("doc 1 editor = %v, want user 2", editor)
	}
	// documents.editor_id is not a field of Document at all, and the relation
	// still loads: the key lives in the schema, which is where it belongs.
	secondEditor, loaded := docs[1].Editor.Get()
	if !loaded {
		t.Fatal("doc 2 editor is unloaded")
	}
	if secondEditor != nil {
		t.Errorf("doc 2 editor = %v, want absent", secondEditor)
	}

	if c.count() != 1 {
		t.Errorf("ran %d statements, want 1:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
	// Two relations to one table need two occurrences of it.
	sql := c.all()[0]
	if !strings.Contains(sql, `AS "_r0"`) || !strings.Contains(sql, `AS "_r1"`) {
		t.Errorf("the two joins do not have distinct aliases:\n%s", sql)
	}
}

func TestWith_compositeKey(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	// The constraint pairs (branch_region, branch_code) with (region, code).
	// Sorting either side by name would pair code with region and attach every
	// branch to the wrong tenant — which the two tenants sharing a code are
	// here to catch.
	tenants, err := d.Tenants.Query().
		With(gendemo.Tenants.Branches).
		OrderBy(gendemo.Tenants.Region.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(tenants) != 2 {
		t.Fatalf("got %d tenants, want 2", len(tenants))
	}

	labels := func(bs []gendemo.Branch) string {
		out := make([]string, len(bs))
		for i, b := range bs {
			out[i] = b.Label
		}
		slicesSort(out)
		return strings.Join(out, ",")
	}

	eu, loaded := tenants[0].Branches.Get()
	if !loaded {
		t.Fatal("the eu tenant has unloaded branches")
	}
	if got := labels(eu); got != "Berlin,Paris" {
		t.Errorf("eu branches = %q, want Berlin,Paris", got)
	}
	us, _ := tenants[1].Branches.Get()
	if got := labels(us); got != "Austin" {
		t.Errorf("us branches = %q, want Austin", got)
	}

	if c.count() != 2 {
		t.Errorf("ran %d statements, want 2", c.count())
	}
	// Both key columns travel as arrays, matched by PostgreSQL rather than
	// compared in Go.
	if !strings.Contains(c.all()[1], "WITH ORDINALITY") {
		t.Errorf("the loader does not use ordinality:\n%s", c.all()[1])
	}
}

func TestWith_compositeKeyToOne(t *testing.T) {
	testdb.AdminDSN(t)
	d, _ := relDB(t)

	branches, err := d.Branches.Query().
		With(gendemo.Branches.Tenant).
		OrderBy(gendemo.Branches.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []string{"Acme Europe", "Acme Europe", "Acme America"}
	for i, b := range branches {
		tenant, loaded := b.Tenant.Get()
		if !loaded || tenant == nil {
			t.Fatalf("branch %d has tenant %v", b.ID, tenant)
		}
		if tenant.Name != want[i] {
			t.Errorf("branch %s belongs to %q, want %q", b.Label, tenant.Name, want[i])
		}
	}
}

func TestWith_unloadedUnlessRequested(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	user, err := d.Users.Query().Where(gendemo.Users.ID.Eq(1)).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	// Nothing loads itself. A relation nobody asked for stays unloaded rather
	// than fetching on first read, which is what turns a loop into a thousand
	// queries.
	if _, loaded := user.Posts.Get(); loaded {
		t.Error("Posts is loaded, but nothing asked for it")
	}
	if _, loaded := user.Profile.Get(); loaded {
		t.Error("Profile is loaded, but nothing asked for it")
	}
	if c.count() != 1 {
		t.Errorf("reading one user cost %d statements", c.count())
	}
}

func TestWith_one(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	user, err := d.Users.Query().
		With(gendemo.Users.Profile, gendemo.Users.Posts).
		Where(gendemo.Users.Email.Eq("a@example.com")).
		One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if profile, loaded := user.Profile.Get(); !loaded || profile == nil {
		t.Errorf("Profile = %v (loaded %v)", profile, loaded)
	}
	if posts, loaded := user.Posts.Get(); !loaded || len(posts) != 3 {
		t.Errorf("Posts = %d rows (loaded %v), want 3", len(posts), loaded)
	}
	if c.count() != 2 {
		t.Errorf("ran %d statements, want 2", c.count())
	}
}

func TestWith_oneDoesNotLoadRelationsItRejects(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	// A root that found nothing has no parents to load relations for, so the
	// batched statement is never run.
	_, err := d.Users.Query().
		With(gendemo.Users.Posts).
		Where(gendemo.Users.Email.Eq("nobody@example.com")).
		One(t.Context())
	if !errors.Is(err, orm.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if c.count() != 1 {
		t.Errorf("a not-found One ran %d statements, want 1:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}

	c.reset()
	_, err = d.Users.Query().
		With(gendemo.Users.Posts).
		Where(gendemo.Users.Active.Eq(true)).
		One(t.Context())
	if !errors.Is(err, orm.ErrMultipleRows) {
		t.Fatalf("error = %v, want ErrMultipleRows", err)
	}
	if c.count() != 1 {
		t.Errorf("an ambiguous One ran %d statements, want 1", c.count())
	}
}

func TestWith_countAndExistsIgnoreRelations(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	q := d.Users.Query().With(gendemo.Users.Posts, gendemo.Users.Profile).Where(gendemo.Users.Active.Eq(true))

	c.reset()
	n, err := q.Clone().Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
	// Counting asks a question about the root rows. Loading their relations
	// would be work whose result is thrown away.
	if c.count() != 1 {
		t.Errorf("Count ran %d statements, want 1:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
	for _, sql := range c.all() {
		if strings.Contains(sql, "ORDINALITY") || strings.Contains(sql, "LEFT JOIN") {
			t.Errorf("Count ran a relation statement:\n%s", sql)
		}
	}

	c.reset()
	ok, err := q.Clone().Exists(t.Context())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("Exists = false")
	}
	if c.count() != 1 {
		t.Errorf("Exists ran %d statements, want 1:\n%s", c.count(), strings.Join(c.all(), "\n"))
	}
}

func TestWith_rows(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	t.Run("a folded relation streams", func(t *testing.T) {
		c.reset()
		var seen int
		for u, err := range d.Users.Query().With(gendemo.Users.Profile).OrderBy(gendemo.Users.ID.Asc()).Rows(t.Context()) {
			if err != nil {
				t.Fatalf("Rows: %v", err)
			}
			if _, loaded := u.Profile.Get(); !loaded {
				t.Errorf("user %d has an unloaded profile", u.ID)
			}
			seen++
		}
		if seen != 3 {
			t.Errorf("streamed %d users, want 3", seen)
		}
		if c.count() != 1 {
			t.Errorf("streaming ran %d statements, want 1", c.count())
		}
	})

	t.Run("a batched relation is refused before anything runs", func(t *testing.T) {
		c.reset()
		var got error
		for _, err := range d.Users.Query().With(gendemo.Users.Posts).Rows(t.Context()) {
			got = err
			break
		}
		if !errors.Is(got, orm.ErrStreamingRelation) {
			t.Fatalf("error = %v, want ErrStreamingRelation", got)
		}
		if !strings.Contains(got.Error(), "Posts") {
			t.Errorf("error = %v, want it to name the relation", got)
		}
		// Refused, not quietly buffered.
		if c.count() != 0 {
			t.Errorf("the refused stream ran %d statements:\n%s", c.count(), strings.Join(c.all(), "\n"))
		}
	})
}

func TestWith_duplicateRequest(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	c.reset()
	_, err := d.Users.Query().With(gendemo.Users.Posts, gendemo.Users.Posts).All(t.Context())
	if err == nil {
		t.Fatal("the same relation was accepted twice")
	}
	if !strings.Contains(err.Error(), "more than once") {
		t.Errorf("error = %v", err)
	}
	if c.count() != 0 {
		t.Errorf("the refused query ran %d statements", c.count())
	}
}

func TestWith_forUpdateLocksRootRowsOnly(t *testing.T) {
	testdb.AdminDSN(t)
	d, c := relDB(t)

	// PostgreSQL refuses to lock the nullable side of an outer join, so the
	// statement has to name the table it means. That also keeps the lock off
	// tables the caller only asked to read.
	sql, _, err := d.Users.Query().With(gendemo.Users.Profile).ForUpdate().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `FOR UPDATE OF "users"`) {
		t.Errorf("sql = %s\nwant it to lock the root table only", sql)
	}

	c.reset()
	users, err := d.Users.Query().With(gendemo.Users.Profile).ForUpdate().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 3 {
		t.Errorf("locked %d users, want 3", len(users))
	}
}

func TestWith_jsonContract(t *testing.T) {
	testdb.AdminDSN(t)
	d, _ := relDB(t)

	type view struct {
		ID      int64                    `json:"id"`
		Posts   orm.Many[gendemo.Post]   `json:"posts,omitzero"`
		Profile orm.One[gendemo.Profile] `json:"profile,omitzero"`
	}
	encode := func(u gendemo.User) string {
		b, err := json.Marshal(view{ID: u.ID, Posts: u.Posts, Profile: u.Profile})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		return string(b)
	}

	// Unloaded relations are omitted entirely.
	plain, err := d.Users.Query().Where(gendemo.Users.ID.Eq(2)).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got := encode(plain); got != `{"id":2}` {
		t.Errorf("an unloaded relation encoded as %s", got)
	}

	// Loaded and empty is [], loaded and absent is null.
	loaded, err := d.Users.Query().
		With(gendemo.Users.Posts, gendemo.Users.Profile).
		Where(gendemo.Users.ID.Eq(2)).
		One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got := encode(loaded); got != `{"id":2,"posts":[],"profile":null}` {
		t.Errorf("loaded relations encoded as %s", got)
	}
}

func TestWith_relationFailureIsNotPartialSuccess(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+relSeed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	d := gendemo.New(conn)

	// Drop the table the relation reads, so the root statement succeeds and the
	// relation statement cannot.
	if _, err := conn.Exec(t.Context(), "DROP TABLE posts CASCADE"); err != nil {
		t.Fatalf("dropping posts: %v", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(t.Context())) }()

	users, err := d.Users.Query().With(gendemo.Users.Posts).All(t.Context())
	if err == nil {
		t.Fatal("All succeeded although the relation could not be loaded")
	}
	// Returning the roots with the relation quietly unloaded would look
	// exactly like never having asked for it.
	if users != nil {
		t.Errorf("All returned %d rows alongside its error", len(users))
	}
	if !strings.Contains(err.Error(), "Posts") {
		t.Errorf("error = %v, want it to name the relation", err)
	}
}

func slicesSort(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
