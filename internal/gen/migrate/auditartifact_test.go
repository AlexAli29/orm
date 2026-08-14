package migrate_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 adversarial audit: artifacts that are not what they claim.
//
// A migration artifact is a file in a repository. It is edited by hand, merged
// badly, produced by an older tool, and occasionally written by somebody who
// wants the schema to do something the planner refused. Every one of those
// arrives at Parse looking like JSON.
//
// The contract being attacked is the frozen one: an artifact is rejected, or it
// is exactly what it says. There is no third option where a malformed or
// impossible artifact is normalised into something valid — a history quietly
// repaired on read is a history nobody can reason about, and the repair happens
// on every machine differently depending on which version read it.
//
// The layer that refuses matters as much as the refusal. A bad payload should
// fail at parse; an impossible sequence should fail at replay. What must never
// happen is a silent success that leaves the state wrong.

// validArtifact renders a well-formed migration so the attacks can start from
// something the tool itself produced.
func validArtifact(t *testing.T) []byte {
	t.Helper()
	m := &migrate.Migration{
		ID: "0001_totals", Atomic: true,
		Operations: []migrate.Operation{
			migrate.CreateMaterializedView{View: totals(true)},
			migrate.CreateIndex{Schema: "public", Table: "totals", Index: totalsIndex()},
		},
	}
	raw, err := migrate.Render(m)
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return raw
}

// tamper edits the artifact as JSON and renders it back.
func tamper(t *testing.T, raw []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing the artifact: %v", err)
	}
	edit(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("re-rendering: %v", err)
	}
	return out
}

func opsOf(t *testing.T, doc map[string]any) []any {
	t.Helper()
	ops, ok := doc["operations"].([]any)
	if !ok {
		t.Fatalf("the artifact has no operations array: %v", doc)
	}
	return ops
}

// §21: a tampered payload is refused at parse.
func TestAuditArtifact_tamperedPayloadsAreRefused(t *testing.T) {
	base := validArtifact(t)

	for _, c := range []struct {
		what string
		edit func(map[string]any)
	}{
		{
			"an operation nobody implements",
			func(d map[string]any) { opsOf(t, d)[0].(map[string]any)["op"] = "drop_database" },
		},
		{
			"an operation with no kind at all",
			func(d map[string]any) { delete(opsOf(t, d)[0].(map[string]any), "op") },
		},
		{
			"arguments of the wrong shape",
			func(d map[string]any) { opsOf(t, d)[1].(map[string]any)["args"] = "not an object" },
		},
		{
			"a relation kind the format does not have",
			func(d map[string]any) {
				args := opsOf(t, d)[0].(map[string]any)["args"].(map[string]any)
				args["View"] = "not a relation"
			},
		},
		{
			"a format version from the future",
			func(d map[string]any) { d["format"] = 99 },
		},
		{
			"no identifier",
			func(d map[string]any) { delete(d, "id") },
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			raw := tamper(t, base, c.edit)
			m, err := migrate.Parse(raw)
			if err == nil {
				t.Fatalf("%s parsed into %d operation(s); a malformed artifact must be "+
					"refused rather than normalised into something valid",
					c.what, len(m.Operations))
			}
		})
	}
}

// Editing an artifact changes its checksum, which is what makes a history
// tamper-evident.
//
// The checksum is over the operations rather than the bytes, so reformatting is
// invisible and a changed operation is not. Both halves are asserted, because a
// checksum that moved on whitespace would make every reformat look like an
// attack and would be turned off.
func TestAuditArtifact_theChecksumSeesTheOperationsAndNotTheBytes(t *testing.T) {
	base := validArtifact(t)
	original, err := migrate.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	want, err := original.Checksum()
	if err != nil {
		t.Fatal(err)
	}

	// Reformatted: same operations, different bytes.
	reformatted := tamper(t, base, func(map[string]any) {})
	if string(reformatted) == string(base) {
		t.Fatal("the reformat produced identical bytes, so it proves nothing")
	}
	same, err := migrate.Parse(reformatted)
	if err != nil {
		t.Fatalf("a reformatted artifact no longer parses: %v", err)
	}
	got, err := same.Checksum()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("reformatting changed the checksum: %s -> %s. Every reformat would look "+
			"like tampering", want, got)
	}

	// Changed: an index gains a column.
	changed := tamper(t, base, func(d map[string]any) {
		args := opsOf(t, d)[1].(map[string]any)["args"].(map[string]any)
		idx := args["Index"].(map[string]any)
		idx["Columns"] = append(idx["Columns"].([]any), map[string]any{"Name": "email"})
	})
	edited, err := migrate.Parse(changed)
	if err != nil {
		t.Fatalf("the edited artifact does not parse, so the checksum cannot be compared: %v", err)
	}
	after, err := edited.Checksum()
	if err != nil {
		t.Fatal(err)
	}
	if after == want {
		t.Error("changing an index's columns left the checksum unchanged, so a history is " +
			"not tamper-evident")
	}
}

