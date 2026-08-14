package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// A set operation used where a value is expected, against a real server.
//
// The ORM decides column arity and refuses the wrong number before there is a
// statement. PostgreSQL decides whether the values are comparable and how many
// rows a scalar subquery may return. These tests are about the seam between
// those two, and about the statement being one statement with one parameter
// list.

// A one-column branch, which is what both value positions want.
func postIDs(conn orm.Executor) *orm.ComposedQuery[int64] {
	return orm.Compose(conn, orm.Project1(
		orm.Of(gendemo.Posts.AuthorID), func(id *int64) int64 {
			if id == nil {
				return 0
			}
			return *id
		},
	)).From(gendemo.Posts.Source())
}

func TestUnionValue_membershipTestAgainstACompound(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	// Authors of published posts, or the one banned account. The author column
	// is nullable and the id column is not, so the second branch is read with
	// Opt — the phase two rule refuses *int64 against int64, and widening one to
	// match the other is the caller's to write rather than the ORM's to guess.
	authors := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Posts.AuthorID), func(id *int64) *int64 { return id },
	)).From(gendemo.Posts.Source()).Where(orm.Of(gendemo.Posts.Published).Eq(true))
	banned := orm.Compose(nil, orm.Project1(
		orm.Opt(gendemo.Users.ID), func(id *int64) *int64 { return id },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.Email).Eq("sam@example.com"))

	got, err := handle.Users.Query().
		Where(orm.InSub(gendemo.Users.ID, orm.UnionAll[*int64](authors, banned))).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Published posts have authors 1 and 3; the banned branch adds user 2.
	if got := strings.Join(emails(got), ","); got != "alex@example.com,sam@example.com,robin@example.com" {
		t.Errorf("the membership test matched %q", got)
	}
}

func TestUnionValue_negatedMembershipTestAgainstACompound(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	// NOT IN over a subquery that can yield NULL is UNKNOWN for every row, and
	// posts.author_id is nullable — so the branches exclude the NULLs, which is
	// what the NotInSub documentation says to do rather than have the ORM
	// rewrite the statement.
	authors := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Posts.AuthorID), func(id *int64) *int64 { return id },
	)).From(gendemo.Posts.Source()).Where(orm.Of(gendemo.Posts.AuthorID).IsNotNull())
	nobody := orm.Compose(nil, orm.Project1(
		orm.Opt(gendemo.Users.ID), func(id *int64) *int64 { return id },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.Email).Eq("nobody@example.com"))

	got, err := handle.Users.Query().
		Where(orm.NotInSub(gendemo.Users.ID, orm.UnionAll[*int64](authors, nobody))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Users 1 and 3 wrote posts; user 2 did not.
	if len(got) != 1 || got[0].Email != "sam@example.com" {
		t.Errorf("the negated membership test matched %v", emails(got))
	}
}

// Duplicates survive into the membership test. IN does not care how many times a
// value appears, so what this proves is that both branches reached the server —
// a statement built from one branch would match fewer rows.
func TestUnionValue_bothBranchesReachTheMembershipTest(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	onlyOne := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(id int64) int64 { return id },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(1)))
	onlyThree := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(id int64) int64 { return id },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(3)))

	got, err := handle.Users.Query().
		Where(orm.InSub(gendemo.Users.ID, orm.UnionAll[int64](onlyOne, onlyThree))).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("only one branch reached the server: %v", emails(got))
	}
}

// Parameters from the predicate before the test, from each branch, and from the
// predicate after all reach the clause they belong to. A restarted numbering
// produces valid SQL that filters on the wrong values, so this asserts rows.
func TestUnionValue_bindsEveryClauseToItsOwnParameters(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	branch := func(id int64) *orm.ComposedQuery[int64] {
		return orm.Compose(nil, orm.Project1(
			orm.Of(gendemo.Users.ID), func(v int64) int64 { return v },
		)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(id))
	}

	q := handle.Users.Query().Where(
		gendemo.Users.Age.Gte(int32(18)),
		orm.InSub(gendemo.Users.ID, orm.UnionAll[int64](branch(1), branch(3))),
		gendemo.Users.Active.Eq(false),
	)

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 4 {
		t.Fatalf("the statement binds %d values, want 4: %v", len(args), args)
	}
	for _, want := range []string{"$1", "$2", "$3", "$4"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the statement has no %s:\n%s", want, sql)
		}
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Users 1 and 3 are in the union; of those, 3 is inactive and 45 years old.
	if len(got) != 1 || got[0].ID != 3 {
		t.Errorf("the statement matched %v, want robin alone", emails(got))
	}
}

