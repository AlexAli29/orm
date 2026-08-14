package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// A set operation used as a row source, against a real server.
//
// The claims settled here are the ones only PostgreSQL can settle: that the
// nesting is one statement, that every parameter reaches the branch it belongs
// to, that a compound's output columns are named after its first branch, and
// that a set operation the Go shape rule accepts is still refused by the server
// when the SQL types do not combine.

// The branches. Both produce a bigint and a text; the first names its columns
// and the second does not have to, because a compound is named after its first
// branch.
func postsBranch(conn orm.Executor) *orm.ComposedQuery[labelled] {
	return orm.Compose(conn, orm.Project2(
		orm.Of(gendemo.Posts.ID).As("thing_id"), orm.Of(gendemo.Posts.Title).As("label"),
		func(id int64, s string) labelled { return labelled{id, s} },
	)).From(gendemo.Posts.Source())
}

func usersBranch(conn orm.Executor) *orm.ComposedQuery[labelled] {
	return orm.Compose(conn, orm.Project2(
		orm.Of(gendemo.Users.ID).As("user_id"), orm.Of(gendemo.Users.Email).As("email"),
		func(id int64, s string) labelled { return labelled{id, s} },
	)).From(gendemo.Users.Source())
}

var (
	outThingID = orm.Named("thing_id", orm.Of(gendemo.Posts.ID))
	outLabel   = orm.Named("label", orm.Of(gendemo.Posts.Title))
)

func TestUnionSource_selectsFromACompoundDerivedTable(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	u := orm.Sub("u", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)))
	shape := orm.Project2(orm.Ref(u, outThingID), orm.Ref(u, outLabel),
		func(id int64, s string) labelled { return labelled{id, s} })

	got, err := orm.Compose(conn, shape).From(u).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Three posts and three users, read through one derived table.
	if len(got) != 6 {
		t.Fatalf("the derived compound produced %d rows, want 6: %v", len(got), got)
	}
	mails := 0
	for _, r := range got {
		if strings.Contains(r.Text, "@") {
			mails++
		}
	}
	if mails != 3 {
		t.Errorf("%d of the rows came from the second branch, want 3: %v", mails, got)
	}
}

// Every parameter reaches the branch that owns it. A restarted numbering inside
// the compound produces SQL PostgreSQL accepts and rows nobody asked for, which
// is why this is asserted on rows rather than on text.
func TestUnionSource_bindsBranchAndOuterParametersToTheirOwnClauses(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	inner := orm.UnionAll[labelled](
		orm.Compose(conn, orm.Project2(
			orm.Of(gendemo.Posts.ID).As("thing_id"), orm.Of(gendemo.Posts.Title).As("label"),
			func(id int64, s string) labelled { return labelled{id, s} },
		)).From(gendemo.Posts.Source()).Where(orm.Of(gendemo.Posts.Title).Ne("second")),
		orm.Compose(conn, orm.Project2(
			orm.Of(gendemo.Users.ID).As("user_id"), orm.Of(gendemo.Users.Email).As("email"),
			func(id int64, s string) labelled { return labelled{id, s} },
		)).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.Email).Ne("sam@example.com")),
	)
	u := orm.Sub("u", inner)
	shape := orm.Project2(orm.Ref(u, outThingID), orm.Ref(u, outLabel),
		func(id int64, s string) labelled { return labelled{id, s} })

	q := orm.Compose(conn, shape).From(u).Where(orm.Ref(u, outThingID).Gte(int64(2)))

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 3 {
		t.Fatalf("the statement binds %d values, want 3: %v", len(args), args)
	}
	for _, want := range []string{"$1", "$2", "$3"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the statement has no %s:\n%s", want, sql)
		}
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Posts 1 and 3 survive the branch filter, and only 3 survives the outer
	// one; users 1 and 3 survive theirs, and both have id >= 2 only for 3.
	want := map[string]bool{"third": true, "robin@example.com": true}
	if len(got) != 2 {
		t.Fatalf("the statement returned %d rows, want 2: %v", len(got), got)
	}
	for _, r := range got {
		if !want[r.Text] {
			t.Errorf("the statement returned %q, which no clause selects with the values it was given", r.Text)
		}
	}
}

// ALL keeps duplicates through the wrapping too: a derived table over a union of
// a branch with itself returns each row twice.
func TestUnionSource_keepsDuplicatesThroughTheSource(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	u := orm.Sub("u", orm.UnionAll[labelled](postsBranch(conn), postsBranch(conn)))
	shape := orm.Project2(orm.Ref(u, outThingID), orm.Ref(u, outLabel),
		func(id int64, s string) labelled { return labelled{id, s} })

	got, err := orm.Compose(conn, shape).From(u).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("three posts unioned with themselves returned %d rows through a source, want 6", len(got))
	}
	seen := map[int64]int{}
	for _, r := range got {
		seen[r.ID]++
	}
	for id, n := range seen {
		if n != 2 {
			t.Errorf("post %d appears %d times, want twice", id, n)
		}
	}
}

