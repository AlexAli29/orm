package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// M16.5 compatibility: migration artifacts written before this milestone.
//
// The artifact format is frozen and the code that reads it is not: index
// comparison moved into a package of its own during this milestone, and the
// replay path was changed by G2 before that. A migration a user committed months
// ago has to keep parsing, keep checksumming to the same value, and keep
// replaying to the same state — otherwise an upgrade of the tool silently
// invalidates a project's history, and the symptom is a checksum mismatch on a
// migration nobody touched.
//
// The repository's example projects carry committed artifacts from before this
// work. They are the only genuinely historical ones available, so they are what
// this reads — as bytes, from the repository, without rewriting them.

// repoRoot finds the module root from the test's own location, so this runs
// wherever the repository is checked out.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root: %v", root, err)
	}
	return root
}

// historicalArtifacts returns every committed migration artifact in the
// repository, with the commit that last touched it.
func historicalArtifacts(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRoot(t)
	found := map[string][]string{}
	examples := filepath.Join(root, "examples")
	entries, err := os.ReadDir(examples)
	if err != nil {
		t.Fatalf("reading %s: %v", examples, err)
	}
	for _, e := range entries {
		dir := filepath.Join(examples, e.Name(), "migrations")
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".json") {
				found[dir] = append(found[dir], f.Name())
			}
		}
	}
	if len(found) == 0 {
		t.Fatal("no committed migration artifacts were found, so this proves nothing about " +
			"artifacts written before this milestone")
	}
	return found
}

// Every committed artifact still parses, checksums to a stable value, and
// replays.
func TestCompatHistory_committedArtifactsStillParseAndReplay(t *testing.T) {
	dirs := historicalArtifacts(t)
	total := 0

	for dir, files := range dirs {
		for _, name := range files {
			path := filepath.Join(dir, name)
			t.Run(filepath.Base(dir)+"/"+name, func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				m, err := migrate.Parse(raw)
				if err != nil {
					t.Fatalf("an artifact committed before this milestone no longer parses: %v", err)
				}
				if len(m.Operations) == 0 {
					t.Fatal("the artifact carries no operations, so replaying it proves nothing")
				}

				// The checksum is computed over the operations rather than the
				// bytes, so it must survive a round trip through the writer this
				// version of the tool would use. A change here invalidates every
				// history in the wild.
				sum, err := m.Checksum()
				if err != nil {
					t.Fatalf("checksumming: %v", err)
				}
				rendered, err := migrate.Render(m)
				if err != nil {
					t.Fatalf("rendering: %v", err)
				}
				back, err := migrate.Parse(rendered)
				if err != nil {
					t.Fatalf("re-parsing what this version renders: %v", err)
				}
				again, err := back.Checksum()
				if err != nil {
					t.Fatalf("re-checksumming: %v", err)
				}
				if again != sum {
					t.Errorf("the checksum changed across a round trip through this version: "+
						"%s -> %s. Every project holding this migration would report a "+
						"mismatch on a file nobody edited", sum, again)
				}

				// And it replays: the operations rebuild a state without error.
				s := &schema.Schema{}
				for i, op := range m.Operations {
					if err := op.Apply(s); err != nil {
						t.Fatalf("operation %d (%s) no longer replays: %v", i, op.Describe(), err)
					}
				}
				if len(s.Tables)+len(s.Views)+len(s.MaterializedViews)+len(s.Enums) == 0 {
					t.Error("replaying the artifact produced an empty state")
				}
				total++
			})
		}
	}
	if total == 0 {
		t.Fatal("no artifact was replayed")
	}
	t.Logf("%d committed artifact(s) parsed, checksummed and replayed", total)
}

// The bytes on disk are not rewritten by reading them.
//
// A test that made an old artifact pass by regenerating it would prove the
// opposite of what it claims, so this reads the file, does the work, and
// requires the file to be byte-identical afterwards.
func TestCompatHistory_readingAnArtifactDoesNotRewriteIt(t *testing.T) {
	dirs := historicalArtifacts(t)
	for dir, files := range dirs {
		for _, name := range files {
			path := filepath.Join(dir, name)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			set, err := migrate.NewStore(dir).Load()
			if err != nil {
				t.Fatalf("loading %s: %v", dir, err)
			}
			if len(set.Migrations()) == 0 {
				t.Fatalf("%s loaded no migrations", dir)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Errorf("%s changed on disk when it was read", path)
			}
		}
	}
}

// The state the whole committed history describes is still computable.
//
// Store.Load verifies each artifact round-trips to the same checksum as it goes,
// so a history that loads is a history whose every file this version agrees
// with; State then replays all of them in dependency order.
func TestCompatHistory_everyCommittedHistoryStillComputesItsState(t *testing.T) {
	for dir := range historicalArtifacts(t) {
		t.Run(filepath.Base(filepath.Dir(dir)), func(t *testing.T) {
			set, err := migrate.NewStore(dir).Load()
			if err != nil {
				t.Fatalf("a committed history no longer loads: %v", err)
			}
			s, err := set.State()
			if err != nil {
				t.Fatalf("a committed history no longer replays to a state: %v", err)
			}
			if len(s.Tables) == 0 {
				t.Errorf("replaying %s produced no tables", dir)
			}
			// Every relation the history created is present exactly once, which
			// is the duplicate-state defect seen against real committed files.
			seen := map[string]int{}
			for _, tbl := range s.Tables {
				seen[tbl.Qualified()]++
				idx := map[string]int{}
				for _, i := range tbl.Indexes {
					idx[i.Name]++
				}
				for name, n := range idx {
					if n != 1 {
						t.Errorf("%s: index %s appears %d times in the replayed state",
							tbl.Qualified(), name, n)
					}
				}
			}
			for name, n := range seen {
				if n != 1 {
					t.Errorf("relation %s appears %d times in the replayed state", name, n)
				}
			}
		})
	}
}
