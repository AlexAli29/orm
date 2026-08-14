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
	"github.com/jackc/pgx/v5/pgxpool"
)

// The adversarial audit.
//
// These are not the tests that prove each feature works — those exist next to
// the milestone that added them. These are the ones that try to break the
// guarantees: pagination that a relation quietly changes, a count that a
// relation quietly loads for, a per-parent limit that is really a global one, a
// statement count that grows with the data, a connection that never comes back.
//
// Everything here runs against real PostgreSQL, because every guarantee under
// audit is a statement about what PostgreSQL does.

// bigDB seeds a graph large enough that a per-row query would be visible in the
// statement count and a global limit would be visible in the results.
//
// The shape is deliberately lopsided: one user with far more posts than the
// others, one with none at all, and comment counts that differ per post. A
// symmetric fixture hides exactly the bugs this file is looking for.
func bigDB(t *testing.T, users, postsPer, commentsPer int) (*gendemo.DB, *countingExecutor) {
	t.Helper()
	var b strings.Builder
	b.WriteString(schema(t))
	fmt.Fprintf(&b, "\nINSERT INTO users (id, email, age, tags, settings) SELECT g, 'u'||g||'@example.com', 20 + (g %% 40), '{}', '{}' FROM generate_series(1, %d) g;\n", users)
	// User 1 gets postsPer posts, user 2 gets none, everybody else gets three.
	fmt.Fprintf(&b, `INSERT INTO posts (id, author_id, title, published, created_at)
	  SELECT g, 1, 'p'||g, g %% 2 = 0, timestamptz '2024-01-01T00:00:00Z' + (g || ' minutes')::interval FROM generate_series(1, %d) g;`, postsPer)
	fmt.Fprintf(&b, `
	  INSERT INTO posts (id, author_id, title, published, created_at)
	  SELECT %d + g, 3 + (g %% %d), 'q'||g, true, timestamptz '2024-01-01T00:00:00Z' + (g || ' minutes')::interval
	  FROM generate_series(1, %d) g;`, postsPer, max(users-3, 1), 3*max(users-3, 1))
	fmt.Fprintf(&b, `
	  INSERT INTO comments (id, post_id, author_id, body, created_at)
	  SELECT g, 1 + (g %% %d), 1 + (g %% %d), 'c'||g, timestamptz '2024-01-01T00:00:00Z' + (g || ' seconds')::interval
	  FROM generate_series(1, %d) g;`, postsPer, users, commentsPer)
	b.WriteString("\nSELECT setval(pg_get_serial_sequence('users','id'), 100000);")
	b.WriteString("\nSELECT setval(pg_get_serial_sequence('posts','id'), 100000);")
	b.WriteString("\nSELECT setval(pg_get_serial_sequence('comments','id'), 100000);")

	dsn := testdb.Create(t, b.String())
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	return gendemo.New(ex), ex
}

// countingExecutor counts the statements a call actually sends.
type countingExecutor struct {
	orm.Executor
	n   int
	sql []string
}

func (c *countingExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	c.sql = append(c.sql, sql)
	return c.Executor.Query(ctx, sql, args...)
}

func (c *countingExecutor) reset() { c.n, c.sql = 0, nil }

// ---------------------------------------------------------------- Count

