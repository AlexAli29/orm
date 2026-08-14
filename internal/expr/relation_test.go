package expr_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// The two SQL shapes M5 introduces: the LEFT JOIN a to-one relation folds into
// the root statement, and the standalone statement a to-many relation gets.
//
// Both are asserted as whole strings rather than by fragment. The exact text is
// what PostgreSQL plans and what a reader compares against EXPLAIN output, and
// a test that only checks for "JOIN" would not notice the join arriving before
// the WHERE clause it must precede.

func relSources() (*expr.Source, *expr.Source) {
	users := expr.NewSource("public", "users")
	return users, expr.NewSource("public", "posts").Reserved("_r0")
}

func TestSelect_leftJoinForAFoldedRelation(t *testing.T) {
	users, target := relSources()
	s := &expr.Select{
		From: users,
		Columns: []expr.Column{
			{Source: users, Name: "id"},
			{Source: target, Name: "id"},
			{Source: target, Name: "title"},
		},
		Joins: []expr.Join{{
			Kind:   expr.JoinLeft,
			Source: target,
			On: expr.Binary{
				Op:    expr.OpEq,
				Left:  expr.Column{Source: target, Name: "author_id"},
				Right: expr.Column{Source: users, Name: "id"},
			},
		}},
		Where: expr.Binary{Op: expr.OpEq, Left: expr.Column{Source: users, Name: "active"}, Right: expr.Arg{Value: true}},
		Limit: ptr(10),
	}

	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "users"."id", "_r0"."id", "_r0"."title" FROM "public"."users" ` +
		`LEFT JOIN "public"."posts" AS "_r0" ON "_r0"."author_id" = "users"."id" ` +
		`WHERE "users"."active" = $1 LIMIT 10`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 1 || args[0] != true {
		t.Errorf("args = %v, want [true]", args)
	}
}

// A folded relation must never turn a row that has no related row into no row
// at all, which is the whole reason the join is LEFT rather than INNER.
func TestSelect_relationJoinsAreAlwaysOuter(t *testing.T) {
	users, target := relSources()
	s := &expr.Select{
		From:    users,
		Columns: []expr.Column{{Source: users, Name: "id"}},
		Joins:   []expr.Join{{Kind: expr.JoinLeft, Source: target, On: expr.Bool{Value: true}}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(sql, "LEFT JOIN") {
		t.Errorf("SQL = %s, want a LEFT JOIN", sql)
	}
	if strings.Contains(sql, "INNER JOIN") {
		t.Errorf("SQL = %s, an INNER JOIN would drop parents with no related row", sql)
	}
}

// FOR UPDATE over a joined statement has to name the root, or PostgreSQL locks
// the joined rows too — and a relation the caller only asked to read would be
// locked as a side effect of reading it.
func TestSelect_forUpdateNamesTheRootWhenJoined(t *testing.T) {
	users, target := relSources()
	s := &expr.Select{
		From:      users,
		Columns:   []expr.Column{{Source: users, Name: "id"}},
		Joins:     []expr.Join{{Kind: expr.JoinLeft, Source: target, On: expr.Bool{Value: true}}},
		ForUpdate: true,
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.HasSuffix(sql, ` FOR UPDATE OF "users"`) {
		t.Errorf("SQL = %s, want it to end with FOR UPDATE OF the root", sql)
	}
}

func TestRelationSelect_compile(t *testing.T) {
	child := expr.NewSource("public", "posts").Reserved("_c")
	s := &expr.RelationSelect{
		Child: child,
		Columns: []expr.Column{
			{Source: child, Name: "id"},
			{Source: child, Name: "title"},
		},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1, 2, 3}},
	}

	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "_c"."id", "_c"."title" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`JOIN "public"."posts" AS "_c" ON "_c"."author_id" = "_k"."k0"`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 1 {
		t.Fatalf("args = %v, want the one key array", args)
	}
}

// A composite key travels as one array per key column, matched in constraint
// order. Getting the order wrong would match the right number of rows against
// the wrong parents, which is worse than an error.
func TestRelationSelect_compositeKey(t *testing.T) {
	child := expr.NewSource("public", "branches").Reserved("_c")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"text", "int4"},
		ChildKeys: []string{"branch_region", "branch_code"},
		Args:      []any{[]string{"eu"}, []int32{7}},
	}
	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "_c"."id" ` +
		`FROM unnest($1::"text"[], $2::"int4"[]) WITH ORDINALITY AS "_k"("k0", "k1", "ord") ` +
		`JOIN "public"."branches" AS "_c" ON "_c"."branch_region" = "_k"."k0" AND "_c"."branch_code" = "_k"."k1"`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 2 {
		t.Fatalf("args = %v, want one array per key column", args)
	}
}

