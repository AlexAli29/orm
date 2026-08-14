package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Running one class through the whole evidence model, or refusing.
//
// The campaign's failures have all had the same shape: a step nobody checked,
// whose absence looked exactly like a result about the product. A replacement
// that matched nothing. A killer that ran no tests. A killer whose tests all
// skipped. A fixture that did not contain the feature being attacked. Each time,
// the report said "survived" about a mutation that was never applied, never run,
// or never observable.
//
// So a class gets a product result only after every one of these is proven, in
// order, and any failure is recorded as its own state rather than as a survivor:
//
//	fixture precondition   the attacked structure is present, asserted on clean
//	                       code by a permanent test
//	killer smoke           the killer runs, and passes, the tests it names
//	source precondition    the anchor occurs exactly the expected number of times
//	source changed         the file really differs afterwards
//	semantic expression    a narrow probe shows the mutant expresses the intended
//	                       defect, at the level of the code that was changed
//	killer reachability    under the mutant the killer's own named tests fail,
//	                       rather than something else in the package
//	restore clean          the tree is byte-identical afterwards
//
// Only then is the result CAUGHT, SURVIVED or TRUE_NO_OP.

// spec is a declared command: the tests it must run, and the argv that runs them.
type spec struct {
	Expect []string `json:"expect"`
	Argv   []string `json:"argv"`
}

// site is one place a mutation is applied.
type site struct {
	File        string `json:"file"`
	Anchor      string `json:"anchor"`
	Replacement string `json:"replacement"`
	Count       int    `json:"count"`
	// Function names the function the anchor is inside, so that the level the
	// semantic probe has to sit at is written down rather than assumed. See
	// level.go for why. It is optional: the campaigns that predate it have
	// closed accounting.
	Function  string   `json:"function,omitempty"`
	Expresses []string `json:"expresses,omitempty"`
}

// class is one correctness class, exactly as the manifest holds it.
//
// The manifest is decoded into an ordered map rather than a struct so that a
// field this tool does not know about survives a rewrite. Historical evidence is
// not the tooling's to reshape.
type class struct {
	obj *object
}

func (c class) str(key string) string {
	var s string
	if raw, ok := c.obj.get(key); ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func (c class) spec(key string) (spec, bool) {
	raw, ok := c.obj.get(key)
	if !ok {
		return spec{}, false
	}
	var s spec
	if err := json.Unmarshal(raw, &s); err != nil {
		return spec{}, false
	}
	return s, true
}

// sites returns the mutation's sites. A class may declare one site or several:
// several is what a class needs when a property is guaranteed twice over by
// independent mechanisms, and the combined bypass is what distinguishes covered
// from merely unreachable.
func (c class) sites() ([]site, bool) {
	raw, ok := c.obj.get("mutation")
	if !ok {
		return nil, false
	}
	var many []site
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, true
	}
	var one site
	if err := json.Unmarshal(raw, &one); err != nil {
		return nil, false
	}
	return []site{one}, true
}

func (c *class) set(key string, value any) { _ = c.obj.set(key, value) }

// evidence is kept order-preserving too, so appending a dimension does not
// reshuffle the ones already recorded.
func (c class) evidence() *object {
	out := newObject()
	if raw, ok := c.obj.get("evidence"); ok {
		_ = json.Unmarshal(raw, out)
	}
	return out
}

// manifest is the authoritative list, kept in the order the file has it.
type manifest struct {
	path    string
	doc     *object
	classes []class
}

func loadManifest(path string) (*manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := &manifest{path: path, doc: newObject()}
	if err := json.Unmarshal(b, m.doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	raw, ok := m.doc.get("classes")
	if !ok {
		return nil, fmt.Errorf("%s has no classes", path)
	}
	var objs []*object
	if err := json.Unmarshal(raw, &objs); err != nil {
		return nil, fmt.Errorf("parsing the classes of %s: %w", path, err)
	}
	for _, o := range objs {
		m.classes = append(m.classes, class{obj: o})
	}
	return m, nil
}

func (m *manifest) find(id string) (*class, bool) {
	for i := range m.classes {
		if m.classes[i].str("id") == id {
			return &m.classes[i], true
		}
	}
	return nil, false
}

// save writes the manifest back, preserving key order and the two-space indent
// the file already uses.
func (m *manifest) save() error {
	objs := make([]*object, 0, len(m.classes))
	for _, c := range m.classes {
		objs = append(objs, c.obj)
	}
	if err := m.doc.set("classes", objs); err != nil {
		return err
	}
	out, err := indent(m.doc)
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, out, 0o644)
}

