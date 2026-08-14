package orm_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
)

// Ordering a set operation.
//
// PostgreSQL attaches a compound's ORDER BY to the operation rather than to its
// last branch, and it allows only an output column name there — not a qualified
// reference, and not an expression, even one over an output name. So the terms
// are names, checked against the result shape before any SQL exists, and the
// tests below are about which names are allowed and where the clause lands.

// The declarations the union's columns are addressed by. They are the same
// values Ref takes when the union becomes a source, which is the point: a union
// that is both ordered and selected from names its columns once.
var (
	orderThingID = orm.Named("thing_id", orm.Of(Users.ID))
	orderLabel   = orm.Named("label", orm.Of(Users.Email))
)

func orderedUnion() *orm.UnionQuery[sourced] {
	return orm.UnionAll[sourced](srcFromUsers(), srcFromPosts())
}

func TestUnionOrder_sortsTheWholeResult(t *testing.T) {
	sql, args, err := orderedUnion().OrderBy(orderThingID.Desc()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `SELECT "users"."id" AS "thing_id", "users"."email" AS "label" FROM "public"."users" ` +
		`UNION ALL ` +
		`SELECT "posts"."id" AS "post_id", CAST($1 AS "text") AS "kind" FROM "public"."posts" ` +
		`ORDER BY "thing_id" DESC`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 {
		t.Errorf("args = %v, want one", args)
	}
	// A bare identifier, not a qualified one. PostgreSQL refuses the qualified
	// form on a compound outright.
	if strings.Contains(sql, `ORDER BY "users"."thing_id"`) {
		t.Errorf("the ordering term was qualified by a source:\n%s", sql)
	}
}

func TestUnionOrder_sortsBySeveralColumns(t *testing.T) {
	sql, _, err := orderedUnion().OrderBy(orderLabel.Asc(), orderThingID.Desc()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `ORDER BY "label" ASC, "thing_id" DESC`) {
		t.Errorf("the two terms were not written in order:\n%s", sql)
	}
}

// Terms accumulate across calls, as the rest of the package's builders do.
func TestUnionOrder_accumulatesAcrossCalls(t *testing.T) {
	sql, _, err := orderedUnion().OrderBy(orderLabel.Asc()).OrderBy(orderThingID.Desc()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `ORDER BY "label" ASC, "thing_id" DESC`) {
		t.Errorf("a second OrderBy replaced the first:\n%s", sql)
	}
}

// The clause belongs to the operation, and it is written after the last branch
// and before LIMIT, which is where PostgreSQL's grammar puts it.
func TestUnionOrder_landsAfterTheLastBranchAndBeforeTheLimit(t *testing.T) {
	sql, _, err := orderedUnion().OrderBy(orderThingID.Asc()).Limit(10).Offset(5).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `ORDER BY "thing_id" ASC LIMIT 10 OFFSET 5`) {
		t.Errorf("the clauses are not in the grammar's order:\n%s", sql)
	}
	// Not attached to the second branch: the FROM of the last branch is followed
	// by the operation's clauses, not by a branch-local ordering.
	if strings.Contains(sql, `"public"."posts" ORDER BY`) && !strings.Contains(sql, `AS "kind" FROM "public"."posts" ORDER BY`) {
		t.Errorf("the ordering attached to the last branch:\n%s", sql)
	}
}

// A branch that orders itself keeps its own clause, in parentheses, and the
// operation's ordering is separate. The two are different statements and the SQL
// says which is which.
func TestUnionOrder_isNotTheSameAsOrderingABranch(t *testing.T) {
	branchOrdered := orm.Compose(nil, orm.Project2(
		orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Email).As("label"),
		func(id int64, s string) sourced { return sourced{id, s} },
	)).From(usersSrc).OrderBy(orm.Of(Users.ID).Desc()).Limit(2)

	sql, _, err := orm.UnionAll[sourced](branchOrdered, srcFromUsers()).
		OrderBy(orderLabel.Asc()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	// The branch's own clause is parenthesised so that it stays the branch's.
	if !strings.HasPrefix(sql, `(SELECT `) || !strings.Contains(sql, `ORDER BY "users"."id" DESC LIMIT 2)`) {
		t.Errorf("the branch's own ordering was not kept inside it:\n%s", sql)
	}
	// The operation's is at the end, by output name.
	if !strings.HasSuffix(sql, `ORDER BY "label" ASC`) {
		t.Errorf("the operation's ordering is not at the end:\n%s", sql)
	}
}

