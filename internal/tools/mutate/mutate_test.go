package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The harness refuses every shape that once produced a false green.
//
// Each case here is a failure the campaign actually made and was hardened
// against. They are kept as tests of the tool rather than as history, because the
// tool was rewritten and a rewrite is exactly when a guard gets dropped by
// accident: the reasoning lives in the comments, and this is what stops the
// reasoning from being all that is left.
//
// Nothing here needs a database. The killer smoke cases run a throwaway module
// whose tests pass, skip or fail on purpose, which is enough to exercise every
// branch of the decision.

// probeModule writes a tiny module whose tests do exactly what a case needs.
func probeModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module probe\n\ngo 1.24\n")
	write("probe_test.go", `package probe

import "testing"

func TestPasses(t *testing.T) {}

func TestSkips(t *testing.T) { t.Skip("no server") }

func TestFails(t *testing.T) { t.Fatal("deliberately red") }

func TestParent(t *testing.T) {
	t.Run("child", func(t *testing.T) {})
}

func TestParentThatSkips(t *testing.T) {
	t.Run("child", func(t *testing.T) { t.Skip("no server") })
	t.Skip("no server")
}
`)
	return dir
}

func goTestArgv(run string) []string {
	return []string{"go", "test", "-count=1", "-json", ".", "-run", run}
}

// A killer that runs no tests is not evidence.
//
// `go test` exits 0 when a -run pattern matches nothing, so counting the exit
// status would certify a killer that never executed the code it claims to guard.
// This is the shape the malformed command produced: two commands concatenated
// into one shell string made the pattern match nothing.
func TestSmoke_refusesWhenNoTestRan(t *testing.T) {
	dir := probeModule(t)
	_, err := smoke([]string{"TestPasses"}, goTestArgv("^TestNoSuchThing$"), dir)
	if err == nil {
		t.Fatal("a run matching no tests was accepted")
	}
	if !strings.Contains(err.Error(), "ran 0 tests") {
		t.Errorf("the refusal does not say no tests ran: %v", err)
	}
}

// A killer whose tests all skip is not evidence either.
//
// This is the one level deeper. `go test -json` emits a run action for a skipped
// test exactly as it does for one that executes, and the package still exits 0 —
// so counting run actions certifies a killer that never built its fixture.
func TestSmoke_refusesWhenTheExpectedTestSkips(t *testing.T) {
	dir := probeModule(t)
	_, err := smoke([]string{"TestSkips"}, goTestArgv("^TestSkips$"), dir)
	if err == nil {
		t.Fatal("a killer whose only test skipped was accepted")
	}
	if !strings.Contains(err.Error(), "skipped rather than passing") {
		t.Errorf("the refusal does not name the skip: %v", err)
	}
}

// And a parent that skips after its subtests do, which is what a suite gated on
// an absent DSN looks like from outside.
func TestSmoke_refusesWhenASubtreeSkips(t *testing.T) {
	dir := probeModule(t)
	_, err := smoke([]string{"TestParentThatSkips"}, goTestArgv("^TestParentThatSkips$"), dir)
	if err == nil {
		t.Fatal("a killer whose subtree skipped was accepted")
	}
}

// A test that was never run cannot have observed anything.
func TestSmoke_refusesWhenTheExpectedTestNeverRan(t *testing.T) {
	dir := probeModule(t)
	_, err := smoke([]string{"TestSomethingElse"}, goTestArgv("^TestPasses$"), dir)
	if err == nil {
		t.Fatal("a killer naming a test it never ran was accepted")
	}
	if !strings.Contains(err.Error(), "did not run") {
		t.Errorf("the refusal does not say the expected test did not run: %v", err)
	}
}

// A baseline that is not green cannot attribute a later failure to the mutation.
func TestSmoke_refusesWhenTheBaselineIsRed(t *testing.T) {
	dir := probeModule(t)
	_, err := smoke([]string{"TestFails"}, goTestArgv("^TestFails$"), dir)
	if err == nil {
		t.Fatal("a killer whose baseline is red was accepted")
	}
	if !strings.Contains(err.Error(), "not green") {
		t.Errorf("the refusal does not name the red baseline: %v", err)
	}
}

