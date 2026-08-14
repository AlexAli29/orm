package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M16.5 G2 E/F: the runtime negative Refresh Concurrently matrix.
//
// The descriptor matrix already proves which index shapes qualify. What is
// proven here is what the generated repository actually does with that answer,
// and the assertion that matters is not the error it returns — it is that
// PostgreSQL never saw a statement. A preflight that refuses after sending
// REFRESH ... CONCURRENTLY has taken the lock it was trying to avoid and has
// let the server do the refusing.
//
// So every case runs the real generated API through a tracer and counts the
// REFRESH statements that reached the executor.

// refreshEntities declares a materialized view with a chosen index set.
func refreshEntities(indexes string) string {
	return `package domain

//orm:table public.users
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}

//orm:materialized-view public.totals
//orm:definition ` + "`SELECT id AS user_id, email FROM users WHERE active`" + `
//orm:depends-on public.users
` + indexes + `type Total struct {
	UserID int64
	Email  string
}
`
}

// refreshProbe is the program that exercises the generated repository and
// reports what reached PostgreSQL.
const refreshProbe = `package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"example.com/managed/domain"
	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/observe"
	"github.com/jackc/pgx/v5/pgxpool"
)

type counter struct{ refreshes []string }

func (c *counter) Start(ctx context.Context, e observe.StartEvent) context.Context {
	if strings.Contains(strings.ToUpper(e.SQL), "REFRESH MATERIALIZED VIEW") {
		c.refreshes = append(c.refreshes, e.SQL)
	}
	return ctx
}
func (c *counter) End(ctx context.Context, e observe.EndEvent) {}

func main() {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, os.Getenv("DSN"))
	if err != nil {
		panic(err)
	}
	defer pool.Close()

	c := &counter{}
	db := domain.New(orm.Traced(pool, c))
	err = db.Totals.Refresh(ctx, orm.Concurrently())

	concurrent := 0
	for _, s := range c.refreshes {
		if strings.Contains(strings.ToUpper(s), "CONCURRENTLY") {
			concurrent++
		}
	}
	fmt.Printf("statements=%d concurrent=%d\n", len(c.refreshes), concurrent)
	if err != nil {
		fmt.Printf("err=%s\n", strings.ReplaceAll(err.Error(), "\n", " "))
	} else {
		fmt.Printf("err=<nil>\n")
	}
}
`

// refreshCase runs the whole workflow for one index set and returns the probe's
// output.
func refreshCase(t *testing.T, indexes string) string {
	t.Helper()
	p := newProject(t, refreshEntities(indexes))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")
	writeFile(t, filepath.Join(p.Dir, "main.go"), refreshProbe)
	return runProgram(t, p)
}

// Every statically non-qualifying shape refuses before any SQL is sent.
func TestRefreshRuntime_nonQualifyingShapesSendNothing(t *testing.T) {
	for _, c := range []struct{ what, indexes string }{
		{
			"only a non-unique index",
			"//orm:index totals_user_id_idx (UserID)\n",
		},
		{
			"a partial unique index",
			"//orm:index totals_user_id_key (UserID) unique where \"user_id > 0\"\n",
		},
		{
			"an expression unique index",
			"//orm:index totals_lower_key (\"lower(email)\") unique\n",
		},
		{
			"a unique index mixing a plain column and an expression",
			"//orm:index totals_mixed_key (UserID, \"lower(email)\") unique\n",
		},
		{
			"several indexes, none of them qualifying",
			"//orm:index totals_plain_idx (UserID)\n" +
				"//orm:index totals_partial_key (UserID) unique where \"user_id > 0\"\n" +
				"//orm:index totals_expr_key (\"lower(email)\") unique\n",
		},
		{
			// The same set, declared in the other order. A refusal that
			// depended on which index the scanner reached first would be a
			// refusal that could turn into an acceptance on a reformat.
			"the same non-qualifying set, declared in the other order",
			"//orm:index totals_expr_key (\"lower(email)\") unique\n" +
				"//orm:index totals_partial_key (UserID) unique where \"user_id > 0\"\n" +
				"//orm:index totals_plain_idx (UserID)\n",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			got := refreshCase(t, c.indexes)
			if !strings.Contains(got, "statements=0 concurrent=0") {
				t.Errorf("%s reached PostgreSQL. A preflight that refuses after sending the "+
					"statement has already taken the lock it was avoiding:\n%s", c.what, got)
			}
			if !strings.Contains(got, "unique index") {
				t.Errorf("%s did not produce the local refusal:\n%s", c.what, got)
			}
		})
	}
}

// A qualifying index means exactly one concurrent refresh, and it succeeds.
func TestRefreshRuntime_qualifyingShapesRefreshOnce(t *testing.T) {
	for _, c := range []struct{ what, indexes string }{
		{
			"exactly one qualifying index among several",
			"//orm:index totals_plain_idx (UserID)\n" +
				"//orm:index totals_partial_key (UserID) unique where \"user_id > 0\"\n" +
				"//orm:index totals_user_id_key (UserID) unique\n",
		},
		{
			"the same set in another order",
			"//orm:index totals_user_id_key (UserID) unique\n" +
				"//orm:index totals_partial_key (UserID) unique where \"user_id > 0\"\n" +
				"//orm:index totals_plain_idx (UserID)\n",
		},
		{
			"two qualifying indexes",
			"//orm:index aaa_totals_key (UserID) unique\n" +
				"//orm:index zzz_totals_key (UserID) unique\n",
		},
		{
			"two qualifying indexes, declared the other way round",
			"//orm:index zzz_totals_key (UserID) unique\n" +
				"//orm:index aaa_totals_key (UserID) unique\n",
		},
		{
			// INCLUDE is the case the rule accepts rather than rejects, and it
			// was proven only as far as the generated descriptor: the eligibility
			// matrix says such an index qualifies, and nothing sent a statement.
			//
			// That half is the one that could be wrong. Covering columns are not
			// key columns, so the index still covers every row and PostgreSQL
			// accepts it — verified on 14 and 18 — but a rule that offered an
			// index the server rejected would produce exactly the runtime failure
			// the preflight exists to prevent, and no descriptor test would see
			// it. So the statement is sent and required to succeed.
			"a qualifying index carrying INCLUDE columns",
			"//orm:index totals_user_id_key (UserID) unique include (Email)\n",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			got := refreshCase(t, c.indexes)
			if !strings.Contains(got, "statements=1 concurrent=1") {
				t.Errorf("%s did not send exactly one concurrent refresh:\n%s", c.what, got)
			}
			if !strings.Contains(got, "err=<nil>") {
				t.Errorf("%s failed:\n%s", c.what, got)
			}
		})
	}
}

