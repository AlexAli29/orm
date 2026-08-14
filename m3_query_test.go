package orm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
)

// counting is an executor that refuses to be used. It proves the claim that
// matters most about builder errors: a query that failed validation never
// reaches PostgreSQL.
type counting struct {
	calls int
	rows  [][]any
}

func (c *counting) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.calls++
	return &stubRows{rows: c.rows}, nil
}

func countingQuery(t *testing.T) (*orm.Query[User], *counting) {
	t.Helper()
	ex := &counting{}
	return orm.NewRepo(ex, &userMeta).Query(), ex
}

func TestOffset(t *testing.T) {
	tests := []struct {
		name string
		q    func() *orm.Query[User]
		want string
	}{
		{
			name: "alone",
			q:    func() *orm.Query[User] { return userQuery().Offset(100) },
			want: selectAll + " OFFSET 100",
		},
		{
			name: "after a limit",
			q:    func() *orm.Query[User] { return userQuery().Limit(50).Offset(100) },
			want: selectAll + " LIMIT 50 OFFSET 100",
		},
		{
			// The clause order is PostgreSQL's, not the order the calls were
			// made in.
			name: "written before the limit",
			q:    func() *orm.Query[User] { return userQuery().Offset(100).Limit(50) },
			want: selectAll + " LIMIT 50 OFFSET 100",
		},
		{
			name: "zero",
			q:    func() *orm.Query[User] { return userQuery().Offset(0) },
			want: selectAll + " OFFSET 0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, _, err := tt.q().SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if sql != tt.want {
				t.Errorf("sql = %s\nwant %s", sql, tt.want)
			}
		})
	}
}

