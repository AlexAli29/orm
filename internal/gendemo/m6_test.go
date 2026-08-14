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

// Relation options and semi-joins, against real PostgreSQL.
//
// Two claims are being separated throughout, because confusing them is the
// mistake M6 is most able to cause: a filter inside With narrows what is
// loaded, and a semi-join in Where narrows which roots come back. Every test
// here asserts one of them and denies the other.

// optSeed gives three users with deliberately different relation shapes:
// A has published and unpublished posts, B has only unpublished ones, and C has
// none at all. The timestamps interleave across users, so a relation ordering
// that leaked into the root — or a global limit pretending to be per-parent —
// changes the answer.
const optSeed = `
INSERT INTO users (id, email, age, active, state, tags, settings) VALUES
  (1, 'a@example.com', 40, true,  'active',  '{}', '{}'),
  (2, 'b@example.com', 30, false, 'active',  '{}', '{}'),
  (3, 'c@example.com', 20, true,  'pending', '{}', '{}');

INSERT INTO posts (id, author_id, title, published, score, created_at) VALUES
  (1, 1, 'a-old-published',   true,  50,  '2024-01-01T00:00:00Z'),
  (2, 1, 'a-mid-published',   true,  150, '2024-03-01T00:00:00Z'),
  (3, 1, 'a-new-published',   true,  250, '2024-05-01T00:00:00Z'),
  (4, 1, 'a-draft',           false, 999, '2024-06-01T00:00:00Z'),
  (5, 2, 'b-draft-one',       false, 10,  '2024-02-01T00:00:00Z'),
  (6, 2, 'b-draft-two',       false, 20,  '2024-04-01T00:00:00Z');

INSERT INTO profiles (id, user_id, bio) VALUES
  (1, 1, 'the first bio');

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
  (3, 'eu', 'acme', 'Amsterdam'),
  (4, 'us', 'acme', 'Austin');

-- The team slug is citext, so PostgreSQL matches these case-insensitively and
-- a Go comparison of the same two strings would not.
INSERT INTO teams (slug, name) VALUES
  ('Alpha', 'Team Alpha'),
  ('Beta',  'Team Beta');

INSERT INTO members (id, team_slug, nickname) VALUES
  (1, 'ALPHA', 'ana'),
  (2, 'alpha', 'bo'),
  (3, 'BETA',  'cy');
`

func optDB(t *testing.T) (*gendemo.DB, *counter, *pgx.Conn) {
	t.Helper()
	return tracedDB(t, optSeed)
}

// titles reads a user's loaded posts, insisting the relation was loaded at all.
func titles(t *testing.T, u gendemo.User) []string {
	t.Helper()
	posts, ok := u.Posts.Get()
	if !ok {
		t.Fatalf("user %d: Posts is not loaded", u.ID)
	}
	out := make([]string, 0, len(posts))
	for _, p := range posts {
		out = append(out, p.Title)
	}
	return out
}

func byID(t *testing.T, users []gendemo.User) map[int64]gendemo.User {
	t.Helper()
	out := make(map[int64]gendemo.User, len(users))
	for _, u := range users {
		out[u.ID] = u
	}
	return out
}

func ids(users []gendemo.User) []int64 {
	out := make([]int64, 0, len(users))
	for _, u := range users {
		out = append(out, u.ID)
	}
	return out
}

// A filter inside With narrows the relation and nothing else. Every root the
// query would have returned is still returned, and a root with no matching
// related row gets a loaded, empty relation rather than disappearing.
func TestRelWhere_filtersTheRelationAndNotTheRoots(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); len(got) != 3 {
		t.Fatalf("returned users %v, want all three; a relation filter must not filter roots", got)
	}
	by := byID(t, users)

	if got := titles(t, by[1]); len(got) != 3 || strings.Contains(strings.Join(got, ","), "draft") {
		t.Errorf("user 1 posts = %v, want only the published ones", got)
	}
	// B has posts, none published; C has none at all. Both are loaded and
	// empty, which is a different fact from unloaded.
	for _, id := range []int64{2, 3} {
		if got := titles(t, by[id]); len(got) != 0 {
			t.Errorf("user %d posts = %v, want none", id, got)
		}
		if by[id].Posts.IsZero() {
			t.Errorf("user %d: the relation reads as unloaded, but it was asked for and found nothing", id)
		}
	}
}

