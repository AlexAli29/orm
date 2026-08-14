package gendemo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M11.4: the advanced expressions.
//
// Two things are checked for every one of them. That the value PostgreSQL
// computes is the value the handwritten SQL computes, and that the type this
// package claims is the type pg_typeof reports — because a result type stated
// in a comment is a result type nobody verified.

// oneRow builds a composed query selecting one expression from one seeded user,
// which is the shape most of these checks take.
func oneValue[T any](t *testing.T, db *gendemo.DB, v orm.Selectable[orm.Composed, T]) T {
	t.Helper()
	shape := orm.Project1(v, func(x T) T { return x })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))).
		One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	return got
}

func TestCase_withElseIsNotNullable(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	label := orm.Case(orm.Cond(gendemo.Users.Active.Eq(true)), orm.Val("active")).
		When(orm.Cond(gendemo.Users.Age.Lt(int32(18))), orm.Val("minor")).
		Else(orm.Val("inactive"))

	type row struct {
		ID    int64
		Label string
	}
	shape := orm.Project2(orm.Of(gendemo.Users.ID), label,
		func(id int64, l string) row { return row{ID: id, Label: l} })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	rows, err := conn.Query(t.Context(), `
		SELECT id, CASE WHEN active THEN 'active' WHEN age < 18 THEN 'minor' ELSE 'inactive' END
		FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("handwritten query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if got[i] != (row{ID: id, Label: label}) {
			t.Errorf("row %d = %+v, handwritten SQL returned {%d %s}", i, got[i], id, label)
		}
		i++
	}
	if i != len(got) {
		t.Errorf("the ORM returned %d rows, handwritten SQL %d", len(got), i)
	}
	// The branches are evaluated in order: user 2 is both active and under 18,
	// and the first matching branch wins.
	if got[1].Label != "active" {
		t.Errorf("branch order was not preserved: %+v", got[1])
	}
}

// A CASE with no ELSE is NULL for a row matching no branch, whatever its
// branches produce, so the result type is nullable.
func TestCase_withoutElseIsNullable(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	label := orm.Case(orm.Cond(gendemo.Users.Age.Gte(int32(100))), orm.Val("ancient")).End()
	got := oneValue(t, db, label)
	if got != nil {
		t.Errorf("a CASE matching no branch produced %q, want NULL", *got)
	}
}

func TestCoalesce_isNotNullableWhenTheFallbackIsNot(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	// nickname is nullable and user 2's is NULL.
	nick := orm.Coalesce(orm.Val("anonymous"), orm.Of(gendemo.Users.Nickname))
	shape := orm.Project1(nick, func(s string) string { return s })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got[0] != "alex" || got[1] != "anonymous" {
		t.Errorf("got %v, want the nickname then the fallback", got)
	}
}

func TestNullIf_isAlwaysNullable(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	// email is NOT NULL, and NULLIF still produces NULL when the two match.
	same := orm.NullIf(gendemo.Users.Email, orm.Val("alex@example.com"))
	if got := oneValue(t, db, same); got != nil {
		t.Errorf("NULLIF of two equal values produced %q, want NULL", *got)
	}
	other := orm.NullIf(gendemo.Users.Email, orm.Val("nobody@example.com"))
	if got := oneValue(t, db, other); got == nil || *got != "alex@example.com" {
		t.Errorf("NULLIF of two different values produced %v", got)
	}
}

func TestCast_takesItsGoTypeFromTheTarget(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	text := orm.Cast(gendemo.Users.ID, orm.Text)
	if got := oneValue(t, db, text); got != "1" {
		t.Errorf("casting id to text produced %q", got)
	}
	// A cast of a nullable expression stays nullable.
	nullable := orm.CastNull(gendemo.Users.Nickname, orm.Text)
	shape := orm.Project1(nullable, func(s *string) *string { return s })
	all, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if all[1] != nil {
		t.Errorf("casting NULL produced %q, want NULL", *all[1])
	}
}

func TestStringAndDateFunctions(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	if got := oneValue(t, db, orm.Upper(gendemo.Users.Email)); got != "ALEX@EXAMPLE.COM" {
		t.Errorf("upper = %q", got)
	}
	if got := oneValue(t, db, orm.Lower(orm.Cast(orm.Val("ABC"), orm.Text))); got != "abc" {
		t.Errorf("lower = %q", got)
	}
	if got := oneValue(t, db, orm.Trim(orm.Cast(orm.Val("  x  "), orm.Text))); got != "x" {
		t.Errorf("btrim = %q", got)
	}
	if got := oneValue(t, db, orm.Length(gendemo.Users.Email)); got != 16 {
		t.Errorf("length = %d", got)
	}
	// concat is variadic over "any", so its arguments carry no type for
	// PostgreSQL to infer and a bare parameter has to say what it is.
	concat := orm.Concat(orm.Of(gendemo.Users.Email), orm.Cast(orm.Val("!"), orm.Text))
	if got := oneValue(t, db, concat); got != "alex@example.com!" {
		t.Errorf("concat = %q", got)
	}
	// The first user was created on 2024-01-01T09:00:00Z.
	if got := oneValue(t, db, orm.Extract(orm.Year, gendemo.Users.CreatedAt, orm.Integer)); got != 2024 {
		t.Errorf("extract(year) = %d", got)
	}
	truncated := oneValue(t, db, orm.DateTrunc(orm.Day, gendemo.Users.CreatedAt))
	if !truncated.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("date_trunc(day) = %v", truncated)
	}
	if got := oneValue(t, db, orm.Now()); got.IsZero() {
		t.Error("now() produced the zero time")
	}
	// A nullable input keeps a nullable result.
	if got := oneValue(t, db, orm.LowerNull(gendemo.Users.Nickname)); got == nil || *got != "alex" {
		t.Errorf("lower of a nullable column = %v", got)
	}
}

// Scenario J: DISTINCT ON, compared against the handwritten statement.
func TestDistinctOn_keepsOneRowPerKey(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	shape := orm.Project2(
		orm.Of(gendemo.Posts.AuthorID), orm.Of(gendemo.Posts.ID),
		func(a *int64, id int64) [2]int64 {
			var out [2]int64
			if a != nil {
				out[0] = *a
			}
			out[1] = id
			return out
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Posts.Source()).
		DistinctOn(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(orm.Of(gendemo.Posts.AuthorID).Asc(), orm.Of(gendemo.Posts.CreatedAt).Desc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	want := handwrittenIDs(t, conn, `
		SELECT DISTINCT ON (author_id) id FROM posts
		ORDER BY author_id, created_at DESC`)
	if len(got) != len(want) {
		t.Fatalf("got %d rows, handwritten SQL returned %d", len(got), len(want))
	}
	for i := range got {
		if got[i][1] != want[i] {
			t.Errorf("row %d kept post %d, handwritten SQL kept %d", i, got[i][1], want[i])
		}
	}
}

func TestDistinctOn_isNotDistinct(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Posts.ID), func(id int64) int64 { return id })
	_, _, err := orm.Compose(nil, shape).
		From(gendemo.Posts.Source()).
		Distinct().
		DistinctOn(orm.Of(gendemo.Posts.AuthorID)).
		SQL()
	if err == nil {
		t.Fatal("a statement that is both DISTINCT and DISTINCT ON compiled")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error = %v", err)
	}
}

// A composite key compared as a tuple, which is one comparison rather than a
// conjunction of two.
func TestRowComparison(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO tenants (code, region, name) VALUES ('b1', 'eu', 'One'), ('b2', 'us', 'Two')`)
	m11exec(t, conn, `INSERT INTO branches (id, branch_code, branch_region, label) VALUES
	    (1, 'b1', 'eu', 'first'), (2, 'b2', 'us', 'second')`)

	shape := orm.Project1(orm.Of(gendemo.Branches.ID), func(id int64) int64 { return id })

	t.Run("equality", func(t *testing.T) {
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Branches.Source()).
			Where(orm.Row2Eq(gendemo.Branches.Region, gendemo.Branches.Code, "eu", "b1")).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		assertIDs(t, conn,
			`SELECT id FROM branches WHERE (branch_region, branch_code) = ('eu', 'b1')`, got)
	})
	t.Run("membership", func(t *testing.T) {
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Branches.Source()).
			Where(orm.Row2In(gendemo.Branches.Region, gendemo.Branches.Code,
				orm.Both("eu", "b1"), orm.Both("us", "b2"))).
			OrderBy(orm.Of(gendemo.Branches.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		assertIDs(t, conn, `
			SELECT id FROM branches
			WHERE (branch_region, branch_code) IN (('eu','b1'), ('us','b2'))
			ORDER BY id`, got)
	})
	t.Run("membership of nothing", func(t *testing.T) {
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Branches.Source()).
			Where(orm.Row2In(gendemo.Branches.Region, gendemo.Branches.Code)).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("membership of no tuples matched %v", got)
		}
	})
}

