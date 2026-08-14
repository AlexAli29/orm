package gendemo_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// Nested relation loading, against real PostgreSQL.
//
// The claim under test throughout is that the cost of a requested tree follows
// the shape of the tree and not the number of rows in it, and that every level
// keeps its own filter, its own ordering and its own limit.

// nestSeed builds a graph with a shape worth asserting: user 1 has posts with
// comments and reactions, user 2 has posts with no comments, user 3 has no
// posts at all. Profiles and avatars give the three to-one states — present,
// present-but-childless, and absent — and the tenants give a composite key at
// two levels.
const nestSeed = `
INSERT INTO users (id, email, age, active, state, manager_id, tags, settings) VALUES
  (1, 'a@example.com', 40, true,  'active',  NULL, '{}', '{}'),
  (2, 'b@example.com', 30, true,  'active',  1,    '{}', '{}'),
  (3, 'c@example.com', 20, false, 'pending', 2,    '{}', '{}');

INSERT INTO avatars (id, url) VALUES (1, 'https://example.com/a.png');

-- User 1 has a profile with an avatar, user 2 a profile without one, and user 3
-- no profile at all.
INSERT INTO profiles (id, user_id, bio, avatar_id) VALUES
  (1, 1, 'the first bio', 1),
  (2, 2, NULL, NULL);

INSERT INTO categories (id, name) VALUES (1, 'general'), (2, 'internal');

INSERT INTO posts (id, author_id, title, published, score, category_id, created_at) VALUES
  (1, 1, 'a-old-published', true,  50,  1,    '2024-01-01T00:00:00Z'),
  (2, 1, 'a-mid-published', true,  150, 2,    '2024-03-01T00:00:00Z'),
  (3, 1, 'a-new-published', true,  250, NULL, '2024-05-01T00:00:00Z'),
  (4, 1, 'a-draft',         false, 999, 1,    '2024-06-01T00:00:00Z'),
  (5, 2, 'b-published',     true,  10,  1,    '2024-02-01T00:00:00Z');

-- Post 1 carries visible and hidden comments; post 2 only hidden ones; posts 3
-- to 5 none. Comment authors are users 2 and 3, and one comment has no author.
INSERT INTO comments (id, post_id, author_id, body, visible, score, created_at) VALUES
  (1, 1, 2,    'c-old-visible', true,  5,  '2024-01-02T00:00:00Z'),
  (2, 1, 3,    'c-new-visible', true,  9,  '2024-01-04T00:00:00Z'),
  (3, 1, 2,    'c-hidden',      false, 1,  '2024-01-03T00:00:00Z'),
  (4, 1, NULL, 'c-anonymous',   true,  2,  '2024-01-05T00:00:00Z'),
  (5, 2, 2,    'c-only-hidden', false, 0,  '2024-03-02T00:00:00Z'),
  (6, 3, 3,    'c-on-newest',   true,  7,  '2024-05-02T00:00:00Z');

INSERT INTO reactions (id, comment_id, author_id, kind) VALUES
  (1, 1, 1, 'up'),
  (2, 1, 2, 'down'),
  (3, 2, 1, 'up');

INSERT INTO documents (id, title, creator_id, editor_id) VALUES
  (1, 'doc one', 1, 2),
  (2, 'doc two', 2, 3),
  (3, 'doc three', 3, NULL);

INSERT INTO tenants (region, code, name) VALUES
  ('eu', 'acme', 'Acme Europe'),
  ('us', 'acme', 'Acme America'),
  ('eu', 'none', 'Empty Europe');

INSERT INTO branches (id, branch_region, branch_code, label) VALUES
  (1, 'eu', 'acme', 'Berlin'),
  (2, 'eu', 'acme', 'Paris'),
  (3, 'us', 'acme', 'Austin');

INSERT INTO branch_events (id, event_region, event_code, label) VALUES
  (1, 'eu', 'acme', 'launch'),
  (2, 'eu', 'acme', 'review'),
  (3, 'us', 'acme', 'kickoff');

SELECT setval(pg_get_serial_sequence('users', 'id'), (SELECT max(id) FROM users));
SELECT setval(pg_get_serial_sequence('posts', 'id'), (SELECT max(id) FROM posts));
SELECT setval(pg_get_serial_sequence('comments', 'id'), (SELECT max(id) FROM comments));
SELECT setval(pg_get_serial_sequence('reactions', 'id'), (SELECT max(id) FROM reactions));
SELECT setval(pg_get_serial_sequence('profiles', 'id'), (SELECT max(id) FROM profiles));
SELECT setval(pg_get_serial_sequence('avatars', 'id'), (SELECT max(id) FROM avatars));
SELECT setval(pg_get_serial_sequence('categories', 'id'), (SELECT max(id) FROM categories));
SELECT setval(pg_get_serial_sequence('documents', 'id'), (SELECT max(id) FROM documents));
SELECT setval(pg_get_serial_sequence('branches', 'id'), (SELECT max(id) FROM branches));
SELECT setval(pg_get_serial_sequence('branch_events', 'id'), (SELECT max(id) FROM branch_events));
`

func nestDB(t *testing.T) (*gendemo.DB, *counter, *pgx.Conn) {
	t.Helper()
	return tracedDB(t, nestSeed)
}

// posts reads a user's loaded posts, insisting the relation was loaded at all.
func posts(t *testing.T, u gendemo.User) []gendemo.Post {
	t.Helper()
	rows, ok := u.Posts.Get()
	if !ok {
		t.Fatalf("user %d: Posts is not loaded", u.ID)
	}
	return rows
}

// comments reads a post's loaded comments.
func comments(t *testing.T, p gendemo.Post) []gendemo.Comment {
	t.Helper()
	rows, ok := p.Comments.Get()
	if !ok {
		t.Fatalf("post %d: Comments is not loaded", p.ID)
	}
	return rows
}

func bodies(cs []gendemo.Comment) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

// Two levels, plain. Every level is loaded for every parent, including the
// parents nothing matched.
func TestNested_basic(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.With(gendemo.Posts.Comments)).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1, 2, 3}) {
		t.Fatalf("returned %v, want every user", got)
	}
	if got := c.count(); got != 3 {
		t.Errorf("ran %d statements, want the root and one per level:\n%s", got, strings.Join(c.all(), "\n"))
	}

	by := byID(t, users)
	if got := len(posts(t, by[1])); got != 4 {
		t.Errorf("user 1 has %d posts, want 4", got)
	}
	// A user with no posts has the relation loaded and empty, and there is
	// nothing below it to load.
	if got := posts(t, by[3]); len(got) != 0 {
		t.Errorf("user 3 posts = %v, want none", got)
	}
	// Posts 1, 2 and 3 have comments; post 4 has none, loaded.
	want := map[int64]int{1: 4, 2: 1, 3: 1}
	for _, p := range posts(t, by[1]) {
		if got := len(comments(t, p)); got != want[p.ID] {
			t.Errorf("post %d has %d comments, want %d", p.ID, got, want[p.ID])
		}
	}
	// A post whose comments were never asked about is a different state from a
	// post that has none.
	plain, err := db.Users.Query().With(gendemo.Users.Posts).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, u := range plain {
		for _, p := range posts(t, u) {
			if !p.Comments.IsZero() {
				t.Errorf("post %d: Comments is loaded, but only Posts was asked for", p.ID)
			}
		}
	}
}

