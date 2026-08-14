// Command releasecheck reports whether this repository is ready to be published.
//
// It exists because the two ways a release goes wrong are both invisible from
// inside the repository. A module that builds here can be unusable the moment it
// is tagged, because a local replace directive makes the repository's own layout
// stand in for a version that does not exist yet — and Go ignores a replace in a
// dependency's go.mod, so the consumer gets "unknown revision v0.0.0" and the
// maintainer gets a bug report. And a repository with no LICENSE is not open
// source at all, whatever its README says: the default is exclusive copyright,
// so publishing it grants nobody the right to use it.
//
// Neither is a compile error. Neither is a test failure. Both are found by
// someone else, after the tag.
//
// Usage:
//
//	go run ./internal/tools/releasecheck            # regressions only
//	go run ./internal/tools/releasecheck -release   # everything, before a tag
//
// The default mode is what CI runs: it fails on the things that are fixable
// today and must not regress, and it prints the governance blockers without
// failing on them, because a permanently red pipeline is one nobody reads.
// Release mode fails on everything, and is step one of the release checklist.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// publishable lists the modules that are meant to be tagged and consumed, and
// the modules that exist only inside this repository.
//
// The distinction is the point: the demo and the examples deliberately use
// module paths nobody can resolve, so that the compiler refuses to let them
// import internal packages. Tagging one would be a mistake, and listing them
// here is what lets this tool say so.
var (
	publishable   = []string{".", "ormotel", "ormtest/postgres"}
	unpublishable = []string{"ormextdemo", "examples/production", "examples/hexagonal"}
)

// rootModule is the module the optional ones depend on.
const rootModule = "github.com/AlexAli29/orm"

// placeholder is the version an untagged repository stands in for.
var placeholder = regexp.MustCompile(`^v0\.0\.0(-0\.\d+-[0-9a-f]+)?$`)

func main() {
	release := flag.Bool("release", false, "fail on the governance blockers too, not only on regressions")
	dir := flag.String("dir", ".", "repository root")
	flag.Parse()

	regressions, blockers := check(*dir)

	for _, p := range regressions {
		fmt.Fprintf(os.Stderr, "regression: %s\n", p)
	}
	for _, p := range blockers {
		fmt.Fprintf(os.Stderr, "blocker: %s\n", p)
	}

	if len(blockers) > 0 {
		fmt.Println("PUBLIC RELEASE GOVERNANCE BLOCKED")
	}
	switch {
	case len(regressions) > 0:
		os.Exit(1)
	case len(blockers) > 0 && *release:
		os.Exit(1)
	case len(blockers) > 0:
		fmt.Println("(not failing: these are owner decisions, not regressions; run -release before tagging)")
	default:
		fmt.Println("release ready")
	}
}

// check returns what must never regress and what an owner must decide.
func check(root string) (regressions, blockers []string) {
	if !exists(filepath.Join(root, "LICENSE")) {
		blockers = append(blockers,
			"there is no LICENSE file, so the default is exclusive copyright and publishing "+
				"this grants nobody the right to use it")
	}

	for _, m := range publishable {
		path := filepath.Join(root, filepath.FromSlash(m), "go.mod")
		body, err := os.ReadFile(path)
		if err != nil {
			regressions = append(regressions, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		text := string(body)

		// A replace pointing into this repository is fine while developing and
		// fatal once tagged, because a consumer never sees it.
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "replace ") && !strings.HasPrefix(line, "=> ") {
				continue
			}
			if strings.Contains(line, "=> .") || strings.Contains(line, "=> /") {
				blockers = append(blockers, fmt.Sprintf(
					"%s/go.mod has a local replace (%s): Go ignores a replace in a dependency's "+
						"go.mod, so a consumer of this module resolves the require instead — "+
						"bump it to the tagged version and drop the replace before tagging", m, line))
			}
		}

		// And the require it stands in for must not still be the placeholder.
		if m != "." {
			for _, line := range strings.Split(text, "\n") {
				// Both spellings: the block form indents the module, the
				// single-line form puts "require" in front of it. Parsing only
				// one of them is how a check passes a file it never read.
				f := strings.Fields(strings.TrimSpace(line))
				if len(f) >= 3 && f[0] == "require" {
					f = f[1:]
				}
				if len(f) >= 2 && f[0] == rootModule && placeholder.MatchString(f[1]) {
					blockers = append(blockers, fmt.Sprintf(
						"%s/go.mod requires %s %s, which is not a version anyone can resolve: "+
							"a consumer of this module would get \"unknown revision %s\"",
						m, rootModule, f[1], f[1]))
				}
			}
		}
	}

	// The modules that must never become publishable. A resolvable module path
	// here would mean the compiler had stopped refusing their internal imports.
	for _, m := range unpublishable {
		path := filepath.Join(root, filepath.FromSlash(m), "go.mod")
		body, err := os.ReadFile(path)
		if err != nil {
			continue // Not every checkout has the examples.
		}
		for _, line := range strings.Split(string(body), "\n") {
			f := strings.Fields(strings.TrimSpace(line))
			if len(f) == 2 && f[0] == "module" && strings.HasPrefix(f[1], rootModule) {
				regressions = append(regressions, fmt.Sprintf(
					"%s declares the module path %s, which is publishable and inside the ORM's "+
						"own namespace: that is what lets it import internal packages, and what "+
						"would let a release tag it", m, f[1]))
			}
		}
	}
	return regressions, blockers
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