// Several predicates compose, and the values reach PostgreSQL as parameters in
// the right order behind the key array.
func TestRelWhere_composedPredicates(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.
			Where(gendemo.Posts.Published.Eq(true)).
			Where(gendemo.Posts.Score.Gte(150))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("returned %d users, want one", len(users))
	}
	got := titles(t, users[0])
	if len(got) != 2 {
		t.Fatalf("posts = %v, want the two published posts scoring at least 150", got)
	}
}

// A raw fragment inside a relation filter has to be renumbered past the key
// array, which already occupies $1.
func TestRelWhere_expr(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		With(gendemo.Users.Posts.Where(orm.Expr[gendemo.Post](`"posts"."score" > $1`, 200))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	got := titles(t, users[0])
	if len(got) != 2 {
		t.Fatalf("posts = %v, want the two scoring over 200", got)
	}
}

// The ordering applies inside each parent's relation. The root order is the
// root query's own, and the two do not interfere.
func TestRelOrderBy_appliesWithinEachParent(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Desc()).
		With(gendemo.Users.Posts.OrderBy(gendemo.Posts.CreatedAt.Desc())).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); len(got) != 3 || got[0] != 3 || got[2] != 1 {
		t.Fatalf("root order = %v, want it descending and untouched by the relation's ordering", got)
	}
	by := byID(t, users)
	want := []string{"a-draft", "a-new-published", "a-mid-published", "a-old-published"}
	if got := titles(t, by[1]); !equal(got, want) {
		t.Errorf("user 1 posts = %v, want %v", got, want)
	}
	if got, want := titles(t, by[2]), []string{"b-draft-two", "b-draft-one"}; !equal(got, want) {
		t.Errorf("user 2 posts = %v, want %v", got, want)
	}
}

// The limit counts each parent's rows. A limit applied to the statement instead
// would give the first parent everything and every later parent nothing, which
// looks like missing data rather than a mistake.
func TestRelLimit_isPerParent(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, conn := optDB(t)

	// Ten, eight and three posts, so a global limit of five would be obvious.
	var b strings.Builder
	id := 100
	for _, spec := range []struct {
		user, n int
	}{{1, 10}, {2, 8}, {3, 3}} {
		for i := range spec.n {
			id++
			fmt.Fprintf(&b, "INSERT INTO posts (id, author_id, title, published, created_at) VALUES (%d, %d, 'p%d', true, '2025-01-%02dT00:00:00Z');\n",
				id, spec.user, id, i+1)
		}
	}
	execRaw(t, conn, "DELETE FROM posts")
	execRaw(t, conn, b.String())

	c.reset()
	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.
			OrderBy(gendemo.Posts.CreatedAt.Desc()).
			Limit(5)).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	by := byID(t, users)
	for _, want := range []struct {
		user int64
		n    int
	}{{1, 5}, {2, 5}, {3, 3}} {
		if got := titles(t, by[want.user]); len(got) != want.n {
			t.Errorf("user %d loaded %d posts, want %d: %v", want.user, len(got), want.n, got)
		}
	}
	// Each parent's rows are in the order asked for, which for a descending
	// timestamp means the newest first.
	for _, u := range users {
		posts, _ := u.Posts.Get()
		for i := 1; i < len(posts); i++ {
			if posts[i-1].CreatedAt.Before(posts[i].CreatedAt) {
				t.Errorf("user %d relation is not ordered: %v then %v", u.ID, posts[i-1].CreatedAt, posts[i].CreatedAt)
			}
		}
	}
	if got := c.count(); got != 2 {
		t.Errorf("ran %d statements, want the root and one relation:\n%s", got, strings.Join(c.all(), "\n"))
	}
}

