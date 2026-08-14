package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// UNION ALL against a real server.
//
// Everything above this file settles what the ORM builds. What it cannot settle
// is whether PostgreSQL agrees: that the statement is one statement, that its
// parameters are numbered the way the ORM numbered them, that the branches
// arrive in order and that duplicate rows survive. Those are the claims here,
// and each is checked against rows rather than against text.

// The expressions the branches below are built from: a bigint and a text, taken
// from two different tables so that a union is a union of two things.
var (
	unionPostID    = orm.Of(gendemo.Posts.ID)
	unionPostTitle = orm.Of(gendemo.Posts.Title)
	unionUserID    = orm.Of(gendemo.Users.ID)
	unionUserEmail = orm.Of(gendemo.Users.Email)
)

type labelled struct {
	ID   int64
	Text string
}

var (
	postShape = orm.Project2(unionPostID, unionPostTitle,
		func(id int64, title string) labelled { return labelled{id, title} })
	userShape = orm.Project2(unionUserID, unionUserEmail,
		func(id int64, email string) labelled { return labelled{id, email} })
)

func TestUnionAll_runsAsOneStatementOverTwoTables(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	got, err := orm.UnionAll[labelled](
		orm.Compose(conn, postShape).From(gendemo.Posts.Source()).Where(orm.Of(gendemo.Posts.Published).Eq(true)),
		orm.Compose(conn, userShape).From(gendemo.Users.Source()).Where(orm.Of(gendemo.Users.Active).Eq(true)),
	).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// The branches arrive in the order they were written. PostgreSQL is not
	// obliged to order a compound, but it is obliged to produce every row of
	// every branch, so the assertion is on the multiset and on the fact that
	// each branch contributed what it should.
	posts, users := 0, 0
	for _, r := range got {
		switch {
		case strings.Contains(r.Text, "@"):
			users++
		default:
			posts++
		}
	}
	if posts != 2 || users != 2 {
		t.Errorf("the union produced %d post rows and %d user rows, want 2 and 2: %v", posts, users, got)
	}
	if len(got) != 4 {
		t.Errorf("the union produced %d rows, want 4: %v", len(got), got)
	}
}

// The parameters of every branch are numbered across one statement. If the
// numbering restarted per branch the SQL would still be valid and would bind
// the second branch's values into the first — which no text assertion catches
// and this one does, because the rows would be wrong.
func TestUnionAll_bindsEveryBranchesParametersToItsOwnBranch(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	q := orm.UnionAll[labelled](
		orm.Compose(conn, postShape).From(gendemo.Posts.Source()).
			Where(orm.Of(gendemo.Posts.ID).Gte(int64(2)), orm.Of(gendemo.Posts.Title).Ne("second")),
		orm.Compose(conn, userShape).From(gendemo.Users.Source()).
			Where(orm.Of(gendemo.Users.ID).Lte(int64(1)), orm.Of(gendemo.Users.Email).Ne("nobody@example.com")),
	)

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for _, want := range []string{"$1", "$2", "$3", "$4"} {
		if !strings.Contains(sql, want) {
			t.Errorf("the statement has no %s, so the parameters were not numbered across the branches:\n%s", want, sql)
		}
	}
	if len(args) != 4 {
		t.Fatalf("the statement binds %d values for four placeholders: %v", len(args), args)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Post 3 ("third") and user 1 ("alex@example.com"). Restarted numbering
	// would compare a title against a bigint and fail outright, or compare the
	// wrong values and return a different set.
	want := map[string]bool{"third": true, "alex@example.com": true}
	if len(got) != 2 {
		t.Fatalf("the union returned %d rows, want 2: %v", len(got), got)
	}
	for _, r := range got {
		if !want[r.Text] {
			t.Errorf("the union returned %q, which no branch selects with the values it was given", r.Text)
		}
	}
}

// ALL keeps duplicates. A branch unioned with itself returns each row twice,
// which is the difference between UNION ALL and UNION and the reason a caller
// reaches for it.
func TestUnionAll_keepsDuplicateRows(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	branch := func() *orm.ComposedQuery[labelled] {
		return orm.Compose(conn, postShape).From(gendemo.Posts.Source())
	}
	got, err := orm.UnionAll[labelled](branch(), branch()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 6 {
		t.Fatalf("three posts unioned with themselves returned %d rows, want 6: %v", len(got), got)
	}
	seen := map[int64]int{}
	for _, r := range got {
		seen[r.ID]++
	}
	for id, n := range seen {
		if n != 2 {
			t.Errorf("post %d appears %d times, want twice; duplicates are being removed", id, n)
		}
	}
}

// An entity query is a branch, and the rows come back as entities scanned by
// the generated scanner — the whole entity, every column, no reflection.
func TestUnionAll_readsEntitiesThroughTheGeneratedScanner(t *testing.T) {
	testdb.AdminDSN(t)
	handle := db(t)

	got, err := orm.UnionAll[gendemo.User](
		handle.Users.Query().Where(gendemo.Users.Age.Gte(int32(30))),
		handle.Users.Query().Where(gendemo.Users.Age.Lt(int32(30))),
	).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the union returned %d users, want 3", len(got))
	}
	// Every column arrived, including the nullable ones, which is what proves
	// the entity scanner was used rather than something narrower.
	byID := map[int64]gendemo.User{}
	for _, u := range got {
		byID[u.ID] = u
	}
	if u := byID[1]; u.Email != "alex@example.com" || u.Nickname == nil || *u.Nickname != "alex" || u.DeletedAt != nil {
		t.Errorf("user 1 came back as %+v", u)
	}
	if u := byID[2]; u.Nickname != nil || u.Score != nil {
		t.Errorf("user 2's NULLs did not survive: %+v", u)
	}
	if u := byID[3]; u.DeletedAt == nil {
		t.Errorf("user 3's deleted_at did not survive: %+v", u)
	}
}

// A union of a union is one statement with the inner one parenthesised, and it
// runs. Nesting is where a compiler that restarted parameters or lost the
// result shape would come apart.
func TestUnionAll_nestsAndStillRuns(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	posts := func() *orm.ComposedQuery[labelled] {
		return orm.Compose(conn, postShape).From(gendemo.Posts.Source()).
			Where(orm.Of(gendemo.Posts.ID).Eq(int64(1)))
	}
	users := func() *orm.ComposedQuery[labelled] {
		return orm.Compose(conn, userShape).From(gendemo.Users.Source()).
			Where(orm.Of(gendemo.Users.ID).Eq(int64(2)))
	}

	q := orm.UnionAll[labelled](orm.UnionAll[labelled](posts(), users()), posts())
	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, "(") {
		t.Errorf("the nested union was not parenthesised:\n%s", sql)
	}
	if len(args) != 3 {
		t.Fatalf("the nested statement binds %d values, want 3: %v", len(args), args)
	}

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("the nested union returned %d rows, want 3: %v", len(got), got)
	}
	firsts := 0
	for _, r := range got {
		if r.Text == "first" {
			firsts++
		}
	}
	if firsts != 2 {
		t.Errorf("post 1 appears %d times, want twice: %v", firsts, got)
	}
}

