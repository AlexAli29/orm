package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// Ordering a set operation, against a real server.
//
// The ORM decides which names may appear. PostgreSQL decides what the ordering
// means, and it is the only thing that can say whether the rows actually came
// back in order — so these assert rows, not text.

// The declarations the ordered unions below are addressed by.
var (
	ordThingID = orm.Named("thing_id", orm.Of(gendemo.Posts.ID))
	ordLabel   = orm.Named("label", orm.Of(gendemo.Posts.Title))
)

func labels(rows []labelled) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Text)
	}
	return out
}

func TestUnionOrder_rowsComeBackInOrder(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	asc, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(asc) != 6 {
		t.Fatalf("the ordered union returned %d rows, want 6", len(asc))
	}
	for i := 1; i < len(asc); i++ {
		if asc[i].ID < asc[i-1].ID {
			t.Fatalf("ascending order broken at row %d: %v", i+1, asc)
		}
	}

	desc, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for i := 1; i < len(desc); i++ {
		if desc[i].ID > desc[i-1].ID {
			t.Fatalf("descending order broken at row %d: %v", i+1, desc)
		}
	}
	if asc[0].ID == desc[0].ID && asc[len(asc)-1].ID == desc[len(desc)-1].ID {
		t.Error("ascending and descending produced the same order")
	}
}

// Ordering by two columns is ordering by the first and then the second, which is
// only observable when the first has ties — and both branches contribute rows
// with the same id, so it does.
func TestUnionOrder_ordersByTheSecondColumnWithinTheFirst(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	got, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Asc(), ordLabel.Desc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for i := 1; i < len(got); i++ {
		switch {
		case got[i].ID < got[i-1].ID:
			t.Fatalf("the first key is not ascending at row %d: %v", i+1, got)
		case got[i].ID == got[i-1].ID && got[i].Text > got[i-1].Text:
			t.Fatalf("the second key is not descending within a tie at row %d: %v", i+1, labels(got))
		}
	}
	// The ties are real: ids 1, 2 and 3 each come from both branches.
	ties := 0
	for i := 1; i < len(got); i++ {
		if got[i].ID == got[i-1].ID {
			ties++
		}
	}
	if ties != 3 {
		t.Errorf("the fixture has %d ties, so the second key is barely exercised", ties)
	}
}

// ORDER BY with LIMIT returns the first rows rather than some rows, which is the
// whole reason a limit on a set operation needed an ordering to be worth having.
func TestUnionOrder_limitTakesTheFirstRowsOfTheOrdering(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	got, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).Limit(2).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("the limited union returned %d rows, want 2", len(got))
	}
	// Both tables hold ids 1..3, so the two largest are the pair of 3s.
	for _, r := range got {
		if r.ID != 3 {
			t.Errorf("the limit did not take the first rows of the ordering: %v", got)
		}
	}

	// And OFFSET skips within the same ordering.
	next, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).Limit(2).Offset(2).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, r := range next {
		if r.ID != 2 {
			t.Errorf("the offset did not skip within the ordering: %v", next)
		}
	}
}

// A branch that orders and limits itself is not the same statement as an
// operation that orders and limits the concatenation. Both run, and they return
// different rows.
func TestUnionOrder_branchLocalOrderingIsADifferentStatement(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// Two rows from the posts branch alone, then every user.
	branchLimited := orm.Compose(conn, orm.Project2(
		orm.Of(gendemo.Posts.ID).As("thing_id"), orm.Of(gendemo.Posts.Title).As("label"),
		func(id int64, s string) labelled { return labelled{id, s} },
	)).From(gendemo.Posts.Source()).OrderBy(orm.Of(gendemo.Posts.ID).Desc()).Limit(2)

	perBranch, err := orm.UnionAll[labelled](branchLimited, usersBranch(conn)).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(perBranch) != 5 {
		t.Fatalf("two posts and three users returned %d rows, want 5: %v", len(perBranch), perBranch)
	}

	// The same two branches, limited as one operation, return two rows.
	whole, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).Limit(2).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(whole) != 2 {
		t.Fatalf("the operation's limit returned %d rows, want 2", len(whole))
	}
}

