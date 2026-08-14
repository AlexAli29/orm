package main_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every exported symbol in a public package carries documentation.
//
// This is a gate rather than a report. An undocumented export is a symbol
// somebody has to read the implementation to use, and once v1 freezes it, the
// implementation is the only place its meaning was ever written down.
//
// The linter rejects comments that restate the identifier, so passing it means
// the comment says something the name does not.
func TestGodoc_everyPublicSymbolIsDocumented(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", "./internal/tools/godoclint", "-dir", ".", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	t.Errorf("%d exported symbols have no meaningful documentation:\n%s",
		max(len(lines)-1, 0), strings.Join(lines, "\n"))
}
