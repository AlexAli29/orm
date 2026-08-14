package gendemo_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M11.3: WITH, and WITH RECURSIVE.
//
// A CTE is a source, so most of what could be tested here was already settled
// by the derived-table tests. What is specific is declaration order, the shared
// parameter list across several bodies and one main query, and the recursive
// form — whose output types come from the anchor and whose recursive term is
// checked against them.

var (
	activeID    = orm.Named("id", orm.Of(gendemo.Users.ID))
	activeEmail = orm.Named("email", orm.Of(gendemo.Users.Email))
)

func activeUsers() *orm.Source {
	return orm.CTE("active_users", orm.Rows(activeID, activeEmail).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))))
}

func TestCTE_compilesAsAWithItemAndASource(t *testing.T) {
	active := activeUsers()
	shape := orm.Project1(orm.Ref(active, activeEmail), func(s string) string { return s })

	sql, args, err := orm.Compose(nil, shape).With(active).From(active).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	want := `WITH "active_users" AS (SELECT "users"."id" AS "id", "users"."email" AS "email" ` +
		`FROM "public"."users" WHERE "users"."active" = $1) ` +
		`SELECT "active_users"."email" FROM "active_users"`
	if sql != want {
		t.Errorf("SQL =\n  %s\nwant\n  %s", sql, want)
	}
	if len(args) != 1 || args[0] != true {
		t.Errorf("args = %v", args)
	}
}

// Several WITH items and a main query share one parameter list, numbered in the
// order the SQL is written: the first body, then the second, then the query.
func TestCTE_placeholdersAreOneNamespaceAcrossEveryBody(t *testing.T) {
	a := orm.CTE("a", orm.Rows(activeID).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Age.Gt(int32(10)))))
	b := orm.CTE("b", orm.Rows(activeID).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Age.Gt(int32(20)))))

	shape := orm.Project1(orm.Ref(a, activeID), func(id int64) int64 { return id })
	sql, args, err := orm.Compose(nil, shape).
		With(a, b).
		From(a).
		Where(orm.Ref(a, activeID).Gt(int64(30))).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	for i, want := range []string{`"users"."age" > $1`, `"users"."age" > $2`, `"a"."id" > $3`} {
		if !strings.Contains(sql, want) {
			t.Errorf("placeholder %d: %s is missing from\n%s", i+1, want, sql)
		}
	}
	if len(args) != 3 || args[0] != int32(10) || args[1] != int32(20) || args[2] != int64(30) {
		t.Errorf("args = %v, want them in declaration order", args)
	}
}

// A WITH item may name the ones declared before it, and may not name the ones
// declared after.
func TestCTE_dependencyOrderIsSequential(t *testing.T) {
	first := activeUsers()
	second := orm.CTE("second", orm.Rows(orm.Named("id", orm.Ref(first, activeID))).
		From(first).
		Where(orm.Ref(first, activeID).Gt(int64(0))))
	shape := orm.Project1(orm.Ref(second, activeID), func(id int64) int64 { return id })

	t.Run("later names earlier", func(t *testing.T) {
		if _, _, err := orm.Compose(nil, shape).With(first, second).From(second).SQL(); err != nil {
			t.Fatalf("SQL: %v", err)
		}
	})
	t.Run("earlier names later", func(t *testing.T) {
		_, _, err := orm.Compose(nil, shape).With(second, first).From(second).SQL()
		if err == nil {
			t.Fatal("a WITH item naming one declared after it compiled")
		}
		if !strings.Contains(err.Error(), "declared after it") {
			t.Errorf("error = %v", err)
		}
	})
}