// The number of statements is a property of how many relations were asked for,
// never of how many rows came back.
func TestRelLimit_statementCountIsIndependentOfParentCount(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, conn := optDB(t)

	execRaw(t, conn, "DELETE FROM posts")
	execRaw(t, conn, `INSERT INTO users (id, email, age, active, state, tags, settings)
		SELECT g, 'u' || g || '@example.com', 20, true, 'active', '{}', '{}' FROM generate_series(1000, 2000) AS g`)
	execRaw(t, conn, `INSERT INTO posts (author_id, title, published, created_at)
		SELECT u.id, 'p', true, now() FROM users AS u, generate_series(1, 3)`)

	for _, limit := range []int{1, 10, 100, 1000} {
		t.Run(fmt.Sprintf("%d parents", limit), func(t *testing.T) {
			c.reset()
			users, err := db.Users.Query().
				OrderBy(gendemo.Users.ID.Asc()).
				Limit(limit).
				With(gendemo.Users.Posts.
					Where(gendemo.Posts.Published.Eq(true)).
					OrderBy(gendemo.Posts.CreatedAt.Desc()).
					Limit(2)).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			if len(users) == 0 {
				t.Fatal("no users came back")
			}
			if got := c.count(); got != 2 {
				t.Errorf("%d parents cost %d statements, want 2:\n%s", len(users), got, strings.Join(c.all(), "\n"))
			}
		})
	}
}

// Relation options must not reach the root statement. The roots a paginated
// query returns are the same whether or not their relations were asked for.
func TestRelOptions_rootPaginationIsUnchanged(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, conn := optDB(t)

	execRaw(t, conn, `INSERT INTO users (id, email, age, active, state, tags, settings)
		SELECT g, 'u' || g || '@example.com', 20, true, 'active', '{}', '{}' FROM generate_series(10, 60) AS g`)

	base, err := db.Users.Query().OrderBy(gendemo.Users.ID.Asc()).Limit(20).Offset(10).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	withPosts, err := db.Users.Query().
		With(gendemo.Users.Posts.
			Where(gendemo.Posts.Published.Eq(true)).
			OrderBy(gendemo.Posts.CreatedAt.Desc()).
			Limit(5)).
		OrderBy(gendemo.Users.ID.Asc()).Limit(20).Offset(10).All(t.Context())
	if err != nil {
		t.Fatalf("All with relations: %v", err)
	}
	if got, want := ids(withPosts), ids(base); !equalInts(got, want) {
		t.Errorf("root ids = %v, want %v; relation options must not touch the root", got, want)
	}
}

// Two configured relations in one query are independent: neither one's filter,
// ordering or limit reaches the other, and each costs one statement.
func TestRelOptions_severalRelationsAreIndependent(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, conn := optDB(t)
	execRaw(t, conn, "UPDATE users SET manager_id = 1 WHERE id IN (2, 3)")

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.ID.Eq(1)).
		With(
			gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true)).Limit(2),
			gendemo.Users.Reports.OrderBy(gendemo.Users.ID.Desc()),
		).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := c.count(); got != 3 {
		t.Errorf("ran %d statements, want the root and one per relation:\n%s", got, strings.Join(c.all(), "\n"))
	}
	if got := titles(t, users[0]); len(got) != 2 {
		t.Errorf("posts = %v, want the limit to apply to this relation", got)
	}
	reports, ok := users[0].Reports.Get()
	if !ok {
		t.Fatal("Reports is not loaded")
	}
	// The second relation has its own ordering and no limit. If the first
	// relation's options had reached it, it would be capped at two rows in
	// ascending order rather than reporting both descending.
	if len(reports) != 2 || reports[0].ID != 3 || reports[1].ID != 2 {
		t.Errorf("reports = %v, want both, newest id first", ids(reports))
	}
}

// Configuring one relation two different ways is still one relation. Running
// both and letting one overwrite the other would make the result depend on the
// order the options were written in.
func TestRelOptions_duplicateWithIsRefused(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	c.reset()
	_, err := db.Users.Query().
		With(gendemo.Users.Posts.Limit(5), gendemo.Users.Posts.Limit(10)).
		All(t.Context())
	if err == nil {
		t.Fatal("All succeeded with one relation requested twice")
	}
	if !strings.Contains(err.Error(), "requested more than once") {
		t.Errorf("error = %v, want it to name the repeated relation", err)
	}
	if c.count() != 0 {
		t.Errorf("ran %d statements for a query that cannot be built", c.count())
	}
}

