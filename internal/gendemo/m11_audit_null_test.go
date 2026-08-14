package gendemo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// The M11 nullability audit.
//
// Every claim here is adversarial: the aim is to find a path by which a value
// PostgreSQL can return as NULL reaches a Go type that cannot hold one. The
// tests are written from the outside, through the public API, because that is
// where the claim is made.

// A nullable column handed straight to Named — rather than through Of — must
// still be recorded as nullable.
//
// It is the same expression either way, so anything that reads the declaration
// to decide whether NULL can arrive has to agree about the two spellings. The
// recursive convergence check is the reader that matters, and this is the
// shortest statement of the property it depends on.
func TestAudit_nullableColumnIsDeclaredNullableWhicheverWayItIsNamed(t *testing.T) {
	direct := orm.Named("m", gendemo.Users.ManagerID)
	lifted := orm.Named("m", orm.Of(gendemo.Users.ManagerID))

	src := orm.Sub("s", orm.Rows(direct).From(gendemo.Users.Source()))
	other := orm.Sub("s", orm.Rows(lifted).From(gendemo.Users.Source()))

	// Both must be usable through an outer join at the same type, which is
	// only true if both were recorded as nullable.
	for _, tt := range []struct {
		name string
		src  *orm.Source
		out  orm.Out[*int64, *int64]
	}{
		{"named directly", src, direct},
		{"named through Of", other, lifted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shape := orm.Project1(orm.Ref(tt.src, tt.out), func(v *int64) *int64 { return v })
			if _, _, err := orm.Compose(nil, shape).
				From(gendemo.Users.Source()).
				LeftJoin(tt.src, orm.Cond(orm.Expr[orm.Composed]("TRUE"))).
				SQL(); err != nil {
				t.Fatalf("a nullable derived column was refused through a LEFT JOIN: %v", err)
			}
		})
	}
}

// Release-critical: a recursive term that can produce NULL where the anchor
// cannot must be refused, whichever way its outputs were declared.
//
// The output types of a recursive CTE come from the anchor, so accepting this
// leaves a column typed as non-nullable and a query that returns NULL for it.
func TestAudit_recursiveNullabilityCannotBeBypassed(t *testing.T) {
	// The anchor's "root" is users.id, which is NOT NULL. The recursive term's
	// is users.manager_id, which is not — and it is spelled as the bare
	// descriptor rather than through Of.
	anchorID := orm.Named("id", gendemo.Users.ID)
	anchorRoot := orm.Named("root", gendemo.Users.ID)

	tree := orm.RecursiveCTE("tree",
		orm.Rows(anchorID, anchorRoot).
			From(gendemo.Users.Source()).
			Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))),
		func(self *orm.Source) orm.Term {
			return orm.Rows(
				orm.Named("id", gendemo.Users.ID),
				orm.Named("root", gendemo.Users.ManagerID),
			).
				From(gendemo.Users.Source()).
				Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, anchorID)))
		},
	)
	if tree.Err() == nil {
		t.Fatal("a recursive term that can produce NULL was accepted under a NOT NULL anchor; " +
			"the CTE's root column is typed int64 and the query returns NULL for it")
	}
	if !strings.Contains(tree.Err().Error(), "NamedNull") {
		t.Errorf("error = %v, want it to say how to declare the output", tree.Err())
	}
}

// The same bypass through an aggregate: min over an empty group is NULL, so a
// recursive term selecting one cannot sit under a non-nullable anchor.
func TestAudit_recursiveNullabilityThroughAnAggregate(t *testing.T) {
	anchorID := orm.Named("id", gendemo.Users.ID)
	anchorScore := orm.Named("score", gendemo.Users.Age)

	tree := orm.RecursiveCTE("tree",
		orm.Rows(anchorID, anchorScore).
			From(gendemo.Users.Source()).
			Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))),
		func(self *orm.Source) orm.Term {
			return orm.Rows(
				orm.Named("id", gendemo.Users.ID),
				// min(age) is NULL over an empty group.
				orm.Named("score", orm.Min(gendemo.Users.Age)),
			).
				From(gendemo.Users.Source()).
				Join(self, orm.Eq(gendemo.Users.ManagerID, orm.Ref(self, anchorID))).
				GroupBy(orm.Of(gendemo.Users.ID))
		},
	)
	if tree.Err() == nil {
		t.Fatal("a recursive term selecting an aggregate that can be NULL was accepted " +
			"under a NOT NULL anchor")
	}
}

// The nullable form of a value produced by orm.Nullable is nullable, and the
// compiler has to agree — otherwise a legal query is refused.
func TestAudit_orm_NullableIsAcceptedThroughAnOuterJoin(t *testing.T) {
	widened := orm.Nullable(orm.Of(gendemo.Profiles.ID))
	shape := orm.Project2(
		orm.Of(gendemo.Users.ID), widened,
		func(id int64, p *int64) int64 { return id },
	)
	if _, _, err := orm.Compose(nil, shape).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
		SQL(); err != nil {
		t.Fatalf("an expression widened by orm.Nullable was refused through a LEFT JOIN: %v", err)
	}
}

