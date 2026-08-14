package expr

import (
	"errors"
	"strings"
	"testing"
)

var users = NewSource("public", "users")

func col(name string) Column { return Column{Source: users, Name: name} }

// sel wraps a WHERE node in the smallest complete statement, so that the tests
// below assert on the fragment they are about.
func sel(where Node) *Select {
	return &Select{From: users, Columns: []Column{col("id")}, Where: where}
}

const prefix = `SELECT "users"."id" FROM "public"."users"`

func TestCompile_nodes(t *testing.T) {
	tests := []struct {
		name string
		node Node
		sql  string
		args []any
	}{
		{
			name: "a constant needs no parameter",
			node: Bool{Value: true},
			// A top-level TRUE restricts nothing and is dropped entirely.
			sql: prefix,
		},
		{
			name: "FALSE survives",
			node: Bool{Value: false},
			sql:  prefix + " WHERE FALSE",
		},
		{
			name: "binary",
			node: Binary{Op: OpEq, Left: col("id"), Right: Arg{Value: 1}},
			sql:  prefix + ` WHERE "users"."id" = $1`,
			args: []any{1},
		},
		{
			name: "is null",
			node: Unary{Op: OpIsNull, X: col("nickname")},
			sql:  prefix + ` WHERE "users"."nickname" IS NULL`,
		},
		{
			name: "not",
			node: Unary{Op: OpNot, X: Binary{Op: OpEq, Left: col("id"), Right: Arg{Value: 1}}},
			sql:  prefix + ` WHERE NOT "users"."id" = $1`,
			args: []any{1},
		},
		{
			name: "between",
			node: Between{X: col("age"), Lo: Arg{Value: 1}, Hi: Arg{Value: 2}},
			sql:  prefix + ` WHERE "users"."age" BETWEEN $1 AND $2`,
			args: []any{1, 2},
		},
		{
			name: "in",
			node: In{X: col("id"), Values: []Node{Arg{Value: 1}, Arg{Value: 2}}},
			sql:  prefix + ` WHERE "users"."id" IN ($1, $2)`,
			args: []any{1, 2},
		},
		{
			name: "in over nothing",
			node: In{X: col("id")},
			sql:  prefix + " WHERE FALSE",
		},
		{
			name: "a group at the top of a WHERE needs no parentheses",
			node: Group{Op: OpAnd, Items: []Node{
				Binary{Op: OpEq, Left: col("a"), Right: Arg{Value: 1}},
				Binary{Op: OpEq, Left: col("b"), Right: Arg{Value: 2}},
			}},
			sql:  prefix + ` WHERE "users"."a" = $1 AND "users"."b" = $2`,
			args: []any{1, 2},
		},
		{
			name: "a nested group does",
			node: Group{Op: OpAnd, Items: []Node{
				Binary{Op: OpEq, Left: col("a"), Right: Arg{Value: 1}},
				Group{Op: OpOr, Items: []Node{
					Binary{Op: OpEq, Left: col("b"), Right: Arg{Value: 2}},
					Binary{Op: OpEq, Left: col("c"), Right: Arg{Value: 3}},
				}},
			}},
			sql:  prefix + ` WHERE "users"."a" = $1 AND ("users"."b" = $2 OR "users"."c" = $3)`,
			args: []any{1, 2, 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := sel(tt.node).Compile()
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql = %s\nwant %s", sql, tt.sql)
			}
			if len(args) != len(tt.args) {
				t.Fatalf("args = %#v, want %#v", args, tt.args)
			}
			for i := range tt.args {
				if args[i] != tt.args[i] {
					t.Errorf("args[%d] = %#v, want %#v", i, args[i], tt.args[i])
				}
			}
		})
	}
}