// Four levels, with the values checked rather than only the statement count.
// Comment carries neither of its foreign keys as a field, so two consecutive
// levels read their keys from the statement that produced their parents.
func TestNested_fourLevels(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.
			OrderBy(gendemo.Posts.ID.Asc()).
			With(gendemo.Posts.Comments.
				OrderBy(gendemo.Comments.ID.Asc()).
				With(gendemo.Comments.Reactions.
					OrderBy(gendemo.Reactions.ID.Asc()).
					With(gendemo.Reactions.Author)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := c.count(); got != 5 {
		t.Errorf("ran %d statements, want the root and one per level:\n%s", got, strings.Join(c.all(), "\n"))
	}
	if len(users) != 1 {
		t.Fatalf("returned %d users, want one", len(users))
	}

	ps := posts(t, users[0])
	if len(ps) != 4 {
		t.Fatalf("user 1 has %d posts, want 4", len(ps))
	}
	cs := comments(t, ps[0])
	if got := bodies(cs); !equal(got, []string{"c-old-visible", "c-new-visible", "c-hidden", "c-anonymous"}) {
		t.Fatalf("post 1 comments = %v", got)
	}

	// Comment 1 has two reactions, comment 2 one, the rest none.
	wantReactions := []int{2, 1, 0, 0}
	for i, cm := range cs {
		rs, ok := cm.Reactions.Get()
		if !ok {
			t.Fatalf("comment %d: Reactions is not loaded", cm.ID)
		}
		if len(rs) != wantReactions[i] {
			t.Errorf("comment %d has %d reactions, want %d", cm.ID, len(rs), wantReactions[i])
		}
		for _, r := range rs {
			a, ok := r.Author.Get()
			if !ok || a == nil {
				t.Errorf("reaction %d: author = %v, %v; want the row it points at", r.ID, a, ok)
			}
		}
	}
	// Reaction 1 is by user 1 and reaction 2 by user 2, four levels down.
	first, _ := cs[0].Reactions.Get()
	a0, _ := first[0].Author.Get()
	a1, _ := first[1].Author.Get()
	if a0 == nil || a1 == nil || a0.Email != "a@example.com" || a1.Email != "b@example.com" {
		t.Errorf("reaction authors = %v, %v; want users 1 and 2", a0, a1)
	}
}

// The flagship query: every option at two levels, and a to-one below them.
func TestNested_options(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		With(gendemo.Users.Posts.
			Where(gendemo.Posts.Published.Eq(true)).
			OrderBy(gendemo.Posts.CreatedAt.Desc()).
			Limit(2).
			With(gendemo.Posts.Comments.
				Where(gendemo.Comments.Visible.Eq(true)).
				OrderBy(gendemo.Comments.CreatedAt.Desc()).
				Limit(10).
				With(gendemo.Comments.Author))).
		OrderBy(gendemo.Users.ID.Asc()).
		Limit(50).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := c.count(); got != 4 {
		t.Errorf("ran %d statements, want the root and one per level:\n%s", got, strings.Join(c.all(), "\n"))
	}
	// A relation filter narrows the relation, never the roots, at any level.
	if got := ids(users); !equalInts(got, []int64{1, 2, 3}) {
		t.Fatalf("returned %v, want every user", got)
	}
	by := byID(t, users)

	// User 1 has three published posts and asked for two, newest first.
	ps := posts(t, by[1])
	if len(ps) != 2 {
		t.Fatalf("user 1 loaded %d posts, want the two the limit allows: %+v", len(ps), ps)
	}
	if ps[0].Title != "a-new-published" || ps[1].Title != "a-mid-published" {
		t.Errorf("user 1 posts = %v, want the two most recent published ones", []string{ps[0].Title, ps[1].Title})
	}
	for _, p := range ps {
		if !p.Published {
			t.Errorf("post %d is not published but was loaded", p.ID)
		}
	}
	// The comments of each post are filtered and ordered on their own terms,
	// not the posts'. The newest post has one visible comment; the one below it
	// has none.
	if got := bodies(comments(t, ps[0])); !equal(got, []string{"c-on-newest"}) {
		t.Errorf("post %d comments = %v", ps[0].ID, got)
	}
	if got := len(comments(t, ps[1])); got != 0 {
		t.Errorf("post %d loaded %d comments, want none", ps[1].ID, got)
	}
	single, err := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.
			Where(gendemo.Posts.ID.Eq(1)).
			With(gendemo.Posts.Comments.
				Where(gendemo.Comments.Visible.Eq(true)).
				OrderBy(gendemo.Comments.CreatedAt.Desc()).
				With(gendemo.Comments.Author))).
		One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	cs := comments(t, posts(t, single)[0])
	if got := bodies(cs); !equal(got, []string{"c-anonymous", "c-new-visible", "c-old-visible"}) {
		t.Errorf("comments = %v, want the visible ones newest first", got)
	}
	// The anonymous comment has no author, which is loaded-absent rather than
	// unloaded; the others point at the row they name.
	for _, cm := range cs {
		a, ok := cm.Author.Get()
		if !ok {
			t.Fatalf("comment %d: Author is not loaded", cm.ID)
		}
		if (a != nil) != (cm.Body != "c-anonymous") {
			t.Errorf("comment %q author = %v", cm.Body, a)
		}
	}
	// User 2 has one published post with no visible comments; user 3 has none.
	if got := len(posts(t, by[2])); got != 1 {
		t.Errorf("user 2 loaded %d posts, want one", got)
	}
	if got := len(posts(t, by[3])); got != 0 {
		t.Errorf("user 3 loaded %d posts, want none", got)
	}
}

// Loading a level's own relations must not change which rows that level
// selected. The parent's filter, ordering and limit are decided before anything
// below it runs.
func TestNested_descendantsDoNotChangeTheirParents(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := nestDB(t)

	base := func(with ...orm.Loader[gendemo.Post]) []gendemo.User {
		t.Helper()
		users, err := db.Users.Query().
			OrderBy(gendemo.Users.ID.Asc()).
			With(gendemo.Users.Posts.
				Where(gendemo.Posts.Published.Eq(true)).
				OrderBy(gendemo.Posts.CreatedAt.Desc()).
				Limit(2).
				With(with...)).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return users
	}

	shallow := base()
	deep := base(gendemo.Posts.Comments.Where(gendemo.Comments.Visible.Eq(true)))
	if len(shallow) != len(deep) {
		t.Fatalf("root counts differ: %d and %d", len(shallow), len(deep))
	}
	for i := range shallow {
		a, b := posts(t, shallow[i]), posts(t, deep[i])
		if len(a) != len(b) {
			t.Fatalf("user %d loaded %d posts without a nested relation and %d with", shallow[i].ID, len(a), len(b))
		}
		for j := range a {
			if a[j].ID != b[j].ID {
				t.Errorf("user %d post %d = %d without a nested relation and %d with; a descendant changed its parent's selection",
					shallow[i].ID, j, a[j].ID, b[j].ID)
			}
		}
	}
}

// Root pagination is the root query's own, whatever any relation asks for.
func TestNested_rootPaginationIsUnchanged(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, conn := nestDB(t)

	execRaw(t, conn, `INSERT INTO users (id, email, age, active, state, tags, settings)
		SELECT g, 'u' || g || '@example.com', 20, true, 'active', '{}', '{}' FROM generate_series(10, 60) AS g`)

	base, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Limit(20).Offset(10).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	nested, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).Limit(20).Offset(10).
		With(gendemo.Users.Posts.
			OrderBy(gendemo.Posts.CreatedAt.Desc()).
			Limit(5).
			With(gendemo.Posts.Comments.
				OrderBy(gendemo.Comments.CreatedAt.Desc()).
				Limit(10).
				With(gendemo.Comments.Author))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All with a nested tree: %v", err)
	}
	if got, want := ids(nested), ids(base); !equalInts(got, want) {
		t.Errorf("root ids = %v, want %v", got, want)
	}
}