// A per-parent limit of zero is a relation whose answer is known. It is still
// loaded — the caller asked for it — and it costs no statement.
func TestRelLimit_zeroLoadsEmptyWithoutAStatement(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	c.reset()
	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Posts.Limit(0)).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := c.count(); got != 1 {
		t.Errorf("ran %d statements, want only the root:\n%s", got, strings.Join(c.all(), "\n"))
	}
	for _, u := range users {
		posts, ok := u.Posts.Get()
		if !ok {
			t.Errorf("user %d: the relation is unloaded, but zero rows were asked for and delivered", u.ID)
		}
		if len(posts) != 0 {
			t.Errorf("user %d loaded %d posts under a limit of zero", u.ID, len(posts))
		}
	}
}

// A configured to-one relation loads separately, and the filter decides whether
// the relation is there rather than whether the root row is.
func TestRelOptions_configuredHasOne(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	c.reset()
	users, err := db.Users.Query().
		OrderBy(gendemo.Users.ID.Asc()).
		With(gendemo.Users.Profile.Where(gendemo.Profiles.Bio.IsNotNull())).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("returned %d users, want all three", len(users))
	}
	if got := c.count(); got != 2 {
		t.Errorf("ran %d statements, want the root and the relation:\n%s", got, strings.Join(c.all(), "\n"))
	}
	p, ok := users[0].Profile.Get()
	if !ok || p == nil {
		t.Fatalf("user 1 profile = %v, %v; want the matching row", p, ok)
	}
	for _, u := range users[1:] {
		p, ok := u.Profile.Get()
		if !ok {
			t.Errorf("user %d: the relation is unloaded", u.ID)
		}
		if p != nil {
			t.Errorf("user %d profile = %+v, want absent", u.ID, p)
		}
	}
}

