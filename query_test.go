package orm_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
)

// The test entities and their descriptors stand in for generated code. They are
// written by hand so that the compiler's behaviour can be tested without a
// database, a generator run, or a schema.
type User struct {
	ID        int64
	Email     string
	Age       int32
	Active    bool
	Nickname  *string
	ManagerID *int64
	DeletedAt *time.Time
	CreatedAt time.Time
}

type Post struct {
	ID        int64
	Published bool
	CreatedAt time.Time
}

var (
	usersSrc = orm.NewSource("public", "users")
	postsSrc = orm.NewSource("public", "posts")

	Users = struct {
		ID        orm.OrdCol[User, int64]
		Email     orm.TextCol[User]
		Age       orm.OrdCol[User, int32]
		Active    orm.Col[User, bool]
		Nickname  orm.NullTextCol[User]
		ManagerID orm.NullOrdCol[User, int64]
		DeletedAt orm.NullOrdCol[User, time.Time]
		CreatedAt orm.OrdCol[User, time.Time]
	}{
		ID:        orm.NewOrdCol[User, int64](usersSrc, "id"),
		Email:     orm.NewTextCol[User](usersSrc, "email"),
		Age:       orm.NewOrdCol[User, int32](usersSrc, "age"),
		Active:    orm.NewCol[User, bool](usersSrc, "active"),
		Nickname:  orm.NewNullTextCol[User](usersSrc, "nickname"),
		ManagerID: orm.NewNullOrdCol[User, int64](usersSrc, "manager_id"),
		DeletedAt: orm.NewNullOrdCol[User, time.Time](usersSrc, "deleted_at"),
		CreatedAt: orm.NewOrdCol[User, time.Time](usersSrc, "created_at"),
	}

	Posts = struct {
		ID        orm.OrdCol[Post, int64]
		Published orm.Col[Post, bool]
		CreatedAt orm.OrdCol[Post, time.Time]
	}{
		ID:        orm.NewOrdCol[Post, int64](postsSrc, "id"),
		Published: orm.NewCol[Post, bool](postsSrc, "published"),
		CreatedAt: orm.NewOrdCol[Post, time.Time](postsSrc, "created_at"),
	}
)

var userMeta = orm.EntityMeta[User]{
	Table:  orm.TableID{Schema: "public", Name: "users"},
	Source: usersSrc,
	Columns: []orm.ColumnMeta{
		{Name: "id", Field: "ID"},
		{Name: "email", Field: "Email"},
		{Name: "age", Field: "Age"},
		{Name: "active", Field: "Active"},
		{Name: "nickname", Field: "Nickname"},
		{Name: "manager_id", Field: "ManagerID"},
		{Name: "deleted_at", Field: "DeletedAt"},
		{Name: "created_at", Field: "CreatedAt"},
	},
	Dest: func(u *User, idx int) any {
		switch idx {
		case 0:
			return &u.ID
		case 1:
			return &u.Email
		case 2:
			return &u.Age
		case 3:
			return &u.Active
		case 4:
			return &u.Nickname
		case 5:
			return &u.ManagerID
		case 6:
			return &u.DeletedAt
		case 7:
			return &u.CreatedAt
		default:
			return nil
		}
	},
}

// selectAll is the column list every query below starts with, spelled once.
const selectAll = `SELECT "users"."id", "users"."email", "users"."age", "users"."active", ` +
	`"users"."nickname", "users"."manager_id", "users"."deleted_at", "users"."created_at" ` +
	`FROM "public"."users"`

func userQuery() *orm.Query[User] {
	return orm.NewRepo(stubExecutor{}, &userMeta).Query()
}

var epoch = time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)

