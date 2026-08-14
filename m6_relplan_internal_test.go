package orm

import (
	"strings"
	"testing"
)

// The statement a configured relation runs, rendered without a database.
//
// A relation's SQL is not returned by Query.SQL, which stays the root
// statement, so it is built here directly. That is the point: relation SQL is
// assembled by a function that takes a descriptor and returns a statement, so
// it can be asserted the way any other statement is rather than only through
// PostgreSQL.

type relUser struct {
	ID     int64
	Active bool
}

type relPost struct {
	ID        int64
	Published bool
	CreatedAt string
}

var (
	relUsersSrc = NewSource("public", "users")
	relPostsSrc = NewSource("public", "posts")

	relPostID        = NewOrdCol[relPost, int64](relPostsSrc, "id")
	relPostPublished = NewCol[relPost, bool](relPostsSrc, "published")
	relPostCreated   = NewOrdCol[relPost, string](relPostsSrc, "created_at")
)

// relPosts is a to-many relation in the shape the generator emits.
var relPosts = NewManyRel(ManyRelSpec[relUser, relPost]{
	Name:        "Posts",
	Parent:      relUsersSrc,
	Target:      relPostsSrc,
	Keys:        []RelKey{{Parent: "id", Target: "author_id", Type: "int8"}},
	Columns:     []string{"id", "published", "created_at"},
	ExtractKeys: func([]*relUser) ([]any, error) { return []any{[]int64{1, 2}}, nil },
	Dest:        func(p *relPost) []any { return []any{&p.ID, &p.Published, &p.CreatedAt} },
	Attach:      func(*relUser, []relPost) {},
	Refs:        func(_ *relUser, out []*relPost) []*relPost { return out },
})

// relationSQL renders what a configured relation would run.
func relationSQL(t *testing.T, rel Rel[relUser, relPost]) string {
	t.Helper()
	r := rel.relation()
	if err := errsOf(r); err != nil {
		t.Fatalf("relation configuration: %v", err)
	}
	sql, _, err := relationSelect(r, []any{[]int64{1, 2}}, nil).Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return sql
}

func errsOf(r relation[relUser]) error {
	if len(r.cfg.errs) == 0 {
		return nil
	}
	return r.cfg.errs[0]
}

func TestRelationSelect_plainRelation(t *testing.T) {
	const want = `SELECT "_k"."ord", "posts"."id", "posts"."published", "posts"."created_at" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`JOIN "public"."posts" ON "posts"."author_id" = "_k"."k0"`
	if got := relationSQL(t, relPosts); got != want {
		t.Errorf("SQL =\n%s\nwant\n%s", got, want)
	}
}

func TestRelationSelect_configuredRelation(t *testing.T) {
	rel := relPosts.
		Where(relPostPublished.Eq(true)).
		OrderBy(relPostCreated.Desc()).
		Limit(5)

	const want = `SELECT "_k"."ord", "_l"."id", "_l"."published", "_l"."created_at" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`CROSS JOIN LATERAL (` +
		`SELECT "posts"."id", "posts"."published", "posts"."created_at" FROM "public"."posts" ` +
		`WHERE "posts"."author_id" = "_k"."k0" AND "posts"."published" = $2 ` +
		`ORDER BY "posts"."created_at" DESC LIMIT 5` +
		`) AS "_l" ORDER BY "_k"."ord" ASC, "_l"."created_at" DESC`
	if got := relationSQL(t, rel); got != want {
		t.Errorf("SQL =\n%s\nwant\n%s", got, want)
	}
}

