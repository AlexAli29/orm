package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Whether every anchor still matches.
//
// An anchor is a literal quotation of the code it mutates, so editing that code
// silently invalidates it. The campaign already refuses a mutation that matches
// nothing — "a mutation that matches nothing runs the real implementation and
// its killer passes, which looks exactly like a survivor" — but it refuses it
// one class at a time, in the middle of a run, after the tree has been touched.
//
// This asks the same question of every class in one command and touches nothing,
// so a change to production code can be followed by a single check rather than a
// campaign that stops partway. It is the difference between finding out and
// finding out early.
func cmdAnchors(args []string) {
	fs := flag.NewFlagSet("anchors", flag.ExitOnError)
	repo := fs.String("repo", ".", "repository the anchors quote")
	var manifests stringList
	fs.Var(&manifests, "manifest", "manifest to check (repeatable)")
	_ = fs.Parse(args)
	if len(manifests) == 0 {
		manifests = stringList{defaultManifest()}
	}

	checked, stale := 0, 0
	for _, path := range manifests {
		m, err := loadManifest(path)
		if err != nil {
			fail(exitUsage, "%v", err)
		}
		dir := filepath.Dir(path)
		name := filepath.Base(path)
		for _, c := range m.classes {
			sites, ok := c.sites()
			if !ok {
				continue
			}
			for i, s := range sites {
				checked++
				where := c.str("id")
				if len(sites) > 1 {
					where = fmt.Sprintf("%s site %d", where, i+1)
				}
				anchor, err := os.ReadFile(filepath.Join(dir, s.Anchor))
				if err != nil {
					fmt.Printf("%s #%s: reading the anchor: %v\n", name, where, err)
					stale++
					continue
				}
				src, err := os.ReadFile(filepath.Join(*repo, s.File))
				if err != nil {
					fmt.Printf("%s #%s: reading %s: %v\n", name, where, s.File, err)
					stale++
					continue
				}
				want := s.Count
				if want == 0 {
					want = 1
				}
				if got := strings.Count(string(src), string(anchor)); got != want {
					fmt.Printf("%s #%s: the anchor occurs %d time(s) in %s, expected %d\n",
						name, where, got, s.File, want)
					stale++
					continue
				}
				if err := checkSiteFunction(filepath.Join(*repo, s.File), s.Function, string(anchor)); err != nil {
					fmt.Printf("%s #%s: %v\n", name, where, err)
					stale++
				}
			}
		}
	}

	fmt.Printf("%d anchors checked, %d stale\n", checked, stale)
	if stale > 0 {
		fmt.Println("an anchor that no longer matches would make its class look like a survivor;\n" +
			"requote it against the code as it is now, and rerun the class")
		os.Exit(exitResult)
	}
	fmt.Println("every anchor still quotes the code it mutates")
}