func TestForUpdate(t *testing.T) {
	sql, args, err := userQuery().
		Where(Users.Active.Eq(true)).
		OrderBy(Users.CreatedAt.Desc()).
		Limit(50).
		Offset(100).
		ForUpdate().
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."active" = $1` +
		` ORDER BY "users"."created_at" DESC LIMIT 50 OFFSET 100 FOR UPDATE`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	assertArgs(t, args, []any{true})

	// Asking twice is the same query.
	twice, _, err := userQuery().ForUpdate().ForUpdate().SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if twice != selectAll+" FOR UPDATE" {
		t.Errorf("sql = %s", twice)
	}
}

func TestClone_isolatesEveryBranch(t *testing.T) {
	base := userQuery().Where(Users.Active.Eq(true))

	admins := base.Clone().Where(Users.Email.Eq("admin@example.com")).Limit(10)
	recent := base.Clone().Where(Users.Age.Gte(int32(18))).OrderBy(Users.CreatedAt.Desc())

	baseSQL, baseArgs, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if want := selectAll + ` WHERE "users"."active" = $1`; baseSQL != want {
		t.Errorf("the base picked up a branch's conditions:\n%s\nwant\n%s", baseSQL, want)
	}
	assertArgs(t, baseArgs, []any{true})

	adminSQL, _, err := admins.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(adminSQL, "LIMIT 10") || !strings.Contains(adminSQL, `"users"."email" = $2`) {
		t.Errorf("the admins branch = %s", adminSQL)
	}
	if strings.Contains(adminSQL, "ORDER BY") {
		t.Errorf("the admins branch picked up the other branch's ordering: %s", adminSQL)
	}

	recentSQL, _, err := recent.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(recentSQL, "LIMIT") {
		t.Errorf("the recent branch picked up the other branch's limit: %s", recentSQL)
	}
	if !strings.Contains(recentSQL, "ORDER BY") {
		t.Errorf("the recent branch lost its ordering: %s", recentSQL)
	}
}

func TestClone_doesNotShareBackingArrays(t *testing.T) {
	// A shallow copy would share the slice's backing array, so an append that
	// fit in the existing capacity would land in both queries.
	base := userQuery().Where(Users.Active.Eq(true), Users.Age.Gte(int32(1)), Users.ID.Eq(1))
	base.Where(Users.Email.Eq("a")) // grow the slice so capacity is spare

	clone := base.Clone()
	clone.Where(Users.Email.Eq("clone"))
	base.Where(Users.Email.Eq("base"))

	cloneSQL, _, err := clone.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	baseSQL, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Count(cloneSQL, "$") != 5 || strings.Count(baseSQL, "$") != 5 {
		t.Errorf("the two queries have different numbers of conditions:\nclone %s\nbase  %s", cloneSQL, baseSQL)
	}
	_, cloneArgs, _ := clone.SQL()
	_, baseArgs, _ := base.SQL()
	if cloneArgs[4] != "clone" {
		t.Errorf("the clone's last condition is %v, want clone", cloneArgs[4])
	}
	if baseArgs[4] != "base" {
		t.Errorf("the base's last condition is %v, want base", baseArgs[4])
	}
}

func TestClone_copiesLimitAndOffsetByValue(t *testing.T) {
	base := userQuery().Limit(10).Offset(5)
	clone := base.Clone().Limit(20).Offset(15)

	baseSQL, _, _ := base.SQL()
	cloneSQL, _, _ := clone.SQL()
	if !strings.HasSuffix(baseSQL, "LIMIT 10 OFFSET 5") {
		t.Errorf("the base became %s", baseSQL)
	}
	if !strings.HasSuffix(cloneSQL, "LIMIT 20 OFFSET 15") {
		t.Errorf("the clone became %s", cloneSQL)
	}
}

func TestClone_carriesErrorsButNotBackwards(t *testing.T) {
	base := userQuery().Limit(-1)
	clone := base.Clone()

	if _, _, err := clone.SQL(); err == nil {
		t.Error("the clone lost the mistake the base already had")
	}
	// A mistake made after cloning belongs to the branch that made it.
	other := userQuery()
	branch := other.Clone().Offset(-1)
	if _, _, err := other.SQL(); err != nil {
		t.Errorf("a mistake in the branch reached the query it came from: %v", err)
	}
	if _, _, err := branch.SQL(); err == nil {
		t.Error("the branch lost its own mistake")
	}
}

func TestBuilderErrors_accumulate(t *testing.T) {
	q, ex := countingQuery(t)
	q.Limit(-1).Offset(-2).OrderBy(orm.Order[User]{})

	_, _, err := q.SQL()
	if err == nil {
		t.Fatal("SQL succeeded over three mistakes")
	}
	for _, want := range []string{"negative limit -1", "negative offset -2", "no column"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error =\n%v\nwant it to mention %q", err, want)
		}
	}
	if ex.calls != 0 {
		t.Errorf("the executor was called %d times for a query that never compiled", ex.calls)
	}
}

func TestBuilderErrors_noTerminalTouchesTheDatabase(t *testing.T) {
	// Every terminal has to refuse, not just the one that happens to be
	// tested. A terminal that skipped the check would send invalid SQL.
	terminals := map[string]func(*orm.Query[User]) error{
		"SQL":    func(q *orm.Query[User]) error { _, _, err := q.SQL(); return err },
		"All":    func(q *orm.Query[User]) error { _, err := q.All(context.Background()); return err },
		"One":    func(q *orm.Query[User]) error { _, err := q.One(context.Background()); return err },
		"Count":  func(q *orm.Query[User]) error { _, err := q.Count(context.Background()); return err },
		"Exists": func(q *orm.Query[User]) error { _, err := q.Exists(context.Background()); return err },
		"Rows": func(q *orm.Query[User]) error {
			for _, err := range q.Rows(context.Background()) {
				return err
			}
			return errors.New("the iterator yielded nothing at all")
		},
	}
	for name, run := range terminals {
		t.Run(name, func(t *testing.T) {
			q, ex := countingQuery(t)
			q.Limit(-1)

			err := run(q)
			if err == nil {
				t.Fatal("the terminal succeeded over a builder mistake")
			}
			if !strings.Contains(err.Error(), "negative limit") {
				t.Errorf("error = %v", err)
			}
			if ex.calls != 0 {
				t.Errorf("the executor was called %d times", ex.calls)
			}
		})
	}
}

func TestOne(t *testing.T) {
	row := func(id int64) []any {
		return []any{id, "a@example.com", int32(30), true, nil, nil, nil, epoch}
	}
	tests := []struct {
		name string
		rows [][]any
		want error
	}{
		{name: "no rows", rows: nil, want: orm.ErrNotFound},
		{name: "one row", rows: [][]any{row(1)}},
		{name: "two rows", rows: [][]any{row(1), row(2)}, want: orm.ErrMultipleRows},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := stubExecutor{rows: tt.rows}
			u, err := orm.NewRepo(ex, &userMeta).Query().One(t.Context())
			if tt.want != nil {
				if !errors.Is(err, tt.want) {
					t.Fatalf("error = %v, want it to wrap %v", err, tt.want)
				}
				var zero User
				if u != zero {
					t.Errorf("a failed One returned %+v, want the zero entity", u)
				}
				// The message still has to name the table a person is looking at.
				if !strings.Contains(err.Error(), "public.users") {
					t.Errorf("error = %v, want it to name the table", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("One: %v", err)
			}
			if u.ID != 1 {
				t.Errorf("One returned %+v", u)
			}
		})
	}
}

func TestOne_fetchesAtMostTwoRows(t *testing.T) {
	var seen recorder
	_, _ = orm.NewRepo(&seen, &userMeta).Query().Where(Users.Active.Eq(true)).One(t.Context())

	if !strings.HasSuffix(seen.sql, "LIMIT 2") {
		t.Errorf("One ran %s\nwant it to stop at two rows", seen.sql)
	}
}

func TestOne_respectsASmallerLimit(t *testing.T) {
	// A caller who asked for one row meant it, so no ambiguity can arise.
	var seen recorder
	_, _ = orm.NewRepo(&seen, &userMeta).Query().Limit(1).One(t.Context())
	if !strings.HasSuffix(seen.sql, "LIMIT 1") {
		t.Errorf("One ran %s, want the caller's own limit", seen.sql)
	}
}

func TestOne_leavesTheBuilderAlone(t *testing.T) {
	// The internal limit is applied to a copy. If it were not, the query the
	// caller holds would silently acquire it.
	base := userQuery().Where(Users.Active.Eq(true))

	before, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	_, _ = base.Clone().Where(Users.Email.Eq("a")).One(t.Context())
	_, _ = base.One(t.Context())

	after, _, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if after != before {
		t.Errorf("One changed the query it ran on:\n%s\nwas\n%s", after, before)
	}
	if strings.Contains(after, "LIMIT") {
		t.Errorf("One left its own limit behind: %s", after)
	}
}

func TestCount(t *testing.T) {
	var seen recorder
	seen.rows = [][]any{{int64(7)}}
	n, err := orm.NewRepo(&seen, &userMeta).Query().
		Where(Users.Active.Eq(true)).
		OrderBy(Users.CreatedAt.Desc()).
		Limit(50).
		Offset(100).
		Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 7 {
		t.Errorf("Count = %d, want 7", n)
	}

	const want = `SELECT count(*) FROM (SELECT 1 FROM "public"."users"` +
		` WHERE "users"."active" = $1 LIMIT 50 OFFSET 100) AS "_orm_count"`
	if seen.sql != want {
		t.Errorf("Count ran\n%s\nwant\n%s", seen.sql, want)
	}
	// No entity is materialised: the statement asks for a constant, not for
	// the columns.
	if strings.Contains(seen.sql, `"users"."email"`) {
		t.Errorf("Count selected entity columns: %s", seen.sql)
	}
	assertArgs(t, seen.args, []any{true})
}

func TestExists(t *testing.T) {
	tests := []struct {
		name string
		rows [][]any
		want bool
	}{
		{name: "something matches", rows: [][]any{{true}}, want: true},
		{name: "nothing does", rows: [][]any{{false}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ex := stubExecutor{rows: tt.rows}
			got, err := orm.NewRepo(ex, &userMeta).Query().Exists(t.Context())
			if err != nil {
				t.Fatalf("Exists: %v", err)
			}
			if got != tt.want {
				t.Errorf("Exists = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExists_sql(t *testing.T) {
	var seen recorder
	seen.rows = [][]any{{false}}
	_, err := orm.NewRepo(&seen, &userMeta).Query().
		Where(Users.Email.Eq("a@example.com")).
		Limit(0).
		Exists(t.Context())
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	const want = `SELECT EXISTS (SELECT 1 FROM "public"."users"` +
		` WHERE "users"."email" = $1 LIMIT 0)`
	if seen.sql != want {
		t.Errorf("Exists ran\n%s\nwant\n%s", seen.sql, want)
	}
	if strings.Contains(seen.sql, `"users"."email",`) {
		t.Errorf("Exists selected entity columns: %s", seen.sql)
	}
}

func TestRows_streamsAndClosesEarly(t *testing.T) {
	closed := 0
	rows := make([][]any, 5)
	for i := range rows {
		rows[i] = []any{int64(i + 1), "a@example.com", int32(30), true, nil, nil, nil, epoch}
	}
	ex := stubExecutor{rows: rows, closed: &closed}

	var seen []int64
	for u, err := range orm.NewRepo(ex, &userMeta).Query().Rows(t.Context()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		seen = append(seen, u.ID)
		if len(seen) == 2 {
			break
		}
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Errorf("streamed %v, want the first two ids", seen)
	}
	if closed != 1 {
		t.Errorf("rows closed %d times after an early break, want 1", closed)
	}
}

func TestRows_readsEverything(t *testing.T) {
	rows := make([][]any, 3)
	for i := range rows {
		rows[i] = []any{int64(i + 1), "a@example.com", int32(30), true, nil, nil, nil, epoch}
	}
	closed := 0
	ex := stubExecutor{rows: rows, closed: &closed}

	var n int
	for _, err := range orm.NewRepo(ex, &userMeta).Query().Rows(t.Context()) {
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		n++
	}
	if n != 3 {
		t.Errorf("streamed %d rows, want 3", n)
	}
	if closed != 1 {
		t.Errorf("rows closed %d times, want 1", closed)
	}
}

func TestRows_surfacesErrors(t *testing.T) {
	sentinel := errors.New("boom")
	row := []any{int64(1), "a@example.com", int32(30), true, nil, nil, nil, epoch}

	tests := []struct {
		name string
		ex   stubExecutor
		want string
	}{
		{name: "the query fails", ex: stubExecutor{queryErr: sentinel}, want: "querying public.users"},
		{name: "a scan fails", ex: stubExecutor{rows: [][]any{row}, scanErr: sentinel}, want: "scanning public.users"},
		{name: "the stream fails", ex: stubExecutor{rows: [][]any{row}, rowsErr: sentinel}, want: "reading public.users"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got error
			for _, err := range orm.NewRepo(tt.ex, &userMeta).Query().Rows(t.Context()) {
				if err != nil {
					got = err
					break
				}
			}
			if got == nil {
				t.Fatal("the iterator finished without reporting the failure")
			}
			if !strings.Contains(got.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", got, tt.want)
			}
			if !errors.Is(got, sentinel) {
				t.Errorf("error = %v, want it to wrap the cause", got)
			}
		})
	}
}

func TestExpr(t *testing.T) {
	tests := []struct {
		name string
		q    func() *orm.Query[User]
		sql  string
		args []any
	}{
		{
			name: "alone",
			q:    func() *orm.Query[User] { return userQuery().Where(orm.Expr[User]("score > $1", 100)) },
			sql:  selectAll + " WHERE score > $1",
			args: []any{100},
		},
		{
			name: "mixed with a typed predicate",
			q: func() *orm.Query[User] {
				return userQuery().Where(Users.Active.Eq(true), orm.Expr[User]("score > $1", 100))
			},
			sql:  selectAll + ` WHERE "users"."active" = $1 AND score > $2`,
			args: []any{true, 100},
		},
		{
			name: "no arguments at all",
			q:    func() *orm.Query[User] { return userQuery().Where(orm.Expr[User]("score IS NOT NULL")) },
			sql:  selectAll + " WHERE score IS NOT NULL",
		},
		{
			name: "one argument referred to twice",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.Expr[User]("score > $1 OR backup > $1", 100))
			},
			sql:  selectAll + " WHERE score > $1 OR backup > $1",
			args: []any{100},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := tt.q().SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql = %s\nwant %s", sql, tt.sql)
			}
			assertArgs(t, args, tt.args)
		})
	}
}

func TestExpr_invalidFragments(t *testing.T) {
	tests := []struct {
		name string
		pred orm.Predicate[User]
		want string
	}{
		{
			name: "refers past its arguments",
			pred: orm.Expr[User]("score > $2", 100),
			want: "refers to $2",
		},
		{
			name: "an argument nobody refers to",
			pred: orm.Expr[User]("score > $1", 100, 200),
			want: "argument 2 is never referred to",
		},
		{
			name: "an argument with no placeholder at all",
			pred: orm.Expr[User]("score IS NOT NULL", 100),
			want: "argument 1 is never referred to",
		},
		{
			name: "malformed SQL",
			pred: orm.Expr[User]("score > '$1", 100),
			want: "unterminated",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.pred.Err() == nil {
				t.Fatal("the fragment was accepted")
			}
			if !errors.Is(tt.pred.Err(), orm.ErrRawPlaceholder) {
				t.Errorf("error = %v, want it to wrap ErrRawPlaceholder", tt.pred.Err())
			}

			// The mistake has to reach the query, not stop at the predicate.
			q, ex := countingQuery(t)
			_, _, err := q.Where(tt.pred).SQL()
			if err == nil {
				t.Fatal("the query accepted the fragment")
			}
			if !errors.Is(err, orm.ErrRawPlaceholder) {
				t.Errorf("error = %v, want it to wrap ErrRawPlaceholder", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if ex.calls != 0 {
				t.Errorf("the executor was called %d times", ex.calls)
			}
		})
	}
}

func TestExpr_errorSurvivesCombination(t *testing.T) {
	// A malformed fragment folded into an And must not lose its complaint on
	// the way.
	bad := orm.Expr[User]("score > $2", 100)
	for _, p := range []orm.Predicate[User]{
		orm.And(Users.Active.Eq(true), bad),
		orm.Or(Users.Active.Eq(true), bad),
		orm.Not(bad),
	} {
		if p.Err() == nil {
			t.Error("a combination dropped the fragment's error")
		}
	}
}

func TestExpr_valuesAreStillParameters(t *testing.T) {
	const nasty = `'; DROP TABLE users; --`
	sql, args, err := userQuery().Where(orm.Expr[User]("label = $1", nasty)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "DROP TABLE") {
		t.Fatalf("a fragment's value reached the statement text: %s", sql)
	}
	assertArgs(t, args, []any{nasty})
}

