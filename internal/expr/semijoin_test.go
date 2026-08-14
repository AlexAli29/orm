package expr_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// The correlated subquery a relation's Any and None compile to.
//
// What is being asserted is that the root statement keeps its shape: one row
// per root row, no join, no deduplication, and the subquery's source visible
// inside it and nowhere else.

func TestSelect_existsIsCorrelated(t *testing.T) {
	users := expr.NewSource("public", "users")
	posts := expr.NewSource("public", "posts")

	sub := &expr.Select{
		From:      posts,
		SelectOne: true,
		Where: expr.Group{Op: expr.OpAnd, Items: []expr.Node{
			expr.Binary{Op: expr.OpEq,
				Left:  expr.Column{Source: posts, Name: "author_id"},
				Right: expr.Column{Source: users, Name: "id"}},
			expr.Binary{Op: expr.OpEq,
				Left:  expr.Column{Source: posts, Name: "published"},
				Right: expr.Arg{Value: true}},
		}},
	}
	s := &expr.Select{
		From:    users,
		Columns: []expr.Column{{Source: users, Name: "id"}},
		Where:   expr.Exists{Sub: sub},
	}

	sql, args, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "users"."id" FROM "public"."users" WHERE EXISTS (` +
		`SELECT 1 FROM "public"."posts" WHERE "posts"."author_id" = "users"."id" AND "posts"."published" = $1)`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	if len(args) != 1 || args[0] != true {
		t.Errorf("args = %v, want [true]", args)
	}
	// A join would have been the other way to write this, and it would return
	// one root row per matching child.
	if strings.Contains(sql, "JOIN") {
		t.Errorf("SQL = %s, an EXISTS must not join the target into the root", sql)
	}
}

func TestSelect_notExists(t *testing.T) {
	users := expr.NewSource("public", "users")
	posts := expr.NewSource("public", "posts")

	s := &expr.Select{
		From:    users,
		Columns: []expr.Column{{Source: users, Name: "id"}},
		Where: expr.Exists{Not: true, Sub: &expr.Select{
			From:      posts,
			SelectOne: true,
			Where: expr.Binary{Op: expr.OpEq,
				Left:  expr.Column{Source: posts, Name: "author_id"},
				Right: expr.Column{Source: users, Name: "id"}},
		}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "users"."id" FROM "public"."users" WHERE NOT EXISTS (` +
		`SELECT 1 FROM "public"."posts" WHERE "posts"."author_id" = "users"."id")`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
	// The other way to ask this — an outer join tested for NULL — changes the
	// cardinality on the way and is easy to get subtly wrong.
	if strings.Contains(sql, "IS NULL") {
		t.Errorf("SQL = %s, want NOT EXISTS rather than an outer join tested for NULL", sql)
	}
}

// Two subqueries in one statement each get their own frame, so neither leaks
// its source into the other and both correlate against the root.
func TestSelect_severalExists(t *testing.T) {
	users := expr.NewSource("public", "users")
	posts := expr.NewSource("public", "posts")
	profiles := expr.NewSource("public", "profiles")

	correlated := func(child *expr.Source, col string) expr.Node {
		return expr.Exists{Sub: &expr.Select{
			From:      child,
			SelectOne: true,
			Where: expr.Binary{Op: expr.OpEq,
				Left:  expr.Column{Source: child, Name: col},
				Right: expr.Column{Source: users, Name: "id"}},
		}}
	}
	s := &expr.Select{
		From:    users,
		Columns: []expr.Column{{Source: users, Name: "id"}},
		Where: expr.Group{Op: expr.OpAnd, Items: []expr.Node{
			correlated(posts, "author_id"),
			expr.Unary{Op: expr.OpNot, X: correlated(profiles, "user_id")},
		}},
	}
	sql, _, err := s.Compile()
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	const want = `SELECT "users"."id" FROM "public"."users" WHERE ` +
		`EXISTS (SELECT 1 FROM "public"."posts" WHERE "posts"."author_id" = "users"."id") AND ` +
		`(NOT EXISTS (SELECT 1 FROM "public"."profiles" WHERE "profiles"."user_id" = "users"."id"))`
	if sql != want {
		t.Errorf("SQL =\n%s\nwant\n%s", sql, want)
	}
}

// A subquery's source is in scope inside it and out of scope after it. Without
// that, a column of the related table would compile in the clause that follows,
// and PostgreSQL would be the one to complain.
func TestSelect_existsScopeIsPopped(t *testing.T) {
	users := expr.NewSource("public", "users")
	posts := expr.NewSource("public", "posts")

	s := &expr.Select{
		From:    users,
		Columns: []expr.Column{{Source: users, Name: "id"}},
		Where: expr.Group{Op: expr.OpAnd, Items: []expr.Node{
			expr.Exists{Sub: &expr.Select{
				From:      posts,
				SelectOne: true,
				Where: expr.Binary{Op: expr.OpEq,
					Left:  expr.Column{Source: posts, Name: "author_id"},
					Right: expr.Column{Source: users, Name: "id"}},
			}},
			// Outside the subquery this table is not selected from.
			expr.Binary{Op: expr.OpEq,
				Left:  expr.Column{Source: posts, Name: "published"},
				Right: expr.Arg{Value: true}},
		}},
	}
	_, _, err := s.Compile()
	if err == nil {
		t.Fatal("compiled a column of the subquery's table outside the subquery")
	}
	var se *expr.ScopeError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a scope error", err)
	}
	if se.Column != "published" {
		t.Errorf("scope error names %q, want published", se.Column)
	}
}

func TestSelect_existsWithoutSubquery(t *testing.T) {
	users := expr.NewSource("public", "users")
	s := &expr.Select{
		From:    users,
		Columns: []expr.Column{{Source: users, Name: "id"}},
		Where:   expr.Exists{},
	}
	_, _, err := s.Compile()
	if err == nil || !strings.Contains(err.Error(), "no subquery") {
		t.Errorf("error = %v, want it to report a missing subquery", err)
	}
}
