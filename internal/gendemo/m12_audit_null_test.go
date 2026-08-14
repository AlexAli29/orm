package gendemo_test

import (
	"net/netip"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// The M12 nullability audit.
//
// Every M12 expression that claims a non-nullable result is attacked with an
// input PostgreSQL can make NULL: a nullable column, an outer join that matched
// nothing, a document that is SQL NULL. A claim that survives all of those is a
// claim; one that does not is a scan error waiting for the first row that
// exercises it.

// Release-critical: ts_rank over a vector that can be NULL is NULL, so the
// result type has to allow it.
//
// The generated search column is nullable — PostgreSQL's generated columns are
// unless declared otherwise — and ranking a row whose vector is NULL returns
// NULL rather than zero.
func TestAudit_tsRankOverANullableVector(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	// A row whose vector is genuinely NULL: the generated expression yields
	// NULL when either input is.
	m11exec(t, conn, `INSERT INTO articles (title, body) VALUES ('kept', 'text')`)
	m11exec(t, conn, `ALTER TABLE articles DROP COLUMN search`)
	m11exec(t, conn, `ALTER TABLE articles ADD COLUMN search tsvector`)
	m11exec(t, conn, `UPDATE articles SET search = NULL`)

	query := orm.PlainToTSQuery(orm.English, "text")
	rank := orm.TSRankNull(gendemo.Articles.Search, query)

	shape := orm.Project1(rank, func(r *float32) *float32 { return r })
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Articles.Source()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0] != nil {
		t.Errorf("ts_rank of a NULL vector read %v, want NULL", *got[0])
	}
}