// Count counts exactly the rowset All would return: Limit and Offset are part
// of the question, OrderBy is not, and a relation is neither.
func TestAudit_countMatchesAll(t *testing.T) {
	testdb.AdminDSN(t)
	db, ex := bigDB(t, 12, 20, 40)

	base := func() *orm.Query[gendemo.User] {
		return db.Users.Query().Where(gendemo.Users.ID.Gt(1))
	}
	for _, tt := range []struct {
		name string
		q    func() *orm.Query[gendemo.User]
	}{
		{"no rows", func() *orm.Query[gendemo.User] { return db.Users.Query().Where(gendemo.Users.ID.Lt(0)) }},
		{"one row", func() *orm.Query[gendemo.User] { return db.Users.Query().Where(gendemo.Users.ID.Eq(1)) }},
		{"many rows", base},
		{"limit 0", func() *orm.Query[gendemo.User] { return base().Limit(0) }},
		{"limit 1", func() *orm.Query[gendemo.User] { return base().Limit(1) }},
		{"limit beyond the end", func() *orm.Query[gendemo.User] { return base().Limit(1000) }},
		{"offset beyond the end", func() *orm.Query[gendemo.User] { return base().Offset(1000) }},
		{"limit and offset", func() *orm.Query[gendemo.User] { return base().Limit(4).Offset(3) }},
		{"offset with an order", func() *orm.Query[gendemo.User] {
			return base().OrderBy(gendemo.Users.Email.Desc()).Offset(5)
		}},
		{"a semi-join", func() *orm.Query[gendemo.User] {
			return db.Users.Query().Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true)))
		}},
		{"a negative semi-join", func() *orm.Query[gendemo.User] {
			return db.Users.Query().Where(gendemo.Users.Posts.None())
		}},
	} {
		all, err := tt.q().All(t.Context())
		if err != nil {
			t.Fatalf("%s: All: %v", tt.name, err)
		}
		n, err := tt.q().Count(t.Context())
		if err != nil {
			t.Fatalf("%s: Count: %v", tt.name, err)
		}
		if int(n) != len(all) {
			t.Errorf("%s: Count = %d, All returned %d", tt.name, n, len(all))
		}
	}

	// A relation is not part of the rowset, and asking how many roots there are
	// must not load one.
	for _, tt := range []struct {
		name string
		q    *orm.Query[gendemo.User]
	}{
		{"With", db.Users.Query().With(gendemo.Users.Posts)},
		{"nested With", db.Users.Query().With(gendemo.Users.Posts.With(gendemo.Posts.Comments))},
		{"a configured relation", db.Users.Query().With(
			gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true)).Limit(2))},
	} {
		ex.reset()
		n, err := tt.q.Count(t.Context())
		if err != nil {
			t.Fatalf("%s: Count: %v", tt.name, err)
		}
		if n != 12 {
			t.Errorf("%s: Count = %d, want 12", tt.name, n)
		}
		if ex.n != 1 {
			t.Errorf("%s: Count ran %d statements, want 1:\n%s", tt.name, ex.n, strings.Join(ex.sql, "\n"))
		}
	}
}

// ---------------------------------------------------------------- Exists

func TestAudit_existsSemantics(t *testing.T) {
	testdb.AdminDSN(t)
	db, ex := bigDB(t, 12, 20, 40)

	for _, tt := range []struct {
		name string
		q    *orm.Query[gendemo.User]
		want bool
	}{
		{"rows", db.Users.Query(), true},
		{"no rows", db.Users.Query().Where(gendemo.Users.ID.Lt(0)), false},
		{"limit 0", db.Users.Query().Limit(0), false},
		{"limit 1", db.Users.Query().Limit(1), true},
		{"offset beyond the end", db.Users.Query().Offset(1000), false},
		{"offset inside", db.Users.Query().Offset(1), true},
		{"a semi-join that matches", db.Users.Query().Where(gendemo.Users.Posts.Any()), true},
		{"a semi-join that does not", db.Users.Query().
			Where(gendemo.Users.Posts.Any(gendemo.Posts.Title.Eq("nothing"))), false},
	} {
		got, err := tt.q.Exists(t.Context())
		if err != nil {
			t.Fatalf("%s: Exists: %v", tt.name, err)
		}
		if got != tt.want {
			t.Errorf("%s: Exists = %t, want %t", tt.name, got, tt.want)
		}
	}

	// A relation is ignored, and never loaded.
	ex.reset()
	if _, err := db.Users.Query().
		With(gendemo.Users.Posts.With(gendemo.Posts.Comments)).Exists(t.Context()); err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ex.n != 1 {
		t.Errorf("Exists ran %d statements, want 1", ex.n)
	}
}

// ---------------------------------------------------------------- One

