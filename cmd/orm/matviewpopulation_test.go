package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// M16.5 G2 #32: refreshing a materialized view is not a schema change.
//
// Whether a materialized view holds data is runtime state the server owns. It
// changes every time anybody refreshes, it can be false on a database that is
// otherwise identical to another, and no declaration says anything about it.
//
// If it reached the portable artifacts, an ordinary REFRESH would report the
// committed generated code as stale — so a developer would regenerate, commit a
// lock that differs from their colleague's, and learn to keep regenerating until
// the diff stopped appearing. The reconcile tier already proves population is not
// drift; what is proven here is the other half, through the commands a user runs:
// the generated identity does not move, and the bytes do not either.
//
// WITH NO DATA is the stronger direction, because it leaves the view unscannable
// — a state a naive implementation might well decide is worth recording.
func TestMatViewPopulation_isNotGeneratedIdentity(t *testing.T) {
	p := newProject(t, refreshEntities(convergeOne))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")
	p.MustRun("check", "--generated")

	before := struct{ lock, generated string }{
		lock:      readFile(t, filepath.Join(p.Dir, "orm.lock")),
		generated: generatedOf(t, p),
	}

	// Emptying it is the largest change to population there is.
	p.SQL(`REFRESH MATERIALIZED VIEW public.totals WITH NO DATA`)
	if got := p.Query(`SELECT relispopulated::text FROM pg_class
	                    WHERE relname = 'totals' AND relkind = 'm'`); len(got) != 1 || got[0] != "false" {
		t.Fatalf("the materialized view is still populated (%v), so the rest of this test "+
			"would prove nothing", got)
	}

	// The generated code was produced from the same declarations, so it is current.
	code, stdout, stderr := p.Run("check", "--generated")
	if code != exitClean {
		t.Errorf("an unpopulated materialized view made the generated code report as stale. "+
			"Refreshing is not a schema change, and nothing about what was generated "+
			"has moved:\n%s", stdout+stderr)
	}

	// And regenerating produces the same bytes, so nobody is offered a diff.
	p.MustRun("generate")
	if got := readFile(t, filepath.Join(p.Dir, "orm.lock")); got != before.lock {
		t.Errorf("the lock changed after a refresh:\n%s\n%s", before.lock, got)
	}
	if got := generatedOf(t, p); got != before.generated {
		t.Errorf("the generated code changed after a refresh:\n%s",
			diffText(before.generated, got))
	}

	// Repopulating goes back the same way.
	p.SQL(`REFRESH MATERIALIZED VIEW public.totals`)
	p.MustRun("check", "--generated")
	p.MustRun("generate")
	if got := readFile(t, filepath.Join(p.Dir, "orm.lock")); got != before.lock {
		t.Errorf("the lock changed after repopulating:\n%s\n%s", before.lock, got)
	}

	// Nothing a server contributed may be in the artifacts at all.
	for _, forbidden := range []string{"populated", "relispopulated", "oid", "server_version"} {
		if strings.Contains(strings.ToLower(before.lock), forbidden) {
			t.Errorf("the lock contains %q", forbidden)
		}
	}
}
