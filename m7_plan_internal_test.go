package orm

import (
	"context"
	"strings"
	"testing"
)

// The requested relation tree, planned without a database.
//
// What is being checked here is the shape of the plan rather than the rows it
// would produce: the order nodes run in, the strategy each gets, which columns
// a statement has to select on a child's behalf, and that configuring one node
// leaves every other alone.

type relComment struct {
	ID   int64
	Body string
}

var (
	relCommentsSrc = NewSource("public", "comments")
	relCommentBody = NewTextCol[relComment](relCommentsSrc, "body")
)

// relComments is a relation of relPost, so it is what relPosts.With accepts.
func relComments() Rel[relPost, relComment] {
	return NewManyRel(ManyRelSpec[relPost, relComment]{
		Name:        "Comments",
		Parent:      relPostsSrc,
		Target:      relCommentsSrc,
		Keys:        []RelKey{{Parent: "id", Target: "post_id", Type: "int8"}},
		Columns:     []string{"id", "body"},
		ExtractKeys: func([]*relPost) ([]any, error) { return []any{[]int64{}}, nil },
		Dest:        func(c *relComment) []any { return []any{&c.ID, &c.Body} },
		Attach:      func(*relPost, []relComment) {},
		Refs:        func(_ *relPost, out []*relComment) []*relComment { return out },
	})
}

// relCommentsUnmapped is the same relation over a key the entity does not map,
// which its parent's statement therefore has to select.
func relCommentsUnmapped() Rel[relPost, relComment] {
	r := relComments()
	r.rel.extract = nil
	r.rel.auxColumns = []string{"post_id"}
	r.rel.newAux = func() AuxKeys { return nil }
	return r
}

func planOf(t *testing.T, rel Rel[relUser, relPost]) planNode {
	t.Helper()
	node, err := planRelation(newRelNode(rel.relation()), "users", 1, true)
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	return node
}

// Every node knows where it sits, so a failure four levels down can say which
// four rather than naming a relation that appears in three places.
func TestPlan_paths(t *testing.T) {
	node := planOf(t, relPosts.With(relComments()))
	if node.node.path != "users.Posts" {
		t.Errorf("path = %q, want users.Posts", node.node.path)
	}
	if len(node.children) != 1 {
		t.Fatalf("planned %d children, want one", len(node.children))
	}
	if got := node.children[0].node.path; got != "users.Posts.Comments" {
		t.Errorf("child path = %q, want users.Posts.Comments", got)
	}
}

// Only the root statement has somewhere to fold a relation into, so a to-one
// relation nested under another loads in a statement of its own however plain
// it is.
func TestPlan_nestedToOneBatches(t *testing.T) {
	author := NewOneRel(OneRelSpec[relPost, relUser]{
		Name:        "Author",
		Parent:      relPostsSrc,
		Target:      relUsersSrc,
		Keys:        []RelKey{{Parent: "author_id", Target: "id", Type: "int8"}},
		Columns:     []string{"id"},
		Bind:        func() ([]any, func(*relPost)) { return nil, func(*relPost) {} },
		ExtractKeys: func([]*relPost) ([]any, error) { return nil, nil },
		Dest:        func(u *relUser) []any { return []any{&u.ID} },
		Attach:      func(*relPost, *relUser) {},
		Refs:        func(_ *relPost, out []*relUser) []*relUser { return out },
	})
	node := planOf(t, relPosts.With(author))
	if got := node.children[0].strategy; got != stratBatch {
		t.Errorf("nested to-one strategy = %d, want a statement of its own", got)
	}
}

// A child that reads its keys from the statement rather than from the entity
// needs that statement to exist, so its parent gives up folding to provide one.
func TestPlan_auxForcesTheParentToBatch(t *testing.T) {
	profile := func(children ...Loader[relPost]) Rel[relUser, relPost] {
		r := NewOneRel(OneRelSpec[relUser, relPost]{
			Name:        "Profile",
			Parent:      relUsersSrc,
			Target:      relPostsSrc,
			Keys:        []RelKey{{Parent: "id", Target: "user_id", Type: "int8"}},
			Columns:     []string{"id"},
			Bind:        func() ([]any, func(*relUser)) { return nil, func(*relUser) {} },
			ExtractKeys: func([]*relUser) ([]any, error) { return nil, nil },
			Dest:        func(p *relPost) []any { return []any{&p.ID} },
			Attach:      func(*relUser, *relPost) {},
			Refs:        func(_ *relUser, out []*relPost) []*relPost { return out },
		})
		return r.With(children...)
	}

	if got := planOf(t, profile()).strategy; got != stratFold {
		t.Errorf("a plain to-one at the root has strategy %d, want the fold", got)
	}
	// A child reading its keys from the entity changes nothing.
	if got := planOf(t, profile(relComments())).strategy; got != stratFold {
		t.Errorf("strategy = %d, want the fold to survive a child that carries its own keys", got)
	}
	// A child reading them from the statement does.
	if got := planOf(t, profile(relCommentsUnmapped())).strategy; got != stratBatch {
		t.Errorf("strategy = %d, want a statement of its own so the child has one to read from", got)
	}
}