// git runs a git command in a directory and returns its stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

func gitClean(dir string) bool {
	out, err := git(dir, "status", "--porcelain")
	return err == nil && strings.TrimSpace(out) == ""
}

type runFlags struct {
	repo     string
	manifest string
}

// parseRunFlags reads the class id and the flags, in any order.
//
// Go's flag package stops at the first non-flag argument, so `run 13 -repo X`
// would silently ignore -repo and mutate the wrong tree. The Python this
// replaces accepted either order, and a caller who typed the old order and got a
// run against the wrong repository would not be told. So the id is lifted out
// first and the rest is parsed as flags.
func parseRunFlags(name string, args []string) (string, runFlags) {
	valueFlags := map[string]bool{"-repo": true, "--repo": true, "-manifest": true, "--manifest": true}
	id := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			rest = append(rest, a)
		case valueFlags[a]:
			rest = append(rest, a)
			if i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		case strings.HasPrefix(a, "-"):
			rest = append(rest, a)
		case id == "":
			id = a
		default:
			rest = append(rest, a)
		}
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var f runFlags
	fs.StringVar(&f.repo, "repo", ".", "the tree to mutate and run tests in")
	fs.StringVar(&f.manifest, "manifest", defaultManifest(), "the authoritative manifest")
	_ = fs.Parse(rest)
	if id == "" {
		fail(exitUsage, "a class id is required")
	}
	return id, f
}

func cmdVerify(args []string) {
	id, f := parseRunFlags("verify", args)
	m, err := loadManifest(f.manifest)
	if err != nil {
		fail(exitUsage, "%v", err)
	}
	c, ok := m.find(id)
	if !ok {
		fail(exitUsage, "no class %s in the manifest", id)
	}
	killer, has := c.spec("killer")
	if !has {
		// A true no-op has no killer by definition: the mutation changes nothing
		// any test could observe, so there is no test that would go red under it.
		// What proves the class is the combined bypass beside it, which is a class
		// of its own and is verified as one.
		if c.str("result") == "TRUE_NO_OP" {
			proof := c.str("combined_proof")
			if proof == "" {
				fail(exitUsage, "class %s is TRUE_NO_OP and names no combined proof. A no-op "+
					"with nothing standing behind it is indistinguishable from a class nobody "+
					"attacked", id)
			}
			fmt.Printf("#%s: TRUE_NO_OP, proven by %s\n", id, proof)
			ev := c.evidence()
			ev.set("killer_smoke", "n/a — proven by "+proof)
			c.set("evidence", ev)
			if err := m.save(); err != nil {
				fail(exitUsage, "%v", err)
			}
			return
		}
		fail(exitUsage, "class %s declares no killer. A terminal class whose killer is not "+
			"written down cannot be validated, and must be reopened", id)
	}
	got, err := smoke(killer.Expect, killer.Argv, f.repo)
	if err != nil {
		fail(exitKillerExecution, "#%s killer smoke: %v", id, err)
	}
	fmt.Printf("killer smoke ok: %d ran, %d passed, %d skipped; expected %v passed\n",
		got.ran, got.passed, got.skipped, killer.Expect)
	ev := c.evidence()
	ev.set("killer_smoke", true)
	ev.set("killer_ran", got.ran)
	ev.set("killer_passed", got.passed)
	ev.set("killer_skipped", got.skipped)
	c.set("evidence", ev)
	if err := m.save(); err != nil {
		fail(exitUsage, "%v", err)
	}
}

