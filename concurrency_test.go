package orm_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
)

// Generated descriptors are package-level values shared by every query in a
// program. A builder is documented as mutable and single-use; a descriptor is
// not, and the difference is the whole reason the builder methods on Rel and
// Source return copies. These tests exist because that property is invisible in
// ordinary use and catastrophic when it breaks: a filter leaking from one
// request into another is a data leak, not a bug report.
//
// Run under -race, which is where they earn their keep.

func TestConcurrent_queriesFromOneDescriptor(t *testing.T) {
	const goroutines = 32

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine builds a different query from the same globals.
			q := repo(nil).Query().
				Where(Users.Active.Eq(i%2 == 0), Users.Age.Gt(int32(i))).
				OrderBy(Users.CreatedAt.Desc()).
				Limit(i + 1)
			if _, _, err := q.SQL(); err != nil {
				t.Errorf("SQL: %v", err)
			}
		}()
	}
	wg.Wait()
}

// Configuring a relation must not touch the descriptor it was configured from,
// however many goroutines are configuring it at once.
func TestConcurrent_relationConfiguration(t *testing.T) {
	const goroutines = 32

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := usersPosts.
				Where(Posts.Published.Eq(i%2 == 0)).
				OrderBy(Posts.CreatedAt.Desc()).
				Limit(i + 1)
			if _, _, err := repo(nil).Query().With(rel).SQL(); err != nil {
				t.Errorf("SQL: %v", err)
			}
			if _, _, err := repo(nil).Query().Where(usersPosts.Any(Posts.Published.Eq(true))).SQL(); err != nil {
				t.Errorf("Any: %v", err)
			}
		}()
	}
	wg.Wait()

	// The shared descriptor is exactly as it was.
	sql, args, err := repo(nil).Query().With(usersPosts).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 0 || strings.Contains(sql, "published") {
		t.Errorf("the shared relation gained a condition: %s %v", sql, args)
	}
}

// Aliasing a table is the other operation that looks like mutation and must not
// be. Two goroutines aliasing one descriptor differently must not see each
// other's alias.
func TestConcurrent_aliasing(t *testing.T) {
	const goroutines = 16

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			alias := usersSrc.As("u")
			if alias.Ref() != "u" {
				t.Errorf("alias = %q, want u", alias.Ref())
			}
			if usersSrc.Ref() != "users" {
				t.Errorf("the shared source became %q", usersSrc.Ref())
			}
			_ = i
		}()
	}
	wg.Wait()
	if usersSrc.AliasName() != "" {
		t.Errorf("the shared source gained alias %q", usersSrc.AliasName())
	}
}

// A clone is a real copy at every level, so two goroutines branching from one
// base cannot reach each other's conditions.
func TestConcurrent_clonedBuilders(t *testing.T) {
	base := repo(nil).Query().Where(Users.Active.Eq(true)).With(usersPosts)

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := base.Clone().Where(Users.Age.Gt(int32(i)))
			if _, _, err := q.SQL(); err != nil {
				t.Errorf("SQL: %v", err)
			}
		}()
	}
	wg.Wait()

	sql, args, err := base.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if len(args) != 1 {
		t.Errorf("the base accumulated %d conditions, want its own one: %s %v", len(args), sql, args)
	}
}

// Reading through repositories concurrently is the ordinary case, and the
// metadata they share is written once by the generator and never after.
func TestConcurrent_reads(t *testing.T) {
	ex := &countingExecutor{}
	r := orm.NewRepo(ex, &userMeta)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Query().Where(Users.Active.Eq(true)).All(context.Background()); err != nil {
				t.Errorf("All: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := ex.count(); got != 32 {
		t.Errorf("ran %d statements, want one per goroutine", got)
	}
}

// countingExecutor answers every query with no rows and counts the calls.
type countingExecutor struct {
	mu sync.Mutex
	n  int
}

func (e *countingExecutor) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	e.mu.Lock()
	e.n++
	e.mu.Unlock()
	return &stubRows{}, nil
}

func (e *countingExecutor) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.n
}

// M10.1: a result shape is a value meant to be declared once and used by many
// queries, including at the same time. Nothing about scanning may live on it.
func TestConcurrent_projections(t *testing.T) {
	shape := orm.Project3(
		Users.ID, Users.Email, Users.Nickname,
		func(id int64, email string, nick *string) string { return email },
	)
	if shape.Columns() != 3 {
		t.Fatalf("Columns = %d", shape.Columns())
	}

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine builds its own query over the one shared shape,
			// aliases a column, and renders the statement.
			q := orm.Select(repo(nil), shape).
				Where(Users.Active.Eq(true)).
				OrderBy(Users.CreatedAt.Desc()).
				Distinct().
				Limit(5)
			if _, _, err := q.Clone().SQL(); err != nil {
				t.Errorf("SQL: %v", err)
			}
			_ = orm.Project1(Users.Email.As("addr"), func(s string) string { return s })
		}()
	}
	wg.Wait()

	// The shared shape is unchanged, and still renders without the alias one of
	// the goroutines applied to a copy.
	sql, _, err := orm.Select(repo(nil), shape).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if strings.Contains(sql, "addr") {
		t.Errorf("a per-query alias reached the shared descriptor: %s", sql)
	}
}