func TestEqCol(t *testing.T) {
	// A column compared to a column binds no parameter: both sides are
	// identifiers, and there is no value to send.
	sql, args, err := userQuery().Where(Users.ID.EqCol(Users.ManagerID)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if want := selectAll + ` WHERE "users"."id" = "users"."manager_id"`; sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	if len(args) != 0 {
		t.Errorf("comparing two columns bound %d parameters", len(args))
	}
}

func TestEqCol_acrossOccurrencesOfOneTable(t *testing.T) {
	// This is what an alias is for: comparing one occurrence of a table
	// against another. The scope check means it only compiles into a query
	// that actually selects from both, which M3 has no public way to do — so
	// here it is asserted at the level the compiler works at.
	manager := usersAs(t, "manager")

	pred := manager.ID.EqCol(Users.ManagerID)
	if pred.IsZero() {
		t.Fatal("EqCol produced no condition")
	}
	// Used in a query over the alias alone, the other side is out of scope.
	_, _, err := orm.NewRepo(stubExecutor{}, &userMeta).
		QueryFrom(manager.src).
		Where(pred).
		SQL()
	if err == nil {
		t.Fatal("the comparison was accepted with one side out of scope")
	}
	var scopeErr *orm.ScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("error = %v (%T), want a *orm.ScopeError", err, err)
	}
	if scopeErr.Column != "manager_id" {
		t.Errorf("the error names column %q, want the one that is out of scope", scopeErr.Column)
	}
}