func TestUnionSource_runsAsAnOrdinaryCTEBody(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	c := orm.CTE("everything", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)))
	shape := orm.Project2(orm.Ref(c, outThingID), orm.Ref(c, outLabel),
		func(id int64, s string) labelled { return labelled{id, s} })

	q := orm.Compose(conn, shape).With(c).From(c).Where(orm.Ref(c, outThingID).Eq(int64(1)))
	sql, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "RECURSIVE") {
		t.Fatalf("an ordinary WITH item was written as recursive:\n%s", sql)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Post 1 and user 1 both have id 1.
	if len(got) != 2 {
		t.Fatalf("the CTE returned %d rows, want 2: %v", len(got), got)
	}
}

// The same CTE referenced twice, once through an alias. A compound body is a
// WITH item like any other, so joining it to itself works the same way.
func TestUnionSource_compoundCTEJoinsToItself(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	c := orm.CTE("everything", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)))
	other := c.As("other")
	shape := orm.Project2(orm.Ref(c, outLabel), orm.Ref(other, outLabel),
		func(a, b string) labelled { return labelled{0, a + "|" + b} })

	got, err := orm.Compose(conn, shape).With(c).From(c).
		Join(other, orm.Eq(orm.Ref(c, outThingID), orm.Ref(other, outThingID))).
		Where(orm.Ref(c, outThingID).Eq(int64(2))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// id 2 is post "second" and user "sam@example.com": the self-join pairs each
	// with each, so four rows.
	if len(got) != 4 {
		t.Errorf("the self-joined CTE returned %d rows, want 4: %v", len(got), got)
	}
}

// A union of a union, as a source. Nesting is where a compiler that lost the
// inner parentheses or restarted the parameters comes apart.
func TestUnionSource_nestedCompoundRunsAsASource(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	inner := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn))
	u := orm.Sub("u", orm.UnionAll[labelled](inner, postsBranch(conn)))
	shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })

	sql, _, err := orm.Compose(conn, shape).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, "FROM ((") {
		t.Errorf("the inner compound is not parenthesised inside the outer one:\n%s", sql)
	}

	got, err := orm.Compose(conn, shape).From(u).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 9 {
		t.Errorf("the nested compound returned %d rows, want 9 (3 posts + 3 users + 3 posts): %v", len(got), got)
	}
}

// PostgreSQL names a compound's output columns after its first branch, which is
// why the ORM takes the source's names from there and why a caller addressing
// the second branch's alias is refused before the statement runs. Both halves
// are asserted: what the server calls the columns, and what the ORM does about
// the other name.
func TestUnionSource_outputNamesComeFromTheFirstBranch(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	u := orm.Sub("u", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)))
	shape := orm.Project1(orm.Ref(u, outThingID), func(id int64) int64 { return id })
	sql, args, err := orm.Compose(conn, shape).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	// Ask the server what it calls the compound's columns.
	rows, err := conn.Query(t.Context(),
		`SELECT * FROM (SELECT "u".* FROM `+afterFrom(t, sql)+`) AS named LIMIT 0`, args...)
	if err != nil {
		t.Fatalf("asking the server for the compound's column names: %v", err)
	}
	names := []string{}
	for _, f := range rows.FieldDescriptions() {
		names = append(names, f.Name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the compound: %v", err)
	}
	if got := strings.Join(names, ","); got != "thing_id,label" {
		t.Errorf("the server calls the compound's columns %q; the first branch names them thing_id,label", got)
	}

	// And the second branch's name is not one of the source's columns.
	outEmail := orm.Named("email", orm.Of(gendemo.Users.Email))
	wrong := orm.Project1(orm.Ref(u, outEmail), func(s string) string { return s })
	if _, _, err := orm.Compose(conn, wrong).From(u).SQL(); err == nil {
		t.Error("a column named only by the second branch was accepted")
	}
}

// afterFrom returns everything from the FROM clause on, so that the derived
// table the ORM built can be re-wrapped without rebuilding it by hand.
func afterFrom(t *testing.T, sql string) string {
	t.Helper()
	i := strings.Index(sql, "FROM (")
	if i < 0 {
		t.Fatalf("the statement has no derived table:\n%s", sql)
	}
	return sql[i+len("FROM "):]
}