func TestAudit_oneSemantics(t *testing.T) {
	testdb.AdminDSN(t)
	db, ex := bigDB(t, 12, 20, 40)

	for _, tt := range []struct {
		name string
		q    *orm.Query[gendemo.User]
		want error
	}{
		{"no rows", db.Users.Query().Where(gendemo.Users.ID.Lt(0)), orm.ErrNotFound},
		{"one row", db.Users.Query().Where(gendemo.Users.ID.Eq(1)), nil},
		{"many rows", db.Users.Query(), orm.ErrMultipleRows},
		{"limit 0", db.Users.Query().Limit(0), orm.ErrNotFound},
		{"offset beyond the end", db.Users.Query().Offset(1000), orm.ErrNotFound},
		{"one row through a folded relation", db.Users.Query().
			Where(gendemo.Users.ID.Eq(1)).With(gendemo.Users.Profile), nil},
		{"one row through a batched relation", db.Users.Query().
			Where(gendemo.Users.ID.Eq(1)).With(gendemo.Users.Posts), nil},
		{"one row through a nested tree", db.Users.Query().
			Where(gendemo.Users.ID.Eq(1)).With(gendemo.Users.Posts.With(gendemo.Posts.Comments)), nil},
		{"many rows through a nested tree", db.Users.Query().
			With(gendemo.Users.Posts.With(gendemo.Posts.Comments)), orm.ErrMultipleRows},
		{"a semi-join narrowing to one", db.Users.Query().
			Where(gendemo.Users.Posts.Any(gendemo.Posts.Title.Eq("p1"))), nil},
	} {
		_, err := tt.q.One(t.Context())
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: One = %v, want %v", tt.name, err, tt.want)
		}
	}

	// A root cardinality already known to be wrong must not pay for the tree.
	ex.reset()
	if _, err := db.Users.Query().
		With(gendemo.Users.Posts.With(gendemo.Posts.Comments)).One(t.Context()); !errors.Is(err, orm.ErrMultipleRows) {
		t.Fatalf("One = %v", err)
	}
	if ex.n != 1 {
		t.Errorf("a One that could not succeed ran %d statements, want 1:\n%s", ex.n, strings.Join(ex.sql, "\n"))
	}
}

// ---------------------------------------------------- root pagination

// The invariant: asking for relations must not change which roots come back.
//
// Every relation shape is tried against the same paginated query, and the root
// identity sequence has to be identical every time. A join that multiplied rows
// would fail this, and no amount of Go-side deduplication would fix it — the
// LIMIT would already have been applied to the wrong rowset.
func TestAudit_rootPaginationIsUnchangedByEveryRelationShape(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := bigDB(t, 12, 20, 40)

	for _, page := range []struct{ limit, offset int }{
		{3, 0}, {3, 2}, {1, 0}, {5, 7}, {4, 10}, {7, 0},
	} {
		base := func() *orm.Query[gendemo.User] {
			return db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Limit(page.limit).Offset(page.offset)
		}
		want, err := base().All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		wantIDs := ids(want)

		for _, tt := range []struct {
			name string
			q    *orm.Query[gendemo.User]
		}{
			{"has-one", base().With(gendemo.Users.Profile)},
			{"belongs-to", base().With(gendemo.Users.Manager)},
			{"has-many", base().With(gendemo.Users.Posts)},
			{"two siblings", base().With(gendemo.Users.Posts, gendemo.Users.Profile)},
			{"three siblings", base().With(gendemo.Users.Posts, gendemo.Users.Profile, gendemo.Users.Manager)},
			{"nested", base().With(gendemo.Users.Posts.With(gendemo.Posts.Comments))},
			{"nested three deep", base().With(
				gendemo.Users.Posts.With(gendemo.Posts.Comments.With(gendemo.Comments.Author)))},
			{"a filtered relation", base().With(gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true)))},
			{"an ordered relation", base().With(gendemo.Users.Posts.OrderBy(gendemo.Posts.CreatedAt.Desc()))},
			{"a limited relation", base().With(gendemo.Users.Posts.Limit(2))},
			{"a self relation", base().With(gendemo.Users.Manager.With(gendemo.Users.Manager))},
			{"a relation that matches nothing", base().With(
				gendemo.Users.Posts.Where(gendemo.Posts.Title.Eq("nothing")))},
			{"a semi-join alongside", db.Users.Query().
				Where(gendemo.Users.ID.Gt(0)).
				OrderBy(gendemo.Users.ID.Asc()).Limit(page.limit).Offset(page.offset).
				With(gendemo.Users.Posts)},
		} {
			got, err := tt.q.All(t.Context())
			if err != nil {
				t.Fatalf("limit %d offset %d, %s: All: %v", page.limit, page.offset, tt.name, err)
			}
			if fmt.Sprint(ids(got)) != fmt.Sprint(wantIDs) {
				t.Errorf("limit %d offset %d, %s: roots = %v, want %v",
					page.limit, page.offset, tt.name, ids(got), wantIDs)
			}
		}
	}
}

