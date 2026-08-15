package workflows_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The workflow-path check.
//
// A step that names a directory which does not exist fails, but it fails
// twenty-five minutes into a run and only once every step before it passes.
// `go test ./gen/migrate/` sat in this workflow from the first commit and was
// never reached, because the step before it was failing for an unrelated
// reason; fixing that one is what finally surfaced this one. The feedback loop
// for a typo should not be a CI run.
//
// Only paths written as ./something are checked. Everything else in a workflow
// is a shell word this has no business interpreting, and a checker that guessed
// would produce failures nobody could act on.

var (
	relPath = regexp.MustCompile(`(?:^|[\s'"=(])\./([A-Za-z0-9_./-]+)`)
	// Go's wildcard is a pattern, not a directory: ./... means this module and
	// ./internal/gen/... means that subtree.
	wildcard = regexp.MustCompile(`/?\.\.\.$`)
)

func TestWorkflows_everyPathExists(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	dir := filepath.Join(root, ".github", "workflows")

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the workflows: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no workflows found; this check is looking in the wrong place")
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}

		for i, line := range strings.Split(string(body), "\n") {
			for _, m := range relPath.FindAllStringSubmatch(line, -1) {
				p := wildcard.ReplaceAllString(strings.TrimSuffix(m[1], "/"), "")
				if p == "" || p == "." {
					continue // ./... is the whole module
				}
				checked++
				if _, err := os.Stat(filepath.Join(root, p)); err != nil {
					t.Errorf("%s:%d names ./%s, which does not exist", e.Name(), i+1, p)
				}
			}
		}
	}

	t.Logf("checked %d paths across the workflows", checked)
}
