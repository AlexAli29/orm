package orm_test

import (
	"context"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
)

// Benchmarks for the work this package does between the caller and pgx.
//
// They exist to notice a regression, not to produce a number for a README. What
// they measure is deliberately the part with no database in it: once a
// statement is on the wire, the round trip is orders of magnitude larger than
// anything here and would drown out exactly the changes these are meant to
// catch. Nothing in CI fails on a threshold.

func BenchmarkPredicate(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_ = orm.And(
			Users.Active.Eq(true),
			Users.Age.Gte(21),
			Users.Email.ILike("%@example.com"),
		)
	}
}

func BenchmarkCompile_small(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		q := repo(nil).Query().Where(Users.Active.Eq(true)).Limit(50)
		if _, _, err := q.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

// The shape dynamic filtering actually produces: a slice of predicates built
// from whatever the request asked for.
func BenchmarkCompile_tenPredicates(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		ps := make([]orm.Predicate[User], 0, 10)
		for i := range 5 {
			ps = append(ps, Users.Age.Gt(int32(i)), Users.Email.ILike("%example.com"))
		}
		q := repo(nil).Query().
			Where(orm.And(ps...)).
			OrderBy(Users.CreatedAt.Desc()).
			Limit(50).
			Offset(100)
		if _, _, err := q.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_semiJoin(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		q := repo(nil).Query().Where(usersPosts.Any(Posts.Published.Eq(true)))
		if _, _, err := q.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClone(b *testing.B) {
	base := repo(nil).Query().
		Where(Users.Active.Eq(true), Users.Age.Gte(21)).
		OrderBy(Users.CreatedAt.Desc()).
		With(usersPosts)

	b.ReportAllocs()
	for b.Loop() {
		_ = base.Clone()
	}
}

// Relation planning is what a nested With costs before any statement runs.
func BenchmarkRelationPlan(b *testing.B) {
	rel := usersPosts.
		Where(Posts.Published.Eq(true)).
		OrderBy(Posts.CreatedAt.Desc()).
		Limit(5)

	b.ReportAllocs()
	for b.Loop() {
		q := repo(nil).Query().With(rel)
		if _, _, err := q.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

// Scanning is the hot path of any read, and it is the one place where a
// regression to reflection would be invisible in tests and obvious here.
func BenchmarkScan(b *testing.B) {
	rows := make([][]any, 1000)
	for i := range rows {
		rows[i] = []any{
			int64(i), "user@example.com", int32(30), true,
			nil, nil, nil, time.Time{},
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		// A fresh executor per iteration, because a result set is consumed as
		// it is read and the point is to measure reading it.
		r := orm.NewRepo(&benchExecutor{rows: rows}, &userMeta)
		out, err := r.Query().All(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != len(rows) {
			b.Fatalf("scanned %d rows", len(out))
		}
	}
}

// Attaching related rows by ordinal is the other per-row cost, and the one that
// would grow quietly if anything reintroduced a Go-side key comparison.
func BenchmarkRelationAttach(b *testing.B) {
	const parents = 200
	rows := make([][]any, 0, parents*5)
	for p := range parents {
		for range 5 {
			rows = append(rows, []any{int64(p + 1), int64(1), false, time.Time{}})
		}
	}
	root := make([][]any, parents)
	for i := range root {
		root[i] = []any{int64(i + 1), "user@example.com", int32(30), true, nil, nil, nil, time.Time{}}
	}

	b.ReportAllocs()
	for b.Loop() {
		ex := &benchExecutor{rows: root, then: rows}
		if _, err := orm.NewRepo(ex, &userMeta).Query().With(usersPosts).All(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

// benchExecutor answers the first query with rows and the next with then.
type benchExecutor struct {
	rows [][]any
	then [][]any
	n    int
}

func (e *benchExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	e.n++
	if e.n == 1 {
		return &stubRows{rows: e.rows}, nil
	}
	return &stubRows{rows: e.then}, nil
}

// M10.1: a projection reads fewer columns and builds a smaller value than the
// entity path, and the point of these is that it actually costs less rather
// than merely selecting less.

type benchSummary struct {
	ID    int64
	Email string
}

var benchTwo = orm.Project2(Users.ID, Users.Email,
	func(id int64, email string) benchSummary { return benchSummary{ID: id, Email: email} })

func BenchmarkCompile_projection(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		q := orm.Select(repo(nil), benchTwo).Where(Users.Active.Eq(true)).Limit(50)
		if _, _, err := q.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

// Two columns out of the eight the entity has.
func BenchmarkScan_projectionTwoColumns(b *testing.B) {
	rows := make([][]any, 1000)
	for i := range rows {
		rows[i] = []any{int64(i), "user@example.com"}
	}
	b.ReportAllocs()
	for b.Loop() {
		r := orm.NewRepo(&benchExecutor{rows: rows}, &userMeta)
		out, err := orm.Select(r, benchTwo).All(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != len(rows) {
			b.Fatalf("scanned %d rows", len(out))
		}
	}
}

// The whole entity width, so the projection scanner is measured against the
// entity scanner over the same number of values.
func BenchmarkScan_projectionEightColumns(b *testing.B) {
	type wide struct {
		ID    int64
		Email string
	}
	shape := orm.Project8(
		Users.ID, Users.Email, Users.Age, Users.Active,
		Users.Nickname, Users.ManagerID, Users.DeletedAt, Users.CreatedAt,
		func(id int64, email string, age int32, active bool,
			nick *string, mgr *int64, deleted *time.Time, created time.Time,
		) wide {
			return wide{ID: id, Email: email}
		},
	)
	rows := make([][]any, 1000)
	for i := range rows {
		rows[i] = []any{
			int64(i), "user@example.com", int32(30), true,
			nil, nil, nil, time.Time{},
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		r := orm.NewRepo(&benchExecutor{rows: rows}, &userMeta)
		out, err := orm.Select(r, shape).All(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != len(rows) {
			b.Fatalf("scanned %d rows", len(out))
		}
	}
}

// M10.3: the write side of the expression system.

func BenchmarkCompile_writeExpression(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		u := repo(nil).Update().
			Set(Users.Age.SetExpr(Users.Age.Add(1))).
			Where(Users.ID.Eq(1))
		if _, _, err := u.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCompile_updateReturning(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		t := orm.UpdateReturning(
			repo(nil).Update().Set(Users.Age.SetExpr(Users.Age.Add(1))).Where(Users.ID.Eq(1)),
			benchTwo,
		)
		if _, _, err := t.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScan_returning(b *testing.B) {
	rows := make([][]any, 500)
	for i := range rows {
		rows[i] = []any{int64(i), "user@example.com"}
	}
	b.ReportAllocs()
	for b.Loop() {
		r := orm.NewRepo(&benchExecutor{rows: rows}, &userMeta)
		out, err := orm.UpdateReturning(
			r.Update().Set(Users.Age.Set(1)).All(), benchTwo).All(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		if len(out) != len(rows) {
			b.Fatalf("scanned %d rows", len(out))
		}
	}
}

func BenchmarkCompile_aggregate(b *testing.B) {
	shape := orm.Project2(
		Users.ManagerID, orm.Count[User](),
		func(mgr *int64, n int64) int64 { return n },
	)
	b.ReportAllocs()
	for b.Loop() {
		q := orm.Select(repo(nil), shape).
			Where(Users.Age.Gt(18)).
			GroupBy(Users.ManagerID).
			Having(orm.Count[User]().Gt(1))
		if _, _, err := q.SQL(); err != nil {
			b.Fatal(err)
		}
	}
}