// Release-critical: NULL and a Go zero value must stay distinguishable through
// an outer join. A parent with no child reads nil; a parent whose child holds
// false, 0 and "" reads those values.
func TestAudit_nullIsNotTheZeroValue(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)

	// A child row whose every value is the Go zero of its type.
	m11exec(t, conn, `
		INSERT INTO users (id, email, age, active, state, tags, settings, created_at)
		VALUES (7, '', 0, false, 'pending', '{}', '{}', '2024-01-01T00:00:00Z')`)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (7, 'matches'), (8, 'no match')`)

	child := gendemo.Users.As("child")
	type row struct {
		Cat      int64
		ID       *int64
		Email    *string
		Age      *int32
		Active   *bool
		Created  *time.Time
		Settings *map[string]any
	}
	shape := orm.Project7(
		orm.Of(gendemo.Categories.ID),
		orm.Opt(child.ID),
		orm.Opt(child.Email),
		orm.Opt(child.Age),
		orm.Opt(child.Active),
		orm.Opt(child.CreatedAt),
		orm.Opt(child.Settings),
		func(cat int64, id *int64, email *string, age *int32, active *bool,
			created *time.Time, settings *map[string]any,
		) row {
			return row{Cat: cat, ID: id, Email: email, Age: age, Active: active,
				Created: created, Settings: settings}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)).
		OrderBy(orm.Of(gendemo.Categories.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows", len(got))
	}

	matched, missing := got[0], got[1]
	// The matched row holds real zero values, and every one of them arrives as
	// a present pointer to the zero rather than as nil.
	switch {
	case matched.ID == nil || *matched.ID != 7:
		t.Errorf("the matched child's id = %v", matched.ID)
	case matched.Email == nil || *matched.Email != "":
		t.Errorf("an empty string arrived as %v, want a pointer to \"\"", matched.Email)
	case matched.Age == nil || *matched.Age != 0:
		t.Errorf("a zero integer arrived as %v, want a pointer to 0", matched.Age)
	case matched.Active == nil || *matched.Active != false:
		t.Errorf("a false boolean arrived as %v, want a pointer to false", matched.Active)
	case matched.Created == nil:
		t.Error("a timestamp arrived as nil")
	case matched.Settings == nil:
		t.Error("an empty jsonb document arrived as nil")
	}
	// The unmatched row is NULL throughout.
	if missing.ID != nil || missing.Email != nil || missing.Age != nil ||
		missing.Active != nil || missing.Created != nil || missing.Settings != nil {
		t.Errorf("a parent with no child read %+v, want every field nil", missing)
	}
}

// Release-critical: every physically NOT NULL column of an absent right-hand
// source reads as NULL, across the types the schema provides.
func TestAudit_outerJoinNullCorpus(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (99, 'no match')`)

	child := gendemo.Users.As("child")
	type row struct {
		ID       *int64             // int8   NOT NULL
		Email    *string            // text   NOT NULL
		Age      *int32             // int4   NOT NULL
		Active   *bool              // bool   NOT NULL
		State    *gendemo.UserState // enum NOT NULL
		Tags     *[]string          // text[] NOT NULL
		Settings *map[string]any    // jsonb  NOT NULL
		Created  *time.Time         // timestamptz NOT NULL
	}
	shape := orm.Project8(
		orm.Opt(child.ID), orm.Opt(child.Email), orm.Opt(child.Age), orm.Opt(child.Active),
		orm.Opt(child.State), orm.Opt(child.Tags), orm.Opt(child.Settings), orm.Opt(child.CreatedAt),
		func(id *int64, email *string, age *int32, active *bool, state *gendemo.UserState,
			tags *[]string, settings *map[string]any, created *time.Time,
		) row {
			return row{ID: id, Email: email, Age: age, Active: active, State: state,
				Tags: tags, Settings: settings, Created: created}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)).
		Where(orm.Cond(gendemo.Categories.ID.Eq(int64(99)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	r := got[0]
	if r.ID != nil || r.Email != nil || r.Age != nil || r.Active != nil ||
		r.State != nil || r.Tags != nil || r.Settings != nil || r.Created != nil {
		t.Errorf("an absent right-hand source read %+v, want every value NULL", r)
	}

	// citext, through a second table, because users has none.
	member := gendemo.Members.As("m")
	slug := orm.Project1(orm.Opt(member.TeamSlug), func(s *string) *string { return s })
	rows, err := orm.Compose(db.Executor(), slug).
		From(gendemo.Categories.Source()).
		LeftJoin(member.Source(), orm.Cond(orm.Expr[orm.Composed]("FALSE"))).
		Where(orm.Cond(gendemo.Categories.ID.Eq(int64(99)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("citext: %v", err)
	}
	if len(rows) != 1 || rows[0] != nil {
		t.Errorf("a NOT NULL citext column of an absent source read %v", rows)
	}
}

// Nullability has to survive every value-producing path, not only a bare
// column selection.
func TestAudit_nullabilityPropagatesThroughEveryExpressionPath(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (99, 'no match')`)

	child := gendemo.Users.As("child")
	age := orm.Opt(child.Age)     // *int32, source-nullable
	email := orm.Opt(child.Email) // *string, source-nullable
	one := int32(1)

	build := func(t *testing.T, v orm.Selectable[orm.Composed, *int32]) *int32 {
		t.Helper()
		shape := orm.Project1(v, func(x *int32) *int32 { return x })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(99)))).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		return got
	}

	t.Run("arithmetic", func(t *testing.T) {
		for name, v := range map[string]orm.Selectable[orm.Composed, *int32]{
			"add": age.Add(&one),
			"sub": age.Sub(&one),
			"mul": age.Mul(&one),
			"div": age.Div(&one),
			"col": age.AddOf(age),
		} {
			if got := build(t, v); got != nil {
				t.Errorf("%s of an absent source read %d, want NULL", name, *got)
			}
		}
	})
	t.Run("cast", func(t *testing.T) {
		if got := build(t, orm.CastNull(age, orm.Integer)); got != nil {
			t.Errorf("a cast of an absent source read %d, want NULL", *got)
		}
	})
	t.Run("case", func(t *testing.T) {
		// A CASE whose branch value comes from the absent source is NULL for
		// that row, and its type has to allow it.
		c := orm.Case(orm.Cond(gendemo.Categories.ID.Gt(int64(0))), age).Else(age)
		if got := build(t, c); got != nil {
			t.Errorf("a CASE over an absent source read %d, want NULL", *got)
		}
	})
	t.Run("coalesce keeps a proven fallback non-null", func(t *testing.T) {
		fallback := orm.Coalesce(orm.Val(int32(-1)), age)
		shape := orm.Project1(fallback, func(x int32) int32 { return x })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(99)))).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		if got != -1 {
			t.Errorf("coalesce over an absent source read %d, want the fallback", got)
		}
	})
	t.Run("nullif", func(t *testing.T) {
		if got := build(t, orm.NullIf(age, orm.Val(int32(0)))); got != nil {
			t.Errorf("nullif over an absent source read %d, want NULL", *got)
		}
	})
	t.Run("aggregate", func(t *testing.T) {
		shape := orm.Project1(orm.OfNull(orm.Min(child.Age)), func(x *int32) *int32 { return x })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(99)))).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		if got != nil {
			t.Errorf("min over an absent source read %d, want NULL", *got)
		}
	})
	t.Run("window", func(t *testing.T) {
		lag := orm.Lag(email).Over(orm.Window().OrderBy(orm.Of(gendemo.Categories.ID).Asc()))
		shape := orm.Project1(lag, func(x *string) *string { return x })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(99)))).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		if got != nil {
			t.Errorf("lag over an absent source read %q, want NULL", *got)
		}
	})
}

// The whole stack at once: a window function over an outer-joined source,
// wrapped in a derived table, outer-joined again, and read.
func TestAudit_nullabilityThroughWindowDerivedAndOuterJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m11env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (99, 'no match')`)

	child := gendemo.Users.As("child")
	catID := orm.Named("cat", orm.Of(gendemo.Categories.ID))
	// lag over a source-nullable column: nullable twice over.
	prev := orm.Named("prev", orm.Lag(orm.Opt(child.Email)).
		Over(orm.Window().OrderBy(orm.Of(gendemo.Categories.ID).Asc())))

	inner := orm.Sub("ranked", orm.Rows(catID, prev).
		From(gendemo.Categories.Source()).
		LeftJoin(child.Source(), orm.Eq(child.ID, gendemo.Categories.ID)))

	shape := orm.Project2(
		orm.Of(gendemo.Categories.ID), orm.OptRef(inner, prev),
		func(id int64, prev *string) *string { return prev },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(inner, orm.Eq(orm.Ref(inner, catID), gendemo.Categories.ID)).
		OrderBy(orm.Of(gendemo.Categories.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no rows, so the composition proved nothing")
	}
	for _, v := range got {
		if v != nil && *v == "" {
			t.Errorf("a lag over an absent source read %q rather than NULL", *v)
		}
	}
}

// An INNER JOIN makes nothing nullable, and the audit has to confirm the fix
// for outer joins did not simply widen everything.
func TestAudit_innerJoinKeepsIntrinsicNullability(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m11env(t)

	// profiles.id is NOT NULL and stays so through an INNER JOIN; profiles.bio
	// is nullable and stays so.
	type row struct {
		ID  int64
		Bio *string
	}
	shape := orm.Project2(
		orm.Of(gendemo.Profiles.ID), orm.Of(gendemo.Profiles.Bio),
		func(id int64, bio *string) row { return row{ID: id, Bio: bio} },
	)
	if _, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Users.Source()).
		Join(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
		All(t.Context()); err != nil {
		t.Fatalf("an INNER JOIN widened a NOT NULL column: %v", err)
	}
}
