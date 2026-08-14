package orm_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
)

// UNION ALL through the public API.
//
// What is under test is the contract a caller sees: which builders can be a
// branch, what makes two branches compatible, what a mismatch says, and that a
// union which validated renders one statement with one parameter list.
//
// The SQL is asserted in full rather than by substring. A compound is exactly
// the place where a fragment can look right and bind the wrong values.

// The projections the branches below are built from. They are values, so the
// same shape can be selected from two different sources — which is what most
// unions are for.
var (
	unionActive = orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Email),
		func(id int64, email string) [2]any { return [2]any{id, email} },
	)
	unionPosts = orm.Project2(
		orm.Of(Posts.ID), orm.Cast(orm.Val("archived"), orm.Text).As("email"),
		func(id int64, email string) [2]any { return [2]any{id, email} },
	)
)

func composedUsers() *orm.ComposedQuery[[2]any] {
	return orm.Compose(nil, unionActive).From(usersSrc)
}

func composedPosts() *orm.ComposedQuery[[2]any] {
	return orm.Compose(nil, unionPosts).From(postsSrc)
}

func TestUnion_rendersOneStatementFromTwoBranches(t *testing.T) {
	sql, args, err := orm.UnionAll[[2]any](composedUsers(), composedPosts()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id", CAST($1 AS "text") AS "email" FROM "public"."posts"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 || args[0] != "archived" {
		t.Errorf("args = %v, want [archived]", args)
	}
}