// A schema-qualified type — an enum or a domain — is two identifiers, and both
// have to be quoted or a mixed-case schema would not resolve.
func TestRelationSelect_qualifiedKeyType(t *testing.T) {
	child := expr.NewSource("public", "memberships").Reserved("_c")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"app.user_state"},
		ChildKeys: []string{"state"},
		Args:      []any{[]string{"active"}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.Contains(sql, `unnest($1::"app"."user_state"[])`) {
		t.Errorf("SQL = %s, want the schema and the type each quoted", sql)
	}
}

func TestRelationSelect_errors(t *testing.T) {
	child := expr.NewSource("public", "posts").Reserved("_c")
	cols := []expr.Column{{Source: child, Name: "id"}}

	tests := []struct {
		name string
		stmt *expr.RelationSelect
		want string
	}{
		{
			name: "no child table",
			stmt: &expr.RelationSelect{Columns: cols, KeyTypes: []string{"int8"}, ChildKeys: []string{"author_id"}, Args: []any{nil}},
			want: "no child table",
		},
		{
			name: "no columns",
			stmt: &expr.RelationSelect{Child: child, KeyTypes: []string{"int8"}, ChildKeys: []string{"author_id"}, Args: []any{nil}},
			want: "no columns",
		},
		{
			name: "no key columns",
			stmt: &expr.RelationSelect{Child: child, Columns: cols},
			want: "no key columns",
		},
		{
			name: "a key type with no child column",
			stmt: &expr.RelationSelect{Child: child, Columns: cols, KeyTypes: []string{"int8", "text"}, ChildKeys: []string{"author_id"}, Args: []any{nil, nil}},
			want: "2 key types for 1 child columns",
		},
		{
			name: "a key column with no array",
			stmt: &expr.RelationSelect{Child: child, Columns: cols, KeyTypes: []string{"int8"}, ChildKeys: []string{"author_id"}},
			want: "0 key arrays for 1 key columns",
		},
		{
			name: "an unnamed key type",
			stmt: &expr.RelationSelect{Child: child, Columns: cols, KeyTypes: []string{""}, ChildKeys: []string{"author_id"}, Args: []any{nil}},
			want: "unnamed key type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.stmt.Compile()
			if err == nil {
				t.Fatalf("Compile succeeded, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// Reserved is how the planner takes a name As refuses, and it must leave the
// receiver alone for the same reason As does.
func TestSource_reserved(t *testing.T) {
	users := expr.NewSource("public", "users")
	r := users.Reserved("_r0")

	if r.Ref() != "_r0" {
		t.Errorf("Ref = %q, want the reserved alias", r.Ref())
	}
	if r.Err() != nil {
		t.Errorf("AliasErr = %v, want the compiler's own prefix to be allowed", r.Err())
	}
	if users.AliasName() != "" {
		t.Errorf("the receiver gained alias %q", users.AliasName())
	}
	if err := users.As("_r0").Err(); err == nil {
		t.Error("As accepted a reserved alias, which is the caller's route in")
	}
}

func ptr[T any](v T) *T { return &v }

// Relation options: filtering, ordering, and the per-parent limit.
//
// The shapes are asserted whole because each is a claim about semantics that a
// fragment check would miss — that a filter restricts the children and not the
// parents, that the ordinal leads the ordering, and that a limit lands inside
// the LATERAL subquery where it counts one parent's rows rather than every
// parent's.

func TestRelationSelect_filtered(t *testing.T) {
	child := expr.NewSource("public", "posts")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}, {Source: child, Name: "title"}},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1, 2}},
		Where: expr.Binary{Op: expr.OpEq,
			Left:  expr.Column{Source: child, Name: "published"},
			Right: expr.Arg{Value: true}},
	}
	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "posts"."id", "posts"."title" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`JOIN "public"."posts" ON "posts"."author_id" = "_k"."k0" ` +
		`WHERE "posts"."published" = $2`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	// The key array occupies $1, so the filter's own value has to be $2. A
	// relation filter that renumbered from $1 would bind the array to the
	// filter and the filter's value to nothing.
	if len(args) != 2 || args[1] != true {
		t.Errorf("args = %v, want the key array then the filter's value", args)
	}
	// A filter must not reach the join, where it would drop the parents whose
	// children do not match rather than leaving them with an empty relation.
	if strings.Contains(sql, `ON "posts"."author_id" = "_k"."k0" AND`) {
		t.Errorf("SQL = %s, the filter belongs in the WHERE clause", sql)
	}
}

// A filter without a limit uses the flat join. LATERAL would give the same
// answer and make the server evaluate a subquery once per parent for nothing.
func TestRelationSelect_filteredIsNotLateral(t *testing.T) {
	child := expr.NewSource("public", "posts")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1}},
		Where:     expr.Binary{Op: expr.OpEq, Left: expr.Column{Source: child, Name: "published"}, Right: expr.Arg{Value: true}},
		OrderBy:   []expr.Order{{Column: expr.Column{Source: child, Name: "created_at"}, Desc: true}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if strings.Contains(sql, "LATERAL") {
		t.Errorf("SQL = %s, want the flat join when no per-parent limit was asked for", sql)
	}
}