func TestCompile_selectShape(t *testing.T) {
	limit := 5
	s := &Select{
		From:    users,
		Columns: []Column{col("id"), col("email")},
		Where:   Binary{Op: OpEq, Left: col("active"), Right: Arg{Value: true}},
		OrderBy: []Order{{Column: col("created_at"), Desc: true}, {Column: col("id")}},
		Limit:   &limit,
	}
	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "users"."id", "users"."email" FROM "public"."users"` +
		` WHERE "users"."active" = $1` +
		` ORDER BY "users"."created_at" DESC, "users"."id" ASC LIMIT 5`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	if len(args) != 1 || args[0] != true {
		t.Errorf("args = %#v", args)
	}
}

func TestCompile_unqualifiedSource(t *testing.T) {
	// A source with no schema is legal and resolves through the search path.
	// The column has to come from that same occurrence: a column belongs to
	// the source it was built from, not to any table of the same name.
	bare := NewSource("", "users")
	s := &Select{From: bare, Columns: []Column{{Source: bare, Name: "id"}}}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if want := `SELECT "users"."id" FROM "users"`; sql != want {
		t.Errorf("sql = %s, want %s", sql, want)
	}
}

func TestCompile_errors(t *testing.T) {
	// These are the shapes the public API cannot produce — And and Or collapse
	// their degenerate cases, and a descriptor always carries a source. The
	// compiler refuses them anyway, because a tree that renders to nothing at
	// all is a far worse outcome than an error.
	tests := []struct {
		name string
		sel  *Select
		want string
	}{
		{
			name: "no source",
			sel:  &Select{Columns: []Column{col("id")}},
			want: "no source table",
		},
		{
			name: "no columns",
			sel:  &Select{From: users},
			want: "no columns",
		},
		{
			name: "negative limit",
			sel:  &Select{From: users, Columns: []Column{col("id")}, Limit: ptr(-1)},
			want: "negative limit",
		},
		{
			name: "empty group",
			sel:  sel(Group{Op: OpAnd}),
			want: "empty AND group",
		},
		{
			name: "group with a non-boolean operator",
			sel:  sel(Group{Op: OpEq, Items: []Node{Bool{}}}),
			want: "group operator",
		},
		{
			name: "unknown binary operator",
			sel:  sel(Binary{Op: 99, Left: col("id"), Right: Arg{Value: 1}}),
			want: "binary operator",
		},
		{
			name: "unknown unary operator",
			sel:  sel(Unary{Op: 99, X: col("id")}),
			want: "unary operator",
		},
		{
			name: "column with no source",
			sel:  sel(Binary{Op: OpEq, Left: Column{Name: "id"}, Right: Arg{Value: 1}}),
			want: "refers to no source",
		},
		{
			name: "empty identifier",
			sel:  &Select{From: users, Columns: []Column{{Source: users, Name: ""}}},
			want: "empty SQL identifier",
		},
		{
			name: "identifier with a NUL byte",
			sel:  &Select{From: users, Columns: []Column{{Source: users, Name: "a\x00b"}}},
			want: "NUL byte",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := tt.sel.Compile()
			if err == nil {
				t.Fatal("Compile succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestIsTrue(t *testing.T) {
	if !IsTrue(Bool{Value: true}) {
		t.Error("Bool{true} is not TRUE")
	}
	for _, n := range []Node{Bool{Value: false}, col("id"), nil} {
		if IsTrue(n) {
			t.Errorf("%#v reported as TRUE", n)
		}
	}
}

func TestOp_string(t *testing.T) {
	if got := OpILike.String(); got != "ILIKE" {
		t.Errorf("OpILike = %q", got)
	}
	if got := Op(99).String(); got != "?" {
		t.Errorf("an unregistered operator renders as %q", got)
	}
}

func ptr[T any](v T) *T { return &v }

func TestCompile_clauseOrder(t *testing.T) {
	// PostgreSQL fixes this order. Writing them in any other one is a syntax
	// error, so the assertion is about the grammar rather than about taste.
	limit, offset := 50, 100
	s := &Select{
		From:      users,
		Columns:   []Column{col("id")},
		Where:     Binary{Op: OpEq, Left: col("active"), Right: Arg{Value: true}},
		OrderBy:   []Order{{Column: col("created_at"), Desc: true}},
		Limit:     &limit,
		Offset:    &offset,
		ForUpdate: true,
	}
	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "users"."id" FROM "public"."users"` +
		` WHERE "users"."active" = $1` +
		` ORDER BY "users"."created_at" DESC` +
		` LIMIT 50 OFFSET 100 FOR UPDATE`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %#v", args)
	}
}