// One statement, one parameter list, numbered across the branches in the order
// the SQL reads. Restarting the numbering per branch produces SQL that is valid
// and binds the wrong values, which is the failure this asserts against.
func TestUnion_numbersPlaceholdersAcrossEveryBranch(t *testing.T) {
	left := orm.Compose(nil, unionActive).
		From(usersSrc).
		Where(orm.Of(Users.Age).Gt(18), orm.Of(Users.Email).Eq("a@example.com"))
	right := orm.Compose(nil, unionActive).
		From(usersSrc).
		Where(orm.Of(Users.Age).Lt(65), orm.Of(Users.Email).Eq("b@example.com"))

	sql, args, err := orm.UnionAll[[2]any](left, right).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`WHERE "users"."age" > $1 AND "users"."email" = $2 ` +
		`UNION ALL ` +
		`SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`WHERE "users"."age" < $3 AND "users"."email" = $4`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	wantArgs := []any{int32(18), "a@example.com", int32(65), "b@example.com"}
	if len(args) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Errorf("$%d = %#v, want %#v", i+1, args[i], wantArgs[i])
		}
	}
}

// Three branches are one operation over three inputs, not a tree of pairs.
func TestUnion_takesMoreThanTwoBranches(t *testing.T) {
	sql, _, err := orm.UnionAll[[2]any](composedUsers(), composedUsers(), composedUsers()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if n := strings.Count(sql, "UNION ALL"); n != 2 {
		t.Errorf("three branches produced %d operators:\n%s", n, sql)
	}
	if strings.Contains(sql, "(SELECT") {
		t.Errorf("three peers were rendered as nested pairs:\n%s", sql)
	}
}

// A validated union is itself a branch, and the shape it was validated with
// travels with the value rather than being recovered from the SQL it renders.
func TestUnion_nestsAsABranchOfAnotherUnion(t *testing.T) {
	inner := orm.UnionAll[[2]any](composedUsers(), composedPosts())
	sql, args, err := orm.UnionAll[[2]any](inner, composedUsers()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `(SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id", CAST($1 AS "text") AS "email" FROM "public"."posts") ` +
		`UNION ALL ` +
		`SELECT "users"."id", "users"."email" FROM "public"."users"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want one", args)
	}
}

// A nested union carries the shape it validated, so a mismatch against it is
// caught at the outer level too. If the shape were lost on the way out, this
// would render happily and fail in PostgreSQL.
func TestUnion_aNestedUnionStillValidatesItsOuterBranches(t *testing.T) {
	inner := orm.UnionAll[[2]any](composedUsers(), composedPosts())
	wrongWidth := orm.Compose(nil, orm.Project3(
		orm.Of(Users.ID), orm.Of(Users.Email), orm.Of(Users.Active),
		func(id int64, email string, ok bool) [2]any { return [2]any{id, email} },
	)).From(usersSrc)

	_, _, err := orm.UnionAll[[2]any](inner, wrongWidth).SQL()
	if err == nil {
		t.Fatal("a nested union accepted a branch of a different width, so its shape was not carried out")
	}
	if !strings.Contains(err.Error(), "2 columns") || !strings.Contains(err.Error(), "3") {
		t.Errorf("the diagnostic %q does not name the two widths", err)
	}
}

// The result type R is not the compatibility rule. Each pair below builds the
// same Go value out of columns that are not the same column, so the compiler is
// satisfied and only the result shape can refuse them.
func TestUnion_refusesBranchesThatAgreeOnlyOnTheGoResultType(t *testing.T) {
	// Differing only in the Go type of the slot: both columns are non-nullable,
	// both branches produce a string, and the columns are text and int32.
	byEmail := orm.Compose(nil, orm.Project1(
		orm.Of(Users.Email), func(s string) string { return s },
	)).From(usersSrc)
	byAge := orm.Compose(nil, orm.Project1(
		orm.Of(Users.Age), func(n int32) string { return strconv.Itoa(int(n)) },
	)).From(usersSrc)

	_, _, err := orm.UnionAll[string](byEmail, byAge).SQL()
	if err == nil {
		t.Fatal("two branches producing string from a text column and an int32 column were accepted; " +
			"R alone is being used as the compatibility rule")
	}
	if !strings.Contains(err.Error(), "column 1") || !strings.Contains(err.Error(), "int32") {
		t.Errorf("the diagnostic %q does not say which column differs and how", err)
	}

	// Differing in nullability as well: a text column against a nullable one,
	// both flattened to a string by the branch that reads them.
	byNickname := orm.Compose(nil, orm.Project1(
		orm.Of(Users.Nickname), func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
	)).From(usersSrc)

	if _, _, err := orm.UnionAll[string](byEmail, byNickname).SQL(); err == nil {
		t.Fatal("a nullable text column was accepted against a non-nullable one")
	}
}

// The construction-time negative matrix: everything the Go compiler cannot
// catch, refused when the union is built and before any SQL exists.
//
// Nullability with an identical Go type is not here because in the typed layer
// it comes from a descriptor rather than from an expression; it is covered by
// TestUnion_refusesEntitiesWhoseDescriptorsDisagree.
func TestUnion_refusesIncompatibleBranchesWhenTheUnionIsBuilt(t *testing.T) {
	twoCols := composedUsers()
	threeCols := orm.Compose(nil, orm.Project3(
		orm.Of(Users.ID), orm.Of(Users.Email), orm.Of(Users.Active),
		func(id int64, email string, ok bool) [2]any { return [2]any{id, email} },
	)).From(usersSrc)
	narrowFirst := orm.Compose(nil, orm.Project2(
		orm.Of(Users.Age), orm.Of(Users.Email),
		func(age int32, email string) [2]any { return [2]any{age, email} },
	)).From(usersSrc)
	cases := []struct {
		name  string
		build func() (string, []any, error)
		want  []string
	}{
		{
			name:  "a different number of columns",
			build: orm.UnionAll[[2]any](twoCols, threeCols).SQL,
			want:  []string{"branch 1 selects 2 columns", "branch 2 selects 3"},
		},
		{
			name:  "a narrower integer in the same slot",
			build: orm.UnionAll[[2]any](twoCols, narrowFirst).SQL,
			want:  []string{"branch 2 result column 1", "int32", "int64"},
		},
		{
			name:  "one branch",
			build: orm.UnionAll[[2]any](composedUsers()).SQL,
			want:  []string{"at least two branches", "given 1"},
		},
		{
			name:  "no branches",
			build: orm.UnionAll[[2]any]().SQL,
			want:  []string{"at least two branches", "given 0"},
		},
		{
			name:  "a nil branch",
			build: orm.UnionAll[[2]any](composedUsers(), nil).SQL,
			want:  []string{"branch 2 is nil"},
		},
		{
			name:  "a branch that failed to build",
			build: orm.UnionAll[[2]any](composedUsers(), orm.Compose(nil, unionActive)).SQL,
			want:  []string{"branch 2"},
		},
		{
			name:  "a negative limit",
			build: orm.UnionAll[[2]any](composedUsers(), composedUsers()).Limit(-1).SQL,
			want:  []string{"LIMIT -1"},
		},
		{
			name:  "a negative offset",
			build: orm.UnionAll[[2]any](composedUsers(), composedUsers()).Offset(-3).SQL,
			want:  []string{"OFFSET -3"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := c.build()
			if err == nil {
				t.Fatalf("%s was accepted and rendered:\n%s", c.name, sql)
			}
			if sql != "" || args != nil {
				t.Errorf("a refused union still produced SQL %q and args %v", sql, args)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the diagnostic %q does not mention %q", err, w)
				}
			}
		})
	}
}

// Every later branch is compared against the first, not against the one before
// it. Otherwise branch 3 could drift from branch 1 by agreeing with branch 2 —
// which matters because branch 1 owns the scanner every row is read with.
func TestUnion_comparesEveryBranchAgainstTheFirst(t *testing.T) {
	// Three branches, all two columns of the same Go types, where the third is
	// nullable in slot 2 and the second is not. Comparing neighbours would
	// accept it if 2 and 3 agreed; here they do not, so the pairing has to be
	// forced the other way: make 2 match 1 and 3 differ from both.
	first := composedUsers()
	second := composedUsers()
	third := orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Nickname),
		func(id int64, s *string) [2]any { return [2]any{id, s} },
	)).From(usersSrc)

	_, _, err := orm.UnionAll[[2]any](first, second, third).SQL()
	if err == nil {
		t.Fatal("a third branch of a different shape was accepted")
	}
	if !strings.Contains(err.Error(), "branch 3") {
		t.Errorf("the diagnostic %q does not name branch 3", err)
	}
	if !strings.Contains(err.Error(), "branch 1") {
		t.Errorf("the diagnostic %q does not say it was compared against branch 1", err)
	}
}

// Two incompatible branches produce one diagnostic naming both, rather than a
// panic or a statement that fails in PostgreSQL. Every mistake is reported —
// the builder accumulates them, as the rest of the package does.
func TestUnion_reportsEveryMistakeAtOnce(t *testing.T) {
	threeCols := orm.Compose(nil, orm.Project3(
		orm.Of(Users.ID), orm.Of(Users.Email), orm.Of(Users.Active),
		func(id int64, email string, ok bool) [2]any { return [2]any{id, email} },
	)).From(usersSrc)

	_, _, err := orm.UnionAll[[2]any](composedUsers(), threeCols, nil).Limit(-2).SQL()
	if err == nil {
		t.Fatal("a union with three separate mistakes was accepted")
	}
	for _, w := range []string{"branch 2", "branch 3 is nil", "LIMIT -2"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the diagnostic %q does not mention %q", err, w)
		}
	}
}

// A query built with Rows has no result type: it exists to be a source. Letting
// one be a branch would produce a union with nothing to scan into.
func TestUnion_refusesASourceOnlyQueryAsABranch(t *testing.T) {
	rows := orm.Rows(orm.Named("id", orm.Of(Users.ID))).From(usersSrc)

	_, _, err := orm.UnionAll[orm.NoResult](rows, rows).SQL()
	if err == nil {
		t.Fatal("a query built with Rows was accepted as a branch")
	}
	if !strings.Contains(err.Error(), "no result shape") {
		t.Errorf("the diagnostic %q does not say the branch cannot describe its result", err)
	}
}

// An entity query is a branch, and its shape comes from the generated
// descriptor: the column order the descriptor declares, its nullability, and
// the Go types its Dest hands out.
func TestUnion_acceptsEntityQueries(t *testing.T) {
	repo := orm.NewRepo(nil, &userMeta)
	sql, args, err := orm.UnionAll[User](
		repo.Query().Where(Users.Age.Gt(18)),
		repo.Query().Where(Users.Active.Eq(false)),
	).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."age" > $1 ` +
		`UNION ALL ` + selectAll + ` WHERE "users"."active" = $2`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 2 {
		t.Errorf("args = %v, want two", args)
	}
}

// A projection over an entity repository is a branch too, so all three builders
// that can describe a result can be one. Its shape is the projection's, which is
// why an entity-rooted projection unions with a composed one of the same shape.
func TestUnion_acceptsProjectionQueriesOverARepository(t *testing.T) {
	repo := orm.NewRepo(stubExecutor{rows: [][]any{{int64(1), "a@example.com"}}}, &userMeta)
	entityRooted := orm.Select(repo, orm.Project2(
		Users.ID, Users.Email,
		func(id int64, email string) [2]any { return [2]any{id, email} },
	)).Where(Users.Active.Eq(true))

	sql, args, err := orm.UnionAll[[2]any](entityRooted, composedUsers()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "users"."email" FROM "public"."users" WHERE "users"."active" = $1 ` +
		`UNION ALL ` +
		`SELECT "users"."id", "users"."email" FROM "public"."users"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want one", args)
	}

	got, err := orm.UnionAll[[2]any](entityRooted, composedUsers()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0][0] != int64(1) {
		t.Errorf("All returned %v", got)
	}
}

// A projection branch whose shape disagrees is refused wherever it was rooted:
// the rule is the shape, not which builder produced it.
func TestUnion_refusesAProjectionQueryOfAnotherShape(t *testing.T) {
	repo := orm.NewRepo(nil, &userMeta)
	narrow := orm.Select(repo, orm.Project2(
		Users.Age, Users.Email,
		func(age int32, email string) [2]any { return [2]any{age, email} },
	))

	_, _, err := orm.UnionAll[[2]any](composedUsers(), narrow).SQL()
	if err == nil {
		t.Fatal("a projection query of another shape was accepted")
	}
	if !strings.Contains(err.Error(), "branch 2 result column 1") {
		t.Errorf("the diagnostic %q does not name the column that differs", err)
	}
}

// The entity branch is not accepted on the strength of E. Two entities of one
// Go type whose descriptors disagree describe different results.
func TestUnion_refusesEntitiesWhoseDescriptorsDisagree(t *testing.T) {
	relaxed := userMeta
	relaxed.Columns = append([]orm.ColumnMeta(nil), userMeta.Columns...)
	relaxed.Columns[1].NotNull = true // the same Go field, a different catalog

	_, _, err := orm.UnionAll[User](
		orm.NewRepo(nil, &userMeta).Query(),
		orm.NewRepo(nil, &relaxed).Query(),
	).SQL()
	if err == nil {
		t.Fatal("two descriptors of one Go type but different nullability were accepted, " +
			"so the entity shape is being read from E")
	}
	if !strings.Contains(err.Error(), "result column 2") {
		t.Errorf("the diagnostic %q does not name the column that differs", err)
	}
}

// LIMIT and OFFSET belong to the operation, not to the last branch. Pushing
// them down would cap a branch and concatenate the rest in full.
func TestUnion_limitAndOffsetApplyToTheWholeResult(t *testing.T) {
	sql, _, err := orm.UnionAll[[2]any](composedUsers(), composedUsers()).Limit(10).Offset(5).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "users"."id", "users"."email" FROM "public"."users" ` +
		`LIMIT 10 OFFSET 5`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
}

// The first branch owns the result. Its scanner reads every row, whatever the
// later branches were built with, and PostgreSQL names the compound's columns
// after it too.
func TestUnion_theFirstBranchOwnsTheScanner(t *testing.T) {
	type tagged struct {
		id  int64
		via string
	}
	fromFirst := orm.Compose[tagged](nil, orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Email),
		func(id int64, email string) tagged { return tagged{id, "first"} },
	)).From(usersSrc)
	fromSecond := orm.Compose[tagged](nil, orm.Project2(
		orm.Of(Users.ID), orm.Of(Users.Email),
		func(id int64, email string) tagged { return tagged{id, "second"} },
	)).From(usersSrc)

	ex := stubExecutor{rows: [][]any{
		{int64(1), "a@example.com"},
		{int64(2), "b@example.com"},
	}}
	got, err := orm.UnionAll[tagged](fromFirst, fromSecond).Using(ex).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	for i, r := range got {
		if r.via != "first" {
			t.Errorf("row %d was read by the %s branch's scanner; the first branch owns the result", i+1, r.via)
		}
	}
}

// The executor comes from the first branch when the caller set none, and Using
// overrides it. A union whose branches were built on a handle should be
// runnable without repeating the handle.
func TestUnion_takesTheExecutorFromTheFirstBranch(t *testing.T) {
	ex := stubExecutor{rows: [][]any{{int64(7), "a@example.com"}}}
	inherited := orm.Compose(ex, unionActive).From(usersSrc)
	none := orm.Compose(nil, unionActive).From(usersSrc)

	got, err := orm.UnionAll[[2]any](inherited, none).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}

	if _, err := orm.UnionAll[[2]any](none, none).All(t.Context()); err == nil {
		t.Error("a union with no executor anywhere ran")
	}
}

// Duplicates are kept and the branches arrive in order. That is what ALL means,
// and it is the property a caller reaching for UNION ALL over UNION wants.
func TestUnion_keepsDuplicatesAndBranchOrder(t *testing.T) {
	closed := 0
	ex := stubExecutor{
		closed: &closed,
		rows: [][]any{
			{int64(1), "a@example.com"},
			{int64(1), "a@example.com"},
			{int64(2), "b@example.com"},
		},
	}
	got, err := orm.UnionAll[[2]any](composedUsers(), composedUsers()).Using(ex).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := [][2]any{
		{int64(1), "a@example.com"},
		{int64(1), "a@example.com"},
		{int64(2), "b@example.com"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i+1, got[i], want[i])
		}
	}
	if closed != 1 {
		t.Errorf("rows closed %d times, want exactly 1", closed)
	}
}

func TestUnion_oneAndRowsReadTheSameStatement(t *testing.T) {
	single := stubExecutor{rows: [][]any{{int64(1), "a@example.com"}}}
	got, err := orm.UnionAll[[2]any](composedUsers(), composedUsers()).Using(single).One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if got[0] != int64(1) {
		t.Errorf("One returned %v", got)
	}

	two := stubExecutor{rows: [][]any{{int64(1), "a@example.com"}, {int64(2), "b@example.com"}}}
	if _, err := orm.UnionAll[[2]any](composedUsers(), composedUsers()).Using(two).One(t.Context()); !errors.Is(err, orm.ErrMultipleRows) {
		t.Errorf("One over two rows returned %v, want ErrMultipleRows", err)
	}

	none := stubExecutor{}
	if _, err := orm.UnionAll[[2]any](composedUsers(), composedUsers()).Using(none).One(t.Context()); !errors.Is(err, orm.ErrNotFound) {
		t.Errorf("One over no rows returned %v, want ErrNotFound", err)
	}

	seen := 0
	for row, err := range orm.UnionAll[[2]any](composedUsers(), composedUsers()).Using(two).Rows(t.Context()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		seen++
		_ = row
	}
	if seen != 2 {
		t.Errorf("Rows yielded %d rows, want 2", seen)
	}
}

// A refused union stays refused: every terminal reports the same thing, so a
// caller cannot reach the database by picking a different one.
func TestUnion_everyTerminalRefusesABrokenUnion(t *testing.T) {
	broken := func() *orm.UnionQuery[[2]any] {
		return orm.UnionAll[[2]any](composedUsers(), nil).Using(stubExecutor{})
	}
	if _, err := broken().All(t.Context()); err == nil {
		t.Error("All ran a refused union")
	}
	if _, err := broken().One(t.Context()); err == nil {
		t.Error("One ran a refused union")
	}
	for _, err := range broken().Rows(t.Context()) {
		if err == nil {
			t.Error("Rows ran a refused union")
		}
		break
	}
	if _, _, err := broken().SQL(); err == nil {
		t.Error("SQL rendered a refused union")
	}
}