func TestRelationSelect_ordered(t *testing.T) {
	child := expr.NewSource("public", "posts")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1}},
		OrderBy: []expr.Order{
			{Column: expr.Column{Source: child, Name: "created_at"}, Desc: true},
			{Column: expr.Column{Source: child, Name: "id"}},
		},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "posts"."id" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`JOIN "public"."posts" ON "posts"."author_id" = "_k"."k0" ` +
		`ORDER BY "_k"."ord" ASC, "posts"."created_at" DESC, "posts"."id" ASC`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
}

// No ordering was asked for, so none is invented. Sorting by the ordinal alone
// would be work the caller never requested, and ordering by a primary key
// nobody named would be an ordering the ORM made up.
func TestRelationSelect_unorderedStaysUnordered(t *testing.T) {
	child := expr.NewSource("public", "posts")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if strings.Contains(sql, "ORDER BY") {
		t.Errorf("SQL = %s, want no ordering when none was requested", sql)
	}
}

func TestRelationSelect_perParentLimit(t *testing.T) {
	child := expr.NewSource("public", "posts")
	limit := 5
	s := &expr.RelationSelect{
		Child: child,
		Columns: []expr.Column{
			{Source: child, Name: "id"},
			{Source: child, Name: "title"},
			{Source: child, Name: "created_at"},
		},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1, 2, 3}},
		Where:     expr.Binary{Op: expr.OpEq, Left: expr.Column{Source: child, Name: "published"}, Right: expr.Arg{Value: true}},
		OrderBy:   []expr.Order{{Column: expr.Column{Source: child, Name: "created_at"}, Desc: true}},
		Limit:     &limit,
	}
	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "_l"."id", "_l"."title", "_l"."created_at" ` +
		`FROM unnest($1::"int8"[]) WITH ORDINALITY AS "_k"("k0", "ord") ` +
		`CROSS JOIN LATERAL (` +
		`SELECT "posts"."id", "posts"."title", "posts"."created_at" FROM "public"."posts" ` +
		`WHERE "posts"."author_id" = "_k"."k0" AND "posts"."published" = $2 ` +
		`ORDER BY "posts"."created_at" DESC LIMIT 5` +
		`) AS "_l" ` +
		`ORDER BY "_k"."ord" ASC, "_l"."created_at" DESC`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 2 || args[1] != true {
		t.Errorf("args = %v, want the key array then the filter's value", args)
	}
	// The limit has to be inside the subquery. Outside it, it would count rows
	// across every parent and the first parent with five rows would take them
	// all.
	if strings.HasSuffix(sql, "LIMIT 5") {
		t.Errorf("SQL = %s, a trailing LIMIT counts rows across every parent", sql)
	}
}

func TestRelationSelect_perParentLimitOverACompositeKey(t *testing.T) {
	child := expr.NewSource("public", "branches")
	limit := 2
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"text", "text"},
		ChildKeys: []string{"branch_region", "branch_code"},
		Args:      []any{[]string{"eu"}, []string{"acme"}},
		OrderBy:   []expr.Order{{Column: expr.Column{Source: child, Name: "label"}}},
		Limit:     &limit,
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "_k"."ord", "_l"."id" ` +
		`FROM unnest($1::"text"[], $2::"text"[]) WITH ORDINALITY AS "_k"("k0", "k1", "ord") ` +
		`CROSS JOIN LATERAL (` +
		`SELECT "branches"."id" FROM "public"."branches" ` +
		`WHERE "branches"."branch_region" = "_k"."k0" AND "branches"."branch_code" = "_k"."k1" ` +
		`ORDER BY "branches"."label" ASC LIMIT 2` +
		`) AS "_l" ` +
		`ORDER BY "_k"."ord" ASC, "_l"."label" ASC`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
}

// A raw fragment inside a relation filter keeps working: its own placeholders
// are renumbered past the key arrays that already occupy the earlier positions.
func TestRelationSelect_rawFragmentPlaceholders(t *testing.T) {
	child := expr.NewSource("public", "posts")
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1}},
		Where:     expr.Raw{SQL: `"posts"."score" > $1`, Args: []any{100}},
	}
	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if !strings.HasSuffix(sql, `WHERE "posts"."score" > $2`) {
		t.Errorf("SQL = %s, want the fragment's own $1 renumbered to $2", sql)
	}
	if len(args) != 2 || args[1] != 100 {
		t.Errorf("args = %v, want the key array then the fragment's value", args)
	}
}

func TestRelationSelect_negativeLimit(t *testing.T) {
	child := expr.NewSource("public", "posts")
	limit := -1
	s := &expr.RelationSelect{
		Child:     child,
		Columns:   []expr.Column{{Source: child, Name: "id"}},
		KeyTypes:  []string{"int8"},
		ChildKeys: []string{"author_id"},
		Args:      []any{[]int64{1}},
		Limit:     &limit,
	}
	if _, _, err := s.Compile(); err == nil || !strings.Contains(err.Error(), "negative relation limit") {
		t.Errorf("error = %v, want a negative limit to be refused", err)
	}
}