// The number of statements follows the shape of the tree. It must not follow
// the number of rows at any level.
func TestNested_statementCountIsIndependentOfRowCount(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, conn := nestDB(t)

	execRaw(t, conn, `INSERT INTO users (id, email, age, active, state, tags, settings)
		SELECT g, 'u' || g || '@example.com', 20, true, 'active', '{}', '{}' FROM generate_series(1000, 1100) AS g`)
	execRaw(t, conn, `INSERT INTO posts (author_id, title, published, created_at)
		SELECT u.id, 'p', true, now() FROM users AS u, generate_series(1, 5)`)
	execRaw(t, conn, `INSERT INTO comments (post_id, author_id, body, visible)
		SELECT p.id, 1, 'c', true FROM posts AS p, generate_series(1, 4)`)

	for _, roots := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("%d roots", roots), func(t *testing.T) {
			c.reset()
			users, err := db.Users.Query().
				OrderBy(gendemo.Users.ID.Asc()).
				Limit(roots).
				With(gendemo.Users.Posts.With(
					gendemo.Posts.Comments.With(gendemo.Comments.Author))).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if len(users) == 0 {
				t.Fatal("no users came back")
			}
			if got := c.count(); got != 4 {
				t.Errorf("%d roots cost %d statements, want 4:\n%s", len(users), got, strings.Join(c.all(), "\n"))
			}
		})
	}
}

// Siblings at one level are independent requests, and each costs what its own
// strategy costs. A folded to-one costs nothing; everything else costs one.
func TestNested_siblingStatementCount(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(
			gendemo.Users.Posts.With(gendemo.Posts.Comments, gendemo.Posts.Category),
			gendemo.Users.Profile,
		).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// The root and Posts, then Comments and Category. Profile is a plain to-one
	// at the root, so it rides in the root statement.
	if got := c.count(); got != 4 {
		t.Errorf("ran %d statements, want 4:\n%s", got, strings.Join(c.all(), "\n"))
	}
	by := byID(t, users)
	if _, ok := by[1].Profile.Get(); !ok {
		t.Error("user 1: Profile is not loaded")
	}
	for _, p := range posts(t, by[1]) {
		if _, ok := p.Category.Get(); !ok {
			t.Errorf("post %d: Category is not loaded", p.ID)
		}
		if _, ok := p.Comments.Get(); !ok {
			t.Errorf("post %d: Comments is not loaded", p.ID)
		}
	}
	// Post 3 has no category, which is loaded-absent.
	for _, p := range posts(t, by[1]) {
		cat, _ := p.Category.Get()
		if (cat != nil) != (p.ID != 3) {
			t.Errorf("post %d category = %v", p.ID, cat)
		}
	}
}