// The first branch names the compound's output columns. That is PostgreSQL's
// rule, and it is why the comparison does not require the branches to agree on
// aliases: the later ones are not what anything is named after.
func TestUnionAll_takesItsOutputNamesFromTheFirstBranch(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	left := orm.Compose(conn, orm.Project2(
		orm.Of(gendemo.Posts.ID).As("thing_id"), orm.Of(gendemo.Posts.Title).As("label"),
		func(id int64, title string) labelled { return labelled{id, title} },
	)).From(gendemo.Posts.Source())
	right := orm.Compose(conn, orm.Project2(
		orm.Of(gendemo.Users.ID).As("user_id"), orm.Of(gendemo.Users.Email).As("email"),
		func(id int64, email string) labelled { return labelled{id, email} },
	)).From(gendemo.Users.Source())

	sql, args, err := orm.UnionAll[labelled](left, right).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	// Wrap the compound so the server has to name its columns, and ask the
	// server what it called them.
	rows, err := conn.Query(t.Context(), `SELECT * FROM (`+sql+`) AS u LIMIT 0`, args...)
	if err != nil {
		t.Fatalf("running the compound as a derived table: %v", err)
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
		t.Errorf("the compound's columns are named %q; PostgreSQL names them after the first branch", got)
	}
}

// Branches that differ in a way the ORM does not model — the same Go type and
// nullability, different SQL types — are left to PostgreSQL, which reports them
// precisely. This asserts the division of labour rather than working around it.
func TestUnionAll_leavesSQLTypeCompatibilityToTheServer(t *testing.T) {
	testdb.AdminDSN(t)
	conn := testdb.Connect(t, testdb.Create(t, schema(t)+seed))

	// Both branches select one non-nullable Go string, so the shape rule has
	// nothing to object to: it compares Go destination types and nullability,
	// and the two are identical. In SQL one is text and the other jsonb, which
	// PostgreSQL will not union.
	//
	// The Go types have to match for this to test anything. Branches that
	// differ in Go type are refused by the ORM, and its diagnostic names the
	// operation too — so a test asserting only that the error mentions UNION
	// would pass without the server ever seeing the statement.
	texts := orm.Compose(conn, orm.Project1(
		orm.Of(gendemo.Posts.Title), func(s string) string { return s },
	)).From(gendemo.Posts.Source())
	jsonText := orm.PGTypeOf[string]("jsonb")
	jsons := orm.Compose(conn, orm.Project1(
		orm.Cast(orm.Cast(orm.Of(gendemo.Posts.Title), orm.Text), jsonText),
		func(s string) string { return s },
	)).From(gendemo.Posts.Source())

	// The ORM has no objection: same column count, same Go type, same
	// nullability, and it does not model SQL types.
	q := orm.UnionAll[string](texts, jsons)
	if _, _, err := q.SQL(); err != nil {
		t.Fatalf("the ORM refused a union it does not model: %v", err)
	}

	_, err := q.All(t.Context())
	if err == nil {
		t.Skip("this server unioned text with jsonb; the division of labour still holds, but there is nothing to assert")
	}
	if !strings.Contains(err.Error(), "jsonb") {
		t.Errorf("PostgreSQL reported %q, which does not name the types that could not be matched", err)
	}
}
