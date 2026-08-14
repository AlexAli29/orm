package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Proving a killer can observe anything before trusting its verdict.
//
// A mutation campaign has several ways to lie to itself. The first is a
// replacement that matches nothing, which apply refuses. The second is a killer
// that runs no tests: `go test` exits 0, the mutation appears to survive, and the
// class looks covered. That happened — a malformed -run pattern, assembled by
// concatenating two commands into one shell string, executed zero tests and the
// result was recorded as a survivor.
//
// The third is a killer whose tests all skip. Every fixture that matters in this
// project needs a PostgreSQL server and calls t.Skip when ORM_TEST_ADMIN_DSN or
// ORM_TEST_DSN_PG14..PG18 is unset. `go test -json` emits a "run" action for a
// skipped test exactly as it does for one that executes, and the package still
// exits 0 — so counting "run" actions certifies a killer that never connected to
// a database, never built a fixture and could not have observed any mutation. It
// is the same false green one level deeper, and it is why a test must be seen to
// pass rather than merely to start.
//
// Killers are argv vectors for the same reason. The concatenation that produced
// the first failure is only possible when a command is a string somebody
// assembles.

// testRun is what one `go test -json` invocation did.
type testRun struct {
	ran     map[string]bool
	passed  map[string]bool
	skipped map[string]bool
	failed  map[string]bool
	// green reports whether the process exited 0.
	green  bool
	stdout string
	stderr string
}

func (r testRun) counts() (ran, passed, skipped int) {
	return len(r.ran), len(r.passed), len(r.skipped)
}

// sortedNames returns a set's members in a stable order, so a refusal reads the
// same way on every run.
func sortedNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// under reports whether name is want or a subtest of it.
func under(want, name string) bool {
	return name == want || strings.HasPrefix(name, want+"/")
}

// goTest runs a command and parses `go test -json` from its output.
//
// The command is an argv vector. Nothing here builds a shell string, so the
// malformed-command failure cannot be reproduced by this tool.
func goTest(argv []string, dir string) (testRun, error) {
	if len(argv) == 0 {
		return testRun{}, fmt.Errorf("no command given")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	r := testRun{
		ran:     map[string]bool{},
		passed:  map[string]bool{},
		skipped: map[string]bool{},
		failed:  map[string]bool{},
		green:   runErr == nil,
		stdout:  stdout.String(),
		stderr:  stderr.String(),
	}
	// A line that is not JSON is ordinary build output, and is skipped rather
	// than treated as a failure: `go test -json` interleaves both.
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
	for sc.Scan() {
		var ev struct {
			Action string `json:"Action"`
			Test   string `json:"Test"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Test == "" {
			continue
		}
		switch ev.Action {
		case "run":
			r.ran[ev.Test] = true
		case "pass":
			r.passed[ev.Test] = true
		case "skip":
			r.skipped[ev.Test] = true
		case "fail":
			r.failed[ev.Test] = true
		}
	}
	return r, nil
}

// smokeResult is a successful validation, for a caller that wants the counts.
type smokeResult struct {
	ran, passed, skipped int
}

// smoke proves a command ran, and passed, the tests it names.
//
// The error it returns is the refusal; the caller decides which exit code names
// it, because the same failure means something different for a killer than for a
// fixture precondition.
func smoke(expected []string, argv []string, dir string) (smokeResult, error) {
	if len(expected) == 0 {
		return smokeResult{}, fmt.Errorf("no expected test named; a run with nothing to look for proves nothing")
	}
	r, err := goTest(argv, dir)
	if err != nil {
		return smokeResult{}, err
	}

	if len(r.ran) == 0 {
		return smokeResult{}, fmt.Errorf("ran 0 tests. `go test` exiting 0 is not evidence — a "+
			"malformed -run pattern matches nothing and every mutation then looks like a "+
			"survivor. command: %s\n%s%s",
			strings.Join(argv, " "), tail(r.stdout, 2000), tail(r.stderr, 2000))
	}
	if !r.green {
		return smokeResult{}, fmt.Errorf("the baseline is not green (%d ran, %d failed: %v), so a "+
			"later failure could not be attributed to the mutation\n%s%s",
			len(r.ran), len(r.failed), first(sortedNames(r.failed), 5),
			tail(r.stdout, 3000), tail(r.stderr, 2000))
	}
	for _, want := range expected {
		var matched []string
		for name := range r.ran {
			if under(want, name) {
				matched = append(matched, name)
			}
		}
		sort.Strings(matched)
		if len(matched) == 0 {
			return smokeResult{}, fmt.Errorf("expected test %q did not run; ran: %v",
				want, sortedNames(r.ran))
		}
		var gone []string
		for _, n := range matched {
			if r.skipped[n] {
				gone = append(gone, n)
			}
		}
		if len(gone) > 0 {
			return smokeResult{}, fmt.Errorf("expected test %q skipped rather than passing: %v. "+
				"A skipped test emits a run action and leaves the package green, so counting "+
				"runs would certify a killer that never built its fixture. This suite skips "+
				"when ORM_TEST_ADMIN_DSN or ORM_TEST_DSN_PG14..PG18 is unset", want, gone)
		}
		anyPassed := false
		for _, n := range matched {
			if r.passed[n] {
				anyPassed = true
				break
			}
		}
		if !anyPassed {
			return smokeResult{}, fmt.Errorf("expected test %q started but never reported a result: %v",
				want, matched)
		}
	}
	ran, passed, skipped := r.counts()
	return smokeResult{ran, passed, skipped}, nil
}

func cmdSmoke(args []string) {
	code := exitKillerExecution
	label := "killer smoke"
	if len(args) > 0 && args[0] == "--as-precondition" {
		code = exitFixturePrecondition
		label = "fixture precondition"
		args = args[1:]
	}
	split := -1
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	if split < 0 {
		fail(code, "usage: smoke [--as-precondition] <expected-test>... -- <argv>...")
	}
	expected := args[:split]
	argv := args[split+1:]
	if len(argv) == 0 {
		fail(code, "no command given")
	}

	got, err := smoke(expected, argv, "")
	if err != nil {
		fail(code, "%v", err)
	}
	fmt.Printf("%s ok: %d ran, %d passed, %d skipped; expected %v passed\n",
		label, got.ran, got.passed, got.skipped, expected)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func first[T any](s []T, n int) []T {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