var _ = os.DevNull

// The stale-positive lifecycle: generated metadata says a concurrent refresh is
// possible, and the database no longer agrees.
//
// This is the case the preflight cannot get right on its own. Generated code
// carries the name of a qualifying index; a migration can remove that index
// without the code being regenerated, and the descriptor is then optimistic. The
// contract being frozen here is that optimism cannot make PostgreSQL accept an
// invalid operation — the statement is sent, the server refuses it, and its
// refusal is what the caller receives. A local error invented at that point
// would be a lie about which authority spoke.
func TestRefreshRuntime_staleOptimisticDescriptorDefersToPostgreSQL(t *testing.T) {
	withIndex := "//orm:index totals_user_id_key (UserID) unique\n"
	p := newProject(t, refreshEntities(withIndex))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("generate")
	writeFile(t, filepath.Join(p.Dir, "main.go"), refreshProbe)

	// It works while the index is there.
	if got := runProgram(t, p); !strings.Contains(got, "statements=1 concurrent=1") ||
		!strings.Contains(got, "err=<nil>") {
		t.Fatalf("the qualifying index did not permit a concurrent refresh:\n%s", got)
	}

	// Remove the index through the ordinary managed path, and deliberately do
	// not regenerate. The committed descriptor is now optimistic.
	p.Entities(refreshEntities(""))
	p.MustRun("makemigrations", "--name", "drop-index")
	p.MustRun("migrate")

	// The staleness itself is reported, which is the other half of the contract.
	code, stdout, stderr := p.Run("check", "--generated")
	out := stdout + stderr
	if code == exitClean {
		t.Errorf("generated code that no longer matches the schema was reported current:\n%s", out)
	}

	// The old binary still believes it can refresh concurrently. It sends the
	// statement, and PostgreSQL refuses it.
	got := runProgram(t, p)
	if !strings.Contains(got, "statements=1 concurrent=1") {
		t.Errorf("the stale descriptor did not send the statement, so the local preflight "+
			"invented an answer it no longer had the facts for:\n%s", got)
	}
	if strings.Contains(got, "err=<nil>") {
		t.Fatalf("PostgreSQL accepted a concurrent refresh with no qualifying index:\n%s", got)
	}
	// PostgreSQL's own error, not a local rewrite of it.
	if !strings.Contains(got, "SQLSTATE 55000") {
		t.Errorf("the server's error did not reach the caller intact:\n%s", got)
	}
	if strings.Contains(got, "Add one, or refresh without Concurrently") {
		t.Errorf("the server's refusal was replaced by the local preflight message, which "+
			"claims the ORM knew something it did not:\n%s", got)
	}

	// Regenerating converts it back into a local refusal that sends nothing.
	p.MustRun("generate")
	p.MustRun("check", "--generated")
	got = runProgram(t, p)
	if !strings.Contains(got, "statements=0 concurrent=0") {
		t.Errorf("after regeneration the refusal still reaches PostgreSQL:\n%s", got)
	}
	if !strings.Contains(got, "unique index") {
		t.Errorf("after regeneration the local refusal is missing:\n%s", got)
	}
}

// The duplicate-index-state defect, caught the way a user would meet it.
//
// The old bug put a materialized view's indexes into the migration state twice:
// once from the relation operation's payload and once from the CreateIndex
// beside it. Nothing about the database was wrong — it only ever saw one CREATE
// INDEX — so an assertion on the catalog finds nothing. What breaks is the next
// plan, which sees an index the declarations do not have and writes a migration
// to drop it, and then writes it again on the following run.
//
// So the assertion is convergence: after changing one index and migrating, a
// further makemigrations must have nothing to say.
func TestMatViewConvergence_indexChangeSettles(t *testing.T) {
	one := "//orm:index totals_user_id_key (UserID) unique\n"
	two := "//orm:index totals_user_id_key (UserID) unique\n" +
		"//orm:index totals_email_idx (Email)\n"

	p := newProject(t, refreshEntities(one))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("the project did not converge before any change:\n%s", out)
	}

	// Add an index, apply it, and converge again.
	p.Entities(refreshEntities(two))
	p.MustRun("makemigrations", "--name", "add-index")
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("after adding an index the project does not converge. The migration state "+
			"holds an index the declarations do not, which is what duplicated index state "+
			"looks like from outside:\n%s", out)
	}

	// Remove it again, which is where a duplicate shows most clearly: the state
	// still believes one copy remains.
	p.Entities(refreshEntities(one))
	p.MustRun("makemigrations", "--name", "drop-index")
	p.MustRun("migrate")
	p.MustRun("check")
	if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
		t.Fatalf("after removing an index the project does not converge:\n%s", out)
	}
}