// A run with nothing to look for proves nothing.
func TestSmoke_refusesWhenNothingIsExpected(t *testing.T) {
	dir := probeModule(t)
	if _, err := smoke(nil, goTestArgv("^TestPasses$"), dir); err == nil {
		t.Fatal("a smoke run naming no expected test was accepted")
	}
}

// And the shape that must be accepted, so the refusals above are not simply a
// harness that refuses everything.
func TestSmoke_acceptsATestThatRanAndPassed(t *testing.T) {
	dir := probeModule(t)
	got, err := smoke([]string{"TestPasses"}, goTestArgv("^TestPasses$"), dir)
	if err != nil {
		t.Fatalf("a passing test was refused: %v", err)
	}
	if got.ran != 1 || got.passed != 1 || got.skipped != 0 {
		t.Errorf("counts = %+v, want one ran, one passed, none skipped", got)
	}
	// A parent named alone is satisfied by the parent passing, which is what a
	// table-driven killer looks like.
	if _, err := smoke([]string{"TestParent"}, goTestArgv("^TestParent$"), dir); err != nil {
		t.Errorf("a passing parent with subtests was refused: %v", err)
	}
	// And a subtest may be named directly.
	if _, err := smoke([]string{"TestParent/child"}, goTestArgv("^TestParent$"), dir); err != nil {
		t.Errorf("a passing subtest named directly was refused: %v", err)
	}
}

// An anchor that matches nothing is a harness failure, never a survivor.
//
// A replacement matching nothing runs the real implementation, its killer passes,
// and the result is indistinguishable from a mutation the product survived.
func TestApply_refusesAnAnchorThatMatchesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc f() bool { return true }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := applyMutation(path, "this text is nowhere", "replacement", 1)
	if err == nil {
		t.Fatal("an anchor matching nothing was applied")
	}
	if !strings.Contains(err.Error(), "occurs 0 time(s)") {
		t.Errorf("the refusal does not report the count: %v", err)
	}
	assertUnchanged(t, path, "package p\n\nfunc f() bool { return true }\n")
}

// An anchor that matches a different number of times than declared is refused
// too, because the mutation would land somewhere its author did not describe.
func TestApply_refusesTheWrongAnchorCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.go")
	src := "package p\n\nfunc a() bool { return true }\nfunc b() bool { return true }\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := applyMutation(path, "return true", "return false", 1)
	if err == nil {
		t.Fatal("an anchor occurring twice was applied as though it occurred once")
	}
	if !strings.Contains(err.Error(), "occurs 2 time(s), expected 1") {
		t.Errorf("the refusal does not report both counts: %v", err)
	}
	assertUnchanged(t, path, src)

	// And the same anchor is applied when the count is declared honestly.
	if _, n, err := applyMutation(path, "return true", "return false", 2); err != nil || n != 2 {
		t.Errorf("an honestly declared count was refused: n=%d err=%v", n, err)
	}
}

// A replacement identical to its anchor changes nothing, and a mutation that
// changes nothing is an evidence failure rather than a no-op result.
func TestApply_refusesAReplacementThatChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.go")
	src := "package p\n\nfunc f() bool { return true }\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := applyMutation(path, "return true", "return true", 1)
	if err == nil {
		t.Fatal("a replacement identical to its anchor was applied")
	}
	if !strings.Contains(err.Error(), "identical") {
		t.Errorf("the refusal does not say the replacement changed nothing: %v", err)
	}
	assertUnchanged(t, path, src)
}

// A missing file is a harness failure rather than a panic.
func TestApply_refusesAMissingFile(t *testing.T) {
	_, _, err := applyMutation(filepath.Join(t.TempDir(), "absent.go"), "a", "b", 1)
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("a missing file produced %v", err)
	}
}

// The mutation really is written, so the campaign's next step has something to
// observe.
func TestApply_writesTheMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.go")
	if err := os.WriteFile(path, []byte("package p\n\nfunc f() bool { return true }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, n, err := applyMutation(path, "return true", "return false", 1)
	if err != nil {
		t.Fatalf("a well-formed mutation was refused: %v", err)
	}
	if n != 1 || !strings.Contains(msg, "1 occurrence") {
		t.Errorf("msg=%q n=%d", msg, n)
	}
	assertUnchanged(t, path, "package p\n\nfunc f() bool { return false }\n")
}

func assertUnchanged(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("the file is\n%q\nwant\n%q", got, want)
	}
}

// The exit codes are the externally observable distinction between an evidence
// failure and a product result.
//
// A caller that saw 4 recorded a killer-execution failure rather than a survivor,
// and 5 a fixture precondition rather than either. Collapsing them would put the
// campaign back where it started, so they are pinned through the real binary.
func TestExitCodes_nameTheKindOfRefusal(t *testing.T) {
	tool := buildTool(t)
	probe := probeModule(t)

	for _, c := range []struct {
		what string
		args []string
		want int
	}{
		{"a killer that ran nothing", append([]string{"smoke", "TestPasses", "--"},
			goTestArgv("^TestNoSuchThing$")...), exitKillerExecution},
		{"a killer whose test skipped", append([]string{"smoke", "TestSkips", "--"},
			goTestArgv("^TestSkips$")...), exitKillerExecution},
		{"a fixture precondition that cannot hold",
			append([]string{"smoke", "--as-precondition", "TestSkips", "--"},
				goTestArgv("^TestSkips$")...), exitFixturePrecondition},
		{"an anchor that matches nothing",
			[]string{"apply", filepath.Join(probe, "go.mod"), mustWrite(t, "nowhere"), mustWrite(t, "x"), "1"},
			exitHarness},
		{"a smoke run with no expected test",
			append([]string{"smoke", "--"}, goTestArgv("^TestPasses$")...), exitKillerExecution},
		{"an unknown subcommand", []string{"frobnicate"}, exitUsage},
	} {
		t.Run(c.what, func(t *testing.T) {
			cmd := exec.Command(tool, c.args...)
			cmd.Dir = probe
			_ = cmd.Run()
			if got := cmd.ProcessState.ExitCode(); got != c.want {
				t.Errorf("exit %d, want %d", got, c.want)
			}
		})
	}

	// And the accepted shape exits 0, so the codes above are a taxonomy rather
	// than a tool that always fails.
	cmd := exec.Command(tool, append([]string{"smoke", "TestPasses", "--"}, goTestArgv("^TestPasses$")...)...)
	cmd.Dir = probe
	if err := cmd.Run(); err != nil {
		t.Errorf("a valid smoke run exited non-zero: %v", err)
	}
}

func mustWrite(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildTool(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "mutate")
	cmd := exec.Command("go", "build", "-o", out, ".")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the tool: %v\n%s", err, b)
	}
	return out
}