// Filtering and ordering without a limit keep the flat join. Forcing LATERAL
// would make the server run a subquery per parent to reach the same rows.
func TestRelationSelect_noLimitNoLateral(t *testing.T) {
	rel := relPosts.Where(relPostPublished.Eq(true)).OrderBy(relPostCreated.Desc())
	sql := relationSQL(t, rel)
	if strings.Contains(sql, "LATERAL") {
		t.Errorf("SQL = %s, want the flat join without a per-parent limit", sql)
	}
	const want = `SELECT "_k"."ord", "posts"."id", "posts"."published", "posts"."created_at" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`JOIN "public"."posts" ON "posts"."author_id" = "_k"."k0" ` +
		`WHERE "posts"."published" = $2 ` +
		`ORDER BY "_k"."ord" ASC, "posts"."created_at" DESC`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
}

// Configuring a relation returns a copy. The generated descriptor is a
// package-level value shared by every query in the program, so a method that
// changed it in place would leak one query's filter into all the others.
func TestRel_configurationIsCopied(t *testing.T) {
	a := relPosts.Limit(5)
	b := relPosts.Where(relPostPublished.Eq(true))
	c := relPosts

	// The correlation is a WHERE of its own, so what marks a filter is the
	// filtered column rather than the keyword.
	if sql := relationSQL(t, a); !strings.Contains(sql, "LIMIT 5") || strings.Contains(sql, `"posts"."published" =`) {
		t.Errorf("limited copy = %s, want only a limit", sql)
	}
	if sql := relationSQL(t, b); !strings.Contains(sql, `"posts"."published" =`) || strings.Contains(sql, "LIMIT") {
		t.Errorf("filtered copy = %s, want only a filter", sql)
	}
	if sql := relationSQL(t, c); strings.Contains(sql, `"posts"."published" =`) || strings.Contains(sql, "LIMIT") {
		t.Errorf("original = %s, want no configuration at all", sql)
	}
}

// Two copies branching off one configured relation must not see each other's
// options, which is what a shared backing array would produce.
func TestRel_branchesDoNotShareStorage(t *testing.T) {
	base := relPosts.Where(relPostPublished.Eq(true))
	a := base.Where(relPostID.Gt(10))
	b := base.Where(relPostID.Lt(100))

	if sql := relationSQL(t, a); !strings.Contains(sql, `"posts"."id" > $3`) || strings.Contains(sql, `"posts"."id" < `) {
		t.Errorf("first branch = %s, want only its own condition", sql)
	}
	if sql := relationSQL(t, b); !strings.Contains(sql, `"posts"."id" < $3`) || strings.Contains(sql, `"posts"."id" > `) {
		t.Errorf("second branch = %s, want only its own condition", sql)
	}
	// posts.id is in the select list either way, so it is the comparison that
	// tells a leaked condition from a column being read.
	if sql := relationSQL(t, base); strings.Contains(sql, `"posts"."id" >`) || strings.Contains(sql, `"posts"."id" <`) {
		t.Errorf("base = %s, want neither branch's condition", sql)
	}
}

func TestRel_whereAccumulatesWithAnd(t *testing.T) {
	twice := relPosts.Where(relPostPublished.Eq(true)).Where(relPostID.Gt(10))
	once := relPosts.Where(relPostPublished.Eq(true), relPostID.Gt(10))
	if got, want := relationSQL(t, twice), relationSQL(t, once); got != want {
		t.Errorf("two calls =\n%s\none call =\n%s\nwant the same relation", got, want)
	}
	if sql := relationSQL(t, twice); !strings.Contains(sql, `WHERE "posts"."published" = $2 AND "posts"."id" > $3`) {
		t.Errorf("SQL = %s, want the two predicates combined with AND", sql)
	}
}

// A second OrderBy refines the first rather than replacing it. Replacement
// would make the earlier call look effective while doing nothing.
func TestRel_orderByAppends(t *testing.T) {
	rel := relPosts.OrderBy(relPostCreated.Desc()).OrderBy(relPostID.Asc())
	if sql := relationSQL(t, rel); !strings.Contains(sql, `ORDER BY "_k"."ord" ASC, "posts"."created_at" DESC, "posts"."id" ASC`) {
		t.Errorf("SQL = %s, want both orderings in the order they were given", sql)
	}
}

// A raw fragment inside a relation filter is renumbered past the key arrays,
// which already occupy the first parameter positions.
func TestRelationSelect_exprPlaceholders(t *testing.T) {
	rel := relPosts.Where(Expr[relPost](`"posts"."score" > $1`, 100))
	r := rel.relation()
	sql, args, err := relationSelect(r, []any{[]int64{1, 2}}, nil).Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.HasSuffix(sql, `WHERE "posts"."score" > $2`) {
		t.Errorf("SQL = %s, want the fragment's $1 renumbered to $2", sql)
	}
	if len(args) != 2 || args[1] != 100 {
		t.Errorf("args = %v, want the key array then the fragment's value", args)
	}
}

// The strategy is what everything else keys off: how many statements a query
// runs, whether it may stream, and which occurrence the target is read from.
func TestRelation_strategy(t *testing.T) {
	mapped := func(parents []*relUser) ([]any, error) { return []any{[]int64{}}, nil }
	one := func(extract func([]*relUser) ([]any, error)) Rel[relUser, relPost] {
		s := OneRelSpec[relUser, relPost]{
			Name:        "Author",
			Parent:      relUsersSrc,
			Target:      relPostsSrc,
			Keys:        []RelKey{{Parent: "id", Target: "author_id", Type: "int8"}},
			Columns:     []string{"id"},
			Bind:        func() ([]any, func(*relUser)) { return nil, func(*relUser) {} },
			ExtractKeys: extract,
			Dest:        func(p *relPost) []any { return []any{&p.ID} },
			Attach:      func(*relUser, *relPost) {},
			Refs:        func(_ *relUser, out []*relPost) []*relPost { return out },
		}
		// A relation whose parent keys are unmapped reads them from the
		// statement that loaded its parents instead.
		if extract == nil {
			s.AuxColumns = []string{"author_id"}
			s.NewAux = func() AuxKeys { return nil }
		}
		return NewOneRel(s)
	}

	tests := []struct {
		name string
		rel  Rel[relUser, relPost]
		want relStrategy
	}{
		{name: "a plain to-one folds", rel: one(mapped), want: stratFold},
		{name: "a configured to-one batches", rel: one(mapped).Where(relPostPublished.Eq(true)), want: stratBatch},
		{
			// There is nowhere to read the parent keys from, so the relation
			// stays in the join and the filter travels into its condition.
			name: "a configured to-one with unmapped keys stays folded",
			rel:  one(nil).Where(relPostPublished.Eq(true)),
			want: stratFoldTarget,
		},
		{name: "a plain to-many batches", rel: relPosts, want: stratBatch},
		{name: "a to-many with no rows asked for runs nothing", rel: relPosts.Limit(0), want: stratEmpty},
		{name: "a to-one with no rows asked for runs nothing", rel: one(mapped).Limit(0), want: stratEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node, err := planRelation(newRelNode(tt.rel.relation()), "users", 1, true)
			if err != nil {
				t.Fatalf("planning: %v", err)
			}
			if node.strategy != tt.want {
				t.Errorf("strategy = %d, want %d", node.strategy, tt.want)
			}
		})
	}
}

// A relation read through a join has no per-parent ordering or limit to apply,
// so asking for one is refused rather than quietly ignored.
func TestRelation_unmappedKeysRefuseOrderAndLimit(t *testing.T) {
	unmapped := NewOneRel(OneRelSpec[relUser, relPost]{
		Name:       "Author",
		Parent:     relUsersSrc,
		Target:     relPostsSrc,
		Keys:       []RelKey{{Parent: "id", Target: "author_id", Type: "int8"}},
		Columns:    []string{"id"},
		Bind:       func() ([]any, func(*relUser)) { return nil, func(*relUser) {} },
		AuxColumns: []string{"author_id"},
		NewAux:     func() AuxKeys { return nil },
		Dest:       func(p *relPost) []any { return []any{&p.ID} },
		Attach:     func(*relUser, *relPost) {},
		Refs:       func(_ *relUser, out []*relPost) []*relPost { return out },
	})
	for _, tt := range []struct {
		name string
		rel  Rel[relUser, relPost]
		want string
	}{
		{name: "ordered", rel: unmapped.OrderBy(relPostCreated.Desc()), want: "cannot be ordered"},
		{name: "limited", rel: unmapped.Limit(3), want: "cannot be limited"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := planRelation(newRelNode(tt.rel.relation()), "users", 1, true)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