func TestArrayOperators(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	goTag := orm.Val("go")
	goOnly := orm.Val([]string{"go"})

	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
		sql  string
	}{
		{"any", orm.AnyOf(goTag, gendemo.Users.Tags),
			`SELECT id FROM users WHERE 'go' = ANY(tags) ORDER BY id`},
		{"contains", orm.ArrayContains(gendemo.Users.Tags, goOnly),
			`SELECT id FROM users WHERE tags @> ARRAY['go'] ORDER BY id`},
		{"contained by", orm.ArrayContainedBy(gendemo.Users.Tags, orm.Val([]string{"go", "sql", "ops"})),
			`SELECT id FROM users WHERE tags <@ ARRAY['go','sql','ops'] ORDER BY id`},
		{"overlaps", orm.ArrayOverlaps(gendemo.Users.Tags, orm.Val([]string{"ops"})),
			`SELECT id FROM users WHERE tags && ARRAY['ops'] ORDER BY id`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orm.Compose(db.Executor(), shape).
				From(gendemo.Users.Source()).
				Where(tt.pred).
				OrderBy(orm.Of(gendemo.Users.ID).Asc()).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			assertIDs(t, conn, tt.sql, got)
		})
	}
}

func TestJSONOperators(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
		sql  string
	}{
		{"has key", orm.JSONHasKey(gendemo.Users.Settings, "tier"),
			`SELECT id FROM users WHERE settings ? 'tier' ORDER BY id`},
		{"contains", orm.JSONContains(gendemo.Users.Settings, orm.Val(map[string]any{"tier": "gold"})),
			`SELECT id FROM users WHERE settings @> '{"tier":"gold"}'::jsonb ORDER BY id`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := orm.Compose(db.Executor(), shape).
				From(gendemo.Users.Source()).
				Where(tt.pred).
				OrderBy(orm.Of(gendemo.Users.ID).Asc()).
				All(t.Context())
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			assertIDs(t, conn, tt.sql, got)
		})
	}

	tier := orm.JSONText(gendemo.Users.Settings, "tier")
	if got := oneValue(t, db, tier); got == nil || *got != "gold" {
		t.Errorf("settings->>'tier' = %v", got)
	}
	missing := orm.JSONText(gendemo.Users.Settings, "absent")
	if got := oneValue(t, db, missing); got != nil {
		t.Errorf("a missing key produced %q, want NULL", *got)
	}
}