// The same for a belongs-to, where the correlation runs the other way.
func TestRelOptions_configuredBelongsTo(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	posts, err := db.Posts.Query().
		OrderBy(gendemo.Posts.ID.Asc()).
		With(gendemo.Posts.Author.Where(gendemo.Users.Active.Eq(true))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(posts) != 6 {
		t.Fatalf("returned %d posts, want all six", len(posts))
	}
	for _, p := range posts {
		a, ok := p.Author.Get()
		if !ok {
			t.Fatalf("post %d: Author is not loaded", p.ID)
		}
		// Users 1 and 3 are active; user 2 is not, so its posts get an absent
		// author rather than vanishing.
		wantAuthor := p.AuthorID != nil && *p.AuthorID != 2
		if (a != nil) != wantAuthor {
			t.Errorf("post %d author = %v, want present == %v", p.ID, a, wantAuthor)
		}
	}
}

// A to-one relation whose parent key the entity does not map cannot be batched:
// there is nowhere to read the key from. It stays in the join, with the filter
// in the join condition, where it makes the relation absent rather than the
// root row missing.
func TestRelOptions_configuredUnmappedForeignKey(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	c.reset()
	docs, err := db.Documents.Query().
		OrderBy(gendemo.Documents.ID.Asc()).
		With(gendemo.Documents.Editor.Where(gendemo.Users.Active.Eq(true))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("returned %d documents, want all three", len(docs))
	}
	// One statement, because the relation is read through the root's own join.
	if got := c.count(); got != 1 {
		t.Errorf("ran %d statements, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}
	// Document 1's editor is user 2, who is not active; document 2's is user 3,
	// who is; document 3 has no editor at all.
	want := []bool{false, true, false}
	for i, d := range docs {
		e, ok := d.Editor.Get()
		if !ok {
			t.Fatalf("document %d: Editor is not loaded", d.ID)
		}
		if (e != nil) != want[i] {
			t.Errorf("document %d editor = %v, want present == %v", d.ID, e, want[i])
		}
	}
}

// Ordering and limiting a relation read through a join is refused rather than
// ignored: a join has no per-parent ordering to apply.
func TestRelOptions_unmappedForeignKeyRefusesOrderAndLimit(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	for _, tt := range []struct {
		name string
		rel  orm.Rel[gendemo.Document, gendemo.User]
		want string
	}{
		{name: "ordered", rel: gendemo.Documents.Editor.OrderBy(gendemo.Users.ID.Asc()), want: "cannot be ordered"},
		{name: "limited", rel: gendemo.Documents.Editor.Limit(1), want: "cannot be limited"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c.reset()
			_, err := db.Documents.Query().With(tt.rel).All(t.Context())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			if c.count() != 0 {
				t.Errorf("ran %d statements for a query that cannot be built", c.count())
			}
		})
	}
}

// A composite relation matches every key component in constraint order, and the
// options travel with it.
func TestRelOptions_compositeKey(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	tenants, err := db.Tenants.Query().
		OrderBy(gendemo.Tenants.Region.Asc(), gendemo.Tenants.Code.Asc()).
		With(gendemo.Tenants.Branches.
			OrderBy(gendemo.Branches.Label.Asc()).
			Limit(2)).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(tenants) != 3 {
		t.Fatalf("returned %d tenants, want three", len(tenants))
	}
	labels := func(tn gendemo.Tenant) []string {
		bs, ok := tn.Branches.Get()
		if !ok {
			t.Fatalf("tenant %s/%s: Branches is not loaded", tn.Region, tn.Code)
		}
		out := make([]string, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.Label)
		}
		return out
	}
	// eu/acme has three branches and is capped at two, alphabetically; eu/none
	// has none; us/acme has one.
	if got, want := labels(tenants[0]), []string{"Amsterdam", "Berlin"}; !equal(got, want) {
		t.Errorf("eu/acme branches = %v, want %v", got, want)
	}
	if got := labels(tenants[1]); len(got) != 0 {
		t.Errorf("eu/none branches = %v, want none", got)
	}
	if got, want := labels(tenants[2]), []string{"Austin"}; !equal(got, want) {
		t.Errorf("us/acme branches = %v, want %v", got, want)
	}
}

func TestRelOptions_compositeSemiJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	withAny, err := db.Tenants.Query().
		Where(gendemo.Tenants.Branches.Any()).
		OrderBy(gendemo.Tenants.Region.Asc(), gendemo.Tenants.Code.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(withAny) != 2 {
		t.Fatalf("Any returned %d tenants, want the two with branches", len(withAny))
	}
	withNone, err := db.Tenants.Query().Where(gendemo.Tenants.Branches.None()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(withNone) != 1 || withNone[0].Code != "none" {
		t.Fatalf("None returned %+v, want only the tenant with no branches", withNone)
	}
	// A composite correlation that paired the columns the other way round would
	// match nothing here, because no branch has region 'acme'.
	filtered, err := db.Tenants.Query().
		Where(gendemo.Tenants.Branches.Any(gendemo.Branches.Label.Eq("Austin"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Region != "us" {
		t.Fatalf("filtered = %+v, want only us/acme", filtered)
	}
}

// citext compares case-insensitively, which Go's string equality does not.
// Matching these rows in Go would drop every one of them.
func TestRelOptions_postgresDecidesEquality(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	teams, err := db.Teams.Query().OrderBy(gendemo.Teams.Slug.Asc()).
		With(gendemo.Teams.Members.OrderBy(gendemo.Members.Nickname.Asc())).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(teams) != 2 {
		t.Fatalf("returned %d teams, want two", len(teams))
	}
	members, ok := teams[0].Members.Get()
	if !ok {
		t.Fatal("Members is not loaded")
	}
	// The team's slug is 'Alpha' and its members' are 'ALPHA' and 'alpha'.
	if len(members) != 2 {
		t.Errorf("team %q loaded %d members, want 2; PostgreSQL matches citext case-insensitively", teams[0].Slug, len(members))
	}
	// The same equality has to decide the semi-join.
	withMembers, err := db.Teams.Query().Where(gendemo.Teams.Members.Any()).All(t.Context())
	if err != nil {
		t.Fatalf("Any: %v", err)
	}
	if len(withMembers) != 2 {
		t.Errorf("Any returned %d teams, want both", len(withMembers))
	}
}

// Any filters the roots and loads nothing. Reading the relation as well means
// asking for it as well.
func TestAny_filtersRootsWithoutLoading(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); len(got) != 1 || got[0] != 1 {
		t.Fatalf("returned %v, want only the user with a published post", got)
	}
	if !users[0].Posts.IsZero() {
		t.Error("Posts is loaded, but filtering by a relation is not asking for it")
	}
	if got := c.count(); got != 1 {
		t.Errorf("ran %d statements, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}
}

func TestAny_withoutPredicates(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	users, err := db.Users.Query().
		Where(gendemo.Users.Posts.Any()).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1, 2}) {
		t.Errorf("returned %v, want the users with any post at all", got)
	}
}

func TestNone_withAndWithoutPredicates(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	unpublished, err := db.Users.Query().
		Where(gendemo.Users.Posts.None(gendemo.Posts.Published.Eq(true))).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// B has only drafts and C has nothing; both have no published post.
	if got := ids(unpublished); !equalInts(got, []int64{2, 3}) {
		t.Errorf("returned %v, want the users with no published post", got)
	}

	postless, err := db.Users.Query().
		Where(gendemo.Users.Posts.None()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(postless); !equalInts(got, []int64{3}) {
		t.Errorf("returned %v, want the user with no posts", got)
	}
}

// A to-one relation is a relation like any other, so it answers existence
// questions too. Users with no profile is a useful thing to ask.
func TestNone_onAToOneRelation(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	users, err := db.Users.Query().
		Where(gendemo.Users.Profile.None()).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{2, 3}) {
		t.Errorf("returned %v, want the users with no profile", got)
	}
}

// A belongs-to whose foreign key is NULL has no related row, so PostgreSQL
// answers Any with false and None with true without anything in Go deciding it.
func TestAny_onABelongsToWithANullKey(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, conn := optDB(t)

	execRaw(t, conn, `INSERT INTO posts (id, author_id, title, published, created_at)
		VALUES (99, NULL, 'orphan', true, now())`)

	active, err := db.Posts.Query().
		Where(gendemo.Posts.Author.Any(gendemo.Users.Active.Eq(true))).
		OrderBy(gendemo.Posts.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, p := range active {
		if p.ID == 99 {
			t.Error("the post with no author matched Any")
		}
	}
	orphaned, err := db.Posts.Query().Where(gendemo.Posts.Author.None()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(orphaned) != 1 || orphaned[0].ID != 99 {
		t.Errorf("None returned %+v, want the post with no author", ids2(orphaned))
	}
}

// The two concerns compose without either one leaking into the other: the roots
// are filtered by the semi-join, and the loaded relation is filtered by its own
// options.
func TestAny_withWith(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	c.reset()
	users, err := db.Users.Query().
		Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).
		With(gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1}) {
		t.Fatalf("returned %v, want only the user with a published post", got)
	}
	if got := titles(t, users[0]); len(got) != 3 {
		t.Errorf("posts = %v, want the three published ones", got)
	}
	// The existence test is part of the root statement, and the relation is one
	// more. Reusing the subquery as the relation's own would be a cleverness
	// nobody asked for.
	if got := c.count(); got != 2 {
		t.Errorf("ran %d statements, want the root and the relation:\n%s", got, strings.Join(c.all(), "\n"))
	}
}

// A semi-join is part of the root predicate, so unlike With it does affect
// counting and existence.
func TestAny_countAndExists(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	n, err := db.Users.Query().Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("Count = %d, want 1; Any is a root predicate and must be counted", n)
	}

	found, err := db.Users.Query().Where(gendemo.Users.Posts.None()).Exists(t.Context())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !found {
		t.Error("Exists = false, want true; one user has no posts")
	}

	// With, by contrast, is ignored by both, and no relation statement runs.
	c.reset()
	n, err = db.Users.Query().
		With(gendemo.Users.Posts.Where(gendemo.Posts.Published.Eq(true)).Limit(5)).
		Count(t.Context())
	if err != nil {
		t.Fatalf("Count with relations: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3; a relation filter must not narrow the roots", n)
	}
	if got := c.count(); got != 1 {
		t.Errorf("Count ran %d statements, want one:\n%s", got, strings.Join(c.all(), "\n"))
	}
}

// One keeps its own contract. A semi-join narrows which row it is looking at
// and changes nothing about how it insists on exactly one.
func TestAny_withOne(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	u, err := db.Users.Query().
		Where(
			gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true)),
			gendemo.Users.Email.Eq("a@example.com"),
		).
		One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if u.ID != 1 {
		t.Errorf("One returned user %d, want 1", u.ID)
	}

	_, err = db.Users.Query().
		Where(
			gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true)),
			gendemo.Users.Email.Eq("c@example.com"),
		).
		One(t.Context())
	if !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}

	_, err = db.Users.Query().Where(gendemo.Users.Posts.None()).Where(gendemo.Users.Active.Eq(true)).One(t.Context())
	if err != nil && !errors.Is(err, orm.ErrMultipleRows) && !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("error = %v, want One's own contract to be unchanged", err)
	}
}