func TestCompile_offsetAlone(t *testing.T) {
	offset := 10
	s := &Select{From: users, Columns: []Column{col("id")}, Offset: &offset}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if want := prefix + " OFFSET 10"; sql != want {
		t.Errorf("sql = %s, want %s", sql, want)
	}
}

func TestCompile_negativeOffset(t *testing.T) {
	s := &Select{From: users, Columns: []Column{col("id")}, Offset: ptr(-1)}
	if _, _, err := s.Compile(); err == nil || !strings.Contains(err.Error(), "negative offset") {
		t.Errorf("error = %v, want a negative offset", err)
	}
}

func TestCompile_count(t *testing.T) {
	limit, offset := 50, 100
	tests := []struct {
		name string
		sel  *Select
		want string
	}{
		{
			name: "unrestricted",
			sel:  &Select{From: users, Columns: []Column{col("id")}},
			want: `SELECT count(*) FROM (SELECT 1 FROM "public"."users") AS "_orm_count"`,
		},
		{
			name: "with a condition",
			sel: &Select{From: users, Columns: []Column{col("id")},
				Where: Binary{Op: OpEq, Left: col("active"), Right: Arg{Value: true}}},
			want: `SELECT count(*) FROM (SELECT 1 FROM "public"."users" WHERE "users"."active" = $1) AS "_orm_count"`,
		},
		{
			// A bare count(*) with a LIMIT would count the limit away and
			// report the whole table. Wrapping is what keeps the answer equal
			// to the number of rows All would return.
			name: "with a limit",
			sel:  &Select{From: users, Columns: []Column{col("id")}, Limit: &limit},
			want: `SELECT count(*) FROM (SELECT 1 FROM "public"."users" LIMIT 50) AS "_orm_count"`,
		},
		{
			name: "with an offset",
			sel:  &Select{From: users, Columns: []Column{col("id")}, Offset: &offset},
			want: `SELECT count(*) FROM (SELECT 1 FROM "public"."users" OFFSET 100) AS "_orm_count"`,
		},
		{
			name: "with both",
			sel:  &Select{From: users, Columns: []Column{col("id")}, Limit: &limit, Offset: &offset},
			want: `SELECT count(*) FROM (SELECT 1 FROM "public"."users" LIMIT 50 OFFSET 100) AS "_orm_count"`,
		},
		{
			// Ordering cannot change how many rows there are, and sorting rows
			// nobody reads is work for nothing.
			name: "ordering is dropped",
			sel: &Select{From: users, Columns: []Column{col("id")},
				OrderBy: []Order{{Column: col("created_at"), Desc: true}}},
			want: `SELECT count(*) FROM (SELECT 1 FROM "public"."users") AS "_orm_count"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, _, err := CountFrom(tt.sel).Compile()
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if sql != tt.want {
				t.Errorf("sql = %s\nwant %s", sql, tt.want)
			}
		})
	}
}

func TestCompile_countLeavesTheOriginalAlone(t *testing.T) {
	// The count wraps a copy. If it rewrote the statement in place, the
	// query it came from would afterwards select the constant 1.
	sel := &Select{From: users, Columns: []Column{col("id")},
		OrderBy: []Order{{Column: col("id")}}}
	if _, _, err := CountFrom(sel).Compile(); err != nil {
		t.Fatalf("Compile: %v", err)
	}
	sql, _, err := sel.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if want := prefix + ` ORDER BY "users"."id" ASC`; sql != want {
		t.Errorf("the original statement became %s\nwant %s", sql, want)
	}
}

func TestCompile_exists(t *testing.T) {
	zero := 0
	tests := []struct {
		name string
		sel  *Select
		want string
	}{
		{
			name: "unrestricted",
			sel:  &Select{From: users, Columns: []Column{col("id")}},
			want: `SELECT EXISTS (SELECT 1 FROM "public"."users")`,
		},
		{
			name: "with a condition",
			sel: &Select{From: users, Columns: []Column{col("id")},
				Where: Binary{Op: OpEq, Left: col("id"), Right: Arg{Value: 1}}},
			want: `SELECT EXISTS (SELECT 1 FROM "public"."users" WHERE "users"."id" = $1)`,
		},
		{
			// A query limited to no rows matches nothing, and the limit has to
			// reach the inner statement for that to be what the server is
			// asked.
			name: "limited to nothing",
			sel:  &Select{From: users, Columns: []Column{col("id")}, Limit: &zero},
			want: `SELECT EXISTS (SELECT 1 FROM "public"."users" LIMIT 0)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, _, err := ExistsFrom(tt.sel).Compile()
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if sql != tt.want {
				t.Errorf("sql = %s\nwant %s", sql, tt.want)
			}
		})
	}
}