func TestQuery_SQL(t *testing.T) {
	tests := []struct {
		name string
		q    func() *orm.Query[User]
		sql  string
		args []any
	}{
		{
			name: "no restriction at all",
			q:    func() *orm.Query[User] { return userQuery() },
			sql:  selectAll,
		},
		{
			name: "Eq",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Active.Eq(true)) },
			sql:  selectAll + ` WHERE "users"."active" = $1`,
			args: []any{true},
		},
		{
			name: "Ne",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Active.Ne(false)) },
			sql:  selectAll + ` WHERE "users"."active" <> $1`,
			args: []any{false},
		},
		{
			name: "Gt",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Age.Gt(int32(18))) },
			sql:  selectAll + ` WHERE "users"."age" > $1`,
			args: []any{int32(18)},
		},
		{
			name: "Gte",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Age.Gte(int32(18))) },
			sql:  selectAll + ` WHERE "users"."age" >= $1`,
			args: []any{int32(18)},
		},
		{
			name: "Lt",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Age.Lt(int32(65))) },
			sql:  selectAll + ` WHERE "users"."age" < $1`,
			args: []any{int32(65)},
		},
		{
			name: "Lte",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Age.Lte(int32(65))) },
			sql:  selectAll + ` WHERE "users"."age" <= $1`,
			args: []any{int32(65)},
		},
		{
			name: "Between",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Age.Between(18, 65)) },
			sql:  selectAll + ` WHERE "users"."age" BETWEEN $1 AND $2`,
			args: []any{int32(18), int32(65)},
		},
		{
			name: "ordering a timestamp needs no Go ordering",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.CreatedAt.Gte(epoch)) },
			sql:  selectAll + ` WHERE "users"."created_at" >= $1`,
			args: []any{epoch},
		},
		{
			name: "Like",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Email.Like("%example.com")) },
			sql:  selectAll + ` WHERE "users"."email" LIKE $1`,
			args: []any{"%example.com"},
		},
		{
			name: "ILike",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Email.ILike("%alex%")) },
			sql:  selectAll + ` WHERE "users"."email" ILIKE $1`,
			args: []any{"%alex%"},
		},
		{
			name: "In",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.ID.In(1, 2, 3)) },
			sql:  selectAll + ` WHERE "users"."id" IN ($1, $2, $3)`,
			args: []any{int64(1), int64(2), int64(3)},
		},
		{
			name: "In over nothing matches nothing",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.ID.In()) },
			sql:  selectAll + ` WHERE FALSE`,
		},
		{
			name: "IsNull",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.DeletedAt.IsNull()) },
			sql:  selectAll + ` WHERE "users"."deleted_at" IS NULL`,
		},
		{
			name: "IsNotNull",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Nickname.IsNotNull()) },
			sql:  selectAll + ` WHERE "users"."nickname" IS NOT NULL`,
		},
		{
			name: "a nullable column still compares by value",
			q:    func() *orm.Query[User] { return userQuery().Where(Users.Nickname.Eq("alex")) },
			sql:  selectAll + ` WHERE "users"."nickname" = $1`,
			args: []any{"alex"},
		},
		{
			name: "And",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.And(Users.Active.Eq(true), Users.Age.Gte(int32(18))))
			},
			sql:  selectAll + ` WHERE "users"."active" = $1 AND "users"."age" >= $2`,
			args: []any{true, int32(18)},
		},
		{
			name: "Or",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.Or(Users.Active.Eq(true), Users.Age.Gte(int32(65))))
			},
			sql:  selectAll + ` WHERE "users"."active" = $1 OR "users"."age" >= $2`,
			args: []any{true, int32(65)},
		},
		{
			name: "a nested group is parenthesised",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.And(
					Users.Active.Eq(true),
					orm.Or(Users.Age.Lt(int32(18)), Users.Age.Gt(int32(65))),
				))
			},
			sql:  selectAll + ` WHERE "users"."active" = $1 AND ("users"."age" < $2 OR "users"."age" > $3)`,
			args: []any{true, int32(18), int32(65)},
		},
		{
			name: "Not",
			q:    func() *orm.Query[User] { return userQuery().Where(orm.Not(Users.Active.Eq(true))) },
			sql:  selectAll + ` WHERE NOT "users"."active" = $1`,
			args: []any{true},
		},
		{
			name: "Not of a group",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.Not(orm.And(Users.Active.Eq(true), Users.Age.Gte(int32(18)))))
			},
			sql:  selectAll + ` WHERE NOT ("users"."active" = $1 AND "users"."age" >= $2)`,
			args: []any{true, int32(18)},
		},
		{
			name: "several predicates in one Where are AND",
			q: func() *orm.Query[User] {
				return userQuery().Where(Users.Active.Eq(true), Users.Age.Gte(int32(18)))
			},
			sql:  selectAll + ` WHERE "users"."active" = $1 AND "users"."age" >= $2`,
			args: []any{true, int32(18)},
		},
		{
			name: "several Where calls are AND",
			q: func() *orm.Query[User] {
				return userQuery().Where(Users.Active.Eq(true)).Where(Users.Age.Gte(int32(18)))
			},
			sql:  selectAll + ` WHERE "users"."active" = $1 AND "users"."age" >= $2`,
			args: []any{true, int32(18)},
		},
		{
			name: "OrderBy",
			q: func() *orm.Query[User] {
				return userQuery().OrderBy(Users.CreatedAt.Desc(), Users.ID.Asc())
			},
			sql: selectAll + ` ORDER BY "users"."created_at" DESC, "users"."id" ASC`,
		},
		{
			name: "repeated OrderBy appends",
			q: func() *orm.Query[User] {
				return userQuery().OrderBy(Users.CreatedAt.Desc()).OrderBy(Users.ID.Asc())
			},
			sql: selectAll + ` ORDER BY "users"."created_at" DESC, "users"."id" ASC`,
		},
		{
			name: "Limit",
			q:    func() *orm.Query[User] { return userQuery().Limit(10) },
			sql:  selectAll + ` LIMIT 10`,
		},
		{
			name: "a zero limit is a query that returns nothing",
			q:    func() *orm.Query[User] { return userQuery().Limit(0) },
			sql:  selectAll + ` LIMIT 0`,
		},
		{
			name: "everything at once",
			q: func() *orm.Query[User] {
				return userQuery().
					Where(Users.Active.Eq(true), Users.Age.Gte(int32(18))).
					OrderBy(Users.CreatedAt.Desc()).
					Limit(50)
			},
			sql: selectAll + ` WHERE "users"."active" = $1 AND "users"."age" >= $2` +
				` ORDER BY "users"."created_at" DESC LIMIT 50`,
			args: []any{true, int32(18)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, args, err := tt.q().SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if sql != tt.sql {
				t.Errorf("sql =\n%s\nwant\n%s", sql, tt.sql)
			}
			assertArgs(t, args, tt.args)
		})
	}
}