// A semi-join filters rows; it does not bring the related table into the
// statement, so locking the roots stays locking the roots.
func TestAny_withForUpdate(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	sql, _, err := db.Users.Query().
		Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).
		ForUpdate().
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, "FOR UPDATE") {
		t.Errorf("SQL = %s, want a plain FOR UPDATE over the root", sql)
	}
	users, err := db.Users.Query().
		Where(gendemo.Users.Posts.Any(gendemo.Posts.Published.Eq(true))).
		ForUpdate().
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1}) {
		t.Errorf("returned %v, want the locked root to be the matching one", got)
	}
}

// Rows is genuinely streaming, so a relation that has to wait for every root
// row is refused rather than quietly buffered. Configuring a to-one relation
// moves it into that category, and Rows has to notice.
func TestRows_rejectsConfiguredRelations(t *testing.T) {
	testdb.AdminDSN(t)
	db, c, _ := optDB(t)

	tests := []struct {
		name string
		q    func() *orm.Query[gendemo.User]
		want error
	}{
		{
			name: "a plain to-one still streams",
			q: func() *orm.Query[gendemo.User] {
				return db.Users.Query().With(gendemo.Users.Profile)
			},
		},
		{
			name: "a configured to-one does not",
			q: func() *orm.Query[gendemo.User] {
				return db.Users.Query().With(gendemo.Users.Profile.Where(gendemo.Profiles.Bio.IsNotNull()))
			},
			want: orm.ErrStreamingRelation,
		},
		{
			name: "a to-many never did",
			q: func() *orm.Query[gendemo.User] {
				return db.Users.Query().With(gendemo.Users.Posts.Limit(5))
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

// A relation of an aliased table correlates against the alias. Falling back to
// the table's own occurrence would compile to a statement naming a table the
// query does not select from.
func TestAny_throughAnAlias(t *testing.T) {
	testdb.AdminDSN(t)
	db, _, _ := optDB(t)

	alias := gendemo.Users.As("u")
	users, err := db.Users.QueryFrom(alias.Source()).
		Where(alias.Posts.Any(gendemo.Posts.Published.Eq(true))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := ids(users); !equalInts(got, []int64{1}) {
		t.Errorf("returned %v, want the user with a published post", got)
	}

	// The global descriptor's relation names the table's own occurrence, which
	// this query does not select from.
	_, err = db.Users.QueryFrom(alias.Source()).
		Where(gendemo.Users.Posts.Any()).
		All(t.Context())
	var scopeErr interface{ Error() string }
	if !errors.As(err, &scopeErr) || !strings.Contains(err.Error(), "scope error") {
		t.Errorf("error = %v, want a scope error naming the occurrence the query does not read", err)
	}
}

func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalInts(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func ids2(posts []gendemo.Post) []int64 {
	out := make([]int64, 0, len(posts))
	for _, p := range posts {
		out = append(out, p.ID)
	}
	return out
}