func TestCompile_aliasedSource(t *testing.T) {
	manager := users.As("manager")
	s := &Select{
		From:    manager,
		Columns: []Column{{Source: manager, Name: "id"}, {Source: manager, Name: "email"}},
		Where:   Binary{Op: OpEq, Left: Column{Source: manager, Name: "email"}, Right: Arg{Value: "a@example.com"}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "manager"."id", "manager"."email" FROM "public"."users" AS "manager"` +
		` WHERE "manager"."email" = $1`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
}

func TestSource_asDoesNotDisturbTheOriginal(t *testing.T) {
	base := NewSource("public", "users")
	alias := base.As("manager")

	if base.alias != "" {
		t.Errorf("As changed the source it was called on: %+v", base)
	}
	if alias == base {
		t.Error("As returned the same source; two occurrences must be two values")
	}
	if alias.schema != base.schema || alias.table != base.table {
		t.Errorf("the alias names a different table: %+v", alias)
	}
	if got := alias.Ref(); got != "manager" {
		t.Errorf("columns of the alias qualify against %q", got)
	}
	if got := base.Ref(); got != "users" {
		t.Errorf("columns of the original qualify against %q", got)
	}
	// Two aliases of one table are two occurrences, not one.
	if a, b := base.As("a"), base.As("a"); a == b {
		t.Error("two calls to As returned the same source")
	}
}

func TestCompile_scopeRejectsAnUnavailableSource(t *testing.T) {
	manager := users.As("manager")
	// The query selects from users; manager is a different occurrence and
	// nothing brings it into scope, so a predicate over it names a table the
	// FROM clause does not.
	s := sel(Binary{Op: OpEq, Left: Column{Source: manager, Name: "email"}, Right: Arg{Value: "a"}})

	_, _, err := s.Compile()
	if err == nil {
		t.Fatal("Compile accepted a column from a table the query does not select from")
	}
	var scopeErr *ScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("error = %v (%T), want a *ScopeError", err, err)
	}
	if scopeErr.Column != "email" || scopeErr.Source != manager {
		t.Errorf("the error does not name the offending column: %+v", scopeErr)
	}
	for _, want := range []string{
		`column "manager"."email" is not available`,
		"the query selects from:",
		"public.users",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error =\n%s\nwant it to contain %q", err, want)
		}
	}
}

func TestCompile_scopeRejectsAnOrderingOutsideIt(t *testing.T) {
	manager := users.As("manager")
	s := &Select{From: users, Columns: []Column{col("id")},
		OrderBy: []Order{{Column: Column{Source: manager, Name: "id"}}}}
	if _, _, err := s.Compile(); err == nil {
		t.Error("Compile accepted an ordering term outside the query's scope")
	}
}

func TestCompile_invalidAlias(t *testing.T) {
	tests := []struct {
		name  string
		alias string
		want  string
	}{
		{name: "empty", alias: "", want: "cannot be empty"},
		{name: "reserved prefix", alias: "_orm_count", want: "reserved"},
		{name: "NUL byte", alias: "a\x00b", want: "NUL byte"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := users.As(tt.alias)
			s := &Select{From: src, Columns: []Column{{Source: src, Name: "id"}}}
			_, _, err := s.Compile()
			if err == nil {
				t.Fatalf("Compile accepted the alias %q", tt.alias)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestScope_aliasCollision(t *testing.T) {
	// One frame cannot hold two occurrences under one name: the second would
	// shadow the first, and every column of the first would silently resolve
	// to the wrong table.
	var s Scope
	s.Push()
	a := users.As("u")
	b := NewSource("public", "posts").As("u")

	if err := s.Add(a); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := s.Add(b)
	if err == nil {
		t.Fatal("Add accepted a second occurrence under the same alias")
	}
	var collision *AliasCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("error = %v (%T), want an *AliasCollisionError", err, err)
	}
	if collision.Alias != "u" {
		t.Errorf("the error names alias %q", collision.Alias)
	}
	// Introducing one occurrence twice is the same ambiguity, and PostgreSQL
	// refuses it for the same reason: FROM users JOIN users names one table
	// twice and every column of it becomes ambiguous. A second occurrence
	// needs its own alias.
	dup := s.Add(a)
	if dup == nil {
		t.Fatal("Add accepted the same occurrence twice")
	}
	if !errors.As(dup, &collision) {
		t.Fatalf("error = %v (%T), want an *AliasCollisionError", dup, dup)
	}
	if !strings.Contains(dup.Error(), "introduced twice") {
		t.Errorf("error = %v, want it to say the occurrence is introduced twice", dup)
	}
}

func TestCompile_rawFragment(t *testing.T) {
	tests := []struct {
		name string
		node Node
		sql  string
		args []any
	}{
		{
			name: "alone",
			node: Raw{SQL: "score > $1", Args: []any{100}},
			sql:  prefix + " WHERE score > $1",
			args: []any{100},
		},
		{
			name: "after a typed predicate, renumbered",
			node: Group{Op: OpAnd, Items: []Node{
				Binary{Op: OpEq, Left: col("active"), Right: Arg{Value: true}},
				Raw{SQL: "score > $1", Args: []any{100}},
			}},
			sql:  prefix + ` WHERE "users"."active" = $1 AND score > $2`,
			args: []any{true, 100},
		},
		{
			name: "before a typed predicate",
			node: Group{Op: OpAnd, Items: []Node{
				Raw{SQL: "score > $1", Args: []any{100}},
				Binary{Op: OpEq, Left: col("active"), Right: Arg{Value: true}},
			}},
			sql:  prefix + ` WHERE score > $1 AND "users"."active" = $2`,
			args: []any{100, true},
		},
		{
			name: "two fragments",
			node: Group{Op: OpAnd, Items: []Node{
				Raw{SQL: "score > $1", Args: []any{100}},
				Raw{SQL: "rank < $1", Args: []any{5}},
			}},
			sql:  prefix + " WHERE score > $1 AND rank < $2",
			args: []any{100, 5},
		},
		{
			// A local placeholder used twice binds one argument, as it would in
			// a statement written by hand.
			name: "one placeholder referenced twice",
			node: Raw{SQL: "score > $1 OR backup_score > $1", Args: []any{100}},
			sql:  prefix + " WHERE score > $1 OR backup_score > $1",
			args: []any{100},
		},
		{
			name: "two placeholders out of order",
			node: Raw{SQL: "score BETWEEN $2 AND $1", Args: []any{100, 1}},
			sql:  prefix + " WHERE score BETWEEN $1 AND $2",
			args: []any{1, 100},
		},
		{
			name: "a literal that looks like a placeholder",
			node: Raw{SQL: "label = '$1' AND score > $1", Args: []any{100}},
			sql:  prefix + " WHERE label = '$1' AND score > $1",
			args: []any{100},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := sel(tt.node).Compile()
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql = %s\nwant %s", sql, tt.sql)
			}
			if len(args) != len(tt.args) {
				t.Fatalf("args = %#v, want %#v", args, tt.args)
			}
			for i := range tt.args {
				if args[i] != tt.args[i] {
					t.Errorf("args[%d] = %#v, want %#v", i, args[i], tt.args[i])
				}
			}
		})
	}
}

func TestCompile_rawFragmentValidation(t *testing.T) {
	_, _, err := sel(Raw{SQL: "score > $2", Args: []any{100}}).Compile()
	if err == nil {
		t.Fatal("Compile accepted a fragment referring past its arguments")
	}
	if !errors.Is(err, ErrRawPlaceholder) {
		t.Errorf("error = %v, want it to wrap ErrRawPlaceholder", err)
	}
}