// Both consecutive levels here read their keys from the statement that produced
// their parents: Comment maps neither post_id nor author_id.
func TestNested_unmappedForeignKeys(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.
			OrderBy(gendemo.Posts.ID.Asc()).
			With(gendemo.Posts.Comments.
				OrderBy(gendemo.Comments.ID.Asc()).
				With(gendemo.Comments.Author))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := c.count(); got != 4 {
		t.Errorf("ran %d statements, want 4:\n%s", got, strings.Join(c.all(), "\n"))
	}
	cs := comments(t, posts(t, users[0])[0])
	if len(cs) != 4 {
		t.Fatalf("post 1 has %d comments, want 4", len(cs))
	}
	// Comments 1 and 3 are by user 2, comment 2 by user 3, comment 4 by nobody.
	wantEmail := []string{"b@example.com", "c@example.com", "b@example.com", ""}
	for i, cm := range cs {
		a, ok := cm.Author.Get()
		if !ok {
			t.Fatalf("comment %d: Author is not loaded", cm.ID)
		}
		got := ""
		if a != nil {
			got = a.Email
		}
		if got != wantEmail[i] {
			t.Errorf("comment %d author = %q, want %q", cm.ID, got, wantEmail[i])
		}
	}
	// The statement that loaded the comments had to select the key columns the
	// entity does not carry.
	if !strings.Contains(c.all()[2], `"comments"."author_id"`) {
		t.Errorf("the comments statement does not select the key its child needs:\n%s", c.all()[2])
	}
}

// A composite relation at two levels, each pairing its own key columns in its
// own constraint order.
func TestNested_compositeKeys(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	tenants, err := db.Tenants.Query().
		OrderBy(gendemo.Tenants.Region.Asc(), gendemo.Tenants.Code.Asc()).
		With(
			gendemo.Tenants.Branches.OrderBy(gendemo.Branches.Label.Asc()).Limit(1),
			gendemo.Tenants.Events.OrderBy(gendemo.BranchEvents.Label.Asc()),
		).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := c.count(); got != 3 {
		t.Errorf("ran %d statements, want the root and one per relation:\n%s", got, strings.Join(c.all(), "\n"))
	}
	if len(tenants) != 3 {
		t.Fatalf("returned %d tenants, want three", len(tenants))
	}
	// eu/acme: one branch under the limit, and both of its events.
	bs, ok := tenants[0].Branches.Get()
	if !ok || len(bs) != 1 || bs[0].Label != "Amsterdam" && bs[0].Label != "Berlin" {
		t.Errorf("eu/acme branches = %+v", bs)
	}
	evs, ok := tenants[0].Events.Get()
	if !ok || len(evs) != 2 {
		t.Fatalf("eu/acme events = %+v", evs)
	}
	if evs[0].Label != "launch" || evs[1].Label != "review" {
		t.Errorf("eu/acme events = %v, want them alphabetical", []string{evs[0].Label, evs[1].Label})
	}
	// eu/none shares a region with eu/acme and a code with nothing, so a
	// correlation that paired the columns the other way round would give it
	// rows it must not have.
	if evs, _ := tenants[1].Events.Get(); len(evs) != 0 {
		t.Errorf("eu/none events = %+v, want none", evs)
	}
	if evs, _ := tenants[2].Events.Get(); len(evs) != 1 || evs[0].Label != "kickoff" {
		t.Errorf("us/acme events = %+v", evs)
	}
}

// A finite chain of a relation to its own table is finite because it was
// written that way. Nothing recurses on its own.
func TestNested_selfRelation(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(3)).
		With(gendemo.Users.Manager.With(gendemo.Users.Manager.With(gendemo.Users.Manager))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("returned %d users, want one", len(users))
	}
	// User 3 reports to 2, who reports to 1, who reports to nobody. The first
	// level folds into the root statement; the two below it cost one each.
	if got := c.count(); got != 3 {
		t.Errorf("ran %d statements, want 3:\n%s", got, strings.Join(c.all(), "\n"))
	}
	m1, ok := users[0].Manager.Get()
	if !ok || m1 == nil || m1.ID != 2 {
		t.Fatalf("manager = %v, want user 2", m1)
	}
	m2, ok := m1.Manager.Get()
	if !ok || m2 == nil || m2.ID != 1 {
		t.Fatalf("manager's manager = %v, want user 1", m2)
	}
	m3, ok := m2.Manager.Get()
	if !ok {
		t.Fatal("the third level is not loaded")
	}
	if m3 != nil {
		t.Errorf("user 1 has manager %v, want none", m3)
	}
}