// The columns a child asked for follow the entity's own in the statement, so
// the scanner and the select list cannot disagree about which is which.
func TestPlan_auxColumnsJoinTheStatement(t *testing.T) {
	r := relPosts.With(relCommentsUnmapped()).relation()
	sql, _, err := relationSelect(r, []any{[]int64{1}}, []string{"post_id"}).Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "posts"."id", "posts"."published", "posts"."created_at", "posts"."post_id" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`JOIN "public"."posts" ON "posts"."author_id" = "_k"."k0"`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
}

// A limit of zero is a relation with a known answer, and a relation of it has
// no rows to attach to, so neither costs a statement.
func TestPlan_limitZeroSubtree(t *testing.T) {
	node := planOf(t, relPosts.Limit(0).With(relComments()))
	if node.strategy != stratEmpty {
		t.Errorf("strategy = %d, want the empty load", node.strategy)
	}
	if len(node.children) != 1 {
		t.Fatalf("planned %d children, want the child to survive planning", len(node.children))
	}
}

// The same relation twice under one parent is one request written twice.
// Running both would make the result depend on which attached last.
func TestPlan_duplicateChildren(t *testing.T) {
	tests := []struct {
		name string
		rel  Rel[relUser, relPost]
	}{
		{name: "identical", rel: relPosts.With(relComments(), relComments())},
		{name: "configured differently", rel: relPosts.With(relComments().Limit(5), relComments().Limit(10))},
		{name: "across calls", rel: relPosts.With(relComments()).With(relComments())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := planRelation(newRelNode(tt.rel.relation()), "users", 1, true)
			if err == nil || !strings.Contains(err.Error(), "requested more than once") {
				t.Errorf("error = %v, want the repeated relation to be reported", err)
			}
		})
	}
}

// The same relation under two different parents is two different requests. What
// makes them different is the path, not the entity they happen to target.
func TestPlan_sameRelationOnDifferentBranches(t *testing.T) {
	creator := NewOneRel(OneRelSpec[relUser, relPost]{
		Name:        "Creator",
		Parent:      relUsersSrc,
		Target:      relPostsSrc,
		Keys:        []RelKey{{Parent: "id", Target: "creator_id", Type: "int8"}},
		Columns:     []string{"id"},
		Bind:        func() ([]any, func(*relUser)) { return nil, func(*relUser) {} },
		ExtractKeys: func([]*relUser) ([]any, error) { return nil, nil },
		Dest:        func(p *relPost) []any { return []any{&p.ID} },
		Attach:      func(*relUser, *relPost) {},
		Refs:        func(_ *relUser, out []*relPost) []*relPost { return out },
	})
	a := planOf(t, relPosts.With(relComments()))
	b := planOf(t, creator.With(relComments()))
	if a.children[0].node.path == b.children[0].node.path {
		t.Fatalf("both branches got the path %q; a relation's identity is where it was asked for", a.children[0].node.path)
	}
	if got, want := b.children[0].node.path, "users.Creator.Comments"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
}

// Nothing recurses on its own, so this guards against a program that builds a
// tree rather than against the ORM building one. Saying so beats exhausting the
// stack while planning.
func TestPlan_depthGuard(t *testing.T) {
	deep := relComments()
	for range maxRelationDepth + 2 {
		deep = relCommentsChain(deep)
	}
	_, err := planRelation(newRelNode(relPosts.With(deep).relation()), "users", 1, true)
	if err == nil || !strings.Contains(err.Error(), "nested more than") {
		t.Errorf("error = %v, want the depth guard to report the tree", err)
	}
}