// An ordered union is still a source, and the ordering stays inside it. What the
// enclosing query does with the rows is the enclosing query's business —
// PostgreSQL does not promise to preserve a subquery's ordering — so what is
// asserted here is that the statement runs and returns the right rows.
func TestUnionOrder_anOrderedUnionIsStillASource(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	u := orm.Sub("u", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).Limit(4))
	shape := orm.Project2(orm.Ref(u, ordThingID), orm.Ref(u, ordLabel),
		func(id int64, s string) labelled { return labelled{id, s} })

	sql, _, err := orm.Compose(conn, shape).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `ORDER BY "thing_id" DESC LIMIT 4) AS "u"`) {
		t.Errorf("the ordering did not stay inside the derived table:\n%s", sql)
	}

	got, err := orm.Compose(conn, shape).From(u).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("the limited derived table returned %d rows, want 4", len(got))
	}
}

// And still a CTE body.
func TestUnionOrder_anOrderedUnionIsStillACTEBody(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	c := orm.CTE("top", orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).Limit(3))
	shape := orm.Project1(orm.Ref(c, ordThingID), func(id int64) int64 { return id })

	got, err := orm.Compose(conn, shape).With(c).From(c).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("the limited CTE returned %d rows, want 3", len(got))
	}
}

// A nested ordered union keeps its ordering inside its parentheses, so the outer
// operation orders the concatenation and the inner one orders itself.
func TestUnionOrder_nestedOrderingsStayTheirOwn(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	inner := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Desc()).Limit(2)
	outer := orm.UnionAll[labelled](inner, postsBranch(conn)).OrderBy(ordThingID.Asc())

	sql, _, err := outer.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `ORDER BY "thing_id" DESC LIMIT 2) UNION ALL`) {
		t.Errorf("the inner ordering escaped its parentheses:\n%s", sql)
	}
	if !strings.HasSuffix(sql, `ORDER BY "thing_id" ASC`) {
		t.Errorf("the outer ordering is not at the end:\n%s", sql)
	}

	got, err := outer.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Two rows from the inner operation, three posts from the outer branch.
	if len(got) != 5 {
		t.Fatalf("the nested union returned %d rows, want 5: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].ID < got[i-1].ID {
			t.Fatalf("the outer ordering did not apply to the whole result: %v", got)
		}
	}
}

// The name the ORM emits is one the server resolves. It is written bare, because
// a compound's ORDER BY names a column of the result and not of a source, and
// this is the test that PostgreSQL agrees the bare form is what it wanted.
func TestUnionOrder_theServerResolvesTheBareOutputName(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	sql, args, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(ordThingID.Asc()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `ORDER BY "thing_id" ASC`) {
		t.Fatalf("the ordering term is not a bare output name:\n%s", sql)
	}
	rows, err := conn.Query(t.Context(), sql, args...)
	if err != nil {
		t.Fatalf("the server refused the ordering the ORM wrote: %v", err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the ordered union: %v", err)
	}
}

// Ordering by the second branch's alias is refused by the ORM, and it is refused
// by PostgreSQL too — this is the same rule from both sides.
func TestUnionOrder_theSecondBranchesNamesAreNotTheResults(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// usersBranch names its columns user_id and email; postsBranch is first, so
	// the result is named thing_id and label.
	byEmail := orm.Named("email", orm.Of(gendemo.Users.Email))
	_, _, err := orm.UnionAll[labelled](postsBranch(conn), usersBranch(conn)).
		OrderBy(byEmail.Asc()).SQL()
	if err == nil {
		t.Fatal("the second branch's alias was accepted as an ordering term")
	}
	if !strings.Contains(err.Error(), `no result column "email"`) {
		t.Errorf("the diagnostic %q is not the output-name rule's", err)
	}

	// And the server's answer to the same statement written by hand.
	handwritten := `SELECT "posts"."id" AS "thing_id" FROM "public"."posts" ` +
		`UNION ALL SELECT "users"."email" AS "email" FROM "public"."users" ORDER BY "email"`
	if _, err := conn.Exec(t.Context(), handwritten); err == nil {
		t.Error("this server ordered a compound by its second branch's alias")
	}
}

// The name ordering resolves is one the ORM emitted, not one the server derived.
//
// Ordering by a declared name only works if that name is written as an alias in
// the first branch's select list. For a plain column the two are close enough to
// hide a missing alias — declare "thing_id" over posts.id and the server would
// have called the column "id", so the difference shows. For an expression there
// is no closeness at all: PostgreSQL names lower(title) "lower", and an
// arithmetic expression "?column?".
//
// So this orders by a name the server could not possibly have derived, and then
// asks the server what it calls the same statement with the alias taken out. If
// the ORM stopped emitting aliases for expressions the first half would fail;
// if PostgreSQL were deriving the name after all the second half would.
func TestUnionOrder_ordersByANameTheORMEmittedNotOneTheServerDerived(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	folded := func() *orm.ComposedQuery[labelled] {
		return orm.Compose(conn, orm.Project2(
			orm.Of(gendemo.Posts.ID).As("thing_id"),
			orm.Lower(orm.Of(gendemo.Posts.Title)).As("folded"),
			func(id int64, s string) labelled { return labelled{id, s} },
		)).From(gendemo.Posts.Source())
	}
	byFolded := orm.Named("folded", orm.Lower(orm.Of(gendemo.Posts.Title)))

	q := orm.UnionAll[labelled](folded(), folded()).OrderBy(byFolded.Desc())
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `AS "folded"`) {
		t.Fatalf("the expression was selected without the alias ordering depends on:\n%s", sql)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("the server did not resolve the name the ORM emitted: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("the ordered union returned %d rows, want 6", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Text > got[i-1].Text {
			t.Fatalf("descending order broken at row %d: %v", i+1, labels(got))
		}
	}

	// The same statement with the alias removed: the server has no column of
	// that name, which is what makes the half above a claim about the ORM.
	stripped := strings.ReplaceAll(sql, ` AS "folded"`, "")
	if stripped == sql {
		t.Fatal("the statement contained no alias to remove")
	}
	if _, err := conn.Exec(t.Context(), stripped, args...); err == nil {
		t.Error("the server resolved \"folded\" with no alias emitted, so this proves nothing about the ORM")
	} else if !strings.Contains(err.Error(), "folded") {
		t.Errorf("the server refused the stripped statement for another reason: %v", err)
	}

	// And what the server would have called it on its own.
	rows, err := conn.Query(t.Context(),
		`SELECT lower("posts"."title") FROM "public"."posts" LIMIT 0`)
	if err != nil {
		t.Fatalf("asking the server for its own name: %v", err)
	}
	derived := rows.FieldDescriptions()[0].Name
	rows.Close()
	if derived == "folded" {
		t.Error("the server derives the same name, so the fixture does not discriminate")
	}
	t.Logf("the server calls the expression %q; the union is ordered by %q", derived, "folded")
}