// Two branches reaching the same entity are two different requests, and neither
// one's rows may end up on the other.
func TestNested_repeatedTargetOnDifferentBranches(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	docs, err := db.Documents.Query().
		OrderBy(gendemo.Documents.ID.Asc()).
		With(
			gendemo.Documents.Creator.With(gendemo.Users.Profile),
			gendemo.Documents.Editor.With(gendemo.Users.Posts.Limit(1)),
		).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("returned %d documents, want three", len(docs))
	}
	// Document 1's creator is user 1 and its editor user 2; document 3 has no
	// editor at all.
	creator, ok := docs[0].Creator.Get()
	if !ok || creator == nil || creator.ID != 1 {
		t.Fatalf("document 1 creator = %v", creator)
	}
	if p, ok := creator.Profile.Get(); !ok || p == nil {
		t.Errorf("the creator's profile = %v, %v; want the row it has", p, ok)
	}
	// The creator branch asked for a profile and not for posts, so the posts
	// stay unloaded even though the editor branch asked for them.
	if !creator.Posts.IsZero() {
		t.Error("the creator gained the editor branch's relation")
	}
	editor, ok := docs[0].Editor.Get()
	if !ok || editor == nil || editor.ID != 2 {
		t.Fatalf("document 1 editor = %v", editor)
	}
	if ps, ok := editor.Posts.Get(); !ok || len(ps) != 1 {
		t.Errorf("the editor's posts = %v, %v; want the one the limit allows", ps, ok)
	}
	if !editor.Profile.IsZero() {
		t.Error("the editor gained the creator branch's relation")
	}
	if _, ok := docs[2].Editor.Get(); !ok {
		t.Error("document 3: Editor is not loaded")
	}
}

// A to-one that matched nothing has no entity below it, so there is nothing for
// its own relations to be loaded onto. That is correct rather than missing.
func TestNested_absentToOneHasNoChildren(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Profile.With(gendemo.Profiles.Avatar)).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Profile is a plain to-one at the root and folds; Avatar reads a key
	// Profile does not map, so Profile gives up the fold to select it.
	if got := c.count(); got != 3 {
		t.Errorf("ran %d statements, want 3:\n%s", got, strings.Join(c.all(), "\n"))
	}
	by := byID(t, users)

	p1, ok := by[1].Profile.Get()
	if !ok || p1 == nil {
		t.Fatalf("user 1 profile = %v, %v", p1, ok)
	}
	if a, ok := p1.Avatar.Get(); !ok || a == nil {
		t.Errorf("user 1 avatar = %v, %v; want the row it points at", a, ok)
	}
	p2, ok := by[2].Profile.Get()
	if !ok || p2 == nil {
		t.Fatalf("user 2 profile = %v, %v", p2, ok)
	}
	if a, ok := p2.Avatar.Get(); !ok || a != nil {
		t.Errorf("user 2 avatar = %v, %v; want loaded and absent", a, ok)
	}
	p3, ok := by[3].Profile.Get()
	if !ok {
		t.Fatal("user 3: Profile is not loaded")
	}
	if p3 != nil {
		t.Errorf("user 3 profile = %v, want absent", p3)
	}
}

// A relation that loaded nothing has no parents for its own relations, so the
// level below it is not work that was skipped — it does not exist.
func TestNested_emptyParentShortCircuit(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	tests := []struct {
		name string
		rel  orm.Rel[gendemo.User, gendemo.Post]
		want int
	}{
		{
			name: "no rows matched",
			rel:  gendemo.Users.Posts.Where(gendemo.Posts.Title.Eq("nothing matches this")).With(gendemo.Posts.Comments),
			want: 2,
		},
		{
			name: "no rows asked for",
			rel:  gendemo.Users.Posts.Limit(0).With(gendemo.Posts.Comments.With(gendemo.Comments.Author)),
			want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.reset()
			users, err := db.Users.Query().With(tt.rel).All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if got := c.count(); got != tt.want {
				t.Errorf("ran %d statements, want %d:\n%s", got, tt.want, strings.Join(c.all(), "\n"))
			}
			for _, u := range users {
				if got := posts(t, u); len(got) != 0 {
					t.Errorf("user %d loaded %d posts, want none", u.ID, len(got))
				}
			}
		})
	}
}