// The same invariant one level down: which posts a parent loads cannot depend
// on whether the posts' own relations were also asked for.
func TestAudit_parentRelationSelectionIsUnchangedByDescendants(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := bigDB(t, 12, 20, 40)

	titles := func(users []gendemo.User) string {
		var b strings.Builder
		for _, u := range users {
			posts, _ := u.Posts.Get()
			for _, p := range posts {
				fmt.Fprintf(&b, "%d:%s ", u.ID, p.Title)
			}
		}
		return b.String()
	}

	for _, rel := range []struct {
		name string
		with func() orm.Loader[gendemo.User]
	}{
		{"plain", func() orm.Loader[gendemo.User] { return gendemo.Users.Posts }},
		{"limited", func() orm.Loader[gendemo.User] {
			return gendemo.Users.Posts.OrderBy(gendemo.Posts.ID.Asc()).Limit(3)
		}},
		{"filtered and limited", func() orm.Loader[gendemo.User] {
			return gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true)).
				OrderBy(gendemo.Posts.ID.Asc()).Limit(2)
		}},
	} {
		alone, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).With(rel.with()).All(t.Context())
		if err != nil {
			t.Fatalf("%s: All: %v", rel.name, err)
		}
		withKids, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).
			With(rel.with().(interface {
				With(...orm.Loader[gendemo.Post]) orm.Rel[gendemo.User, gendemo.Post]
			}).With(gendemo.Posts.Comments)).All(t.Context())
		if err != nil {
			t.Fatalf("%s with descendants: All: %v", rel.name, err)
		}
		if a, b := titles(alone), titles(withKids); a != b {
			t.Errorf("%s: loading the descendants changed the parent selection:\n    %s\n    %s", rel.name, a, b)
		}
	}
}

// ------------------------------------------------------- per-parent limit

