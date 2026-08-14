package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
)

// Recomputing the campaign's accounting from the manifest.
//
// A previous report said thirty-one attempted and seventeen unattempted against
// forty-six intended. Those do not add up, and the arithmetic was counted from
// narrative rather than from a list. This counts from the list.

// requiredDimensions is what a terminal class must have proven.
//
// A TRUE_NO_OP has no killer failure to point at by definition; its proof is the
// combined bypass recorded as its own class, so killer reachability is not
// required of it.
var requiredDimensions = []string{
	"source_precondition", "source_changed", "semantic_expression",
	"fixture_precondition", "killer_smoke", "killer_reachability", "restore_clean",
}

func cmdTotals(args []string) {
	fs := flag.NewFlagSet("totals", flag.ExitOnError)
	path := fs.String("manifest", defaultManifest(), "the authoritative manifest")
	_ = fs.Parse(args)

	m, err := loadManifest(*path)
	if err != nil {
		fail(exitUsage, "%v", err)
	}

	tally := map[string]int{}
	var terminal []class
	for _, c := range m.classes {
		r := c.str("result")
		tally[r]++
		if r == "CAUGHT" || r == "TRUE_NO_OP" {
			terminal = append(terminal, c)
		}
	}
	unattempted := tally["UNATTEMPTED"]
	open := len(m.classes) - len(terminal) - unattempted

	fmt.Printf("total        %d\n", len(m.classes))
	names := make([]string, 0, len(tally))
	for k := range tally {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		fmt.Printf("%-28s %d\n", k, tally[k])
	}
	fmt.Printf("terminal     %d\n", len(terminal))
	fmt.Printf("open         %d\n", open)
	fmt.Printf("unattempted  %d\n", unattempted)
	closes := len(terminal) + open + unattempted
	fmt.Printf("arithmetic   %d == %d: %v\n", closes, len(m.classes), closes == len(m.classes))

	type gap struct {
		id      string
		missing []string
	}
	var incomplete []gap
	for _, c := range terminal {
		ev := c.evidence()
		need := requiredDimensions
		if c.str("result") == "TRUE_NO_OP" {
			need = nil
			for _, d := range requiredDimensions {
				if d != "killer_reachability" {
					need = append(need, d)
				}
			}
		}
		var missing []string
		for _, d := range need {
			raw, ok := ev.get(d)
			if !ok || !truthy(raw) {
				missing = append(missing, d)
			}
		}
		if len(missing) > 0 {
			incomplete = append(incomplete, gap{c.str("id"), missing})
		}
	}
	if len(incomplete) > 0 {
		fmt.Println("\nterminal classes with unproven evidence dimensions:")
		for _, g := range incomplete {
			fmt.Printf("  #%s: %v\n", g.id, g.missing)
		}
	} else {
		fmt.Println("\nevery terminal class has all evidence dimensions proven")
	}
	if len(incomplete) > 0 || closes != len(m.classes) {
		os.Exit(exitResult)
	}
}

// truthy reads a recorded dimension the way the Python did: anything that is not
// false, zero, empty or null counts as proven, so a note explaining why a
// dimension is not applicable still satisfies it.
func truthy(raw json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false
	}
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}