// Rows streams from the root statement, so it is allowed only when the whole
// requested tree arrives with it. A relation that folds but whose own relations
// do not is still a query that cannot stream.
func TestNested_rowsRejectsABatchedDescendant(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	tests := []struct {
		name string
		q    func() *orm.Query[gendemo.User]
		want error
	}{
		{
			name: "a plain to-one still streams",
			q:    func() *orm.Query[gendemo.User] { return db.Users.Query().With(gendemo.Users.Profile) },
		},
		{
			name: "a to-one whose own relation batches does not",
			q: func() *orm.Query[gendemo.User] {
				return db.Users.Query().With(gendemo.Users.Profile.With(gendemo.Profiles.Avatar))
			},
			want: orm.ErrStreamingRelation,
		},
		{
			name: "nor does one three levels down",
			q: func() *orm.Query[gendemo.User] {
				return db.Users.Query().With(gendemo.Users.Posts.With(gendemo.Posts.Comments))
			},
			want: orm.ErrStreamingRelation,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c.reset()
			var got error
			for _, err := range tt.q().Rows(t.Context()) {
				if err != nil {
					got = err
					break
				}
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("Rows: %v", got)
				}
				return
			}
			if !errors.Is(got, tt.want) {
				t.Fatalf("error = %v, want %v", got, tt.want)
			}
			// Refused before anything was sent, which is what "do not buffer"
			// means in practice.
			if c.count() != 0 {
				t.Errorf("ran %d statements before refusing:\n%s", c.count(), strings.Join(c.all(), "\n"))
			}
		})
	}
}

