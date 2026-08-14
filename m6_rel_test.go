package orm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
)

// recordingExecutor keeps every statement it is asked to run, so that a test
// can assert how many there were and what they said. It returns one root row,
// which is what makes a relation load at all.
type recordingExecutor struct {
	sql []string
}

func (e *recordingExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	e.sql = append(e.sql, sql)
	if len(e.sql) > 1 {
		return &stubRows{}, nil
	}
	return &stubRows{rows: [][]any{{
		int64(1), "a@example.com", int32(30), true, nil, nil, nil, time.Time{},
	}}}, nil
}

func repo(ex orm.Executor) *orm.Repo[User] { return orm.NewRepo(ex, &userMeta) }

func postRepo(ex orm.Executor) *orm.Repo[Post] { return orm.NewRepo(ex, &postSemiMeta) }

// postSemiMeta is the smallest metadata a Post query needs to render.
var postSemiMeta = orm.EntityMeta[Post]{
	Table:   orm.TableID{Schema: "public", Name: "posts"},
	Source:  postsSrc,
	Columns: []orm.ColumnMeta{{Name: "id", Field: "ID"}},
	Dest:    func(p *Post, idx int) any { return &p.ID },
}

// Relation options and semi-joins, without a database.
//
// The descriptors here stand in for generated ones. What the tests are about is
// the two halves of M6 that can be decided without asking PostgreSQL anything:
// that configuring a relation produces a modified copy and never touches the
// original, and that Any and None compile to a correlated subquery which leaves
// the root statement's shape alone.

// usersPosts is a to-many relation over posts.author_id, in the shape the
// generator emits it.
var usersPosts = orm.NewManyRel(orm.ManyRelSpec[User, Post]{
	Name:    "Posts",
	Parent:  usersSrc,
	Target:  postsSrc,
	Keys:    []orm.RelKey{{Parent: "id", Target: "author_id", Type: "int8"}},
	Columns: []string{"id", "published", "created_at"},
	ExtractKeys: func(parents []*User) ([]any, error) {
		ids := make([]int64, len(parents))
		for i, p := range parents {
			ids[i] = p.ID
		}
		return []any{ids}, nil
	},
	Dest:   func(p *Post) []any { return []any{&p.ID, &p.Published, &p.CreatedAt} },
	Attach: func(u *User, rows []Post) {},
	Refs:   func(_ *User, out []*Post) []*Post { return out },
})

// postsAuthor is the to-one relation back, whose parent key is mapped and which
// therefore has both loading strategies available to it.
var postsAuthor = orm.NewOneRel(orm.OneRelSpec[Post, User]{
	Name:        "Author",
	Parent:      postsSrc,
	Target:      usersSrc,
	Keys:        []orm.RelKey{{Parent: "author_id", Target: "id", Type: "int8"}},
	Columns:     []string{"id", "email"},
	Bind:        func() ([]any, func(*Post)) { return nil, func(*Post) {} },
	ExtractKeys: func(parents []*Post) ([]any, error) { return []any{[]int64{}}, nil },
	Dest:        func(u *User) []any { return []any{&u.ID, &u.Email} },
	Attach:      func(p *Post, u *User) {},
	Refs:        func(_ *Post, out []*User) []*User { return out },
})

