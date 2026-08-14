package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The level control.
//
// Every class is refuted at two levels: a narrow probe at the level of the code
// that changed, and a killer at the level of the guarantee a caller loses. The
// campaign proves both fail under the mutant, and the site's function name is
// checked against the anchor, so the level a class claims is honest.
//
// Neither of those proves the probe is sensitive to that function in particular.
// A probe broad enough to fail under a mutation anywhere passes containment and
// passes the campaign, and its verdict would then be about the codebase rather
// than about the code that changed. That is the failure the level claim is for,
// and it is the one neither existing check can see.
//
// So: a probe must survive a mutation in a different function. Same-function
// overlap is legitimate — two classes attacking one rule from two angles should
// share probes — and everything else is the claim itself, stated as an
// experiment rather than as an intention.
//
// The cross-product is small because the batches are. Probes are grouped by the
// package they run in, so one mutation costs one `go test` per package rather
// than one per probe.

// probeRef is one class's semantic probe, and the function its own mutation is
// in — which is what decides whether a given mutation should leave it alone.
type probeRef struct {
	id       string
	function string
	pkg      string
	tests    []string
	argvDir  string
}

func cmdCrossLevel(args []string) {
	fs := flag.NewFlagSet("crosslevel", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository to mutate")
	var manifests stringList
	fs.Var(&manifests, "manifest", "manifest to read (repeatable)")
	_ = fs.Parse(args)
	if len(manifests) == 0 {
		manifests = stringList{defaultManifest()}
	}

	type entry struct {
		id       string
		function string
		sites    []site
		probe    probeRef
		dir      string
	}
	var entries []entry
	for _, path := range manifests {
		m, err := loadManifest(path)
		if err != nil {
			fail(exitUsage, "%v", err)
		}
		dir := filepath.Dir(path)
		for _, c := range m.classes {
			sites, ok := c.sites()
			if !ok || len(sites) == 0 {
				continue
			}
			fn := sites[0].Function
			if fn == "" {
				fmt.Printf("skipped #%s: its sites do not name a function, so its level cannot be controlled\n", c.str("id"))
				continue
			}
			for _, s := range sites[1:] {
				if s.Function != fn {
					// A class spanning functions has no single level; its
					// probe is exempt rather than silently checked against one.
					fn = ""
				}
			}
			if fn == "" {
				fmt.Printf("skipped #%s: its sites are in different functions, so it has no single level\n", c.str("id"))
				continue
			}
			sem, ok := c.spec("semantic")
			if !ok {
				continue
			}
			entries = append(entries, entry{
				id: c.str("id"), function: fn, sites: sites, dir: dir,
				probe: probeRef{
					id: c.str("id"), function: fn,
					pkg:   packageOf(sem.Argv),
					tests: topLevel(sem.Expect),
				},
			})
		}
	}
	if len(entries) < 2 {
		fail(exitUsage, "the control needs at least two classes naming a function and %d were found", len(entries))
	}

	fmt.Printf("%d classes, controlling each probe against every mutation in another function\n", len(entries))
	violations := 0
	checked := 0
	for _, mut := range entries {
		// The probes that must survive this mutation: every one whose class
		// lives in another function.
		byPkg := map[string][]probeRef{}
		for _, other := range entries {
			if other.function == mut.function {
				continue
			}
			byPkg[other.probe.pkg] = append(byPkg[other.probe.pkg], other.probe)
		}
		if len(byPkg) == 0 {
			continue
		}
		bad := crossRun(*repo, mut.dir, mut.id, mut.sites, byPkg)
		for _, b := range bad {
			fmt.Printf("  #%s (%s) also fails %s, whose class #%s is in %s\n",
				mut.id, mut.function, b.test, b.probe, b.function)
			violations++
		}
		for _, ps := range byPkg {
			checked += len(ps)
		}
		fmt.Printf("#%s (%s): %d probes in other functions, %d disturbed\n",
			mut.id, mut.function, countProbes(byPkg), len(bad))
	}

	fmt.Printf("\n%d probe/mutation pairs controlled, %d violations\n", checked, violations)
	if violations > 0 {
		fmt.Println("a probe that fails under a mutation in another function is not evidence about its own;\n" +
			"narrow the probe, or record the pair as a justified overlap")
		os.Exit(exitResult)
	}
	fmt.Println("every probe survived every mutation outside its own function")
}

type violation struct{ test, probe, function string }

// crossRun applies one class's mutation, runs the other classes' probes grouped
// by package, and restores.
func crossRun(repo, dir, id string, sites []site, byPkg map[string][]probeRef) []violation {
	files := make([]string, 0, len(sites))
	for _, s := range sites {
		files = append(files, s.File)
	}
	defer func() { _, _ = git(repo, append([]string{"checkout", "--"}, files...)...) }()

	for _, s := range sites {
		count := s.Count
		if count == 0 {
			count = 1
		}
		anchor, err := os.ReadFile(filepath.Join(dir, s.Anchor))
		if err != nil {
			fail(exitHarness, "#%s: reading the anchor: %v", id, err)
		}
		replacement, err := os.ReadFile(filepath.Join(dir, s.Replacement))
		if err != nil {
			fail(exitHarness, "#%s: reading the replacement: %v", id, err)
		}
		if _, _, err := applyMutation(filepath.Join(repo, s.File), string(anchor), string(replacement), count); err != nil {
			fail(exitHarness, "#%s mutation (%s): %v", id, s.File, err)
		}
	}

	var bad []violation
	for pkg, probes := range byPkg {
		names := map[string]probeRef{}
		for _, p := range probes {
			for _, t := range p.tests {
				names[t] = p
			}
		}
		run := make([]string, 0, len(names))
		for t := range names {
			run = append(run, "^"+t+"$")
		}
		sort.Strings(run)
		argv := []string{"go", "test", "-json", "-count=1", pkg, "-run", strings.Join(run, "|")}
		r, err := goTest(argv, repo)
		if err != nil {
			fail(exitHarness, "#%s: running the other probes: %v", id, err)
		}
		for t, p := range names {
			// A probe that did not run at all is as bad as one that failed: the
			// control would be vacuous.
			if !r.ran[t] {
				bad = append(bad, violation{test: t + " (did not run)", probe: p.id, function: p.function})
				continue
			}
			if r.failed[t] {
				bad = append(bad, violation{test: t, probe: p.id, function: p.function})
			}
		}
	}
	sort.Slice(bad, func(i, j int) bool { return bad[i].test < bad[j].test })
	return bad
}

func countProbes(byPkg map[string][]probeRef) int {
	n := 0
	for _, ps := range byPkg {
		n += len(ps)
	}
	return n
}

// packageOf finds the package argument of a `go test` vector.
func packageOf(argv []string) string {
	for _, a := range argv[1:] {
		if a == "." || strings.HasPrefix(a, "./") {
			return a
		}
	}
	return "."
}

// topLevel strips subtest paths, because a probe is run by its top-level name.
func topLevel(expect []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(expect))
	for _, e := range expect {
		name := e
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[:i]
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// stringList collects a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }
