package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Applying one mutation, proving it actually changed the source.
//
// A mutation campaign is only evidence if every mutation was applied. A textual
// replacement that matches nothing runs the unmodified implementation, its killer
// passes, and the result is indistinguishable from a surviving mutation — so a
// campaign without this check reports its own broken tooling as proof of safety.
// That happened, and it is why this exists.
//
// Every refusal exits 3, which callers must record as HARNESS FAILURE rather than
// as a survivor.

func cmdApply(args []string) {
	if len(args) < 3 {
		fail(exitHarness, "usage: apply <file> <anchor-file> <replacement-file> [count]")
	}
	path, anchorPath, replacementPath := args[0], args[1], args[2]
	count := 1
	if len(args) > 3 {
		n, err := strconv.Atoi(args[3])
		if err != nil {
			fail(exitHarness, "count %q is not a number", args[3])
		}
		count = n
	}

	anchor, err := os.ReadFile(anchorPath)
	if err != nil {
		fail(exitHarness, "reading the anchor: %v", err)
	}
	replacement, err := os.ReadFile(replacementPath)
	if err != nil {
		fail(exitHarness, "reading the replacement: %v", err)
	}

	out, applied, err := applyMutation(path, string(anchor), string(replacement), count)
	if err != nil {
		fail(exitHarness, "%v", err)
	}
	fmt.Println(out)
	_ = applied
}

// applyMutation performs the replacement and returns the message a caller
// prints, or the reason it refused.
//
// It is separate from the command so the campaign can call it directly and the
// parity tests can drive it without a subprocess.
func applyMutation(path, anchor, replacement string, count int) (string, int, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, fmt.Errorf("%s: no such file", path)
		}
		return "", 0, fmt.Errorf("%s: %v", path, err)
	}

	found := strings.Count(string(src), anchor)
	if found != count {
		return "", 0, fmt.Errorf("%s: anchor occurs %d time(s), expected %d. "+
			"A mutation that matches nothing runs the real implementation and its killer "+
			"passes, which looks exactly like a survivor", path, found, count)
	}

	next := strings.Replace(string(src), anchor, replacement, count)
	if next == string(src) {
		return "", 0, fmt.Errorf("%s: the replacement is identical to the anchor, so nothing changed", path)
	}

	// The file's own mode is kept: a mutation that changed permissions would be
	// a change the campaign did not intend and did not record.
	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(next), mode); err != nil {
		return "", 0, fmt.Errorf("%s: writing: %v", path, err)
	}
	return fmt.Sprintf("mutated %s: %d occurrence(s)", path, found), found, nil
}

// cmdEdit is apply with the anchor and replacement given as arguments rather
// than as files.
//
// It exists for the evidence shell scripts, which express an attack as a literal
// pair and would otherwise need a temporary file each. The guards are the same
// ones: the anchor must occur exactly as many times as declared, and the write
// must genuinely change the file. A convenience that skipped those would be a
// second, weaker way to mutate the tree.
func cmdEdit(args []string) {
	fs := flag.NewFlagSet("edit", flag.ExitOnError)
	file := fs.String("file", "", "the file to edit")
	old := fs.String("old", "", "the anchor, which must occur exactly -count times")
	replacement := fs.String("new", "", "what replaces it")
	count := fs.Int("count", 1, "how many occurrences the anchor must have")
	_ = fs.Parse(args)

	if *file == "" || *old == "" {
		fail(exitHarness, "usage: edit -file F -old ANCHOR -new REPLACEMENT [-count N]")
	}
	msg, _, err := applyMutation(*file, *old, *replacement, *count)
	if err != nil {
		fail(exitHarness, "%v", err)
	}
	fmt.Println(msg)
}