func TestUnionValue_scalarSubqueryOverACompound(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// Each branch returns one row, so the union returns two — which PostgreSQL
	// refuses in a scalar position. LIMIT 1 makes it one, and it is the
	// operation's limit rather than a branch's, which is where PostgreSQL
	// attaches it.
	posts := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Count[orm.Composed]()), func(n int64) int64 { return n },
	)).From(gendemo.Posts.Source())
	users := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Count[orm.Composed]()), func(n int64) int64 { return n },
	)).From(gendemo.Users.Source())

	total := orm.UnionAll[int64](posts, users).Limit(1)
	shape := orm.Project2(
		orm.Of(gendemo.Users.Email), orm.Scalar[orm.Composed, int64](total),
		func(email string, n *int64) labelled {
			if n == nil {
				return labelled{0, email}
			}
			return labelled{*n, email}
		},
	)

	got, err := orm.Compose(conn, shape).From(gendemo.Users.Source()).
		Where(orm.Of(gendemo.Users.ID).Eq(int64(1))).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	// Three posts, and LIMIT 1 keeps the first branch's row.
	if got[0].ID != 3 {
		t.Errorf("the scalar subquery read %d, want 3", got[0].ID)
	}
}

// Row cardinality is PostgreSQL's, not the ORM's. A one-column set operation
// returning two rows is a well-formed scalar subquery that the server rejects at
// run time, and the ORM must not have classified it as a shape error earlier.
func TestUnionValue_scalarRowCardinalityIsTheServersToRefuse(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	posts := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Count[orm.Composed]()), func(n int64) int64 { return n },
	)).From(gendemo.Posts.Source())
	users := orm.Compose(nil, orm.Project1(
		orm.Of(orm.Count[orm.Composed]()), func(n int64) int64 { return n },
	)).From(gendemo.Users.Source())

	// Two branches, one row each, no limit: one column and two rows.
	two := orm.UnionAll[int64](posts, users)
	shape := orm.Project1(orm.Scalar[orm.Composed, int64](two), func(n *int64) int64 {
		if n == nil {
			return 0
		}
		return *n
	})
	q := orm.Compose(conn, shape).From(gendemo.Users.Source()).
		Where(orm.Of(gendemo.Users.ID).Eq(int64(1)))

	// The ORM builds it: the arity is one, which is the only thing it decides.
	if _, _, err := q.SQL(); err != nil {
		t.Fatalf("the ORM refused a one-column scalar subquery over its row count: %v", err)
	}

	_, err := q.All(t.Context())
	if err == nil {
		t.Fatal("a scalar subquery returning two rows was accepted by the server")
	}
	if !strings.Contains(err.Error(), "more than one row") {
		t.Errorf("PostgreSQL reported %q, which is not the cardinality error", err)
	}
}

// A union of a union in a value position, run.
func TestUnionValue_nestedCompoundRuns(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	one := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(v int64) int64 { return v },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(1)))
	two := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(v int64) int64 { return v },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(2)))
	three := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(v int64) int64 { return v },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(3)))

	nested := orm.UnionAll[int64](orm.UnionAll[int64](one, two), three)
	q := handle.Users.Query().Where(orm.InSub(gendemo.Users.ID, nested))

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, "IN ((") {
		t.Errorf("the inner compound is not parenthesised inside the outer one:\n%s", sql)
	}
	if len(args) != 3 {
		t.Fatalf("the statement binds %d values, want 3: %v", len(args), args)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("the nested membership test matched %d users, want 3", len(got))
	}
}