func TestRel_limitErrors(t *testing.T) {
	tests := []struct {
		name string
		rel  orm.Rel[User, Post]
		want string
	}{
		{name: "negative", rel: usersPosts.Limit(-1), want: "negative limit -1"},
		{name: "set twice", rel: usersPosts.Limit(5).Limit(10), want: "limit set twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := repo(nil).Query().With(tt.rel)
			_, _, err := q.SQL()
			if err == nil {
				t.Fatalf("SQL succeeded, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
			// The mistake is caught before anything is sent, so an executor
			// would never have been reached.
			if _, err := q.All(context.Background()); err == nil {
				t.Error("All succeeded on a query that cannot be built")
			}
		})
	}
}

// A limit of zero is a relation with a known answer. It stays requested — the
// caller asked for it and gets it, loaded and empty — and costs no statement.
func TestRel_limitZeroRunsNoStatement(t *testing.T) {
	attached := 0
	rel := orm.NewManyRel(orm.ManyRelSpec[User, Post]{
		Name:        "Posts",
		Parent:      usersSrc,
		Target:      postsSrc,
		Keys:        []orm.RelKey{{Parent: "id", Target: "author_id", Type: "int8"}},
		Columns:     []string{"id"},
		ExtractKeys: func([]*User) ([]any, error) { return []any{[]int64{}}, nil },
		Dest:        func(p *Post) []any { return []any{&p.ID} },
		Attach:      func(u *User, rows []Post) { attached++ },
		Refs:        func(_ *User, out []*Post) []*Post { return out },
	}).Limit(0)

	ex := &recordingExecutor{}
	roots, err := repo(ex).Query().With(rel).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(ex.sql) != 1 {
		t.Errorf("ran %d statements, want only the root one:\n%s", len(ex.sql), strings.Join(ex.sql, "\n"))
	}
	if attached != len(roots) {
		t.Errorf("attached the relation to %d of %d roots; a relation asked for is always loaded", attached, len(roots))
	}
}

func TestRel_any(t *testing.T) {
	sql, args, err := repo(nil).Query().Where(usersPosts.Any(Posts.Published.Eq(true))).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const want = `WHERE EXISTS (SELECT 1 FROM "public"."posts" ` +
		`WHERE "posts"."author_id" = "users"."id" AND "posts"."published" = $1)`
	if !strings.Contains(sql, want) {
		t.Errorf("SQL =\n%s\nwant it to contain\n%s", sql, want)
	}
	if len(args) != 1 || args[0] != true {
		t.Errorf("args = %v, want the predicate's value", args)
	}
}

// With no predicates the question is only whether a related row exists, which
// is a useful thing to ask and needs no special spelling.
func TestRel_anyWithoutPredicates(t *testing.T) {
	sql, args, err := repo(nil).Query().Where(usersPosts.Any()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const want = `WHERE EXISTS (SELECT 1 FROM "public"."posts" WHERE "posts"."author_id" = "users"."id")`
	if !strings.Contains(sql, want) {
		t.Errorf("SQL =\n%s\nwant it to contain\n%s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

func TestRel_none(t *testing.T) {
	sql, _, err := repo(nil).Query().Where(usersPosts.None(Posts.Published.Eq(true))).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `WHERE NOT EXISTS (SELECT 1 FROM "public"."posts"`) {
		t.Errorf("SQL =\n%s\nwant a NOT EXISTS", sql)
	}
}

// The relation is to-one and the correlation runs the other way, from the
// entity's own column to the target's key. Both directions come from the
// foreign key reconciliation proved, not from which side declares the field.
func TestRel_anyOnABelongsTo(t *testing.T) {
	sql, _, err := postRepo(nil).Query().Where(postsAuthor.Any(Users.Active.Eq(true))).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const want = `WHERE EXISTS (SELECT 1 FROM "public"."users" ` +
		`WHERE "users"."id" = "posts"."author_id" AND "users"."active" = $1)`
	if !strings.Contains(sql, want) {
		t.Errorf("SQL =\n%s\nwant it to contain\n%s", sql, want)
	}
}

// A semi-join filters roots. It does not load anything, so a query that only
// asks Any still asks for one statement and leaves the relation alone.
func TestRel_anyLoadsNothing(t *testing.T) {
	ex := &recordingExecutor{}
	if _, err := repo(ex).Query().Where(usersPosts.Any()).All(context.Background()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(ex.sql) != 1 {
		t.Errorf("ran %d statements, want one:\n%s", len(ex.sql), strings.Join(ex.sql, "\n"))
	}
}

// Ordering and limiting say which related rows to load. Neither can change
// whether one exists, so passing them to Any is a mistake rather than a no-op.
func TestRel_anyRejectsLoadingOptions(t *testing.T) {
	tests := []struct {
		name string
		p    orm.Predicate[User]
		want string
	}{
		{name: "ordered", p: usersPosts.OrderBy(Posts.ID.Asc()).Any(), want: "OrderBy configures which related rows load"},
		{name: "limited", p: usersPosts.Limit(5).Any(), want: "Limit configures how many related rows load"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.p.Err(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A configured Where is part of the same question — which related rows count —
// so it travels into the subquery rather than being silently dropped.
func TestRel_anyKeepsAConfiguredWhere(t *testing.T) {
	sql, args, err := repo(nil).Query().
		Where(usersPosts.Where(Posts.Published.Eq(true)).Any(Posts.ID.Gt(10))).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `"posts"."published" = $1 AND "posts"."id" > $2`) {
		t.Errorf("SQL =\n%s\nwant both conditions inside the subquery", sql)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want both values", args)
	}
}

// A relation to its own table cannot take predicates in a semi-join: the
// subquery and the row being tested would be two occurrences of one table under
// one name, and a predicate naming that table could mean either.
func TestRel_anyOnASelfRelation(t *testing.T) {
	reports := orm.NewManyRel(orm.ManyRelSpec[User, User]{
		Name:        "Reports",
		Parent:      usersSrc,
		Target:      usersSrc,
		Keys:        []orm.RelKey{{Parent: "id", Target: "manager_id", Type: "int8"}},
		Columns:     []string{"id"},
		ExtractKeys: func([]*User) ([]any, error) { return nil, nil },
		Dest:        func(u *User) []any { return []any{&u.ID} },
		Attach:      func(*User, []User) {},
		Refs:        func(_ *User, out []*User) []*User { return out },
	})

	// Without predicates there is nothing to be ambiguous, so the subquery
	// reads an occurrence of its own under a reserved alias.
	sql, _, err := repo(nil).Query().Where(reports.Any()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const want = `WHERE EXISTS (SELECT 1 FROM "public"."users" AS "_sReports" ` +
		`WHERE "_sReports"."manager_id" = "users"."id")`
	if !strings.Contains(sql, want) {
		t.Errorf("SQL =\n%s\nwant it to contain\n%s", sql, want)
	}

	// With one, it is refused rather than compiled into a statement that
	// compares a row with itself.
	if err := reports.Any(Users.Active.Eq(true)).Err(); err == nil ||
		!strings.Contains(err.Error(), "relates users to itself") {
		t.Errorf("error = %v, want a self relation with predicates to be refused", err)
	}
}
