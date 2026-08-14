// Command mutate is the mutation campaign's harness.
//
// It replaces three Python scripts that grew alongside the M16.5 evidence work.
// Nothing about what it does is new: every guard here exists because the campaign
// once reported a result it had not earned, and the port keeps each one with the
// reasoning that produced it. What changes is that a repository whose product is
// written in Go no longer needs a Python interpreter to check its own evidence.
//
// The campaign has several ways to lie to itself, and each subcommand closes one:
//
//	apply        a replacement that matches nothing runs the real implementation,
//	             its killer passes, and the result is indistinguishable from a
//	             surviving mutation
//	smoke        a killer that executes no tests — or whose tests all skip — exits
//	             0, so the mutation appears to survive
//	run          a class gets a product result only after every evidence dimension
//	             holds, and each failure is recorded as itself rather than folded
//	             into "survived"
//	totals       the arithmetic comes from the manifest, because prose cannot count
//
// Usage:
//
//	go run ./internal/tools/mutate apply  <file> <anchor-file> <replacement-file> [count]
//	go run ./internal/tools/mutate smoke  [--as-precondition] <expected-test>... -- <argv>...
//	go run ./internal/tools/mutate run    <class-id> [-repo DIR] [-manifest PATH]
//	go run ./internal/tools/mutate verify <class-id> [-repo DIR] [-manifest PATH]
//	go run ./internal/tools/mutate totals [-manifest PATH]
//	go run ./internal/tools/mutate anchors [-repo DIR] [-manifest PATH]...
//	go run ./internal/tools/mutate crosslevel [-repo DIR] [-manifest PATH]...
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Exit codes. These are the externally observable distinction between an
// evidence failure and a product result, and they are the ones the Python
// harness used: a caller that saw 4 recorded KILLER EXECUTION FAILURE rather
// than SURVIVED, and that mapping is what stops a broken harness from being
// read as proof the product is safe.
const (
	exitOK = 0
	// exitResult means the pipeline ran and the class is not CAUGHT.
	exitResult = 1
	// exitUsage is a caller mistake, distinct from a refusal about evidence.
	exitUsage = 2
	// exitHarness is a mutation that could not be applied as specified.
	exitHarness = 3
	// exitKillerExecution is a killer that could not have observed anything.
	exitKillerExecution = 4
	// exitFixturePrecondition is a fixture that does not contain what the class
	// attacks. It is a different finding from a mistyped killer.
	exitFixturePrecondition = 5
	// exitSemanticExpression is a mutation that does not express the defect it
	// claims.
	exitSemanticExpression = 6
)

func usage() {
	fmt.Fprint(os.Stderr, `mutate: the mutation campaign's harness

  apply  <file> <anchor-file> <replacement-file> [count]
      Apply one mutation, refusing unless the anchor occurs exactly count times
      and the write genuinely changes the file.

  edit   -file F -old ANCHOR -new REPLACEMENT [-count N]
      The same, with the anchor and replacement given literally.

  smoke  [--as-precondition] <expected-test>... -- <argv>...
      Prove a command runs, and passes, the tests it names, before its verdict is
      trusted. Killers are argv vectors, never shell strings.

  run    <class-id> [-repo DIR] [-manifest PATH]
      Take one class through every evidence dimension and record the result.

  verify <class-id> [-repo DIR] [-manifest PATH]
      Killer-smoke validation on its own, for a class that already has a result.

  totals [-manifest PATH]
      Recompute the campaign's accounting from the manifest.

  anchors [-repo DIR] [-manifest PATH]...
      Check that every anchor still quotes the code it mutates, without touching
      anything. Run it after editing production code.

  crosslevel [-repo DIR] [-manifest PATH]...
      Control every semantic probe against every mutation in another function. A
      probe that fails there is not evidence about its own.
`)
}

// defaultManifest finds manifest.json beside this source file, so the tool works
// from any working directory without a flag.
func defaultManifest() string {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "manifest.json"
	}
	return filepath.Join(filepath.Dir(self), "manifest.json")
}

// classDir is where a class's anchor and replacement files live.
func classDir() string {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	return filepath.Dir(self)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "apply":
		cmdApply(os.Args[2:])
	case "edit":
		cmdEdit(os.Args[2:])
	case "smoke":
		cmdSmoke(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "totals":
		cmdTotals(os.Args[2:])
	case "crosslevel":
		cmdCrossLevel(os.Args[2:])
	case "anchors":
		cmdAnchors(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mutate: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}
}

// fail prints a refusal and exits with the code that names its kind.
func fail(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "mutate: "+format+"\n", args...)
	os.Exit(code)
}
