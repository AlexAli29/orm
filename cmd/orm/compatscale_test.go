package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// M16.5 compatibility: three project sizes, to catch the failures that only
// appear with volume.
//
// This is not a benchmark. What it looks for is the small set of things that are
// invisible at five relations and fatal at a thousand: an ordering pass that is
// quadratic in the dependency graph, a recursive walk that runs out of stack, a
// map ranged over somewhere so that artifacts differ between runs, and anything
// that silently processes a prefix of its input.
//
// So the assertions are correctness and determinism, and the timing is recorded
// rather than asserted — a threshold here would fail on a loaded machine and
// teach everyone to rerun it.

// scaleProject renders a deterministic project of n sources: a mixture of
// tables, views over them and materialized views with indexes, wired into
// dependency chains so the ordering pass has real work to do.
func scaleProject(n int) string {
	var b strings.Builder
	b.WriteString("package domain\n")
	for i := range n {
		switch {
		case i%10 < 6 || i == 0:
			// A table. Every source that is not a table depends on one, so the
			// first source is always a table whatever the ratio says.
			fmt.Fprintf(&b, `
//orm:table public.t%d
type Table%d struct {
	ID    int64 `+"`orm:\"pk,identity\"`"+`
	Name  string
	Score int64
}
`, i, i)
		case i%10 < 8:
			// A view over the nearest table below it.
			base := i - (i % 10)
			fmt.Fprintf(&b, `
//orm:view public.v%d
//orm:definition `+"`SELECT id, name, score FROM t%d`"+`
//orm:depends-on public.t%d
type View%d struct {
	ID    int64
	Name  string
	Score int64
}
`, i, base, base, i)
		default:
			// A materialized view over the view above it, with indexes: one
			// qualifying, one not.
			view := i - (i % 10) + 6
			fmt.Fprintf(&b, `
//orm:materialized-view public.m%d
//orm:definition `+"`SELECT id, name, score FROM v%d`"+`
//orm:depends-on public.v%d
//orm:index m%d_id_key (ID) unique
//orm:index m%d_score_idx (Score desc)
type Mat%d struct {
	ID    int64
	Name  string
	Score int64
}
`, i, view, view, i, i, i)
		}
	}
	return b.String()
}

// countSources reports how many of each kind a rendered project declares, so a
// size that silently rendered fewer is caught before it is used as evidence.
func countSources(src string) (tables, views, mats int) {
	return strings.Count(src, "//orm:table "),
		strings.Count(src, "//orm:view "),
		strings.Count(src, "//orm:materialized-view ")
}

func TestCompatScale_projectsOfEverySize(t *testing.T) {
	for _, n := range []int{10, 100, 1000} {
		t.Run(fmt.Sprintf("%d-sources", n), func(t *testing.T) {
			src := scaleProject(n)
			tables, views, mats := countSources(src)
			if tables+views+mats != n {
				t.Fatalf("the fixture rendered %d+%d+%d sources for a size of %d; a size that "+
					"quietly renders fewer would report a smaller project as green",
					tables, views, mats, n)
			}
			if mats == 0 || views == 0 {
				t.Fatalf("the %d-source fixture has %d views and %d materialized views, so it "+
					"is not the mixture this gate is for", n, views, mats)
			}
			t.Logf("%d sources: %d tables, %d views, %d materialized views", n, tables, views, mats)

			p := newProject(t, src)

			start := time.Now()
			p.MustRun("makemigrations", "--name", "initial")
			planned := time.Since(start)

			start = time.Now()
			p.MustRun("migrate")
			migrated := time.Since(start)

			start = time.Now()
			p.MustRun("check")
			checked := time.Since(start)

			start = time.Now()
			p.MustRun("generate")
			generated := time.Since(start)

			p.MustRun("check", "--generated")
			if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
				t.Fatalf("%d sources did not converge:\n%s", n, truncate(out, 2000))
			}
			t.Logf("%d sources: makemigrations %s, migrate %s, check %s, generate %s",
				n, planned.Round(time.Millisecond), migrated.Round(time.Millisecond),
				checked.Round(time.Millisecond), generated.Round(time.Millisecond))

			// Every declared relation reached the database. A pass that stopped
			// early would converge just as happily on what it did apply.
			live := p.Query(`SELECT count(*)::text FROM pg_class c
			                   JOIN pg_namespace ns ON ns.oid = c.relnamespace
			                  WHERE ns.nspname = 'public' AND c.relkind IN ('r','v','m')
			                    AND c.relname ~ '^[tvm][0-9]+$'`)
			if len(live) != 1 || live[0] != fmt.Sprint(n) {
				t.Errorf("%d sources were declared and the database holds %v", n, live)
			}
			// And every materialized view's indexes exist.
			idx := p.Query(`SELECT count(*)::text FROM pg_index x
			                  JOIN pg_class r ON r.oid = x.indrelid
			                 WHERE r.relkind = 'm' AND r.relname ~ '^m[0-9]+$'`)
			if len(idx) != 1 || idx[0] != fmt.Sprint(mats*2) {
				t.Errorf("%d materialized views declare %d indexes and the catalog holds %v",
					mats, mats*2, idx)
			}

			// Determinism at size: a second identical project produces the same
			// portable bytes. Ordering bugs that only bite on large graphs show
			// up here and nowhere else.
			lockA := readFile(t, filepath.Join(p.Dir, "orm.lock"))
			genA := readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go"))
			q := newProject(t, src)
			q.MustRun("makemigrations", "--name", "initial")
			q.MustRun("migrate")
			q.MustRun("generate")
			if lockB := readFile(t, filepath.Join(q.Dir, "orm.lock")); lockB != lockA {
				t.Errorf("%d sources produced two different locks", n)
			}
			if genB := readFile(t, filepath.Join(q.Dir, "domain", "orm_db.gen.go")); genB != genA {
				t.Errorf("%d sources produced two different generated handles", n)
			}
		})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n... truncated"
}