// §23: impossible sequences fail at replay, deterministically, without leaving a
// half-built state behind.
func TestAuditArtifact_impossibleSequencesAreRefusedAtReplay(t *testing.T) {
	mv := totals(true)
	idx := totalsIndex()

	for _, c := range []struct {
		what string
		ops  []migrate.Operation
		// want names a fragment of the error, so the diagnostic is about the
		// operation rather than a panic recovered somewhere.
		want string
	}{
		{
			"the same materialized view created twice",
			[]migrate.Operation{
				migrate.CreateMaterializedView{View: mv},
				migrate.CreateMaterializedView{View: mv},
			},
			"already exists",
		},
		{
			"the same index created twice",
			[]migrate.Operation{
				migrate.CreateMaterializedView{View: mv},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: idx},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: idx},
			},
			"is already on",
		},
		{
			"an index dropped twice",
			[]migrate.Operation{
				migrate.CreateMaterializedView{View: mv},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: idx},
				migrate.DropIndex{Schema: "public", Table: "totals", Name: idx.Name},
				migrate.DropIndex{Schema: "public", Table: "totals", Name: idx.Name},
			},
			"is not on",
		},
		{
			"a materialized view dropped twice",
			[]migrate.Operation{
				migrate.CreateMaterializedView{View: mv},
				migrate.DropMaterializedView{Schema: "public", Name: "totals"},
				migrate.DropMaterializedView{Schema: "public", Name: "totals"},
			},
			"",
		},
		{
			"an index created after its relation was dropped",
			[]migrate.Operation{
				migrate.CreateMaterializedView{View: mv},
				migrate.DropMaterializedView{Schema: "public", Name: "totals"},
				migrate.CreateIndex{Schema: "public", Table: "totals", Index: idx},
			},
			"no table or materialized view",
		},
		{
			"an index dropped from a relation that does not own it",
			[]migrate.Operation{
				migrate.CreateMaterializedView{View: mv},
				migrate.DropIndex{Schema: "public", Table: "nowhere", Name: idx.Name},
			},
			"no table or materialized view",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			s := &schema.Schema{}
			var failed error
			for _, op := range c.ops {
				if err := op.Apply(s); err != nil {
					failed = err
					break
				}
			}
			if c.want == "" {
				// Replay accepts the sequence. What must then hold is that the
				// state is not corrupt: this is recorded rather than wished at,
				// because the layer that refuses is a design decision and the
				// state being wrong is not.
				if failed != nil {
					t.Logf("replay refused %s: %v", c.what, failed)
					return
				}
				assertStateIsCoherent(t, s, c.what)
				return
			}
			if failed == nil {
				t.Fatalf("%s replayed without error", c.what)
			}
			if !strings.Contains(failed.Error(), c.want) {
				t.Errorf("%s failed with %q, which does not name the problem", c.what, failed)
			}
		})
	}
}

// assertStateIsCoherent checks the replayed state holds nothing twice.
//
// A duplicate is the specific corruption this whole area exists to prevent: it is
// invisible in the database, which only ever saw one statement, and fatal in the
// state, where the next plan finds an object the declarations do not have.
func assertStateIsCoherent(t *testing.T, s *schema.Schema, what string) {
	t.Helper()
	seen := map[string]int{}
	for _, m := range s.MaterializedViews {
		seen[m.Qualified()]++
		idx := map[string]int{}
		for _, i := range m.Indexes {
			idx[i.Name]++
		}
		for name, n := range idx {
			if n != 1 {
				t.Errorf("%s: index %s appears %d times on %s in the replayed state",
					what, name, n, m.Qualified())
			}
		}
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("%s: relation %s appears %d times in the replayed state", what, name, n)
		}
	}
}

// A tampered artifact does not reach a database.
//
// The parse refusal above is the mechanism; this is the consequence, through the
// production loader: a directory containing one bad file yields no set at all,
// rather than a set missing one migration — which would silently plan against a
// history with a hole in it.
func TestAuditArtifact_aBadFileStopsTheWholeHistoryLoading(t *testing.T) {
	dir := t.TempDir()
	good := validArtifact(t)
	writeFileT(t, dir+"/0001_totals.json", string(good))
	bad := tamper(t, good, func(d map[string]any) {
		d["id"] = "0002_broken"
		opsOf(t, d)[0].(map[string]any)["op"] = "not_an_operation"
	})
	writeFileT(t, dir+"/0002_broken.json", string(bad))

	set, err := migrate.NewStore(dir).Load()
	if err == nil {
		t.Fatalf("a directory containing a malformed artifact loaded %d migration(s); "+
			"planning would then run against a history with a hole in it",
			len(set.Migrations()))
	}
	if !strings.Contains(err.Error(), "0002_broken") {
		t.Errorf("the error does not name the file that could not be read: %v", err)
	}
}

// writeFileT writes a file the audit needs, failing the test rather than the
// process.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
