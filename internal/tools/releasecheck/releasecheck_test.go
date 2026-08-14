package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The M16 audit: the release gate is itself tested.
//
// A gate that reports nothing is indistinguishable from a repository with no
// problems, and it is the more likely of the two. So each blocker class is
// reproduced in a temporary tree, and each must be found.

func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// clean is a repository with nothing wrong: licensed, tagged, no local replaces.
func clean() map[string]string {
	return map[string]string{
		"LICENSE": "MIT or whatever the owner picks.\n",
		"go.mod":  "module github.com/AlexAli29/orm\n\ngo 1.24\n",
		"ormotel/go.mod": "module github.com/AlexAli29/orm/ormotel\n\ngo 1.24\n\n" +
			"require github.com/AlexAli29/orm v1.2.3\n",
		"ormtest/postgres/go.mod": "module github.com/AlexAli29/orm/ormtest/postgres\n\ngo 1.25.0\n\n" +
			"require github.com/AlexAli29/orm v1.2.3\n",
		"ormextdemo/go.mod":          "module example.com/ormextdemo\n\ngo 1.24\n",
		"examples/production/go.mod": "module example.com/production\n\ngo 1.25.0\n",
		"examples/hexagonal/go.mod":  "module example.com/hexagonal\n\ngo 1.25.0\n",
	}
}

func TestCheck_cleanRepositoryReportsNothing(t *testing.T) {
	regressions, blockers := check(repo(t, clean()))
	if len(regressions) > 0 || len(blockers) > 0 {
		t.Errorf("a clean repository reported problems:\n%v\n%v", regressions, blockers)
	}
}

func TestCheck_findsTheBlockers(t *testing.T) {
	for _, c := range []struct {
		what   string
		mutate func(m map[string]string)
		want   string
	}{
		{
			"a missing LICENSE",
			func(m map[string]string) { delete(m, "LICENSE") },
			"no LICENSE file",
		},
		{
			"a local replace in a publishable module",
			func(m map[string]string) { m["ormotel/go.mod"] += "\nreplace github.com/AlexAli29/orm => ../\n" },
			"local replace",
		},
		{
			"a placeholder require in a publishable module",
			func(m map[string]string) {
				m["ormotel/go.mod"] = strings.Replace(m["ormotel/go.mod"], "v1.2.3", "v0.0.0", 1)
			},
			"unknown revision",
		},
		{
			"a pseudo-version standing in for an untagged root",
			func(m map[string]string) {
				m["ormtest/postgres/go.mod"] = strings.Replace(
					m["ormtest/postgres/go.mod"], "v1.2.3", "v0.0.0-0.20240101000000-abcdef123456", 1)
			},
			"unknown revision",
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			files := clean()
			c.mutate(files)
			_, blockers := check(repo(t, files))
			if !anyContains(blockers, c.want) {
				t.Errorf("%s was not reported; blockers were:\n%v", c.what, blockers)
			}
		})
	}
}

// A demo or example module that moved into the ORM's own namespace would be
// publishable, and would stop the compiler refusing its internal imports. That
// is a regression rather than an owner decision, so it fails the default run.
func TestCheck_findsAPublishableExample(t *testing.T) {
	files := clean()
	files["ormextdemo/go.mod"] = "module github.com/AlexAli29/orm/ormextdemo\n\ngo 1.24\n"
	regressions, _ := check(repo(t, files))
	if !anyContains(regressions, "publishable") {
		t.Errorf("a demo module inside the ORM's namespace was not reported: %v", regressions)
	}
}

// And the real repository: whatever its governance state, the regressions must
// be empty. This is what CI runs.
func TestCheck_thisRepositoryHasNoRegressions(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	regressions, blockers := check(root)
	if len(regressions) > 0 {
		t.Errorf("this repository has release regressions:\n%s", strings.Join(regressions, "\n"))
	}
	t.Logf("open governance blockers (owner decisions, expected pre-v1):\n%s", strings.Join(blockers, "\n"))
}

func anyContains(xs []string, want string) bool {
	for _, x := range xs {
		if strings.Contains(x, want) {
			return true
		}
	}
	return false
}
