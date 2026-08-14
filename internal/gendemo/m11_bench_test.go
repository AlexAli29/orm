package gendemo_test

import (
	"fmt"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// Compiling composed statements.
//
// None of these assert a threshold. A benchmark that fails a build is a
// benchmark that gets deleted the first time a machine is busy; what these are
// for is the allocation counts, which are stable enough to notice a regression
// in and which say something real — compiling a statement should not allocate
// in proportion to anything but the statement.
//
// The last one is the shape worth watching. Twenty sources and fifty selected
// expressions is past any query somebody writes by hand, and it is where an
// accidental scan of every source for every expression would show up as
// quadratic rather than as slow.

func benchSQL(b *testing.B, build func() (string, []any, error)) {
	b.Helper()
	if _, _, err := build(); err != nil {
		b.Fatalf("the statement does not compile: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := build(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_derivedTable(b *testing.B) {
	stats := orm.Sub("s", postStats())
	shape := orm.Project2(orm.Ref(stats, statsUserID), orm.Ref(stats, statsCount),
		func(id *int64, n int64) int64 { return n })
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).From(stats).SQL()
	})
}

func BenchmarkCompile_correlatedExists(b *testing.B) {
	inner := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID))
	shape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			Where(orm.Exists[orm.Composed](inner)).
			SQL()
	})
}

func BenchmarkCompile_threeJoins(b *testing.B) {
	shape := orm.Project3(
		orm.Of(gendemo.Users.ID), orm.Opt(gendemo.Profiles.Bio), orm.Opt(gendemo.Posts.Title),
		func(id int64, bio, title *string) int64 { return id },
	)
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).
			From(gendemo.Users.Source()).
			LeftJoin(gendemo.Profiles.Source(), orm.Eq(gendemo.Profiles.UserID, gendemo.Users.ID)).
			LeftJoin(gendemo.Posts.Source(), orm.Eq(gendemo.Posts.AuthorID, gendemo.Users.ID)).
			LeftJoin(gendemo.Avatars.Source(),
				orm.Cond(orm.Expr[orm.Composed](`"avatars"."id" = "profiles"."avatar_id"`))).
			SQL()
	})
}

func BenchmarkCompile_multipleCTEs(b *testing.B) {
	id := orm.Named("id", orm.Of(gendemo.Users.ID))
	a := orm.CTE("a", orm.Rows(id).From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))))
	bb := orm.CTE("b", orm.Rows(orm.Named("id", orm.Ref(a, id))).From(a))
	c := orm.CTE("c", orm.Rows(orm.Named("id", orm.Ref(bb, id))).From(bb))
	shape := orm.Project1(orm.Ref(c, id), func(v int64) int64 { return v })
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).With(a, bb, c).From(c).SQL()
	})
}

func BenchmarkCompile_case(b *testing.B) {
	mood := orm.Case(orm.Cond(gendemo.Users.Active.Eq(true)), orm.Val("active")).
		When(orm.Cond(gendemo.Users.Age.Lt(int32(18))), orm.Val("minor")).
		When(orm.Cond(gendemo.Users.Age.Gt(int32(65))), orm.Val("senior")).
		Else(orm.Val("other"))
	shape := orm.Project1(mood, func(s string) string { return s })
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).From(gendemo.Users.Source()).SQL()
	})
}

func BenchmarkCompile_window(b *testing.B) {
	rn := orm.RowNumber().Over(orm.Window().
		PartitionBy(orm.Of(gendemo.Posts.AuthorID)).
		OrderBy(orm.Of(gendemo.Posts.CreatedAt).Desc()).
		Rows(orm.UnboundedPreceding(), orm.CurrentRow()))
	shape := orm.Project1(rn, func(n int64) int64 { return n })
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).From(gendemo.Posts.Source()).SQL()
	})
}

// The composed statement the integration suite runs: a CTE, a derived table,
// two outer joins, a correlated EXISTS, a CASE, an aggregate and a window.
func BenchmarkCompile_maximalComposition(b *testing.B) {
	cteID := orm.Named("id", orm.Of(gendemo.Users.ID))
	active := orm.CTE("active", orm.Rows(cteID).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))))
	stats := orm.Sub("s", postStats())
	recent := orm.Rows(orm.Named("x", orm.Val(1))).
		From(gendemo.Posts.Source()).
		Where(orm.Eq(gendemo.Posts.AuthorID, orm.Ref(active, cteID)))
	comments := orm.Of(orm.CountOf(gendemo.Comments.ID))

	shape := orm.Project4(
		orm.Ref(active, cteID),
		orm.Coalesce(orm.Val(int64(0)), orm.OptRef(stats, statsCount)),
		orm.Case(comments.Gt(int64(1)), orm.Val("busy")).Else(orm.Val("quiet")),
		orm.RowNumber().Over(orm.Window().OrderBy(orm.Ref(active, cteID).Asc())),
		func(id, n int64, mood string, rn int64) int64 { return id },
	)
	benchSQL(b, func() (string, []any, error) {
		return orm.Compose(nil, shape).
			With(active).
			From(active).
			LeftJoin(stats, orm.Eq(orm.Ref(stats, statsUserID), orm.Ref(active, cteID))).
			LeftJoin(gendemo.Comments.Source(),
				orm.Cond(orm.Expr[orm.Composed](`"comments"."author_id" = "active"."id"`))).
			Where(orm.Exists[orm.Composed](recent)).
			GroupBy(orm.Ref(active, cteID), orm.OptRef(stats, statsCount)).
			OrderBy(orm.Ref(active, cteID).Asc()).
			SQL()
	})
}

// Twenty sources and fifty expressions, which is past what anybody writes and
// exactly where a scan of every source for every expression would show up.
func BenchmarkCompile_twentySourcesFiftyExpressions(b *testing.B) {
	const sources = 20
	aliases := make([]*orm.Source, sources)
	for i := range aliases {
		aliases[i] = gendemo.Users.As(fmt.Sprintf("u%02d", i)).Source()
	}

	outs := make([]orm.Output, 0, 50)
	for i := range 50 {
		src := aliases[i%sources]
		name := fmt.Sprintf("c%02d", i)
		outs = append(outs, orm.Named(name, orm.Ref(src, orm.Named("id", orm.Of(gendemo.Users.ID)))))
	}

	build := func() (string, []any, error) {
		q := orm.Rows(outs...).From(aliases[0])
		for i := 1; i < sources; i++ {
			q = q.Join(aliases[i], orm.Cond(orm.Expr[orm.Composed](
				fmt.Sprintf(`%q."id" = %q."id"`, aliases[i].Ref(), aliases[0].Ref()))))
		}
		// Three CTEs on top, so that the WITH clause is walked too.
		cteID := orm.Named("id", orm.Of(gendemo.Users.ID))
		with := make([]*orm.Source, 0, 3)
		for i := range 3 {
			with = append(with, orm.CTE(fmt.Sprintf("w%d", i),
				orm.Rows(cteID).From(gendemo.Users.Source())))
		}
		return q.With(with...).SQL()
	}
	benchSQL(b, build)
}