func cmdRun(args []string) {
	id, f := parseRunFlags("run", args)
	m, err := loadManifest(f.manifest)
	if err != nil {
		fail(exitUsage, "%v", err)
	}
	c, ok := m.find(id)
	if !ok {
		fail(exitUsage, "no class %s in the manifest", id)
	}
	for _, key := range []string{"precondition", "semantic", "killer"} {
		if _, has := c.spec(key); !has {
			fail(exitUsage, "class %s declares no %s", id, key)
		}
	}
	sites, has := c.sites()
	if !has {
		fail(exitUsage, "class %s declares no mutation", id)
	}
	precondition, _ := c.spec("precondition")
	semantic, _ := c.spec("semantic")
	killer, _ := c.spec("killer")
	ev := c.evidence()

	if !gitClean(f.repo) {
		fail(exitUsage, "%s is not clean before the mutation; evidence from a tree carrying "+
			"unknown changes is not attributable", f.repo)
	}

	// 1. The fixture contains what the mutation attacks.
	got, err := smoke(precondition.Expect, precondition.Argv, f.repo)
	if err != nil {
		fail(exitFixturePrecondition, "#%s fixture precondition: %v", id, err)
	}
	fmt.Printf("precondition: fixture precondition ok: %d ran, %d passed, %d skipped; expected %v passed\n",
		got.ran, got.passed, got.skipped, precondition.Expect)
	ev.set("fixture_precondition", true)
	ev.set("precondition_ran", got.ran)
	ev.set("precondition_passed", got.passed)
	ev.set("precondition_skipped", got.skipped)

	// 2. The killer runs, and passes, the tests it names, on clean code.
	got, err = smoke(killer.Expect, killer.Argv, f.repo)
	if err != nil {
		fail(exitKillerExecution, "#%s killer smoke: %v", id, err)
	}
	fmt.Printf("killer smoke: killer smoke ok: %d ran, %d passed, %d skipped; expected %v passed\n",
		got.ran, got.passed, got.skipped, killer.Expect)
	ev.set("killer_smoke", true)
	ev.set("killer_ran", got.ran)
	ev.set("killer_passed", got.passed)
	ev.set("killer_skipped", got.skipped)

	// 3. The semantic probe passes on clean code, so its later failure is the
	//    mutation rather than a test that was always red.
	got, err = smoke(semantic.Expect, semantic.Argv, f.repo)
	if err != nil {
		fail(exitSemanticExpression, "#%s semantic probe on clean code: %v", id, err)
	}
	fmt.Printf("semantic baseline: killer smoke ok: %d ran, %d passed, %d skipped; expected %v passed\n",
		got.ran, got.passed, got.skipped, semantic.Expect)
	ev.set("semantic_ran", got.ran)
	ev.set("semantic_passed", got.passed)
	ev.set("semantic_skipped", got.skipped)

	result, restoreClean := runMutation(f.repo, id, sites, semantic, killer, ev)

	// A class declared a no-op is one whose mutation the rest of the system
	// absorbs: production can never supply the input the removed rule guarded
	// against, so no fixture can observe it. That is a claim rather than an
	// excuse, and it only stands while the combined bypass beside it is CAUGHT —
	// the bypass supplies the input directly and shows the rule is load-bearing
	// rather than dead. If the bypass ever stops catching, this becomes a
	// survivor again.
	if result == "SURVIVED" && c.str("expect_result") == "TRUE_NO_OP" {
		proof := c.str("combined_proof")
		proven, ok := m.find(proof)
		switch {
		case proof == "" || !ok:
			fmt.Printf("SURVIVED: declared TRUE_NO_OP, but its combined proof %q is absent, so "+
				"nothing shows the rule is needed at all\n", proof)
		case proven.str("result") != "CAUGHT":
			fmt.Printf("SURVIVED: declared TRUE_NO_OP, but its combined proof %s is %s rather "+
				"than CAUGHT, so nothing shows the rule is needed at all\n",
				proof, proven.str("result"))
		default:
			result = "TRUE_NO_OP"
			fmt.Printf("TRUE_NO_OP: the killer passed and the rule is shown load-bearing by %s\n", proof)
		}
	}

	ev.set("restore_clean", restoreClean)
	c.set("evidence", ev)
	c.set("result", result)
	if err := m.save(); err != nil {
		fail(exitUsage, "%v", err)
	}
	fmt.Printf("#%s: %s  restore_clean=%v\n", id, result, restoreClean)
	// Anything but CAUGHT exits non-zero, so a caller looping over classes stops
	// on a no-op as well as on a survivor: both are results a person must read.
	if result != "CAUGHT" {
		os.Exit(exitResult)
	}
}

