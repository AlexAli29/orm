package main

import (
	"os"
	"strings"
	"testing"
)

// The UUID qualification runs from one script, and CI calls that script.
//
// The nested module's proof is only worth what its wiring is worth. Two things
// can rot here without any test noticing: CI can drift into its own inlined
// sequence, at which point running the script locally proves something CI does
// not gate on; and the step can go back to `go run ../cmd/orm`, which resolves
// the generator against the consumer module's graph and fails on a dependency
// that module has no business carrying.
//
// So both are asserted, from the root module, where they are in ./... and run
// without a database.
func TestQualifyScript_isTheShapeCIRuns(t *testing.T) {
	ci := readWorkflow(t)
	// Comments are stripped, because this file's own explanation of why the
	// generator is not run with go run contains the phrase it forbids.
	script := uncommented(readText(t, "../../uuidcompat/qualify.sh"))

	step := stepBody(t, ci, "uuid qualification")
	if !strings.Contains(step, "uuidcompat/qualify.sh") {
		t.Errorf("the uuid qualification step does not call qualify.sh, so what CI "+
			"gates on and what a developer runs are two sequences:\n%s", step)
	}
	// The step should be the call and not a second copy of the sequence.
	for _, inlined := range []string{"go test -count=1", "makemigrations", "CREATE DATABASE"} {
		if strings.Contains(step, inlined) {
			t.Errorf("the uuid qualification step inlines %q instead of leaving it to "+
				"qualify.sh:\n%s", inlined, step)
		}
	}

	// The script builds the CLI in the ORM module.
	if !strings.Contains(script, "go build -o") || !strings.Contains(script, "./cmd/orm") {
		t.Error("qualify.sh does not build the CLI from the ORM module")
	}
	if strings.Contains(script, "go run ../cmd/orm") {
		t.Error("qualify.sh runs the generator with go run from inside the consumer " +
			"module; that resolves it against a module graph which does not have " +
			"golang.org/x/tools, and adding it there would make a consumer carry " +
			"the tool's build dependencies")
	}
	// Every command the qualification claims to run has to be in it.
	for _, want := range []string{"migrate", "check --generated", "go test -count=1",
		"makemigrations --check", "go vet ./...", "./bootstrap"} {
		if !strings.Contains(script, want) {
			t.Errorf("qualify.sh does not run %q", want)
		}
	}
	// A script that stops at the first failure is the difference between a gate
	// and a log.
	if !strings.Contains(script, "set -euo pipefail") {
		t.Error("qualify.sh does not set -euo pipefail, so a failing step in the " +
			"middle would not fail the run")
	}
}

// uncommented drops whole-line shell comments and blank lines.
func uncommented(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func readWorkflow(t *testing.T) string {
	t.Helper()
	return readText(t, "../../.github/workflows/ci.yml")
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// stepBody returns the lines of the named workflow step, up to the next step.
func stepBody(t *testing.T, workflow, name string) string {
	t.Helper()
	lines := strings.Split(workflow, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "- name: "+name) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("the workflow has no step named %q", name)
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.Contains(lines[i], "- name: ") {
			return strings.Join(lines[start:i], "\n")
		}
	}
	return strings.Join(lines[start:], "\n")
}
