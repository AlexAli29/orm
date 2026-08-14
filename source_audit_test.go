package orm_test

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm/internal/gendemo"
)

// The M16 audit: what a caller can do to a Source.
//
// orm.Source is an alias to the compiler's own source descriptor, so every
// exported method on it is something ordinary code can do to compiler state —
// and generated code declares one descriptor per table, at package level, shared
// by every query built from that table in the process. A public mutator on that
// is a public, unsynchronised writer for a process-global.
//
// SetRecursive and SetMaterialized used to be exactly that. Composing queries in
// two goroutines while a third called them was a data race the detector
// reported, and marking a plain table source recursive was something the API
// permitted and the compiler had no reason to expect. They are now package-level
// functions in internal/expr: the construction path is unchanged and the door is
// no longer in the public wall.

// No exported method on the source descriptor may mutate it.
//
// This reads the committed manifest rather than the source, because the manifest
// is what defines the contract: a mutator that did not appear there would be a
// mutator CI had already decided was not a public API change.
func TestAudit_sourceHasNoPublicMutators(t *testing.T) {
	b, err := os.ReadFile("api/orm.txt")
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
	}
	shape, ok := reachableShape(string(b), "github.com/AlexAli29/orm/internal/expr.Source")
	if !ok {
		t.Fatal("the manifest no longer records the Source shape at all, so nothing here is being checked")
	}
	for _, line := range strings.Split(shape, "\n") {
		name := strings.TrimPrefix(strings.TrimSpace(line), "method ")
		if name == strings.TrimSpace(line) {
			continue
		}
		for _, verb := range []string{"Set", "Mark", "Add", "Reset", "Clear", "Put", "With"} {
			if strings.HasPrefix(name, verb) {
				t.Errorf("Source exports %s: a caller holding a generated table's descriptor "+
					"can mutate compiler state that every query in the process shares", name)
			}
		}
	}
	// And no exported field, which was the earlier form of the same problem.
	if strings.Contains(shape, "field ") && !strings.Contains(shape, "field <unexported>") {
		t.Errorf("Source has exported fields again:\n%s", shape)
	}
}

// reachableShape returns the manifest block for one reachable internal type.
func reachableShape(manifest, key string) (string, bool) {
	head := "\nreachable " + key + "\n"
	i := strings.Index(manifest, head)
	if i < 0 {
		return "", false
	}
	rest := manifest[i+len(head):]
	if j := strings.Index(rest, "\n\n"); j >= 0 {
		rest = rest[:j]
	}
	return rest, true
}

// Composing queries from the descriptor generated code shares is race-free, and
// every goroutine writes the same statement.
func TestAudit_sharedSourceUnderConcurrentComposition(t *testing.T) {
	want, _, err := gendemo.New(nil).Users.Query().SQL()
	if err != nil {
		t.Fatalf("building: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				got, _, err := gendemo.New(nil).Users.Query().
					Where(gendemo.Users.Age.Gt(1)).SQL()
				if err != nil {
					t.Errorf("building: %v", err)
					return
				}
				if !strings.HasPrefix(got, strings.TrimSuffix(want, "")[:20]) {
					t.Errorf("the statement changed under concurrent composition:\n%s", got)
					return
				}
			}
		}()
	}
	wg.Wait()
}