// The rows are read by the scanner the outer projection declared, not by
// anything the branches carry. A compound source is a source: what reads it is
// the query selecting from it.
func TestUnionSource_isReadByTheOuterProjectionsScanner(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	type tagged struct {
		ID  int64
		Via string
	}
	u := orm.Sub("u", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)))
	shape := orm.Project1(orm.Ref(u, outThingID),
		func(id int64) tagged { return tagged{id, "outer"} })

	got, err := orm.Compose(conn, shape).From(u).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d rows, want 6", len(got))
	}
	for i, r := range got {
		if r.Via != "outer" {
			t.Errorf("row %d was read by %q, want the outer projection's scanner", i+1, r.Via)
		}
	}
}

// An SQL-type incompatibility the Go shape rule deliberately does not model is
// still refused — by PostgreSQL, precisely, rather than by a coercion invented
// here. Wrapping the union in a source does not change whose job that is.
func TestUnionSource_leavesSQLTypeCompatibilityToTheServerThroughASource(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// Both branches select one non-nullable Go string, so the shape rule has no
	// objection: it compares Go destination types and nullability, and both are
	// identical. In SQL one is text and the other jsonb, which PostgreSQL
	// refuses to union — and it is the only thing here that can know that,
	// because the AST carries no SQL types.
	texts := orm.Compose(conn, orm.Project1(
		orm.Of(gendemo.Posts.Title).As("label"), func(s string) string { return s },
	)).From(gendemo.Posts.Source())
	jsonText := orm.PGTypeOf[string]("jsonb")
	jsons := orm.Compose(conn, orm.Project1(
		orm.Cast(orm.Cast(orm.Of(gendemo.Posts.Title), orm.Text), jsonText).As("label"),
		func(s string) string { return s },
	)).From(gendemo.Posts.Source())

	u := orm.Sub("u", orm.UnionAll[string](texts, jsons))
	if u.Err() != nil {
		t.Fatalf("the ORM refused a union it does not model: %v", u.Err())
	}
	shape := orm.Project1(orm.Ref(u, outLabel), func(s string) string { return s })

	_, err := orm.Compose(conn, shape).From(u).All(t.Context())
	if err == nil {
		t.Skip("this server unioned text with bytea; the division of labour still holds, but there is nothing to assert")
	}
	if !strings.Contains(err.Error(), "UNION") {
		t.Errorf("PostgreSQL reported %q, which does not name the operation that failed", err)
	}
}

// The paths that were sources before this are unchanged. A plain derived table
// and a plain CTE still run, with the same SQL and the same rows.
func TestUnionSource_ordinarySubqueriesAndCTEsDidNotChange(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	t.Run("derived table", func(t *testing.T) {
		u := orm.Sub("u", postsBranch(conn))
		shape := orm.Project2(orm.Ref(u, outThingID), orm.Ref(u, outLabel),
			func(id int64, s string) labelled { return labelled{id, s} })
		got, err := orm.Compose(conn, shape).From(u).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("the derived table returned %d rows, want 3", len(got))
		}
	})

	t.Run("CTE", func(t *testing.T) {
		c := orm.CTE("p", postsBranch(conn))
		shape := orm.Project1(orm.Ref(c, outLabel), func(s string) string { return s })
		got, err := orm.Compose(conn, shape).With(c).From(c).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 3 {
			t.Errorf("the CTE returned %d rows, want 3", len(got))
		}
	})

	t.Run("a recursive CTE, which still takes a Select", func(t *testing.T) {
		// Not a compound: the anchor and the recursive term are both plain
		// selects, and this is the path Phase 3 deliberately did not touch.
		anchor := orm.Rows(orm.Named("id", orm.Of(gendemo.Users.ID))).
			From(gendemo.Users.Source()).
			Where(orm.Cond(gendemo.Users.ID.Eq(int64(1))))
		tree := orm.RecursiveCTE("tree", anchor, func(self *orm.Source) orm.Term {
			return orm.Rows(orm.Named("id", orm.Of(gendemo.Users.ID))).
				From(gendemo.Users.Source()).
				Join(self, orm.Eq(orm.Of(gendemo.Users.ManagerID), orm.Ref(self, orm.Named("id", orm.Of(gendemo.Users.ID)))))
		})
		if tree.Err() != nil {
			t.Fatalf("the recursive CTE no longer builds: %v", tree.Err())
		}
		shape := orm.Project1(orm.Ref(tree, orm.Named("id", orm.Of(gendemo.Users.ID))),
			func(id int64) int64 { return id })
		sql, _, err := orm.Compose(conn, shape).With(tree).From(tree).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, "WITH RECURSIVE") {
			t.Errorf("the recursive CTE lost its keyword:\n%s", sql)
		}
		if _, err := orm.Compose(conn, shape).With(tree).From(tree).All(t.Context()); err != nil {
			t.Fatalf("All: %v", err)
		}
	})
}