// Every result type this package claims, checked against the server rather than
// against a comment. A claim pg_typeof disagrees with is a scan error waiting
// for the first row that exercises it.
func TestPGTypeOf_M11Expressions(t *testing.T) {
	testdb.AdminDSN(t)
	_, conn := m11env(t)

	for _, tt := range []struct {
		what string
		sql  string
		want string
	}{
		{"CASE over text", `SELECT pg_typeof(CASE WHEN true THEN 'a' ELSE 'b' END)`, "text"},
		{"coalesce over text", `SELECT pg_typeof(coalesce(nickname, 'x')) FROM users LIMIT 1`, "text"},
		{"nullif over text", `SELECT pg_typeof(nullif(email, 'x')) FROM users LIMIT 1`, "text"},
		{"cast to text", `SELECT pg_typeof(CAST(id AS "text")) FROM users LIMIT 1`, "text"},
		{"cast to bigint", `SELECT pg_typeof(CAST(age AS "int8")) FROM users LIMIT 1`, "bigint"},
		{"cast to integer", `SELECT pg_typeof(CAST(id AS "int4")) FROM users LIMIT 1`, "integer"},
		{"cast to double", `SELECT pg_typeof(CAST(id AS "float8")) FROM users LIMIT 1`, "double precision"},
		{"cast to boolean", `SELECT pg_typeof(CAST(1 AS "bool"))`, "boolean"},
		{"lower", `SELECT pg_typeof(lower(email)) FROM users LIMIT 1`, "text"},
		{"upper", `SELECT pg_typeof(upper(email)) FROM users LIMIT 1`, "text"},
		{"btrim", `SELECT pg_typeof(btrim(email)) FROM users LIMIT 1`, "text"},
		{"length", `SELECT pg_typeof(length(email)) FROM users LIMIT 1`, "integer"},
		{"concat", `SELECT pg_typeof(concat('a', 'b'))`, "text"},
		{"now", `SELECT pg_typeof(now())`, "timestamp with time zone"},
		{"date_trunc", `SELECT pg_typeof(date_trunc('day', created_at)) FROM users LIMIT 1`, "timestamp with time zone"},
		// The reason Extract carries a cast: PostgreSQL's own result is
		// numeric, which no Go type in this package claims.
		{"extract", `SELECT pg_typeof(extract(year FROM created_at)) FROM users LIMIT 1`, "numeric"},
		{"extract cast to integer", `SELECT pg_typeof(CAST(extract(year FROM created_at) AS "int4")) FROM users LIMIT 1`, "integer"},
		{"json ->>", `SELECT pg_typeof(settings ->> 'tier') FROM users LIMIT 1`, "text"},
		{"json ?", `SELECT pg_typeof(settings ? 'tier') FROM users LIMIT 1`, "boolean"},
		{"row equality", `SELECT pg_typeof((1, 2) = (1, 2))`, "boolean"},
		{"any", `SELECT pg_typeof('go' = ANY(tags)) FROM users LIMIT 1`, "boolean"},
	} {
		t.Run(tt.what, func(t *testing.T) {
			if got := pgTypeOf(t, conn, tt.sql); got != tt.want {
				t.Errorf("pg_typeof = %q, this package claims %q", got, tt.want)
			}
		})
	}
}