// Limit on a relation is per parent, and the asymmetric fixture is the point:
// a global limit would give the first parent everything and the rest nothing.
func TestAudit_relationLimitIsPerParent(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+`
		INSERT INTO users (id, email, age, tags, settings) VALUES
		  (1, 'a@example.com', 30, '{}', '{}'),
		  (2, 'b@example.com', 30, '{}', '{}'),
		  (3, 'c@example.com', 30, '{}', '{}'),
		  (4, 'd@example.com', 30, '{}', '{}');
		INSERT INTO posts (id, author_id, title, created_at)
		  SELECT g, 1, 'a'||g, timestamptz '2024-01-01T00:00:00Z' + (g||' minutes')::interval FROM generate_series(1, 100) g;
		INSERT INTO posts (id, author_id, title, created_at)
		  SELECT 100 + g, 2, 'b'||g, timestamptz '2024-01-01T00:00:00Z' + (g||' minutes')::interval FROM generate_series(1, 50) g;
		INSERT INTO posts (id, author_id, title, created_at)
		  SELECT 200 + g, 3, 'c'||g, timestamptz '2024-01-01T00:00:00Z' + (g||' minutes')::interval FROM generate_series(1, 3) g;
	`)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	ex := &countingExecutor{Executor: conn}
	db := gendemo.New(ex)

	users, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.OrderBy(gendemo.Posts.ID.Asc()).Limit(5)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := []int{5, 5, 3, 0}
	for i, u := range users {
		posts, ok := u.Posts.Get()
		if !ok {
			t.Fatalf("user %d has no posts loaded", u.ID)
		}
		if len(posts) != want[i] {
			t.Errorf("user %d loaded %d posts, want %d", u.ID, len(posts), want[i])
		}
		// The ordering decides which ones, and it has to hold inside each
		// parent rather than across the result as a whole.
		for j := 1; j < len(posts); j++ {
			if posts[j-1].ID >= posts[j].ID {
				t.Errorf("user %d: posts are not in order: %v", u.ID, posts)
				break
			}
		}
		if len(posts) > 0 && posts[0].AuthorID == nil {
			t.Errorf("user %d: a post came back with no author", u.ID)
		}
	}
	if ex.n != 2 {
		t.Errorf("a per-parent limit over 4 parents ran %d statements, want 2", ex.n)
	}

	// Descending order takes the other end, which proves the limit is applied
	// after the ordering rather than to whatever PostgreSQL returned first.
	users, err = db.Users.Query().Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.OrderBy(gendemo.Posts.ID.Desc()).Limit(3)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	posts, _ := users[0].Posts.Get()
	if len(posts) != 3 || posts[0].ID != 100 || posts[2].ID != 98 {
		t.Errorf("descending per-parent limit = %v", posts)
	}
}

// ---------------------------------------------------- relation locality

// A predicate inside With filters the relation, never the roots — at every
// depth. The fixture has parents that match, parents that do not, and parents
// with no children at all, because each of those is a different way to get it
// wrong.
func TestAudit_relationFilteringIsLocalAtEveryDepth(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := bigDB(t, 12, 20, 40)

	allUsers, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	filtered, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true)).
			With(gendemo.Posts.Comments.Where(gendemo.Comments.Body.Like("c1%")))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if fmt.Sprint(ids(allUsers)) != fmt.Sprint(ids(filtered)) {
		t.Fatalf("a relation predicate filtered the roots:\n    %v\n    %v", ids(filtered), ids(allUsers))
	}

	// A predicate that matches nothing anywhere still leaves every root, with
	// every relation loaded and empty.
	none, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.Where(gendemo.Posts.Title.Eq("nothing")).
			With(gendemo.Posts.Comments)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if fmt.Sprint(ids(none)) != fmt.Sprint(ids(allUsers)) {
		t.Errorf("an empty relation filtered the roots")
	}
	for _, u := range none {
		posts, ok := u.Posts.Get()
		if !ok {
			t.Errorf("user %d: posts unloaded, want loaded and empty", u.ID)
		}
		if len(posts) != 0 {
			t.Errorf("user %d: %d posts survived a predicate that matches nothing", u.ID, len(posts))
		}
	}

	// The nested predicate did not remove any post either.
	for _, u := range filtered {
		posts, _ := u.Posts.Get()
		for _, p := range posts {
			if !p.Published {
				t.Errorf("an unpublished post survived the relation predicate")
			}
			if _, ok := p.Comments.Get(); !ok {
				t.Errorf("post %d: comments unloaded", p.ID)
			}
		}
	}
}

// ------------------------------------------------- statement-count bounds