func TestCTE_refusesTwoItemsOfOneName(t *testing.T) {
	a := activeUsers()
	b := orm.CTE("active_users", orm.Rows(activeID).From(gendemo.Users.Source()))
	shape := orm.Project1(orm.Ref(a, activeID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).With(a, b).From(a).SQL()
	if err == nil {
		t.Fatal("two WITH items of one name compiled")
	}
	if !strings.Contains(err.Error(), "declared twice") {
		t.Errorf("error = %v", err)
	}
}

// One WITH item referenced twice under two aliases is two sources, told apart
// by identity rather than by name.
func TestCTE_isReferencedTwiceUnderTwoAliases(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	active := activeUsers()
	a, b := active.As("a"), active.As("b")

	shape := orm.Project2(
		orm.Ref(a, activeID), orm.Ref(b, activeID),
		func(x, y int64) [2]int64 { return [2]int64{x, y} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		With(active).
		From(a).
		Join(b, orm.Cond(orm.Expr[orm.Composed](`"b"."id" > "a"."id"`))).
		OrderBy(orm.Ref(a, activeID).Asc(), orm.Ref(b, activeID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		WITH active_users AS (SELECT id, email FROM users WHERE active)
		SELECT a.id FROM active_users a JOIN active_users b ON b.id > a.id
		ORDER BY a.id, b.id`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
}

// A CTE named after a table shadows it inside this statement, which is
// PostgreSQL's rule. The two never blur: a table source writes its
// schema-qualified name and a CTE reference writes the bare one.
func TestCTE_nameMayShadowATable(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	shadow := orm.CTE("users", orm.Rows(activeID).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))))
	shape := orm.Project1(orm.Ref(shadow, activeID), func(id int64) int64 { return id })

	sql, _, err := orm.Compose(db.Executor(), shape).With(shadow).From(shadow).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasSuffix(sql, `FROM "users"`) {
		t.Errorf("the outer FROM does not name the CTE: %s", sql)
	}
	if !strings.Contains(sql, `FROM "public"."users"`) {
		t.Errorf("the body no longer names the table it reads: %s", sql)
	}
	got, err := orm.Compose(db.Executor(), shape).With(shadow).From(shadow).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `WITH users AS (SELECT id FROM users WHERE id = 1) SELECT id FROM users`)
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("got %v, handwritten SQL returned %v", got, want)
	}
}

func TestCTE_materializationHints(t *testing.T) {
	body := func() orm.SourceTerm {
		return orm.Rows(activeID, activeEmail).
			From(gendemo.Users.Source()).
			Where(orm.Cond(gendemo.Users.Active.Eq(true)))
	}
	for _, tt := range []struct {
		name string
		opts []orm.CTEOption
		want string
	}{
		{"unhinted", nil, `AS (`},
		{"materialized", []orm.CTEOption{orm.Materialized}, `AS MATERIALIZED (`},
		{"not materialized", []orm.CTEOption{orm.NotMaterialized}, `AS NOT MATERIALIZED (`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cte := orm.CTE("c", body(), tt.opts...)
			shape := orm.Project1(orm.Ref(cte, activeID), func(id int64) int64 { return id })
			sql, _, err := orm.Compose(nil, shape).With(cte).From(cte).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if !strings.Contains(sql, tt.want) {
				t.Errorf("SQL = %s", sql)
			}
		})
	}
}

// Scenario H: a hierarchy walked with WITH RECURSIVE, compared against the
// handwritten statement.
func TestRecursiveCTE_walksAHierarchy(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	// alex manages sam, sam manages robin.
	m11exec(t, conn, `UPDATE users SET manager_id = 1 WHERE id = 2`)
	m11exec(t, conn, `UPDATE users SET manager_id = 2 WHERE id = 3`)

	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	// manager_id is nullable, so the anchor declares it nullable and the
	// recursive term agrees — which is the convergence check doing its job.
	managerID := orm.Named("manager_id", orm.Of(gendemo.Users.ManagerID))

	tree := orm.RecursiveCTE("tree",
		orm.Rows(id, managerID).
			From(gendemo.Users.Source()).
			Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))),
		func(self *orm.Source) orm.Term {
			return orm.Rows(
				orm.Named("id", orm.Of(gendemo.Users.ID)),
				orm.Named("manager_id", orm.Of(gendemo.Users.ManagerID)),
			).
				From(gendemo.Users.Source()).
				Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, id)))
		},
	)

	type row struct {
		ID      int64
		Manager *int64
	}
	shape := orm.Project2(orm.Ref(tree, id), orm.Ref(tree, managerID),
		func(id int64, m *int64) row { return row{ID: id, Manager: m} })

	got, err := orm.Compose(db.Executor(), shape).
		With(tree).
		From(tree).
		OrderBy(orm.Ref(tree, id).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want the whole chain", len(got))
	}
	if got[0].Manager != nil {
		t.Errorf("the root of the tree has a manager: %+v", got[0])
	}
	if got[2].Manager == nil || *got[2].Manager != 2 {
		t.Errorf("row 3 = %+v, want it reporting manager 2", got[2])
	}

	handwritten := handwrittenIDs(t, conn, `
		WITH RECURSIVE tree(id, manager_id) AS (
		    SELECT id, manager_id FROM users WHERE id = 1
		    UNION ALL
		    SELECT u.id, u.manager_id FROM users u JOIN tree t ON u.manager_id = t.id
		)
		SELECT id FROM tree ORDER BY id`)
	if len(handwritten) != len(got) {
		t.Fatalf("the ORM returned %d rows, handwritten SQL %d", len(got), len(handwritten))
	}
	for i := range got {
		if got[i].ID != handwritten[i] {
			t.Errorf("row %d: the ORM read %d, handwritten SQL read %d", i, got[i].ID, handwritten[i])
		}
	}
}

func TestRecursiveCTE_writesTheRecursiveKeywordAndAColumnList(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	tree := orm.RecursiveCTE("tree",
		orm.Rows(id).From(gendemo.Users.Source()),
		func(self *orm.Source) orm.Term {
			return orm.Rows(orm.Named("id", orm.Of(gendemo.Users.ID))).
				From(gendemo.Users.Source()).
				Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, id)))
		},
	)
	shape := orm.Project1(orm.Ref(tree, id), func(v int64) int64 { return v })
	sql, _, err := orm.Compose(nil, shape).With(tree).From(tree).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.HasPrefix(sql, `WITH RECURSIVE "tree"("id") AS (`) {
		t.Errorf("SQL = %s", sql)
	}
	if !strings.Contains(sql, " UNION ALL ") {
		t.Errorf("the two terms are not combined with UNION ALL: %s", sql)
	}
}

// The output types come from the anchor term. A recursive term that can produce
// NULL where the anchor cannot would leave a result typed as non-nullable and a
// row that is NULL, so it is refused.
func TestRecursiveCTE_refusesANullableRecursiveTermUnderANonNullableAnchor(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	tree := orm.RecursiveCTE("tree",
		orm.Rows(id).From(gendemo.Users.Source()),
		func(self *orm.Source) orm.Term {
			return orm.Rows(orm.Named("id", orm.Opt(gendemo.Users.ID))).
				From(gendemo.Users.Source()).
				Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, id)))
		},
	)
	if tree.Err() == nil {
		t.Fatal("a nullable recursive term under a non-nullable anchor was accepted")
	}
	if !strings.Contains(tree.Err().Error(), "NamedNull") {
		t.Errorf("error = %v", tree.Err())
	}
}

func TestRecursiveCTE_refusesTermsThatDisagree(t *testing.T) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	email := orm.Named("email", orm.Of(gendemo.Users.Email))

	t.Run("different arity", func(t *testing.T) {
		tree := orm.RecursiveCTE("tree",
			orm.Rows(id).From(gendemo.Users.Source()),
			func(self *orm.Source) orm.Term {
				return orm.Rows(id, email).From(gendemo.Users.Source()).
					Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, id)))
			})
		if tree.Err() == nil || !strings.Contains(tree.Err().Error(), "columns in its anchor term") {
			t.Fatalf("error = %v", tree.Err())
		}
	})
	t.Run("different names", func(t *testing.T) {
		tree := orm.RecursiveCTE("tree",
			orm.Rows(id).From(gendemo.Users.Source()),
			func(self *orm.Source) orm.Term {
				return orm.Rows(orm.Named("other", orm.Of(gendemo.Users.ID))).
					From(gendemo.Users.Source()).
					Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, id)))
			})
		if tree.Err() == nil || !strings.Contains(tree.Err().Error(), "names column") {
			t.Fatalf("error = %v", tree.Err())
		}
	})
}

// A CTE joined and aggregated, which needs no special path because a CTE
// reference is a source like any other.
func TestCTE_joinsAndAggregatesLikeAnyOtherSource(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	active := activeUsers()
	type row struct {
		ID    int64
		Posts int64
	}
	shape := orm.Project2(
		orm.Ref(active, activeID), orm.Of(orm.CountOf(gendemo.Posts.ID)),
		func(id, n int64) row { return row{ID: id, Posts: n} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		With(active).
		From(active).
		LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, orm.Ref(active, activeID))).
		GroupBy(orm.Ref(active, activeID)).
		OrderBy(orm.Ref(active, activeID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		WITH active_users AS (SELECT id, email FROM users WHERE active)
		SELECT count(p.id) FROM active_users a
		LEFT JOIN posts p ON p.author_id = a.id
		GROUP BY a.id ORDER BY a.id`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	for i := range got {
		if got[i].Posts != want[i] {
			t.Errorf("row %d counted %d, handwritten SQL counted %d", i, got[i].Posts, want[i])
		}
	}
}

// A data-modifying WITH item: the write runs once, and the statement reads back
// the rows it touched rather than querying for them again.
func TestWritingCTE_readsBackWhatTheWriteTouched(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	type touched struct {
		ID    int64
		Email string
	}
	shape := orm.Project2(
		gendemo.Users.ID.As("id"), gendemo.Users.Email.As("email"),
		func(id int64, email string) touched { return touched{ID: id, Email: email} },
	)
	update := orm.UpdateReturning(
		db.Users.Update().
			Set(gendemo.Users.Age.Set(int32(99))).
			Where(gendemo.Users.Active.Eq(true)),
		shape,
	)
	changed := orm.WritingCTE("changed", update)

	changedID := orm.Named("id", orm.Of(gendemo.Users.ID))
	out := orm.Project1(orm.Ref(changed, changedID), func(id int64) int64 { return id })
	got, err := orm.Compose(db.Executor(), out).
		With(changed).
		From(changed).
		OrderBy(orm.Ref(changed, changedID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("the write returned %v, want the two active users", got)
	}
	// The write ran once and it ran for real.
	after := handwrittenIDs(t, conn, `SELECT id FROM users WHERE age = 99 ORDER BY id`)
	if len(after) != 2 {
		t.Errorf("the database has %d rows with the new age, want 2", len(after))
	}
}

func TestWritingCTE_refusesAnUnnamedReturnedColumn(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	shape := orm.Project1(gendemo.Users.ID, func(id int64) int64 { return id })
	update := orm.UpdateReturning(
		db.Users.Update().Set(gendemo.Users.Age.Set(int32(1))).All(), shape)
	changed := orm.WritingCTE("changed", update)
	if changed.Err() == nil {
		t.Fatal("a data-modifying CTE with an unnamed returned column was accepted")
	}
	if !strings.Contains(changed.Err().Error(), "has no name") {
		t.Errorf("error = %v", changed.Err())
	}
}