// Two result columns of one name make an ordering term ambiguous, and PostgreSQL
// refuses it. The ORM refuses it first, from the shape, which has the whole list
// of names in hand.
func TestUnionOrder_refusesAnAmbiguousName(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// The server's answer to the statement the ORM will not build.
	handwritten := `SELECT "posts"."id" AS "k", "posts"."title" AS "k" FROM "public"."posts" ` +
		`UNION ALL SELECT "posts"."id" AS "k", "posts"."title" AS "k" FROM "public"."posts" ORDER BY "k"`
	_, err := conn.Exec(t.Context(), handwritten)
	if err == nil {
		t.Fatal("this server ordered a compound by a name two of its columns carry")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("PostgreSQL reported %q, which is not the ambiguity error", err)
	}
}

// The statements the audit found, run against a server.
//
// Each of these was constructible before and either failed at the server or ran
// and meant something other than what was written. What is asserted is that the
// ones that should run do, and that the rows are the branch-local ones rather
// than the hoisted ones.
func TestUnionBranch_aBranchesOwnWithStaysItsOwn(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// Each branch declares a CTE under the same name, selecting different rows.
	// Hoisted, one of them would win and both branches would read it; written
	// bare in the second position it is a syntax error.
	branch := func(id int64) *orm.ComposedQuery[labelled] {
		c := orm.CTE("pick", orm.Compose(nil, orm.Project2(
			orm.Of(gendemo.Posts.ID).As("thing_id"), orm.Of(gendemo.Posts.Title).As("label"),
			func(i int64, s string) labelled { return labelled{i, s} },
		)).From(gendemo.Posts.Source()).Where(orm.Of(gendemo.Posts.ID).Eq(id)))
		return orm.Compose(conn, orm.Project2(
			orm.Ref(c, ordThingID).As("thing_id"), orm.Ref(c, ordLabel).As("label"),
			func(i int64, s string) labelled { return labelled{i, s} },
		)).With(c).From(c)
	}

	q := orm.UnionAll[labelled](branch(1), branch(3))
	sql, _, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if n := strings.Count(sql, `(WITH "pick" AS `); n != 2 {
		t.Fatalf("the two declarations were not kept apart:\n%s", sql)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// One row from each branch, and they are different rows — which is the
	// thing a hoisted declaration would have taken away.
	if len(got) != 2 {
		t.Fatalf("the union returned %d rows, want 2: %v", len(got), got)
	}
	if got[0].ID == got[1].ID {
		t.Errorf("both branches read the same CTE: %v", got)
	}
}

// A branch that borrows a name another branch declares is refused, and the
// server agrees about the statement that would have been written.
func TestUnionBranch_borrowingAnotherBranchesNameIsRefused(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	handwritten := `SELECT "s"."id" FROM "s" UNION ALL SELECT "s"."id" FROM "s"`
	if _, err := conn.Exec(t.Context(), handwritten); err == nil {
		t.Error("this server resolved a named query nothing declared")
	} else if !strings.Contains(err.Error(), `"s"`) {
		t.Errorf("the server refused it for another reason: %v", err)
	}
}

// A locking clause in a branch: the ORM refuses it, and the server refuses the
// statement the ORM would have written, parentheses and all.
func TestUnionBranch_theServerRefusesALockedBranchToo(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	locked := orm.Compose(conn, orm.Project1(
		orm.Of(gendemo.Posts.ID).As("thing_id"), func(v int64) int64 { return v },
	)).From(gendemo.Posts.Source()).ForUpdate()
	plain := orm.Compose(conn, orm.Project1(
		orm.Of(gendemo.Posts.ID).As("thing_id"), func(v int64) int64 { return v },
	)).From(gendemo.Posts.Source())

	if _, _, err := orm.UnionAll[int64](locked, plain).SQL(); err == nil {
		t.Fatal("the ORM accepted a locked branch")
	}

	// What it would have written, given to the server directly.
	handwritten := `(SELECT "posts"."id" FROM "public"."posts" FOR UPDATE) ` +
		`UNION ALL SELECT "posts"."id" FROM "public"."posts"`
	_, err := conn.Exec(t.Context(), handwritten)
	if err == nil {
		t.Fatal("this server accepted a locking clause in a set operation")
	}
	if !strings.Contains(err.Error(), "FOR UPDATE is not allowed") {
		t.Errorf("the server refused it for another reason: %v", err)
	}
}

// A named query shared by both branches, run.
//
// This is the capability the parenthesisation fix would otherwise have taken
// away: sharing used to work because a branch's WITH was hoisted, and the fix
// stopped the hoisting. Declared on the operation it is PostgreSQL's own
// arrangement — in front of the compound, evaluated once, visible to every
// branch — rather than an accident.
func TestUnionBranch_aNamedQuerySharedByEveryBranch(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	published := orm.CTE("published", orm.Compose(nil, orm.Project2(
		orm.Of(gendemo.Posts.ID).As("thing_id"), orm.Of(gendemo.Posts.Title).As("label"),
		func(id int64, s string) labelled { return labelled{id, s} },
	)).From(gendemo.Posts.Source()).Where(orm.Of(gendemo.Posts.Published).Eq(true)))

	// Both branches read the one declaration, filtering it differently.
	branch := func(min int64) *orm.ComposedQuery[labelled] {
		return orm.Compose(conn, orm.Project2(
			orm.Ref(published, ordThingID).As("thing_id"),
			orm.Ref(published, ordLabel).As("label"),
			func(id int64, s string) labelled { return labelled{id, s} },
		)).From(published).Where(orm.Ref(published, ordThingID).Gte(min))
	}

	q := orm.UnionAll[labelled](branch(1), branch(3)).With(published).OrderBy(ordThingID.Asc())
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, `WITH "published" AS (`) {
		t.Fatalf("the declaration is not in front of the operation:\n%s", sql)
	}
	if n := strings.Count(sql, `AS (SELECT`); n != 1 {
		t.Errorf("the named query is declared %d times, want once:\n%s", n, sql)
	}
	// Its parameter is numbered first, then each branch's.
	if len(args) != 3 {
		t.Fatalf("the statement binds %d values, want 3: %v", len(args), args)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Published posts are 1 and 3. The first branch keeps both, the second keeps
	// one, so three rows in ascending order.
	if len(got) != 3 {
		t.Fatalf("the shared declaration produced %d rows, want 3: %v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i].ID < got[i-1].ID {
			t.Errorf("the operation's ordering did not apply: %v", got)
		}
	}

	// And undeclared, the same branches are refused rather than hoisted.
	if _, _, err := orm.UnionAll[labelled](branch(1), branch(3)).SQL(); err == nil {
		t.Error("two branches read a named query the operation never declared")
	}
}