// The cost of a tree is the shape of the tree. Three root counts, one shape,
// one statement count.
func TestAudit_statementCountIsBoundedByTheGraph(t *testing.T) {
	testdb.AdminDSN(t)
	db, ex := bigDB(t, 100, 200, 2000)

	for _, roots := range []int{1, 10, 100} {
		ex.reset()
		users, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Limit(roots).
			With(gendemo.Users.Posts.With(gendemo.Posts.Comments.With(gendemo.Comments.Author))).
			All(t.Context())
		if err != nil {
			t.Fatalf("%d roots: All: %v", roots, err)
		}
		if len(users) != roots {
			t.Fatalf("%d roots: got %d", roots, len(users))
		}
		// One root statement and one per relation in the tree, whatever the
		// data underneath it.
		if ex.n != 4 {
			t.Errorf("%d roots ran %d statements, want 4:\n%s", roots, ex.n, strings.Join(ex.sql, "\n"))
		}
	}

	// The whole graph at once, to prove the count does not depend on the size
	// of the intermediate levels either.
	ex.reset()
	users, err := db.Users.Query().
		With(gendemo.Users.Posts.With(gendemo.Posts.Comments.With(gendemo.Comments.Author))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if ex.n != 4 {
		t.Errorf("the whole graph ran %d statements, want 4", ex.n)
	}
	var posts, comments int
	for _, u := range users {
		ps, _ := u.Posts.Get()
		posts += len(ps)
		for _, p := range ps {
			cs, _ := p.Comments.Get()
			comments += len(cs)
			for _, c := range cs {
				if _, ok := c.Author.Get(); !ok {
					t.Fatalf("comment %d: author unloaded", c.ID)
				}
			}
		}
	}
	if posts == 0 || comments == 0 {
		t.Fatalf("the fixture loaded %d posts and %d comments", posts, comments)
	}
}

// A level with no parents is work that does not exist, and must not become a
// statement asked on nobody's behalf.
func TestAudit_emptySubtreesRunNothing(t *testing.T) {
	testdb.AdminDSN(t)
	db, ex := bigDB(t, 12, 20, 40)

	for _, tt := range []struct {
		name string
		q    *orm.Query[gendemo.User]
		want int
	}{
		{"no roots", db.Users.Query().Where(gendemo.Users.ID.Lt(0)).
			With(gendemo.Users.Posts.With(gendemo.Posts.Comments)), 1},
		{"a relation limited to zero", db.Users.Query().
			With(gendemo.Users.Posts.Limit(0).With(gendemo.Posts.Comments)), 1},
		{"a relation that matches nothing", db.Users.Query().
			With(gendemo.Users.Posts.Where(gendemo.Posts.Title.Eq("nothing")).
				With(gendemo.Posts.Comments.With(gendemo.Comments.Author))), 2},
		{"roots limited to zero", db.Users.Query().Limit(0).
			With(gendemo.Users.Posts.With(gendemo.Posts.Comments)), 1},
	} {
		ex.reset()
		if _, err := tt.q.All(t.Context()); err != nil {
			t.Fatalf("%s: All: %v", tt.name, err)
		}
		if ex.n != tt.want {
			t.Errorf("%s ran %d statements, want %d:\n%s", tt.name, ex.n, tt.want, strings.Join(ex.sql, "\n"))
		}
	}
}

// ------------------------------------------------------------- streaming

// Rows is a stream: rows arrive as PostgreSQL produces them, stopping early
// releases the connection, and a tree that cannot stream says so before the
// statement runs rather than after.
func TestAudit_rowsStreamsAndReleases(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+`
		INSERT INTO users (id, email, age, tags, settings)
		  SELECT g, 'u'||g||'@example.com', 30, '{}', '{}' FROM generate_series(1, 5000) g;`)

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parsing the DSN: %v", err)
	}
	// Two connections, so a single leak makes the pool unusable long before
	// the loop below finishes.
	cfg.MaxConns = 2
	cfg.AfterConnect = gendemo.RegisterTypes
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a pool: %v", err)
	}
	defer pool.Close()
	db := gendemo.New(pool)

	for i := range 50 {
		switch i % 5 {
		case 0: // read one row and break
			n := 0
			for _, err := range db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Rows(t.Context()) {
				if err != nil {
					t.Fatalf("Rows: %v", err)
				}
				n++
				break
			}
			if n != 1 {
				t.Fatalf("read %d rows before breaking", n)
			}
		case 1: // read everything
			n := 0
			for _, err := range db.Users.Query().Rows(t.Context()) {
				if err != nil {
					t.Fatalf("Rows: %v", err)
				}
				n++
			}
			if n != 5000 {
				t.Fatalf("streamed %d rows, want 5000", n)
			}
		case 2: // cancel part way through
			ctx, cancel := context.WithCancel(t.Context())
			seen, failed := 0, false
			for _, err := range db.Users.Query().Rows(ctx) {
				if err != nil {
					failed = true
					break
				}
				seen++
				if seen == 3 {
					cancel()
				}
			}
			cancel()
			// Cancelling mid-stream either ends the iteration or surfaces the
			// cancellation. What must not happen is a connection staying out.
			_ = failed
		case 3: // a statement the server refuses
			for _, err := range orm.Raw[gendemo.User](db.Users, "SELECT no_such_column FROM users").Rows(t.Context()) {
				if err == nil {
					t.Fatal("a broken statement yielded a row")
				}
				break
			}
		case 4: // raw, stopped early
			for _, err := range orm.Raw[gendemo.User](db.Users, "SELECT * FROM users ORDER BY id").Rows(t.Context()) {
				if err != nil {
					t.Fatalf("Raw Rows: %v", err)
				}
				break
			}
		}
	}

	// Every connection came back, or acquiring both would block until the
	// context died.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conns := make([]*pgxpool.Conn, 0, 2)
	for range 2 {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("the pool never recovered: %v", err)
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		c.Release()
	}
	if used := pool.Stat().AcquiredConns(); used != 0 {
		t.Errorf("%d connections are still checked out", used)
	}
}