// The manifest is historical evidence, and reading and writing it must not
// reshape it.
//
// Decoding into ordinary maps and writing back would sort every key, producing a
// diff across all forty-six classes in which a real change to one of them would
// be invisible. The round trip is checked on the real manifest, through a copy,
// so the file itself is never at risk.
func TestManifest_roundTripsWithoutReshaping(t *testing.T) {
	original, err := os.ReadFile(defaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(copyPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	m, err := loadManifest(copyPath)
	if err != nil {
		t.Fatalf("the manifest does not parse: %v", err)
	}
	if len(m.classes) != 47 {
		t.Fatalf("the manifest holds %d classes, want 47", len(m.classes))
	}
	if err := m.save(); err != nil {
		t.Fatal(err)
	}

	// The content is identical as data.
	var before, after any
	if err := json.Unmarshal(original, &before); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rewritten, &after); err != nil {
		t.Fatalf("the rewritten manifest does not parse: %v", err)
	}
	if !jsonEqual(before, after) {
		t.Error("a load and save changed the manifest's content")
	}
	// And the key order survived, which is what keeps the diff readable.
	if firstKeys(t, original) != firstKeys(t, rewritten) {
		t.Errorf("the top-level key order changed:\n before %s\n after  %s",
			firstKeys(t, original), firstKeys(t, rewritten))
	}
}

func firstKeys(t *testing.T, b []byte) string {
	t.Helper()
	o := newObject()
	if err := json.Unmarshal(b, o); err != nil {
		t.Fatal(err)
	}
	return strings.Join(o.keys, ",")
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// The campaign's accounting still closes, read by this tool.
func TestManifest_accountingStillCloses(t *testing.T) {
	m, err := loadManifest(defaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	tally := map[string]int{}
	for _, c := range m.classes {
		tally[c.str("result")]++
	}
	if got := len(m.classes); got != 47 {
		t.Errorf("total = %d, want 47", got)
	}
	if tally["CAUGHT"] != 45 {
		t.Errorf("CAUGHT = %d, want 45", tally["CAUGHT"])
	}
	if tally["TRUE_NO_OP"] != 2 {
		t.Errorf("TRUE_NO_OP = %d, want 2", tally["TRUE_NO_OP"])
	}
	for _, bad := range []string{"SURVIVED", "BROKEN", "UNATTEMPTED", "HARNESS_FAILURE",
		"SEMANTIC_EXPRESSION_FAILURE", "FIXTURE_PRECONDITION_FAILURE", "KILLER_EXECUTION_FAILURE"} {
		if tally[bad] != 0 {
			t.Errorf("%s = %d, want 0", bad, tally[bad])
		}
	}
	// The two defence-in-depth classes keep their classification and their
	// combined proofs.
	for _, pair := range [][2]string{{"20", "20C"}, {"38", "38C"}} {
		noop, ok := m.find(pair[0])
		if !ok {
			t.Fatalf("class %s is missing", pair[0])
		}
		if got := noop.str("result"); got != "TRUE_NO_OP" {
			t.Errorf("#%s is %s, want TRUE_NO_OP", pair[0], got)
		}
		if got := noop.str("combined_proof"); got != pair[1] {
			t.Errorf("#%s names %q as its combined proof, want %s", pair[0], got, pair[1])
		}
		proof, ok := m.find(pair[1])
		if !ok {
			t.Fatalf("class %s is missing", pair[1])
		}
		if got := proof.str("result"); got != "CAUGHT" {
			t.Errorf("#%s is %s, want CAUGHT", pair[1], got)
		}
	}
}

// Every declared mutation's anchor still matches its source exactly as declared.
//
// This is the anchor guard applied to the whole campaign at once: an anchor that
// stopped matching would make its class unreproducible, and the campaign would
// only find out the next time somebody ran it.
func TestManifest_everyAnchorStillMatches(t *testing.T) {
	root := filepath.Join(classDir(), "..", "..", "..")
	m, err := loadManifest(defaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, c := range m.classes {
		sites, ok := c.sites()
		if !ok {
			continue
		}
		for _, s := range sites {
			anchor, err := os.ReadFile(filepath.Join(classDir(), s.Anchor))
			if err != nil {
				t.Errorf("#%s: %v", c.str("id"), err)
				continue
			}
			src, err := os.ReadFile(filepath.Join(root, s.File))
			if err != nil {
				t.Errorf("#%s: %v", c.str("id"), err)
				continue
			}
			want := s.Count
			if want == 0 {
				want = 1
			}
			if got := strings.Count(string(src), string(anchor)); got != want {
				t.Errorf("#%s: the anchor occurs %d time(s) in %s, expected %d",
					c.str("id"), got, s.File, want)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no anchors were checked")
	}
	t.Logf("%d mutation site(s) still anchored", checked)
}
