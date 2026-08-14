package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M16.5 G2: the permanent fixtures the index/refresh mutation classes attack.
//
// They are named and shared rather than written inline per test so that a
// fixture precondition and the killer that depends on it are demonstrably the
// same structure. A precondition asserting a property of one declaration set
// while the killer ran against another would be exactly the failure the
// precondition mechanism exists to prevent.

// The convergence fixture: one index, then two. Adding and removing an index is
// what makes duplicated replay state visible from outside, because the duplicate
// only shows in the next plan.
const (
	convergeOne = "//orm:index totals_user_id_key (UserID) unique\n"
	convergeTwo = "//orm:index totals_user_id_key (UserID) unique\n" +
		"//orm:index totals_email_idx (Email)\n"
)

// The eligibility-changing transition: adding a lexically lower qualifying index
// changes which index a concurrent refresh must use, so the canonical answer
// moves without anything else about the mapping moving.
const (
	eligibilityBefore = "//orm:index totals_user_id_key (UserID) unique\n"
	eligibilityAfter  = "//orm:index totals_user_id_key (UserID) unique\n" +
		"//orm:index aaa_totals_key (UserID) unique\n"
)

// The table-only transition: an index change on an ordinary table, with no
// materialized view anywhere in the declarations. A table's generated code does
// not depend on its indexes, so nothing portable may move.
//
// The indexes are unique ones on purpose. What a table's introspected model
// carries is its unique indexes — a plain index is not in it at all — so a
// fixture changing only a plain index could not observe index metadata reaching
// the fingerprint even if it did. The unique index is not the primary key and
// nothing is proved from it here: there are no relations, so no generated code
// depends on it either.
const tableOnlyBefore = `package domain

//orm:table public.users
//orm:index users_email_key (Email) unique
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}
`

const tableOnlyAfter = `package domain

//orm:table public.users
//orm:index users_email_active_key (Email, Active) unique
type User struct {
	ID     int64 ` + "`orm:\"pk,identity\"`" + `
	Email  string
	Active bool
}
`

// convergenceFixture builds the convergence project and drives it to the state
// where an index has been added through the ordinary managed path.
func convergenceFixture(t *testing.T) *project {
	t.Helper()
	p := newProject(t, refreshEntities(convergeOne))
	p.MustRun("makemigrations", "--name", "initial")
	p.MustRun("migrate")
	p.Entities(refreshEntities(convergeTwo))
	p.MustRun("makemigrations", "--name", "add-index")
	p.MustRun("migrate")
	return p
}

// requireOpOn asserts that some committed migration carries an operation of a
// kind against a relation, read out of the artifact rather than assumed from
// the workflow having been run.
func requireOpOn(t *testing.T, p *project, kind, relation string) {
	t.Helper()
	dir := filepath.Join(p.Dir, "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the migrations directory: %v", err)
	}
	var seen []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		// args is decoded lazily and only for the kind being looked for: a
		// create_table's "table" is the whole relation object rather than a
		// name, so one struct cannot describe every operation's arguments.
		var file struct {
			Operations []struct {
				Op   string          `json:"op"`
				Args json.RawMessage `json:"args"`
			} `json:"operations"`
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			t.Fatalf("parsing %s: %v", e.Name(), err)
		}
		for _, o := range file.Operations {
			seen = append(seen, o.Op)
			if o.Op != kind {
				continue
			}
			var args struct {
				Schema string `json:"schema"`
				Table  string `json:"table"`
			}
			if err := json.Unmarshal(o.Args, &args); err != nil {
				t.Fatalf("parsing the arguments of %s in %s: %v", o.Op, e.Name(), err)
			}
			if args.Table == relation {
				return
			}
			seen[len(seen)-1] = o.Op + " on " + args.Schema + "." + args.Table
		}
	}
	t.Fatalf("no %s operation against %s is in any committed migration. The class attacks "+
		"that operation's contribution to replay state, and a workflow that never "+
		"replays it could not observe the mutation. Operations present: %v",
		kind, relation, seen)
}

// qualifyingOf extracts the index name the generated constructor was given.
//
// It is read out of the generated code rather than recomputed, so that the
// precondition observes the product's own answer.
func qualifyingOf(t *testing.T, generated string) string {
	t.Helper()
	const marker = "NewMaterializedViewRepo("
	k := strings.Index(generated, marker)
	if k < 0 {
		t.Fatalf("the generated code constructs no materialized-view repository")
	}
	rest := generated[k:]
	lo := strings.Index(rest, `"`)
	if lo < 0 {
		t.Fatalf("no quoted argument in %.120s", rest)
	}
	hi := strings.Index(rest[lo+1:], `"`)
	if hi < 0 {
		t.Fatalf("unterminated argument in %.120s", rest)
	}
	return rest[lo+1 : lo+1+hi]
}

// generatedOf returns the generated sources a materialized view's descriptor
// lives in.
func generatedOf(t *testing.T, p *project) string {
	t.Helper()
	return readFile(t, filepath.Join(p.Dir, "domain", "orm_db.gen.go")) +
		readFile(t, filepath.Join(p.Dir, "domain", "orm_meta.gen.go"))
}

// lockDigest returns the mapping fingerprint the lock records.
func lockDigest(t *testing.T, p *project) string {
	t.Helper()
	var f struct {
		Version int    `json:"version"`
		Mapping string `json:"mapping_sha256"`
	}
	raw := readFile(t, filepath.Join(p.Dir, "orm.lock"))
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("parsing orm.lock: %v\n%s", err, raw)
	}
	if f.Mapping == "" {
		t.Fatalf("orm.lock records no mapping fingerprint:\n%s", raw)
	}
	return f.Mapping
}

// changedLines returns the lines present in exactly one of two texts.
func changedLines(a, b string) []string {
	count := map[string]int{}
	for _, l := range strings.Split(a, "\n") {
		count[strings.TrimSpace(l)]++
	}
	for _, l := range strings.Split(b, "\n") {
		count[strings.TrimSpace(l)]--
	}
	var out []string
	for _, l := range append(strings.Split(a, "\n"), strings.Split(b, "\n")...) {
		l = strings.TrimSpace(l)
		if l == "" || count[l] == 0 {
			continue
		}
		if !containsString(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func containsString(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