func TestQueryFrom_alias(t *testing.T) {
	manager := usersAs(t, "manager")

	sql, args, err := orm.NewRepo(stubExecutor{}, &userMeta).
		QueryFrom(manager.src).
		Where(manager.Email.Eq("a@example.com")).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const want = `SELECT "manager"."id", "manager"."email", "manager"."age", "manager"."active", ` +
		`"manager"."nickname", "manager"."manager_id", "manager"."deleted_at", "manager"."created_at" ` +
		`FROM "public"."users" AS "manager" WHERE "manager"."email" = $1`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	assertArgs(t, args, []any{"a@example.com"})
}

func TestQueryFrom_scopeError(t *testing.T) {
	manager := usersAs(t, "manager")

	// The query selects from the alias, so the unaliased descriptors name a
	// table that is not in the FROM clause.
	_, _, err := orm.NewRepo(stubExecutor{}, &userMeta).
		QueryFrom(manager.src).
		Where(Users.Email.Eq("a@example.com")).
		SQL()
	if err == nil {
		t.Fatal("the query accepted a column from a table it does not select from")
	}
	var scopeErr *orm.ScopeError
	if !errors.As(err, &scopeErr) {
		t.Fatalf("error = %v (%T), want a *orm.ScopeError", err, err)
	}
	if scopeErr.Column != "email" {
		t.Errorf("the error names column %q", scopeErr.Column)
	}
	for _, want := range []string{`"users"."email" is not available`, "the query selects from:", "public.users AS manager"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error =\n%v\nwant it to contain %q", err, want)
		}
	}
}