func assertArgs(t *testing.T, got, want []any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d] = %#v (%T), want %#v (%T)", i, got[i], got[i], want[i], want[i])
		}
	}
}

func TestQuery_emptyPredicateSliceProducesNoWhere(t *testing.T) {
	// This is the shape dynamic filtering lives in: build a slice, append the
	// filters that apply, and pass whatever is there.
	predicates := []orm.Predicate[User]{}

	sql, args, err := userQuery().Where(orm.And(predicates...)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if sql != selectAll {
		t.Errorf("sql = %s\nwant no WHERE clause at all:\n%s", sql, selectAll)
	}
	if len(args) != 0 {
		t.Errorf("args = %#v, want none", args)
	}
	if strings.Contains(sql, "WHERE") {
		t.Error("an empty predicate set produced a WHERE clause")
	}
}

func TestQuery_zeroPredicateSemantics(t *testing.T) {
	tests := []struct {
		name string
		pred orm.Predicate[User]
		want string
	}{
		// And of nothing restricts nothing, so the clause disappears.
		{name: "And of nothing", pred: orm.And[User](), want: selectAll},
		// Or of nothing matches nothing, which is a real restriction.
		{name: "Or of nothing", pred: orm.Or[User](), want: selectAll + " WHERE FALSE"},
		// And of Or of nothing is still nothing matching.
		{name: "And of Or of nothing", pred: orm.And(orm.Or[User]()), want: selectAll + " WHERE FALSE"},
		// Negating the condition that holds of everything holds of nothing.
		{name: "Not of the zero predicate", pred: orm.Not(orm.Predicate[User]{}), want: selectAll + " WHERE FALSE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sql, _, err := userQuery().Where(tt.pred).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if sql != tt.want {
				t.Errorf("sql = %s\nwant %s", sql, tt.want)
			}
			for _, bad := range []string{"WHERE ()", "IN ()", "AND )", "( AND"} {
				if strings.Contains(sql, bad) {
					t.Errorf("sql contains %q, which is not valid PostgreSQL: %s", bad, sql)
				}
			}
		})
	}
}