// The names are the first branch's, which is PostgreSQL's rule. A name only the
// second branch declared is refused here rather than by the server.
func TestUnionOrder_refusesANameTheResultDoesNotHave(t *testing.T) {
	cases := []struct {
		name string
		term orm.OutputOrder
		want []string
	}{
		{
			name: "a column no branch declares",
			term: orm.Named("nope", orm.Of(Users.ID)).Asc(),
			want: []string{`no result column "nope"`, `it provides "thing_id", "label"`},
		},
		{
			// PostgreSQL: column "kind" does not exist.
			name: "a column only the second branch declares",
			term: orm.Named("kind", orm.Of(Users.Email)).Asc(),
			want: []string{`no result column "kind"`, `"thing_id"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := orderedUnion().OrderBy(c.term).SQL()
			if err == nil {
				t.Fatalf("%s was accepted:\n%s", c.name, sql)
			}
			if sql != "" || args != nil {
				t.Errorf("a refused ordering still produced SQL %q and args %v", sql, args)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the diagnostic %q does not mention %q", err, w)
				}
			}
		})
	}
}

// A union whose first branch names none of its columns has nothing to order by.
// This package does not model the names PostgreSQL derives for itself from a
// bare column, so it says what to do instead of guessing one.
func TestUnionOrder_refusesAUnionWhoseFirstBranchNamesNothing(t *testing.T) {
	u := orm.UnionAll[int64](oneColUsers(), oneColPosts())
	_, _, err := u.OrderBy(orderThingID.Asc()).SQL()
	if err == nil {
		t.Fatal("a union that names none of its columns was ordered")
	}
	if !strings.Contains(err.Error(), "first branch names none of its columns") {
		t.Errorf("the diagnostic %q does not say why there is nothing to order by", err)
	}
	if !strings.Contains(err.Error(), "declare them with As") {
		t.Errorf("the diagnostic %q does not say what to do about it", err)
	}
}

func TestUnionOrder_refusesATermThatNamesNothing(t *testing.T) {
	var empty orm.OutputOrder
	_, _, err := orderedUnion().OrderBy(orderThingID.Asc(), empty).SQL()
	if err == nil {
		t.Fatal("an ordering term naming no column was accepted")
	}
	if !strings.Contains(err.Error(), "ordering term 2 names no output column") {
		t.Errorf("the diagnostic %q does not say which term is empty", err)
	}
}

// An ordering mistake does not stop the rest of the builder being reported: the
// contract is accumulation, as it is everywhere else.
func TestUnionOrder_reportsEveryMistakeAtOnce(t *testing.T) {
	_, _, err := orderedUnion().
		OrderBy(orm.Named("nope", orm.Of(Users.ID)).Asc()).
		Limit(-1).
		SQL()
	if err == nil {
		t.Fatal("a union with two mistakes was accepted")
	}
	for _, w := range []string{`"nope"`, "LIMIT -1"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the diagnostic %q does not mention %q", err, w)
		}
	}
}

// An ordered union is still every other thing a union is. The clause travels
// with the statement into each position, parenthesised where the grammar needs
// it to be.
func TestUnionOrder_anOrderedUnionIsStillEverythingElse(t *testing.T) {
	ordered := func() *orm.UnionQuery[sourced] {
		return orderedUnion().OrderBy(orderThingID.Asc())
	}

	t.Run("a derived table", func(t *testing.T) {
		u := orm.Sub("u", ordered())
		shape := orm.Project1(orm.Ref(u, orderThingID), func(id int64) int64 { return id })
		sql, _, err := orm.Compose(nil, shape).From(u).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, `ORDER BY "thing_id" ASC) AS "u"`) {
			t.Errorf("the ordering did not stay inside the derived table:\n%s", sql)
		}
	})

	t.Run("a CTE body", func(t *testing.T) {
		c := orm.CTE("o", ordered())
		shape := orm.Project1(orm.Ref(c, orderThingID), func(id int64) int64 { return id })
		sql, _, err := orm.Compose(nil, shape).With(c).From(c).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, `ORDER BY "thing_id" ASC) SELECT`) {
			t.Errorf("the ordering did not stay inside the CTE body:\n%s", sql)
		}
	})

	t.Run("a branch of another union", func(t *testing.T) {
		sql, _, err := orm.UnionAll[sourced](ordered().Limit(3), srcFromUsers()).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		// Parenthesised, so its ordering and limit stay its own rather than
		// binding to the outer operation.
		if !strings.HasPrefix(sql, "(SELECT ") {
			t.Errorf("the ordered branch was not parenthesised:\n%s", sql)
		}
		if !strings.Contains(sql, `ORDER BY "thing_id" ASC LIMIT 3) UNION ALL`) {
			t.Errorf("the branch's clauses escaped it:\n%s", sql)
		}
	})

	t.Run("a value subquery", func(t *testing.T) {
		one := orm.Compose(nil, orm.Project1(
			orm.Of(Users.ID).As("thing_id"), func(id int64) int64 { return id },
		)).From(usersSrc)
		u := orm.UnionAll[int64](one, one).OrderBy(orderThingID.Desc())
		sql, _, err := orm.NewRepo(nil, &userMeta).Query().Where(orm.InSub(Users.ID, u)).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		if !strings.Contains(sql, `ORDER BY "thing_id" DESC)`) {
			t.Errorf("the ordering did not stay inside the membership test:\n%s", sql)
		}
	})

	t.Run("read directly", func(t *testing.T) {
		ex := stubExecutor{rows: [][]any{{int64(2), "b@example.com"}, {int64(1), "a@example.com"}}}
		got, err := ordered().Using(ex).All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 2 {
			t.Errorf("All returned %v", got)
		}
	})
}

// Ordering is read-only with respect to the result shape: the same union is
// still a branch, and still refuses one of another shape.
func TestUnionOrder_orderingDoesNotDisturbTheShape(t *testing.T) {
	u := orderedUnion()
	u.OrderBy(orderThingID.Asc())

	if _, _, err := orm.UnionAll[sourced](u, srcFromUsers()).SQL(); err != nil {
		t.Errorf("after being ordered, the union is no longer a branch: %v", err)
	}
	wrong := orm.Compose(nil, orm.Project2(
		orm.Of(Users.Age).As("thing_id"), orm.Of(Users.Email).As("label"),
		func(age int32, s string) sourced { return sourced{int64(age), s} },
	)).From(usersSrc)
	if _, _, err := orm.UnionAll[sourced](u, wrong).SQL(); err == nil {
		t.Error("after being ordered, the union accepted a branch of another shape")
	}
}

// The declaration is one value, used for both ordering and reading. A union that
// is ordered and then selected from names its columns once, and the two uses
// cannot drift apart because there is nothing to keep in step.
func TestUnionOrder_oneDeclarationOrdersAndReads(t *testing.T) {
	u := orm.Sub("u", orderedUnion().OrderBy(orderThingID.Desc()))
	shape := orm.Project2(orm.Ref(u, orderThingID), orm.Ref(u, orderLabel),
		func(id int64, s string) sourced { return sourced{id, s} })

	sql, _, err := orm.Compose(nil, shape).From(u).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `SELECT "u"."thing_id", "u"."label" FROM (`) {
		t.Errorf("the declarations did not read the columns they ordered:\n%s", sql)
	}
	if !strings.Contains(sql, `ORDER BY "thing_id" DESC`) {
		t.Errorf("the ordering names a different column from the one read:\n%s", sql)
	}
}

// A descriptor declaring two columns of one name. Generated code cannot produce
// one — PostgreSQL will not hold two columns of one name in a table — but
// EntityMeta is public, and the guard where a union becomes a source already
// treats it as reachable.
var ambiguousMeta = orm.EntityMeta[User]{
	Table:  orm.TableID{Schema: "public", Name: "users"},
	Source: usersSrc,
	Columns: []orm.ColumnMeta{
		{Name: "k", Field: "ID", NotNull: true},
		{Name: "k", Field: "Email", NotNull: true},
	},
	Dest: func(u *User, i int) any {
		if i == 0 {
			return &u.ID
		}
		return &u.Email
	},
}

// A name identifies a column, and a result that carries one twice is refused
// where the shape is built — so every consumer of the shape inherits it and
// none of them asks.
//
// The rule used to live at the consumers, and that is how it came to be missing
// from one: Sub asked, CTE asked, the ordering path did not, and a fifth
// consumer would have inherited nothing. What is asserted here is the whole set
// at once, because the claim is inheritance rather than four separate checks.
func TestUnionOrder_aResultCannotNameOneColumnTwice(t *testing.T) {
	repo := func() *orm.Repo[User] { return orm.NewRepo(nil, &ambiguousMeta) }
	union := func() *orm.UnionQuery[User] {
		return orm.UnionAll[User](repo().Query(), repo().Query())
	}

	// The union does not form: a branch's shape is what it is validated
	// against, and the descriptor has none.
	_, _, err := union().SQL()
	if err == nil {
		t.Fatal("a union formed over a descriptor naming one column twice")
	}
	for _, w := range []string{`both named "k"`, "columns 1 and 2", "identifies one column"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the diagnostic %q does not mention %q", err, w)
		}
	}

	// And every consumer of the shape refuses, without any of them checking.
	consumers := []struct {
		name string
		err  func() error
	}{
		{"ordered", func() error {
			_, _, err := union().OrderBy(orm.Named("k", orm.Of(Users.ID)).Asc()).SQL()
			return err
		}},
		{"a derived table", func() error { return orm.Sub("u", union()).Err() }},
		{"a CTE body", func() error { return orm.CTE("c", union()).Err() }},
		{"a value subquery", func() error {
			_, _, err := orm.NewRepo(nil, &userMeta).Query().
				Where(orm.InSub(Users.ID, union())).SQL()
			return err
		}},
		{"a branch of another union", func() error {
			_, _, err := orm.UnionAll[User](union(), repo().Query()).SQL()
			return err
		}},
	}
	for _, c := range consumers {
		t.Run(c.name, func(t *testing.T) {
			if err := c.err(); err == nil {
				t.Fatalf("a result naming one column twice was accepted as %s", c.name)
			}
		})
	}

	// The restriction is on describing a result by name, not on reading one. An
	// entity query selects its descriptor's columns positionally and still runs:
	// SELECT k, k FROM t is legal SQL and the generated scanner reads it by
	// position, which is what PostgreSQL permits and this package does not take
	// away.
	if _, _, err := repo().Query().SQL(); err != nil {
		t.Errorf("a plain entity query over the same descriptor was refused: %v", err)
	}
}

// Everywhere else, a duplicate output name is refused before a union can be
// built out of it. These are the routes a projection takes, and each already
// had its own guard; what is asserted here is that none of them lets a
// duplicate through to the ordering path.
func TestUnionOrder_duplicateNamesAreRefusedBeforeTheyReachAUnion(t *testing.T) {
	cases := []struct {
		name  string
		build func() (string, []any, error)
	}{
		{"a composed branch", func() (string, []any, error) {
			dup := orm.Compose(nil, orm.Project2(
				orm.Of(Users.ID).As("k"), orm.Of(Users.Age).As("k"),
				func(a int64, b int32) [2]any { return [2]any{a, b} },
			)).From(usersSrc)
			return orm.UnionAll[[2]any](dup, dup).SQL()
		}},
		{"a projection branch", func() (string, []any, error) {
			repo := orm.NewRepo(nil, &userMeta)
			dup := orm.Select(repo, orm.Project2(
				Users.ID.As("k"), Users.Age.As("k"),
				func(a int64, b int32) [2]any { return [2]any{a, b} },
			))
			return orm.UnionAll[[2]any](dup, dup).SQL()
		}},
		{"a source-only branch", func() (string, []any, error) {
			rows := orm.Rows(
				orm.Named("k", orm.Of(Users.ID)), orm.Named("k", orm.Of(Users.Age)),
			).From(usersSrc)
			return rows.SQL()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := c.build()
			if err == nil {
				t.Fatal("two columns of one name were accepted")
			}
			if !strings.Contains(err.Error(), `"k"`) {
				t.Errorf("the diagnostic %q does not name the column declared twice", err)
			}
		})
	}
}