// relCommentsChain nests a comments relation under a comments relation, which
// the types allow because both sides are stand-ins.
func relCommentsChain(child Loader[relPost]) Rel[relPost, relComment] {
	inner := NewManyRel(ManyRelSpec[relComment, relPost]{
		Name:        "Posts",
		Parent:      relCommentsSrc,
		Target:      relPostsSrc,
		Keys:        []RelKey{{Parent: "id", Target: "comment_id", Type: "int8"}},
		Columns:     []string{"id"},
		ExtractKeys: func([]*relComment) ([]any, error) { return nil, nil },
		Dest:        func(p *relPost) []any { return []any{&p.ID} },
		Attach:      func(*relComment, []relPost) {},
		Refs:        func(_ *relComment, out []*relPost) []*relPost { return out },
	})
	return relComments().With(inner.With(child))
}

// Configuring a nested relation must not reach the copy it branched from. A
// shared backing array is the way this quietly stops being true.
func TestRel_nestedConfigurationIsIsolated(t *testing.T) {
	base := relPosts.With(relComments())
	a := base.With(relCommentsOther())
	b := base.Limit(5)

	if got := len(planOf(t, base).children); got != 1 {
		t.Errorf("base has %d children, want the one it was built with", got)
	}
	if got := len(planOf(t, a).children); got != 2 {
		t.Errorf("branch has %d children, want both", got)
	}
	if got := planOf(t, b); len(got.children) != 1 || got.node.cfg.limit == nil {
		t.Errorf("the limited branch lost a child or its limit: %+v", got.node.cfg)
	}
	if planOf(t, base).node.cfg.limit != nil {
		t.Error("the base gained the branch's limit")
	}
}

// relCommentsOther is a second, differently named relation of relPost.
func relCommentsOther() Rel[relPost, relComment] {
	r := relComments()
	r.rel.name = "Replies"
	return r
}

// A query's clone shares no part of the other's relation tree, at any level.
func TestQuery_cloneDeepCopiesTheTree(t *testing.T) {
	meta := &EntityMeta[relUser]{
		Table:   TableID{Schema: "public", Name: "users"},
		Source:  relUsersSrc,
		Columns: []ColumnMeta{{Name: "id", Field: "ID"}},
		Dest:    func(u *relUser, idx int) any { return &u.ID },
	}
	base := NewRepo(nil, meta).Query().With(relPosts.With(relComments()))
	a, b := base.Clone(), base.Clone()

	if len(a.with) != 1 || len(a.with[0].children) != 1 {
		t.Fatalf("the clone did not carry the tree: %+v", a.with)
	}
	// Reaching into the clone's tree is what a builder method would do; nothing
	// it touches may be visible from the other clone or from the base.
	a.with[0].children[0].name = "changed"
	if b.with[0].children[0].name == "changed" {
		t.Error("the clones share a nested node")
	}
	if base.with[0].children[0].name == "changed" {
		t.Error("the clone shares a nested node with its base")
	}
}

// The plan is built before the root statement runs, so a mistake anywhere in
// the tree is reported without a round trip.
func TestPlan_nestedConfigurationErrorsNameThePath(t *testing.T) {
	meta := &EntityMeta[relUser]{
		Table:   TableID{Schema: "public", Name: "users"},
		Source:  relUsersSrc,
		Columns: []ColumnMeta{{Name: "id", Field: "ID"}},
		Dest:    func(u *relUser, idx int) any { return &u.ID },
	}
	q := NewRepo(nil, meta).Query().With(relPosts.With(relComments().Limit(-1)))
	_, _, err := q.SQL()
	if err == nil {
		t.Fatal("SQL succeeded on a tree with a negative limit in it")
	}
	if !strings.Contains(err.Error(), "users.Posts.Comments") || !strings.Contains(err.Error(), "negative limit") {
		t.Errorf("error = %v, want it to name the path and the mistake", err)
	}
	if _, err := q.All(context.Background()); err == nil {
		t.Error("All ran a query that cannot be built")
	}
}

// A relation filter belongs to the level it was written at. Nothing in the plan
// carries it up to an ancestor or down to a child.
func TestPlan_optionsStayOnTheirOwnLevel(t *testing.T) {
	node := planOf(t, relPosts.
		Where(relPostPublished.Eq(true)).
		Limit(5).
		With(relComments().Where(relCommentBody.Eq("x")).Limit(10)))

	if node.node.cfg.limit == nil || *node.node.cfg.limit != 5 {
		t.Errorf("parent limit = %v, want 5", node.node.cfg.limit)
	}
	child := node.children[0].node.cfg
	if child.limit == nil || *child.limit != 10 {
		t.Errorf("child limit = %v, want 10", child.limit)
	}
	if child.where == nil || node.node.cfg.where == nil {
		t.Error("a level lost its own filter")
	}
}