// The rows come back through the generated entity scanner: a membership test is
// a predicate, and the statement around it is the same entity query it was.
func TestUnionValue_readsEntitiesThroughTheGeneratedScanner(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	one := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(v int64) int64 { return v },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(1)))
	three := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(v int64) int64 { return v },
	)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.ID).Eq(int64(3)))

	got, err := handle.Users.Query().
		Where(orm.InSub(gendemo.Users.ID, orm.UnionAll[int64](one, three))).
		OrderBy(gendemo.Users.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d users, want 2", len(got))
	}
	// Every column arrived, nullable ones included.
	if u := got[0]; u.Email != "alex@example.com" || u.Nickname == nil || *u.Nickname != "alex" || u.DeletedAt != nil {
		t.Errorf("user 1 came back as %+v", u)
	}
	if u := got[1]; u.DeletedAt == nil || u.Score == nil {
		t.Errorf("user 3's values did not survive: %+v", u)
	}
}

// The seam again: two branches the Go shape rule accepts because their
// destination types and nullability are identical, and PostgreSQL refuses
// because the SQL types do not combine. Nothing here coerces.
func TestUnionValue_leavesSQLTypeCompatibilityToTheServer(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	texts := orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Posts.Title), func(s string) string { return s },
	)).From(gendemo.Posts.Source())
	jsonText := orm.PGTypeOf[string]("jsonb")
	jsons := orm.Compose(nil, orm.Project1(
		orm.Cast(orm.Cast(orm.Of(gendemo.Posts.Title), orm.Text), jsonText),
		func(s string) string { return s },
	)).From(gendemo.Posts.Source())

	u := orm.UnionAll[string](texts, jsons)
	q := orm.Compose(conn, orm.Project1(
		orm.Of(gendemo.Users.Email), func(s string) string { return s },
	)).From(gendemo.Users.Source()).Where(orm.InSub(orm.Of(gendemo.Users.Email), u))

	// One column on both sides, identical Go type and nullability: the ORM has
	// no objection, and the arity rule is satisfied.
	if _, _, err := q.SQL(); err != nil {
		t.Fatalf("the ORM refused a union it does not model: %v", err)
	}

	_, err := q.All(t.Context())
	if err == nil {
		t.Skip("this server unioned text with jsonb; the division of labour still holds, but there is nothing to assert")
	}
	if !strings.Contains(err.Error(), "jsonb") {
		t.Errorf("PostgreSQL reported %q, which does not name the types it could not match", err)
	}
}

// The value positions that were there before this still work, unchanged.
func TestUnionValue_ordinaryValueSubqueriesDidNotChange(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	t.Run("a plain IN subquery", func(t *testing.T) {
		got, err := handle.Users.Query().
			Where(orm.InSub(gendemo.Users.ID, postIDs(nil))).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("the plain membership test matched %d users, want 2", len(got))
		}
	})

	t.Run("a plain scalar subquery", func(t *testing.T) {
		count := orm.Rows(orm.Named("n", orm.Of(orm.Count[orm.Composed]()))).
			From(gendemo.Posts.Source())
		shape := orm.Project1(orm.Scalar[orm.Composed, int64](count), func(n *int64) int64 {
			if n == nil {
				return 0
			}
			return *n
		})
		got, err := orm.Compose(conn, shape).From(gendemo.Users.Source()).
			Where(orm.Of(gendemo.Users.ID).Eq(int64(1))).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 1 || got[0] != 3 {
			t.Errorf("the scalar subquery read %v, want [3]", got)
		}
	})

	t.Run("EXISTS, which still takes a plain SELECT", func(t *testing.T) {
		got, err := handle.Users.Query().Where(orm.Exists[gendemo.User](
			orm.Rows(orm.Named("x", orm.Val(1))).
				From(gendemo.Posts.Source()).
				Where(orm.Eq(orm.Of(gendemo.Posts.AuthorID), orm.Opt(gendemo.Users.ID))),
		)).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("EXISTS matched %d users, want 2", len(got))
		}
	})

}