func TestQuery_zeroPredicatesAreIgnoredInsideAGroup(t *testing.T) {
	// A conditionally-built filter that was never set must not turn into a
	// stray operand.
	var unset orm.Predicate[User]
	sql, args, err := userQuery().Where(orm.And(unset, Users.Active.Eq(true), unset)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."active" = $1`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	assertArgs(t, args, []any{true})
}

func TestQuery_argumentNumberingFollowsTextOrder(t *testing.T) {
	sql, args, err := userQuery().
		Where(Users.Email.ILike("%a%")).
		Where(Users.Age.Between(1, 2)).
		Where(Users.ID.In(7, 8)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := selectAll + ` WHERE "users"."email" ILIKE $1 AND "users"."age" BETWEEN $2 AND $3` +
		` AND "users"."id" IN ($4, $5)`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
	assertArgs(t, args, []any{"%a%", int32(1), int32(2), int64(7), int64(8)})
}

func TestQuery_valuesAreNeverInterpolated(t *testing.T) {
	// A value that looks like SQL is still a parameter.
	const nasty = `'; DROP TABLE users; --`
	sql, args, err := userQuery().Where(Users.Email.Eq(nasty)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "DROP TABLE") {
		t.Fatalf("a caller's value reached the statement text: %s", sql)
	}
	assertArgs(t, args, []any{nasty})
}

func TestQuery_identifierQuoting(t *testing.T) {
	// Identifiers come from the catalog, but they are quoted anyway, and a
	// quote inside one is doubled rather than ending the identifier.
	src := orm.NewSource("we ird", `ta"ble`)
	meta := orm.EntityMeta[User]{
		Table:   orm.TableID{Schema: "we ird", Name: `ta"ble`},
		Source:  src,
		Columns: []orm.ColumnMeta{{Name: `co"l`, Field: "ID"}},
		Dest:    func(u *User, idx int) any { return &u.ID },
	}
	col := orm.NewCol[User, int64](src, `co"l`)
	sql, _, err := orm.NewRepo(stubExecutor{}, &meta).Query().Where(col.Eq(1)).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	const want = `SELECT "ta""ble"."co""l" FROM "we ird"."ta""ble" WHERE "ta""ble"."co""l" = $1`
	if sql != want {
		t.Errorf("sql = %s\nwant %s", sql, want)
	}
}

func TestQuery_negativeLimitSurfacesFromTheTerminalOperation(t *testing.T) {
	q := userQuery().Limit(-1)
	// The builder itself does not panic and does not report.
	_, _, err := q.SQL()
	if err == nil {
		t.Fatal("SQL succeeded after a negative limit")
	}
	if !strings.Contains(err.Error(), "negative limit") {
		t.Errorf("error = %v", err)
	}
	if _, err := q.All(t.Context()); err == nil {
		t.Error("All succeeded after a negative limit")
	}
}

func TestQuery_invalidMetadata(t *testing.T) {
	tests := []struct {
		name string
		meta *orm.EntityMeta[User]
		want string
	}{
		{name: "nil", meta: nil, want: "is nil"},
		{
			name: "no table",
			meta: &orm.EntityMeta[User]{Columns: []orm.ColumnMeta{{Name: "id"}}, Dest: userMeta.Dest},
			want: "names no table",
		},
		{
			name: "no columns",
			meta: &orm.EntityMeta[User]{Table: orm.TableID{Schema: "public", Name: "users"}, Dest: userMeta.Dest},
			want: "has no columns",
		},
		{
			name: "no scanner",
			meta: &orm.EntityMeta[User]{Table: orm.TableID{Schema: "public", Name: "users"}, Columns: []orm.ColumnMeta{{Name: "id"}}},
			want: "has no scanner",
		},
		{
			name: "unnamed column",
			meta: &orm.EntityMeta[User]{Table: orm.TableID{Schema: "public", Name: "users"}, Columns: []orm.ColumnMeta{{Name: ""}}, Dest: userMeta.Dest},
			want: "unnamed column",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := orm.NewRepo(stubExecutor{}, tt.meta).Query().SQL()
			if err == nil {
				t.Fatal("SQL succeeded with invalid metadata")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestQuery_noExecutor(t *testing.T) {
	_, err := orm.NewRepo[User](nil, &userMeta).Query().All(t.Context())
	if err == nil {
		t.Fatal("All succeeded with no executor")
	}
	if !strings.Contains(err.Error(), "no executor") {
		t.Errorf("error = %v", err)
	}
}

func TestQuery_isDeterministic(t *testing.T) {
	build := func() (string, []any) {
		sql, args, err := userQuery().
			Where(orm.And(Users.Active.Eq(true), Users.Email.ILike("%a%"))).
			OrderBy(Users.CreatedAt.Desc(), Users.ID.Asc()).
			Limit(50).
			SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		return sql, args
	}
	first, firstArgs := build()
	for i := range 3 {
		sql, args := build()
		if sql != first {
			t.Fatalf("build %d produced different SQL", i+2)
		}
		if len(args) != len(firstArgs) {
			t.Fatalf("build %d produced a different argument count", i+2)
		}
	}
}

func TestQuery_sqlNeedsNoExecutor(t *testing.T) {
	// Rendering a statement does not touch the database, so a repository with
	// no executor can still show its work. That is what makes a query
	// inspectable in a test, or loggable before it runs.
	sql, args, err := orm.NewRepo[User](nil, &userMeta).Query().
		Where(Users.Active.Eq(true)).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if sql != selectAll+` WHERE "users"."active" = $1` {
		t.Errorf("sql = %s", sql)
	}
	assertArgs(t, args, []any{true})
}

func TestQuery_orderByAZeroTerm(t *testing.T) {
	// A zero Order carries no column, so there is nothing to sort by. Emitting
	// ORDER BY "" would be a syntax error at the database; reporting it is the
	// only useful thing left.
	_, _, err := userQuery().OrderBy(orm.Order[User]{}).SQL()
	if err == nil {
		t.Fatal("SQL succeeded with an order term that names no column")
	}
	if !strings.Contains(err.Error(), "no column") {
		t.Errorf("error = %v", err)
	}
}

func TestQuery_builderMethodsNeverPanic(t *testing.T) {
	// Every builder method records rather than raises, so a chain can be
	// written without defending each link.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a builder method panicked: %v", r)
		}
	}()
	q := orm.NewRepo[User](nil, nil).Query().
		Where(orm.Predicate[User]{}).
		Where(Users.Active.Eq(true)).
		OrderBy(orm.Order[User]{}).
		OrderBy(Users.ID.Asc()).
		Limit(-1).
		Limit(10)
	if _, _, err := q.SQL(); err == nil {
		t.Error("SQL succeeded over a builder full of mistakes")
	}
}

func TestQuery_theFirstMistakeIsTheOneReported(t *testing.T) {
	// Later mistakes do not overwrite earlier ones, so the error names what
	// went wrong first rather than last.
	_, _, err := userQuery().Limit(-1).OrderBy(orm.Order[User]{}).SQL()
	if err == nil {
		t.Fatal("SQL succeeded")
	}
	if !strings.Contains(err.Error(), "negative limit") {
		t.Errorf("error = %v, want the first mistake", err)
	}
}

func TestQuery_isReusable(t *testing.T) {
	// SQL is the documented way to inspect a query before running it, so
	// calling it and then All must produce the same statement twice.
	q := userQuery().Where(Users.Age.Gte(int32(18))).Limit(5)

	first, firstArgs, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	second, secondArgs, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if first != second {
		t.Errorf("two renders differ:\n%s\n%s", first, second)
	}
	if len(firstArgs) != len(secondArgs) {
		t.Errorf("two renders produced %d and %d arguments", len(firstArgs), len(secondArgs))
	}
}

func TestPredicate_isZero(t *testing.T) {
	var unset orm.Predicate[User]
	if !unset.IsZero() {
		t.Error("the zero Predicate reported a condition")
	}
	if Users.Active.Eq(true).IsZero() {
		t.Error("a built predicate reported as zero")
	}
	// And of only zero predicates is still the identity, not an empty group.
	if orm.And(unset, unset).IsZero() {
		t.Error("And of zero predicates produced another zero predicate")
	}
}

func TestOrder_isZero(t *testing.T) {
	if !(orm.Order[User]{}).IsZero() {
		t.Error("the zero Order reported a column")
	}
	if Users.ID.Asc().IsZero() {
		t.Error("a built order term reported as zero")
	}
}

func TestCol_column(t *testing.T) {
	if got := Users.Email.Column(); got != "email" {
		t.Errorf("Column() = %q", got)
	}
	if got := Users.DeletedAt.Column(); got != "deleted_at" {
		t.Errorf("Column() = %q", got)
	}
}

func TestTableID_string(t *testing.T) {
	if got := (orm.TableID{Schema: "public", Name: "users"}).String(); got != "public.users" {
		t.Errorf("String() = %q", got)
	}
}

func TestQuery_anEmptyFilterSetAddsNothingToLaterClauses(t *testing.T) {
	// The shape that produced WHERE TRUE AND ...: a dynamic filter set that
	// turned out empty, combined with a filter the caller always applies.
	var predicates []orm.Predicate[User]

	tests := []struct {
		name string
		q    func() *orm.Query[User]
		want string
	}{
		{
			name: "an empty set then a fixed filter",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.And(predicates...)).Where(Users.Active.Eq(true))
			},
			want: selectAll + ` WHERE "users"."active" = $1`,
		},
		{
			name: "a fixed filter then an empty set",
			q: func() *orm.Query[User] {
				return userQuery().Where(Users.Active.Eq(true)).Where(orm.And(predicates...))
			},
			want: selectAll + ` WHERE "users"."active" = $1`,
		},
		{
			name: "an empty set nested inside another",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.And(orm.And(predicates...), Users.Active.Eq(true)))
			},
			want: selectAll + ` WHERE "users"."active" = $1`,
		},
		{
			name: "an empty Or set under Or",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.Or(orm.Or(predicates...), Users.Active.Eq(true)))
			},
			want: selectAll + ` WHERE "users"."active" = $1`,
		},
		{
			name: "nothing but empty sets",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.And(predicates...)).Where(orm.And(predicates...))
			},
			want: selectAll,
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
			if strings.Contains(sql, "TRUE") {
				t.Errorf("the identity reached the statement: %s", sql)
			}
		})
	}
}

func TestQuery_annihilatorsStayVisible(t *testing.T) {
	// A caller who wrote In() over an empty slice is better served seeing why
	// nothing matches than seeing the clause disappear.
	tests := []struct {
		name string
		q    func() *orm.Query[User]
		want string
	}{
		{
			name: "FALSE under AND",
			q: func() *orm.Query[User] {
				return userQuery().Where(Users.ID.In()).Where(Users.Active.Eq(true))
			},
			want: selectAll + ` WHERE FALSE AND "users"."active" = $1`,
		},
		{
			name: "TRUE under OR",
			q: func() *orm.Query[User] {
				return userQuery().Where(orm.Or(orm.And[User](), Users.Active.Eq(true)))
			},
			want: selectAll + ` WHERE TRUE OR "users"."active" = $1`,
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