// With controls materialisation, however deep. Count and Exists return no
// entities, so there is nothing for a relation to be attached to.
func TestNested_countAndExistsIgnoreTheTree(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	tree := gendemo.Users.Posts.
		Where(gendemo.Posts.Published.Eq(true)).
		Limit(5).
		With(gendemo.Posts.Comments.With(gendemo.Comments.Author))

	c.reset()
	n, err := db.Users.Query().With(tree).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want every user", n)
	}
	if got := c.count(); got != 1 {
		t.Errorf("Count ran %d statements, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}

	c.reset()
	found, err := db.Users.Query().With(tree).Exists(t.Context())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !found {
		t.Error("Exists = false, want true")
	}
	if got := c.count(); got != 1 {
		t.Errorf("Exists ran %d statements, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}

	// A semi-join is a different thing: it is part of the root predicate, so it
	// must still be counted.
	n, err = db.Users.Query().
		Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).
		With(tree).
		Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Errorf("Count = %d, want the users with a published post", n)
	}
}

// One keeps its contract, and nothing below the root runs until the root is
// known to be exactly one row.
func TestNested_one(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	tree := gendemo.Users.Posts.Limit(5).With(gendemo.Posts.Comments.Limit(10).With(gendemo.Comments.Author))

	c.reset()
	u, err := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).With(tree).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if u.ID != 1 || len(posts(t, u)) != 4 {
		t.Errorf("One returned user %d with %d posts", u.ID, len(posts(t, u)))
	}
	if got := c.count(); got != 4 {
		t.Errorf("ran %d statements, want 4:\n%s", got, strings.Join(c.all(), "\n"))
	}

	// Nothing matched, so nothing below the root was worth asking about.
	c.reset()
	_, err = db.Users.Query().Where(gendemo.Users.Email.Eq("nobody@example.com")).With(tree).One(t.Context())
	if !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if got := c.count(); got != 1 {
		t.Errorf("ran %d statements for a root that matched nothing, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}

	c.reset()
	_, err = db.Users.Query().With(tree).One(t.Context())
	if !errors.Is(err, orm.ErrMultipleRows) {
		t.Errorf("error = %v, want ErrMultipleRows", err)
	}
	if got := c.count(); got != 1 {
		t.Errorf("ran %d statements for an ambiguous root, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}
}

// Locking the roots locks the roots. A relation read alongside them is not
// locked as a side effect of reading it, at any depth.
func TestNested_forUpdateLocksRootRowsOnly(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	if _, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		ForUpdate().
		With(gendemo.Users.Posts.With(gendemo.Posts.Comments)).
		All(t.Context()); err != nil {
		t.Fatalf("All: %v", err)
	}
	stmts := c.all()
	if len(stmts) != 3 {
		t.Fatalf("ran %d statements, want 3:\n%s", len(stmts), strings.Join(stmts, "\n"))
	}
	if !strings.Contains(stmts[0], "FOR UPDATE") {
		t.Errorf("the root statement does not lock its rows:\n%s", stmts[0])
	}
	for _, s := range stmts[1:] {
		if strings.Contains(s, "FOR UPDATE") {
			t.Errorf("a relation statement locks rows nobody asked to lock:\n%s", s)
		}
	}
}

// A raw fragment works at any level, and each statement numbers its own
// parameters: the key array comes first, so the fragment's own $1 becomes $2.
func TestNested_exprInANestedFilter(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.
			Where(gendemo.Posts.ID.Eq(1)).
			With(gendemo.Posts.Comments.
				Where(orm.Expr[gendemo.Comment](`"comments"."score" > $1`, 4)).
				OrderBy(gendemo.Comments.ID.Asc()))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	cs := comments(t, posts(t, users[0])[0])
	if got := bodies(cs); !equal(got, []string{"c-old-visible", "c-new-visible"}) {
		t.Errorf("comments = %v, want the two scoring over 4", got)
	}
	if !strings.Contains(c.all()[2], `"comments"."score" > $2`) {
		t.Errorf("the fragment was not renumbered past the key array:\n%s", c.all()[2])
	}
}

// The same request produces the same statements, byte for byte, every time.
// Anything read from a map would eventually not.
func TestNested_sqlIsDeterministic(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	run := func() []string {
		c.reset()
		_, err := db.Users.Query().
			OrderBy(gendemo.Users.ID.Asc()).
			With(
				gendemo.Users.Posts.
					Where(gendemo.Posts.Published.Eq(true)).
					OrderBy(gendemo.Posts.CreatedAt.Desc()).
					Limit(5).
					With(
						gendemo.Posts.Comments.OrderBy(gendemo.Comments.ID.Asc()).With(gendemo.Comments.Author),
						gendemo.Posts.Category,
					),
				gendemo.Users.Profile,
			).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		return c.all()
	}
	first := run()
	for range 3 {
		if got := run(); !equal(got, first) {
			t.Fatalf("statements differ between runs:\n%s\n\n%s", strings.Join(first, "\n"), strings.Join(got, "\n"))
		}
	}
	// Deterministic includes the order they ran in: level by level, and within
	// a level in the order they were asked for.
	if len(first) != 5 {
		t.Fatalf("ran %d statements, want 5:\n%s", len(first), strings.Join(first, "\n"))
	}
	for i, want := range []string{`FROM "public"."users"`, `"public"."posts"`, `"public"."comments"`, `"public"."categories"`, `"public"."users"`} {
		if !strings.Contains(first[i], want) {
			t.Errorf("statement %d does not read %s:\n%s", i, want, first[i])
		}
	}
}

// A semi-join over a relation of a relation falls out of the typing: the inner
// predicate is one over the outer relation's target, which is what the outer
// Any takes.
func TestNested_semiJoinComposition(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := nestDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.Posts.Any(gendemo.Posts.Comments.Any(gendemo.Comments.Visible.Eq(true)))).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Only user 1 has a post with a visible comment.
	if got := ids(users); !equalInts(got, []int64{1}) {
		t.Errorf("returned %v, want the user with a visibly commented post", got)
	}
	if got := c.count(); got != 1 {
		t.Errorf("ran %d statements, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}
	if users[0].Posts.IsZero() != true {
		t.Error("Posts is loaded, but filtering by a relation is not asking for it")
	}
}

// Configuring one branch of a tree must not reach another, and a clone of a
// query must share no part of it.
func TestNested_configurationIsIsolated(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := nestDB(t)

	base := gendemo.Users.Posts.With(gendemo.Posts.Comments)
	limited := base.Limit(1)
	withCategory := base.With(gendemo.Posts.Category)

	users, err := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).With(base).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := len(posts(t, users[0])); got != 4 {
		t.Errorf("the base loaded %d posts, want all of them; a branch's limit reached it", got)
	}
	for _, p := range posts(t, users[0]) {
		if !p.Category.IsZero() {
			t.Error("the base gained a branch's relation")
		}
	}

	users, err = db.Users.Query().Where(gendemo.Users.ID.Eq(1)).With(limited).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := len(posts(t, users[0])); got != 1 {
		t.Errorf("the limited branch loaded %d posts, want one", got)
	}

	users, err = db.Users.Query().Where(gendemo.Users.ID.Eq(1)).With(withCategory).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range posts(t, users[0]) {
		if p.Category.IsZero() {
			t.Errorf("post %d: the branch's own relation is missing", p.ID)
		}
	}

	// A clone shares no part of the other's tree either.
	q := db.Users.Query().With(base)
	a, b := q.Clone(), q.Clone()
	if _, err := a.All(t.Context()); err != nil {
		t.Fatalf("All on the first clone: %v", err)
	}
	if _, err := b.All(t.Context()); err != nil {
		t.Fatalf("All on the second clone: %v", err)
	}
}