// runMutation applies, observes and restores, returning the class's result.
//
// The restore happens whatever else does, because a tree left mutated would make
// every later class's evidence unattributable.
func runMutation(repo, id string, sites []site, semantic, killer spec, ev *object) (result string, restoreClean bool) {
	files := make([]string, 0, len(sites))
	for _, s := range sites {
		files = append(files, s.File)
	}
	defer func() {
		_, _ = git(repo, append([]string{"checkout", "--"}, files...)...)
		restoreClean = gitClean(repo)
		if !restoreClean {
			out, _ := git(repo, "status", "--porcelain")
			fmt.Fprint(os.Stderr, out)
		}
	}()

	// 4. Apply, and prove the source really changed.
	for _, s := range sites {
		count := s.Count
		if count == 0 {
			count = 1
		}
		anchor, err := os.ReadFile(filepath.Join(classDir(), s.Anchor))
		if err != nil {
			fail(exitHarness, "#%s: reading the anchor: %v", id, err)
		}
		replacement, err := os.ReadFile(filepath.Join(classDir(), s.Replacement))
		if err != nil {
			fail(exitHarness, "#%s: reading the replacement: %v", id, err)
		}
		if err := checkSiteFunction(filepath.Join(repo, s.File), s.Function, string(anchor)); err != nil {
			fail(exitHarness, "#%s: %v", id, err)
		}
		msg, _, err := applyMutation(filepath.Join(repo, s.File), string(anchor), string(replacement), count)
		if err != nil {
			fail(exitHarness, "#%s mutation (%s): %v", id, s.File, err)
		}
		fmt.Println("mutation:", msg)
	}
	ev.set("source_precondition", true)
	ev.set("mutation_sites", files)

	diff, _ := git(repo, append([]string{"diff", "--stat", "--"}, files...)...)
	changed := 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.Contains(line, "|") {
			changed++
		}
	}
	if changed != len(sites) {
		fail(exitHarness, "#%s: git reports %d changed file(s) for %d mutation site(s):\n%s",
			id, changed, len(sites), diff)
	}
	ev.set("source_changed", true)
	fmt.Println("source changed:", strings.TrimSpace(diff))

	// 5. The mutant expresses the intended defect, observed narrowly.
	r, err := goTest(semantic.Argv, repo)
	if err != nil {
		fail(exitSemanticExpression, "#%s: running the semantic probe: %v", id, err)
	}
	named := namedFailures(r, semantic.Expect)
	if len(named) == 0 {
		fmt.Fprintf(os.Stderr, "\n=== #%s SEMANTIC EXPRESSION FAILED ===\n", id)
		fmt.Fprintf(os.Stderr, "the probe still passes under the mutant, so the mutation does not "+
			"express the defect it claims. ran=%d passed=%d failed=%v skipped=%v\n%s\n",
			len(r.ran), len(r.passed), sortedNames(r.failed), sortedNames(r.skipped),
			tail(r.stdout, 3000))
		os.Exit(exitSemanticExpression)
	}
	ev.set("semantic_expression", true)
	ev.set("semantic_failures_under_mutant", named)
	fmt.Println("semantic expression:", named)

	// 6. The killer's own named tests fail, rather than something else.
	r, err = goTest(killer.Argv, repo)
	if err != nil {
		fail(exitKillerExecution, "#%s: running the killer: %v", id, err)
	}
	named = namedFailures(r, killer.Expect)
	ev.set("killer_reachability", len(named) > 0)
	ev.set("killer_failures_under_mutant", named)
	switch {
	case len(named) > 0:
		fmt.Printf("CAUGHT: %v\n", named)
		return "CAUGHT", false
	case !r.green:
		fmt.Printf("the killer package failed but none of its named tests did: failed=%v\n",
			sortedNames(r.failed))
		return "KILLER_EXECUTION_FAILURE", false
	default:
		// A class declared a no-op is one whose mutation the rest of the system
		// absorbs: production can never supply the input the removed rule was
		// guarding against, so no fixture can observe it. That is a claim, not an
		// excuse, and it only stands while the combined bypass beside it is
		// CAUGHT — the bypass supplies the input directly and shows the rule is
		// load-bearing rather than dead.
		fmt.Printf("SURVIVED: the killer passed under the mutant (%d ran, %d passed)\n",
			len(r.ran), len(r.passed))
		return "SURVIVED", false
	}
}

// namedFailures returns the failing tests the caller named, so a package that
// went red for an unrelated reason is not mistaken for a kill.
func namedFailures(r testRun, expect []string) []string {
	var out []string
	for name := range r.failed {
		for _, want := range expect {
			if under(want, name) {
				out = append(out, name)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}