func TestQueryFrom_rejectsAnotherTable(t *testing.T) {
	_, _, err := orm.NewRepo(stubExecutor{}, &userMeta).
		QueryFrom(postsSrc).
		SQL()
	if err == nil {
		t.Fatal("QueryFrom accepted a source for another table")
	}
	if !strings.Contains(err.Error(), "is not an occurrence of public.users") {
		t.Errorf("error = %v", err)
	}
}

func TestQueryFrom_invalidAlias(t *testing.T) {
	tests := []struct {
		name  string
		alias string
		want  string
	}{
		{name: "empty", alias: "", want: "cannot be empty"},
		{name: "reserved", alias: "_mine", want: "reserved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := usersSrc.As(tt.alias)
			q, ex := countingQuery(t)
			_ = q
			_, _, err := orm.NewRepo(ex, &userMeta).QueryFrom(src).SQL()
			if err == nil {
				t.Fatalf("the alias %q was accepted", tt.alias)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if ex.calls != 0 {
				t.Errorf("the executor was called %d times", ex.calls)
			}
		})
	}
}

// aliasedUsers stands in for what the generator emits for an alias: every
// descriptor rebuilt against the new occurrence.
type aliasedUsers struct {
	src       *orm.Source
	ID        orm.OrdCol[User, int64]
	Email     orm.TextCol[User]
	Age       orm.OrdCol[User, int32]
	ManagerID orm.NullOrdCol[User, int64]
}

func usersAs(t *testing.T, alias string) aliasedUsers {
	t.Helper()
	src := usersSrc.As(alias)
	return aliasedUsers{
		src:       src,
		ID:        orm.NewOrdCol[User, int64](src, "id"),
		Email:     orm.NewTextCol[User](src, "email"),
		Age:       orm.NewOrdCol[User, int32](src, "age"),
		ManagerID: orm.NewNullOrdCol[User, int64](src, "manager_id"),
	}
}

func TestCol_source(t *testing.T) {
	manager := usersAs(t, "manager")

	if got := Users.ID.Source(); got != usersSrc {
		t.Errorf("the unaliased column belongs to %v", got)
	}
	if got := manager.ID.Source(); got != manager.src {
		t.Errorf("the aliased id belongs to %v, want the alias", got)
	}
	if got := manager.Email.Source(); got != manager.src {
		t.Errorf("the aliased email belongs to %v, want the alias", got)
	}
	if manager.src == usersSrc {
		t.Error("the alias shares the original occurrence")
	}
}
