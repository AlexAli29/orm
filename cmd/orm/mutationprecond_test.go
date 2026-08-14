package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// M16.5 G2: the fixture preconditions the mutation campaign depends on.
//
// A mutation is evidence only if the fixture contains the thing it attacks.
// Delete GiST handling and every test stays green when no fixture declares a
// GiST index; the class then looks like a survivor, and the report says the
// product ignored a defect it was never shown. That is not a result about the
// product at all — it is a statement that the test could not have noticed.
//
// Three of these have already been recorded wrongly for exactly that reason. So
// the structural property each class attacks is asserted here first, on clean
// code, from the catalog and from the servers rather than from a test name, and
// a failure is recorded as a fixture-precondition failure rather than as a
// survivor.
//
// The subtests are named for their class so a single one can be demanded by
// name, and so the campaign's record points at an assertion a reader can run.

// surfaceCatalog builds the whole-index-surface fixture and returns, for every
// index on the materialized view, the access method PostgreSQL actually gave it.
//
// The access method is read from pg_am through pg_class rather than from
// indexdef text, because indexdef spells btree by omission and a substring match
// on a name would let a fixture with no GiST index at all satisfy a GiST
// precondition.
func surfaceCatalog(t *testing.T) (methods map[string]string, p *project) {
	t.Helper()
	p = newProject(t, surfaceEntities(surfaceIndexes))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.MustRun("check")

	conn := p.Conn()
	defer func() { _ = conn.Close(t.Context()) }()
	rows, err := conn.Query(t.Context(), `
		SELECT i.relname, am.amname
		  FROM pg_class i
		  JOIN pg_index x  ON x.indexrelid = i.oid
		  JOIN pg_class r  ON r.oid = x.indrelid
		  JOIN pg_am    am ON am.oid = i.relam
		 WHERE r.relname = 'doc_rollup' AND r.relkind = 'm'`)
	if err != nil {
		t.Fatalf("reading index access methods: %v", err)
	}
	defer rows.Close()
	methods = map[string]string{}
	for rows.Next() {
		var name, am string
		if err := rows.Scan(&name, &am); err != nil {
			t.Fatal(err)
		}
		methods[name] = am
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(methods) == 0 {
		t.Fatal("the materialized view carries no indexes at all, so no index precondition " +
			"can hold; the fixture did not reach the catalog")
	}
	return methods, p
}

// requireMethod asserts that a named index exists and uses a named access
// method, identifying it exactly rather than inferring it.
func requireMethod(t *testing.T, methods map[string]string, index, want string) {
	t.Helper()
	got, ok := methods[index]
	if !ok {
		have := make([]string, 0, len(methods))
		for n, m := range methods {
			have = append(have, n+"="+m)
		}
		sort.Strings(have)
		t.Fatalf("no index named %s is on the materialized view. The %s class attacks "+
			"metadata this fixture does not contain, so the mutation could not be "+
			"observed. Catalog: %v", index, want, have)
	}
	if got != want {
		t.Fatalf("%s uses access method %q, not %q. Its name is not evidence of its "+
			"method, and a class attacking %s must be shown a real %s index",
			index, got, want, want, want)
	}
}

// majorsOf returns the server major of every configured DSN, read from the
// server rather than from the name of the variable that pointed at it.
func majorsOf(t *testing.T) map[string]int {
	t.Helper()
	out := map[string]int{}
	for label, dsn := range majorDSNs(t) {
		conn, err := pgx.Connect(t.Context(), dsn)
		if err != nil {
			t.Fatalf("%s: connecting: %v", label, err)
		}
		var num int
		if err := conn.QueryRow(t.Context(),
			`SELECT current_setting('server_version_num')::int / 10000`).Scan(&num); err != nil {
			t.Fatalf("%s: reading the server version: %v", label, err)
		}
		_ = conn.Close(t.Context())
		out[label] = num
	}
	return out
}

// TestMutationFixturePrecondition asserts, per class, that the attacked
// structure is present before any mutation is applied.
func TestMutationFixturePrecondition(t *testing.T) {
	// #13 attacks GiST method metadata. The fixture must contain a real GiST
	// index, identified by its access method in pg_am.
	t.Run("c13", func(t *testing.T) {
		methods, _ := surfaceCatalog(t)
		requireMethod(t, methods, "doc_doc_gist_idx", "gist")
	})

	// #14 attacks BRIN, and the same argument applies.
	t.Run("c14", func(t *testing.T) {
		methods, _ := surfaceCatalog(t)
		requireMethod(t, methods, "doc_score_brin_idx", "brin")
	})

	// #31 attacks the stale-positive path, whose whole claim is that a
	// statement reaches PostgreSQL and the server refuses it. If the probe
	// cannot observe statements reaching the server, a mutation that swallowed
	// the server's error would be indistinguishable from one that never sent
	// anything. So the observation channel is proven first, in the healthy
	// direction: a qualifying index means one concurrent refresh, observed.
	t.Run("c31", func(t *testing.T) {
		got := refreshCase(t, "//orm:index totals_user_id_key (UserID) unique\n")
		if !strings.Contains(got, "statements=1 concurrent=1") {
			t.Fatalf("no REFRESH ... CONCURRENTLY reached PostgreSQL in the healthy case, "+
				"so the fixture cannot show that the stale case reaches it either:\n%s", got)
		}
		if !strings.Contains(got, "err=<nil>") {
			t.Fatalf("the healthy concurrent refresh failed, so a later failure could not "+
				"be attributed to staleness:\n%s", got)
		}
	})

	// #2 attacks CreateIndex's contribution to replay state, and #3 DropIndex's.
	// Both are only reachable if the migrations the workflow writes really carry
	// those operations against the materialized view — a workflow that folded
	// the indexes into the relation operation's payload would never replay
	// either, and the mutation would be unreachable rather than survived.
	t.Run("c2", func(t *testing.T) {
		p := convergenceFixture(t)
		requireOpOn(t, p, "create_index", "totals")
	})
	t.Run("c3", func(t *testing.T) {
		p := convergenceFixture(t)
		// Removing the index is what produces the drop, so the transition is
		// driven before the artifacts are inspected.
		p.Entities(refreshEntities(convergeOne))
		p.MustRun("makemigrations", "--name", "drop-index")
		p.MustRun("migrate")
		requireOpOn(t, p, "drop_index", "totals")
	})

	// #4 attacks convergence itself: a planner that treats duplicated replay
	// state as clean. It is only observable from a fixture that genuinely
	// converges on correct code, because a fixture that never converged would
	// be red before any mutation.
	t.Run("c4", func(t *testing.T) {
		p := convergenceFixture(t)
		if out := p.MustRun("makemigrations"); !strings.Contains(out, "No schema changes detected") {
			t.Fatalf("the fixture does not converge on clean code, so a mutation that broke "+
				"convergence could not be told apart from the fixture:\n%s", out)
		}
	})

	// #21 to #24 attack the eligibility rule, one disqualifying reason each. The
	// matrix has to contain a case of every shape, or removing the reason it
	// tests would change nothing the fixture asks about — and #24 is the
	// opposite direction, so INCLUDE must be present as a shape that qualifies.
	t.Run("cEligibility", func(t *testing.T) {
		cases := eligibilityCases()
		if len(cases) == 0 {
			t.Fatal("the eligibility matrix is empty")
		}
		shapes := map[string]func(eligibilityCase) bool{
			"a plain unique index that qualifies": func(c eligibilityCase) bool {
				return c.want != "" && strings.Contains(c.indexes, "unique") &&
					!strings.Contains(c.indexes, "where") && !strings.Contains(c.indexes, "include")
			},
			"a unique index with INCLUDE that qualifies": func(c eligibilityCase) bool {
				return c.want != "" && strings.Contains(c.indexes, "include")
			},
			"a non-unique index that does not": func(c eligibilityCase) bool {
				return c.want == "" && !strings.Contains(c.indexes, "unique")
			},
			"a partial unique index that does not": func(c eligibilityCase) bool {
				return c.want == "" && strings.Contains(c.indexes, "unique") &&
					strings.Contains(c.indexes, "where")
			},
			"a unique index over an expression that does not": func(c eligibilityCase) bool {
				return c.want == "" && strings.Contains(c.indexes, `("lower(email)") unique`)
			},
			"a unique index mixing a column and an expression that does not": func(c eligibilityCase) bool {
				return c.want == "" && strings.Contains(c.indexes, `(UserID, "lower(email)") unique`)
			},
		}
		for shape, match := range shapes {
			found := false
			for _, c := range cases {
				if match(c) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("the eligibility matrix contains no case of %s, so a rule change "+
					"about it would not be observed", shape)
			}
		}
	})

	// #27 attacks the qualifying index's presence in the fingerprint. The
	// transition must genuinely change which index qualifies — otherwise the
	// fingerprint would be identical either way and removing it from the
	// material would change nothing.
	t.Run("c27", func(t *testing.T) {
		p := newProject(t, refreshEntities(eligibilityBefore))
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")
		before := qualifyingOf(t, generatedOf(t, p))
		firstLock := lockDigest(t, p)

		p.Entities(refreshEntities(eligibilityAfter))
		p.MustRun("makemigrations", "--name", "add-lower-index")
		p.MustRun("migrate")
		p.MustRun("generate")
		after := qualifyingOf(t, generatedOf(t, p))
		secondLock := lockDigest(t, p)

		if before == after {
			t.Fatalf("the qualifying index is %s before and after the transition, so "+
				"removing it from the fingerprint material would change nothing and "+
				"the class could not be caught", before)
		}
		if firstLock == secondLock {
			t.Fatalf("the lock digest did not move across the transition (%s), so there is "+
				"no staleness for the class to hide", firstLock)
		}
		t.Logf("qualifying index changes %s -> %s; lock digest %s -> %s",
			before, after, firstLock, secondLock)
	})

	// #28 attacks the opposite direction: table index metadata leaking into an
	// ordinary table's fingerprint. The fixture must contain no materialized
	// view at all, or the leak could be attributed to the materialized-view
	// path that is supposed to carry index facts.
	t.Run("c28", func(t *testing.T) {
		if strings.Contains(tableOnlyBefore, "materialized-view") ||
			strings.Contains(tableOnlyAfter, "materialized-view") {
			t.Fatal("the table-only fixture declares a materialized view, whose generated " +
				"code legitimately depends on its indexes")
		}
		if tableOnlyBefore == tableOnlyAfter {
			t.Fatal("the two table-only declaration sets are identical, so no index change " +
				"takes place")
		}
		// The difference is an index line and nothing else.
		diff := changedLines(tableOnlyBefore, tableOnlyAfter)
		if len(diff) == 0 {
			t.Fatal("no lines differ between the two table-only declaration sets")
		}
		for _, l := range diff {
			if !strings.Contains(l, "//orm:index") {
				t.Fatalf("the table-only transition changes a line that is not an index "+
					"declaration, so a fingerprint change could not be attributed to "+
					"index metadata: %q", l)
			}
		}
		t.Logf("index-only difference: %v", diff)
	})

	// #30 attacks the current/stale decision. It needs the two digests to
	// genuinely differ at the moment the decision is made, which is what makes
	// accepting a mismatch a wrong answer rather than a correct one.
	t.Run("c30", func(t *testing.T) {
		p := newProject(t, refreshEntities(eligibilityBefore))
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")
		committed := lockDigest(t, p)

		p.Entities(refreshEntities(eligibilityAfter))
		p.MustRun("makemigrations", "--name", "add-lower-index")
		p.MustRun("migrate")
		// Deliberately not regenerated: the lock still holds the old digest.
		stale := lockDigest(t, p)
		if stale != committed {
			t.Fatalf("the lock changed without generate being run: %s -> %s",
				committed, stale)
		}
		// Regenerating in a throwaway copy shows what the digest would now be.
		p.MustRun("generate")
		current := lockDigest(t, p)
		if current == committed {
			t.Fatalf("the mapping fingerprint is unchanged across the transition (%s), so "+
				"there is no mismatch for a stale check to miss", current)
		}
		t.Logf("committed digest %s, digest after the change %s", committed, current)
	})

	// #32 attacks portability by putting runtime population into generated
	// identity. The fixture must genuinely change population while leaving every
	// declaration byte-identical, or a changed artifact could be attributed to
	// the declarations instead.
	t.Run("c32", func(t *testing.T) {
		p := newProject(t, refreshEntities(convergeOne))
		p.MustRun("makemigrations", "--name", "initial")
		p.MustRun("migrate")
		p.MustRun("generate")
		declarations := readFile(t, filepath.Join(p.Dir, "domain", "entities.go"))

		populated := func() bool {
			got := p.Query(`SELECT relispopulated::text FROM pg_class
			                 WHERE relname = 'totals' AND relkind = 'm'`)
			if len(got) != 1 {
				t.Fatalf("the materialized view is not in the catalog: %v", got)
			}
			return got[0] == "true"
		}
		if !populated() {
			t.Fatal("the materialized view is not populated to begin with, so population " +
				"cannot be observed changing")
		}
		p.SQL(`REFRESH MATERIALIZED VIEW public.totals WITH NO DATA`)
		if populated() {
			t.Fatal("WITH NO DATA left the materialized view populated, so runtime " +
				"population does not change in this fixture")
		}
		if now := readFile(t, filepath.Join(p.Dir, "domain", "entities.go")); now != declarations {
			t.Fatal("the declarations changed while population was being changed, so a " +
				"changed artifact could not be attributed to population alone")
		}
		t.Log("population changed from true to false with the declarations untouched")
	})

	// #39 attacks declaration order. The fixture is only able to notice if the
	// two orders genuinely differ, and specifically if the two qualifying
	// candidates swap places — reversing a list whose qualifying entries are
	// adjacent duplicates would change nothing that selection could see.
	t.Run("c39", func(t *testing.T) {
		forward := surfaceIndexes
		lines := strings.Split(strings.TrimRight(surfaceIndexes, "\n"), "\n")
		for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
			lines[i], lines[j] = lines[j], lines[i]
		}
		reversed := strings.Join(lines, "\n") + "\n"
		if forward == reversed {
			t.Fatal("reversing the declaration block produced the same text; there is no " +
				"order difference for a mutation to exploit")
		}
		fa, fz := strings.Index(forward, "aaa_doc_id_key"), strings.Index(forward, "zzz_doc_id_key")
		ra, rz := strings.Index(reversed, "aaa_doc_id_key"), strings.Index(reversed, "zzz_doc_id_key")
		if fa < 0 || fz < 0 || ra < 0 || rz < 0 {
			t.Fatal("the fixture does not declare both qualifying candidates")
		}
		if (fz < fa) == (rz < ra) {
			t.Fatalf("the two qualifying candidates keep their relative order under "+
				"reversal (forward zzz@%d aaa@%d, reversed zzz@%d aaa@%d), so an "+
				"order-dependent selection would pick the same one either way",
				fz, fa, rz, ra)
		}
		// And the lexically first is not the first declared, or encounter order
		// and the stable rule would agree and prove nothing.
		if fz > fa {
			t.Fatal("the fixture declares aaa_doc_id_key before zzz_doc_id_key, so choosing " +
				"the first encountered and choosing the lowest name give the same answer")
		}
	})

	// #41 attacks portability across majors. Two DSNs are not two majors: the
	// version is read from each server.
	t.Run("c41", func(t *testing.T) {
		majors := majorsOf(t)
		if len(majors) < 2 {
			t.Fatalf("only %d server(s) configured; portability across majors cannot be "+
				"observed. Set ORM_TEST_DSN_PG14..PG18", len(majors))
		}
		seen := map[int][]string{}
		for label, m := range majors {
			seen[m] = append(seen[m], label)
		}
		if len(seen) < 2 {
			t.Fatalf("every configured server reports the same major: %v. Injecting the "+
				"major into a fingerprint would then produce identical artifacts and "+
				"the class could not be caught", seen)
		}
		t.Logf("majors present: %v", seen)
	})

	// #42 attacks the database name. Each capture must genuinely run against a
	// differently named database, or injecting the name would change nothing.
	t.Run("c42", func(t *testing.T) {
		a, b := newProject(t, surfaceEntities(surfaceIndexes)), newProject(t, surfaceEntities(surfaceIndexes))
		nameOf := func(p *project) string {
			cfg, err := pgx.ParseConfig(p.DSN)
			if err != nil {
				t.Fatalf("parsing the project DSN: %v", err)
			}
			return cfg.Database
		}
		x, y := nameOf(a), nameOf(b)
		if x == "" || y == "" {
			t.Fatalf("a capture has no database name: %q, %q", x, y)
		}
		if x == y {
			t.Fatalf("both captures use database %q, so a fingerprint carrying the "+
				"database name would still match across them", x)
		}
		t.Logf("database names differ: %s vs %s", x, y)
	})

	// #43 attacks OIDs. They are assigned by the server and there is no rule
	// that two databases must disagree, so it is asserted rather than assumed.
	t.Run("c43", func(t *testing.T) {
		oids := func(p *project) (rel uint32, idx []uint32) {
			conn := p.Conn()
			defer func() { _ = conn.Close(t.Context()) }()
			if err := conn.QueryRow(t.Context(),
				`SELECT 'public.doc_rollup'::regclass::oid`).Scan(&rel); err != nil {
				t.Fatalf("reading the relation OID: %v", err)
			}
			rows, err := conn.Query(t.Context(), `
				SELECT x.indexrelid::oid FROM pg_index x
				  JOIN pg_class r ON r.oid = x.indrelid
				 WHERE r.relname = 'doc_rollup' ORDER BY 1`)
			if err != nil {
				t.Fatalf("reading index OIDs: %v", err)
			}
			defer rows.Close()
			for rows.Next() {
				var o uint32
				if err := rows.Scan(&o); err != nil {
					t.Fatal(err)
				}
				idx = append(idx, o)
			}
			return rel, idx
		}
		build := func() *project {
			p := newProject(t, surfaceEntities(surfaceIndexes))
			p.MustRun("makemigrations", "--name", "initial")
			p.MustRun("migrate")
			return p
		}
		relA, idxA := oids(build())
		relB, idxB := oids(build())
		if relA == 0 || relB == 0 {
			t.Fatal("a capture produced no relation OID")
		}
		if relA == relB {
			t.Fatalf("both captures gave the materialized view OID %d, so an OID in a "+
				"portable artifact would compare equal and the class could not be "+
				"caught", relA)
		}
		if len(idxA) == 0 || len(idxB) == 0 {
			t.Fatal("a capture produced no index OIDs")
		}
		same := 0
		for i := range idxA {
			if i < len(idxB) && idxA[i] == idxB[i] {
				same++
			}
		}
		if same == len(idxA) {
			t.Fatalf("every index OID matched across captures: %v", idxA)
		}
		t.Logf("relation OIDs differ: %d vs %d; index OIDs %v vs %v", relA, relB, idxA, idxB)
	})

	// #44 attacks the F0 boundary by putting server-local canonical text into
	// portable state. That is only catchable if the canonical text genuinely
	// differs between the servers being compared. It does: PostgreSQL 16 stopped
	// qualifying columns its deparser does not need to qualify, so one unchanged
	// definition reads differently on 14 and 16.
	t.Run("c44", func(t *testing.T) {
		majors := majorsOf(t)
		canon := map[string]string{}
		for label, dsn := range majorDSNs(t) {
			conn, err := pgx.Connect(t.Context(), dsn)
			if err != nil {
				t.Fatalf("%s: connecting: %v", label, err)
			}
			name := "canon_precond_" + strings.ToLower(label)
			for _, stmt := range []string{
				`DROP MATERIALIZED VIEW IF EXISTS ` + name,
				`DROP TABLE IF EXISTS ` + name + `_src`,
				`CREATE TABLE ` + name + `_src (id bigint, owner_id bigint, title text)`,
				`CREATE MATERIALIZED VIEW ` + name + ` AS ` +
					`SELECT d.id, d.owner_id, d.title FROM ` + name + `_src d`,
			} {
				if _, err := conn.Exec(t.Context(), stmt); err != nil {
					t.Fatalf("%s: %s: %v", label, stmt, err)
				}
			}
			var def string
			if err := conn.QueryRow(t.Context(),
				`SELECT pg_get_viewdef($1::regclass, true)`, name).Scan(&def); err != nil {
				t.Fatalf("%s: reading the canonical definition: %v", label, err)
			}
			for _, stmt := range []string{
				`DROP MATERIALIZED VIEW IF EXISTS ` + name,
				`DROP TABLE IF EXISTS ` + name + `_src`,
			} {
				if _, err := conn.Exec(t.Context(), stmt); err != nil {
					t.Errorf("%s: cleaning up: %v", label, err)
				}
			}
			_ = conn.Close(t.Context())
			if strings.TrimSpace(def) == "" {
				t.Fatalf("%s produced no canonical definition, so there is no server-local "+
					"value to inject", label)
			}
			canon[label] = def
		}
		if len(canon) < 2 {
			t.Fatalf("only %d server(s) configured; a value that is the same everywhere "+
				"cannot demonstrate a portability break", len(canon))
		}
		distinct := map[string][]string{}
		for label, def := range canon {
			distinct[strings.Join(strings.Fields(def), " ")] = append(distinct[strings.Join(strings.Fields(def), " ")], label)
		}
		if len(distinct) < 2 {
			t.Fatalf("every server deparsed the definition identically, so injecting the "+
				"canonical text would not break the cross-major comparison. Majors "+
				"present: %v", majors)
		}
		for text, labels := range distinct {
			sort.Strings(labels)
			t.Logf("canonical on %v: %s", labels, text)
		}
	})
}

var _ = os.Getenv