// A scan failure ends the iteration with an error rather than a partial result
// that looks complete.
func TestAudit_rowsSurfacesScanFailures(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := bigDB(t, 3, 3, 3)

	// The column list is right in number and wrong in type, which is a failure
	// pgx can only find while scanning.
	seen, got := 0, error(nil)
	for _, err := range orm.Raw[gendemo.Post](db.Posts,
		"SELECT id, author_id, 'not a bool'::text, score, title, created_at FROM posts ORDER BY id").Rows(t.Context()) {
		if err != nil {
			got = err
			break
		}
		seen++
	}
	if got == nil {
		t.Errorf("a scan failure yielded %d rows and no error", seen)
	}
}

// A tree that cannot stream is refused before the statement runs, at every
// depth rather than only at the top.
func TestAudit_rowsRefusesBatchedRelationsAtEveryDepth(t *testing.T) {
	testdb.AdminDSN(t)
	db, ex := bigDB(t, 5, 5, 5)

	for _, tt := range []struct {
		name string
		q    *orm.Query[gendemo.User]
		ok   bool
	}{
		{"a folded relation streams", db.Users.Query().With(gendemo.Users.Manager), true},
		// A relation nested under a folded one is loaded after the root rows,
		// so the tree as a whole cannot stream even though its first level
		// could. The README says so; this is the assertion behind it.
		{"a relation nested under a folded one does not", db.Users.Query().
			With(gendemo.Users.Manager.With(gendemo.Users.Manager)), false},
		{"a batched relation does not", db.Users.Query().With(gendemo.Users.Posts), false},
		{"a batched descendant does not", db.Users.Query().
			With(gendemo.Users.Manager.With(gendemo.Users.Posts)), false},
		{"a batched great-grandchild does not", db.Users.Query().
			With(gendemo.Users.Manager.With(gendemo.Users.Manager.With(gendemo.Users.Posts))), false},
	} {
		ex.reset()
		var err error
		for _, e := range tt.q.Rows(t.Context()) {
			err = e
			break
		}
		switch {
		case tt.ok && err != nil:
			t.Errorf("%s: Rows: %v", tt.name, err)
		case tt.ok:
		case err == nil:
			t.Errorf("%s: Rows streamed", tt.name)
		case !errors.Is(err, orm.ErrStreamingRelation):
			t.Errorf("%s: Rows = %v, want ErrStreamingRelation", tt.name, err)
		case ex.n != 0:
			t.Errorf("%s: refused after running %d statements", tt.name, ex.n)
		}
	}
}
