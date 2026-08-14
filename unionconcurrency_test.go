package orm_test

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
)

// Sharing, and cleaning up.
//
// A Projection is documented as immutable and safe to share, and the output
// declarations a union is ordered and read by are values too. A builder is not
// shared — that is documented as well — so what is under test is that the values
// a builder is built *from* survive being used by many at once, and that every
// terminal closes the rows it opened whatever went wrong.
//
// The race detector needs cgo, which is not always available; this asserts the
// results rather than the memory model, so it is worth having either way.

func TestUnionConcurrency_sharedDeclarationsBuildTheSameStatement(t *testing.T) {
	shape := orm.Project2(
		orm.Of(Users.ID).As("thing_id"), orm.Of(Users.Email).As("label"),
		func(id int64, s string) sourced { return sourced{id, s} },
	)
	outID := orm.Named("thing_id", orm.Of(Users.ID))

	// One statement, built by many goroutines from one Projection and one
	// declaration. Every result has to be identical.
	const n = 64
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
		errs []error
	)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			branch := func() *orm.ComposedQuery[sourced] {
				return orm.Compose(nil, shape).From(usersSrc)
			}
			u := orm.UnionAll[sourced](branch(), branch()).OrderBy(outID.Desc()).Limit(5)
			sql, _, err := u.SQL()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seen[sql]++
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("%d of %d builds failed: %v", len(errs), n, errs[0])
	}
	if len(seen) != 1 {
		t.Fatalf("%d goroutines produced %d different statements", n, len(seen))
	}
	for sql, count := range seen {
		if count != n {
			t.Errorf("one statement was produced %d times, want %d", count, n)
		}
		if !strings.Contains(sql, `ORDER BY "thing_id" DESC LIMIT 5`) {
			t.Errorf("the shared declarations did not reach the statement:\n%s", sql)
		}
	}

	// The declaration is unchanged by being used, so it still names what it did.
	if outID.Name() != "thing_id" {
		t.Errorf("the shared declaration is now named %q", outID.Name())
	}
	if shape.Columns() != 2 {
		t.Errorf("the shared projection now describes %d columns", shape.Columns())
	}
}

// Every terminal closes the rows it opened, on the paths that succeed and on the
// paths that do not. A statement that fails before the query is sent has nothing
// to close, and must not pretend otherwise.
func TestUnionConcurrency_everyTerminalClosesItsRows(t *testing.T) {
	branch := func() *orm.ComposedQuery[int64] {
		return orm.Compose(nil, orm.Project1(orm.Of(Users.ID).As("id"),
			func(v int64) int64 { return v })).From(usersSrc)
	}
	rows := [][]any{{int64(1)}, {int64(2)}}

	cases := []struct {
		name  string
		ex    func(closed *int) stubExecutor
		run   func(q *orm.UnionQuery[int64]) error
		opens bool
	}{
		{"All over rows", func(c *int) stubExecutor { return stubExecutor{rows: rows, closed: c} },
			func(q *orm.UnionQuery[int64]) error { _, err := q.All(t.Context()); return err }, true},
		{"All when a row fails to scan", func(c *int) stubExecutor {
			return stubExecutor{rows: rows, closed: c, scanErr: errors.New("bad row")}
		}, func(q *orm.UnionQuery[int64]) error { _, err := q.All(t.Context()); return err }, true},
		{"All when the stream fails", func(c *int) stubExecutor {
			return stubExecutor{rows: rows, closed: c, rowsErr: errors.New("broken stream")}
		}, func(q *orm.UnionQuery[int64]) error { _, err := q.All(t.Context()); return err }, true},
		{"One over rows", func(c *int) stubExecutor { return stubExecutor{rows: rows, closed: c} },
			func(q *orm.UnionQuery[int64]) error { _, err := q.One(t.Context()); return err }, true},
		{"One over no rows", func(c *int) stubExecutor { return stubExecutor{closed: c} },
			func(q *orm.UnionQuery[int64]) error { _, err := q.One(t.Context()); return err }, true},
		{"Rows read to the end", func(c *int) stubExecutor { return stubExecutor{rows: rows, closed: c} },
			func(q *orm.UnionQuery[int64]) error {
				for _, err := range q.Rows(t.Context()) {
					if err != nil {
						return err
					}
				}
				return nil
			}, true},
		{"Rows abandoned after one", func(c *int) stubExecutor { return stubExecutor{rows: rows, closed: c} },
			func(q *orm.UnionQuery[int64]) error {
				for range q.Rows(t.Context()) {
					break
				}
				return nil
			}, true},
		{"All when the query never runs", func(c *int) stubExecutor {
			return stubExecutor{closed: c, queryErr: errors.New("refused")}
		}, func(q *orm.UnionQuery[int64]) error { _, err := q.All(t.Context()); return err }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			closed := 0
			q := orm.UnionAll[int64](branch(), branch()).Using(c.ex(&closed))
			_ = c.run(q)
			want := 1
			if !c.opens {
				want = 0
			}
			if closed != want {
				t.Errorf("rows closed %d times, want %d", closed, want)
			}
		})
	}
}