// The SQL these constructors emit is the SQL the pg_typeof matrix was written
// against, which is what makes that matrix evidence about this package rather
// than about PostgreSQL.
func TestM11Expressions_compileToTheCheckedSQL(t *testing.T) {
	for _, tt := range []struct {
		what string
		expr orm.Selectable[orm.Composed, string]
		want string
	}{
		{"lower", orm.Lower(gendemo.Users.Email), `lower("users"."email")`},
		{"upper", orm.Upper(gendemo.Users.Email), `upper("users"."email")`},
		{"btrim", orm.Trim(gendemo.Users.Email), `btrim("users"."email")`},
		{"cast", orm.Cast(gendemo.Users.ID, orm.Text), `CAST("users"."id" AS "text")`},
		{"concat", orm.Concat(orm.Of(gendemo.Users.Email), orm.Val("!")), `concat("users"."email", $1)`},
		{"coalesce", orm.Coalesce(orm.Val("x"), orm.Of(gendemo.Users.Nickname)), `coalesce("users"."nickname", $1)`},
	} {
		t.Run(tt.what, func(t *testing.T) {
			shape := orm.Project1(tt.expr, func(s string) string { return s })
			sql, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if !strings.Contains(sql, tt.want) {
				t.Errorf("SQL = %s\nwant it to contain %s", sql, tt.want)
			}
		})
	}

	extract := orm.Extract(orm.Year, gendemo.Users.CreatedAt, orm.Integer)
	shape := orm.Project1(extract, func(v int32) int32 { return v })
	sql, _, err := orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if !strings.Contains(sql, `CAST(extract(year FROM "users"."created_at") AS "int4")`) {
		t.Errorf("SQL = %s", sql)
	}
}
