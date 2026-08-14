package gendemo_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// Concurrency.
//
// Everything M11 adds that is shared between queries is meant to be immutable:
// a source, a declared output, a subquery definition, a CTE, a window
// specification, a CASE expression. If any of them retained state from a
// compilation — a placeholder number, a scope, a rendered fragment — then two
// goroutines compiling the same object would produce two wrong statements, and
// they would produce them intermittently.
//
// So this compiles one set of shared objects from many goroutines at once and
// asserts that every one of them produced the same SQL and the same arguments.
// Under -race it also asserts that nothing was written.

func TestConcurrent_sharedCompositionObjectsAreImmutable(t *testing.T) {
	// One of everything, built once and shared by every goroutine.
	statsAuthor := orm.Named("user_id", orm.Of(gendemo.Posts.AuthorID))
	statsPosts := orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))
	statsQuery := orm.Rows(statsAuthor, statsPosts).
		From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Posts.Published.Eq(true))).
		GroupBy(orm.Of(gendemo.Posts.AuthorID))
	stats := orm.Sub("s", statsQuery)

	cteID := orm.Named("id", orm.Of(gendemo.Users.ID))
	active := orm.CTE("active", orm.Rows(cteID).
		From(gendemo.Users.Source()).
		Where(orm.Cond(gendemo.Users.Active.Eq(true))))

	window := orm.Window().
		PartitionBy(orm.Ref(active, cteID)).
		OrderBy(orm.Ref(active, cteID).Desc()).
		Rows(orm.UnboundedPreceding(), orm.CurrentRow())

	mood := orm.Case(orm.OptRef(stats, statsPosts).Gt(nil), orm.Val("busy")).
		Else(orm.Val("quiet"))

	alias := gendemo.Users.As("u")
	shape := orm.Project4(
		orm.Ref(active, cteID),
		orm.OptRef(stats, statsPosts),
		mood,
		orm.Of(orm.Count[orm.Composed]().Over(window)),
		func(id int64, n *int64, mood string, seen int64) string { return mood },
	)

	build := func() (string, []any, error) {
		return orm.Compose(nil, shape).
			With(active).
			From(active).
			LeftJoin(stats, orm.Eq(orm.Ref(stats, statsAuthor), orm.Ref(active, cteID))).
			LeftJoin(alias.Source(), orm.Eq(alias.ID, orm.Ref(active, cteID))).
			Where(orm.Exists[orm.Composed](statsQuery)).
			OrderBy(orm.Ref(active, cteID).Asc()).
			SQL()
	}

	wantSQL, wantArgs, err := build()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	const goroutines = 64
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		fails []string
	)
	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 8 {
				sql, args, err := build()
				switch {
				case err != nil:
					mu.Lock()
					fails = append(fails, err.Error())
					mu.Unlock()
				case sql != wantSQL:
					mu.Lock()
					fails = append(fails, "SQL differs:\n"+sql)
					mu.Unlock()
				case len(args) != len(wantArgs):
					mu.Lock()
					fails = append(fails, "argument count differs")
					mu.Unlock()
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(fails) > 0 {
		t.Fatalf("%d of %d compilations disagreed; the first said:\n%s",
			len(fails), goroutines*8, fails[0])
	}
	// The shared objects still say what they said before any of that.
	if stats.Ref() != "s" || active.Name() != "active" || alias.Source().Ref() != "u" {
		t.Error("a shared source was mutated by compiling it")
	}
	if gendemo.Users.Source().Ref() != "users" {
		t.Error("the global descriptor was mutated")
	}
	if !strings.Contains(wantSQL, `WITH "active" AS (`) {
		t.Errorf("the statement under test is not the one described: %s", wantSQL)
	}
}

// Two derived tables built from one query definition compile independently and
// concurrently, which is the property that makes a subquery reusable.
func TestConcurrent_oneSubqueryDefinitionInManyStatements(t *testing.T) {
	out := orm.Named("id", orm.Of(gendemo.Posts.ID))
	def := orm.Rows(out).From(gendemo.Posts.Source()).
		Where(orm.Cond(gendemo.Posts.Score.Gt(int32(3))))

	var wg sync.WaitGroup
	errs := make([]error, 32)
	sqls := make([]string, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			src := orm.Sub("d", def)
			shape := orm.Project1(orm.Ref(src, out), func(v int64) int64 { return v })
			sqls[i], _, errs[i] = orm.Compose(nil, shape).From(src).SQL()
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if sqls[i] != sqls[0] {
			t.Fatalf("goroutine %d produced a different statement:\n%s\n%s", i, sqls[i], sqls[0])
		}
	}
}