// The same claim through an outer join: a physically NOT NULL vector read from
// a source that matched nothing is NULL, and so is everything computed from it.
func TestAudit_ftsThroughAnOuterJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (77, 'no match')`)
	m11exec(t, conn, `INSERT INTO articles (title, body) VALUES ('a', 'b')`)

	query := orm.PlainToTSQuery(orm.English, "b")

	t.Run("the vector itself", func(t *testing.T) {
		shape := orm.Project1(orm.Opt(gendemo.Articles.Search),
			func(v *orm.TSVector) *orm.TSVector { return v })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(gendemo.Articles.Source(), orm.Eq(gendemo.Articles.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 1 || got[0] != nil {
			t.Errorf("a vector read through a LEFT JOIN with no match = %v", got)
		}
	})

	t.Run("a rank computed from it", func(t *testing.T) {
		shape := orm.Project1(orm.TSRankNull(orm.Opt(gendemo.Articles.Search), query),
			func(r *float32) *float32 { return r })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(gendemo.Articles.Source(), orm.Eq(gendemo.Articles.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 1 || got[0] != nil {
			t.Errorf("a rank over an absent source = %v", got)
		}
	})

	t.Run("a match test is a three-valued predicate", func(t *testing.T) {
		// @@ over a NULL vector is UNKNOWN, so the row is not kept — which is
		// what the handwritten statement does too.
		shape := orm.Project1(orm.Of(gendemo.Categories.ID), func(v int64) int64 { return v })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(gendemo.Articles.Source(), orm.Eq(gendemo.Articles.ID, gendemo.Categories.ID)).
			Where(orm.Matches(orm.Opt(gendemo.Articles.Search), query)).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if got == nil {
			got = []int64{}
		}
		assertIDs(t, conn, `
			SELECT c.id FROM categories c
			LEFT JOIN articles a ON a.id = c.id
			WHERE a.search @@ plainto_tsquery('english', 'b')
			ORDER BY c.id`, got)
	})
}

// A NOT NULL network column read through an outer join that matched nothing is
// NULL, and so is every function computed from it.
func TestAudit_networkThroughAnOuterJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (77, 'no match')`)

	t.Run("host of an absent address", func(t *testing.T) {
		shape := orm.Project1(orm.HostNull(orm.Opt(gendemo.Networks.Subnet)),
			func(s *string) *string { return s })
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(gendemo.Networks.Source(), orm.Eq(gendemo.Networks.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 1 || got[0] != nil {
			t.Errorf("host of an absent source = %v", got)
		}
	})

	t.Run("masklen and network of an absent address", func(t *testing.T) {
		shape := orm.Project2(
			orm.MaskLenNull(orm.Opt(gendemo.Networks.Subnet)),
			orm.NetworkNull(orm.Opt(gendemo.Networks.Host)),
			func(m *int32, n *netip.Prefix) bool { return m == nil && n == nil },
		)
		got, err := orm.Compose(db.Executor(), shape).
			From(gendemo.Categories.Source()).
			LeftJoin(gendemo.Networks.Source(), orm.Eq(gendemo.Networks.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
			All(t.Context())
		if err != nil {
			t.Fatalf("All: %v", err)
		}
		if len(got) != 1 || !got[0] {
			t.Error("masklen or network of an absent source was not NULL")
		}
	})
}

// Every JSON reader over an outer-joined document keeps its nullability.
func TestAudit_jsonThroughAnOuterJoin(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (77, 'no match')`)

	// settings is NOT NULL in the schema; through a LEFT JOIN it is not.
	doc := orm.Opt(gendemo.Users.Settings)
	run := func(t *testing.T, v orm.Selectable[orm.Composed, *string]) *string {
		t.Helper()
		got, err := orm.Compose(db.Executor(), orm.Project1(v, func(s *string) *string { return s })).
			From(gendemo.Categories.Source()).
			LeftJoin(gendemo.Users.Source(), orm.Eq(gendemo.Users.ID, gendemo.Categories.ID)).
			Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
			One(t.Context())
		if err != nil {
			t.Fatalf("One: %v", err)
		}
		return got
	}

	if got := run(t, orm.JSONText(doc, "x")); got != nil {
		t.Errorf("->> over an absent document = %q", *got)
	}
	if got := run(t, orm.JSONPathText(doc, "a", "b")); got != nil {
		t.Errorf("#>> over an absent document = %q", *got)
	}
	if got := run(t, orm.JSONTypeOf(doc)); got != nil {
		t.Errorf("jsonb_typeof over an absent document = %q", *got)
	}

	// And the jsonb-returning readers too.
	shape := orm.Project2(
		orm.JSONGet(doc, "x"), orm.JSONPathGet(doc, "a"),
		func(a, b *map[string]any) bool { return a == nil && b == nil },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(gendemo.Users.Source(), orm.Eq(gendemo.Users.ID, gendemo.Categories.ID)).
		Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
		One(t.Context())
	if err != nil {
		t.Fatalf("One: %v", err)
	}
	if !got {
		t.Error("-> or #> over an absent document was not NULL")
	}
}

// The maximal chain: an outer join, a JSON path, a cast, a CASE, a derived
// table, and another outer join. Nothing along the way may claim the value
// cannot be NULL.
func TestAudit_m12MaximalNullabilityGraph(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)
	m11exec(t, conn, `INSERT INTO categories (id, name) VALUES (77, 'no match')`)

	doc := orm.Opt(gendemo.Users.Settings)
	age := orm.CastNull(orm.JSONPathText(doc, "profile", "age"), orm.Integer)
	branch := orm.Case(orm.Cond(gendemo.Categories.ID.Gt(int64(0))), age).Else(age)

	catID := orm.Named("cat", orm.Of(gendemo.Categories.ID))
	value := orm.Named("v", branch)
	inner := orm.Sub("inner", orm.Rows(catID, value).
		From(gendemo.Categories.Source()).
		LeftJoin(gendemo.Users.Source(), orm.Eq(gendemo.Users.ID, gendemo.Categories.ID)))

	shape := orm.Project2(
		orm.Of(gendemo.Categories.ID), orm.OptRef(inner, value),
		func(id int64, v *int32) *int32 { return v },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gendemo.Categories.Source()).
		LeftJoin(inner, orm.Eq(orm.Ref(inner, catID), gendemo.Categories.ID)).
		Where(orm.Cond(gendemo.Categories.ID.Eq(int64(77)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || got[0] != nil {
		t.Errorf("the chain read %v, want NULL", got)
	}
}

// The new operator nodes must carry every child's source dependency, or an
// expression naming a source the statement does not introduce would compile.
func TestAudit_operatorNodesKeepEverySourceDependency(t *testing.T) {
	stranger := gendemo.Networks.As("stranger")
	other := gendemo.Users.As("other")
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })

	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
	}{
		{"infix right side out of scope",
			orm.ContainedBy(gendemo.Networks.Host, stranger.Subnet)},
		{"infix left side out of scope",
			orm.ContainedBy(stranger.Host, gendemo.Networks.Subnet)},
		{"json operator over an out-of-scope document",
			orm.JSONHasKey(orm.Opt(other.Settings), "x")},
		{"json containment right side out of scope",
			orm.JSONContains(orm.Opt(gendemo.Users.Settings), orm.Opt(other.Settings))},
		{"fts match over an out-of-scope vector",
			orm.Matches(orm.Opt(gendemo.Articles.Search),
				orm.PlainToTSQuery(orm.English, "x"))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := orm.Compose(nil, shape).
				From(gendemo.Users.Source()).
				Join(gendemo.Networks.Source(), orm.Cond(orm.Expr[orm.Composed]("TRUE"))).
				Where(tt.pred).
				SQL()
			if err == nil {
				t.Fatal("an expression naming a source the statement does not introduce compiled")
			}
		})
	}

	// The prefix node too: a negated query built over an out-of-scope source.
	t.Run("prefix operand out of scope", func(t *testing.T) {
		vector := orm.Concat2TSVector(
			orm.ToTSVector(orm.English, gendemo.Articles.Title),
			orm.ToTSVector(orm.English, gendemo.Articles.Body),
		)
		_, _, err := orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			Where(orm.Matches(vector, orm.NotTSQuery(orm.PlainToTSQuery(orm.English, "x")))).
			SQL()
		if err == nil {
			t.Fatal("a vector over an unattached source compiled")
		}
	})
}

// Nested composition, not just a direct column: the dependency has to survive
// being wrapped several times.
func TestAudit_dependenciesSurviveNesting(t *testing.T) {
	stranger := gendemo.Users.As("stranger")
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(v int64) int64 { return v })

	// A JSON path over an out-of-scope document, cast, wrapped in a CASE,
	// compared — four layers away from the column.
	deep := orm.Case(
		orm.Cond(gendemo.Users.Active.Eq(true)),
		orm.CastNull(orm.JSONPathText(orm.Opt(stranger.Settings), "a"), orm.Integer),
	).End()

	_, _, err := orm.Compose(nil, orm.Project2(orm.Of(gendemo.Users.ID), deep,
		func(id int64, v *int32) int64 { return id })).
		From(gendemo.Users.Source()).
		SQL()
	if err == nil {
		t.Fatal("a four-layer expression over an out-of-scope source compiled")
	}
	_ = shape
}
