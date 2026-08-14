package diag_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
)

// A diagnostic code is machine-observable API.
//
// Projects suppress by code, alert on code, and gate CI on code. That makes the
// register a public contract with the same force as a function signature, and
// these are the gates that hold it: no duplicate, nothing emitted that is not
// registered, and — the rule that matters most — no code ever reused for a
// different meaning.

// Every registered code is complete and self-consistent.
func TestRegistry_isWellFormed(t *testing.T) {
	codes := diag.Codes()
	if len(codes) == 0 {
		t.Fatal("the register is empty")
	}

	seen := make(map[diag.Code]bool, len(codes))
	for _, c := range codes {
		if seen[c] {
			t.Errorf("%s is registered twice; a duplicate makes one of the two meanings silently unreachable", c)
		}
		seen[c] = true

		if !c.Known() {
			t.Errorf("%s is returned by Codes but is not Known", c)
		}
		if c.Title() == "" {
			t.Errorf("%s has no title, so nothing can render it", c)
		}
		// The severity prefix is part of the name and must agree with the
		// registered severity: a code called E019 that warns would be read as
		// an error by every human who saw it.
		switch {
		case strings.HasPrefix(string(c), "E"):
			if c.Severity() != diag.SeverityError {
				t.Errorf("%s is named as an error and registered as %v", c, c.Severity())
			}
		case strings.HasPrefix(string(c), "W"):
			if c.Severity() != diag.SeverityWarning {
				t.Errorf("%s is named as a warning and registered as %v", c, c.Severity())
			}
		default:
			t.Errorf("%s has no severity prefix", c)
		}
	}
}

// Release-critical: every code the generator can emit is registered.
//
// An unregistered code renders without a title and cannot be looked up, and it
// reaches a user as a bare string with no meaning attached. This reads the
// source for every diag.Exxx / diag.Wxxx reference and checks the register
// rather than waiting for one to be emitted at runtime.
func TestRegistry_everyEmittedCodeIsRegistered(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	referenced := map[string]string{} // code -> where
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "api":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // not this test's business
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "diag" {
				return true
			}
			name := sel.Sel.Name
			if len(name) == 4 && (name[0] == 'E' || name[0] == 'W') && isDigits(name[1:]) {
				rel, _ := filepath.Rel(root, path)
				referenced[name] = rel
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(referenced) == 0 {
		t.Fatal("no diagnostic codes were found in the source; this test is checking nothing")
	}

	for code, where := range referenced {
		if !diag.Code(code).Known() {
			t.Errorf("%s is emitted from %s but is not in the register", code, where)
		}
	}
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Retired codes are never reassigned.
//
// M14 retired PL003 rather than reusing it, and the reason generalises: a
// project that suppressed a code would silently start suppressing a different
// finding, and one that alerted on it would silently start alerting on
// something else. Neither has any way to notice.
//
// The list below is the permanent record. A code may be added to it; nothing
// may ever be removed from it, and nothing on it may reappear in the register.
func TestRegistry_retiredCodesAreNeverReused(t *testing.T) {
	retired := []diag.Code{
		// PL003 split rows-discarded findings by node kind and was folded back
		// into PL002 during M14. It belongs to the diagnostics package rather
		// than this register, and is listed here because the rule is one rule.
	}
	for _, c := range retired {
		if c.Known() {
			t.Errorf("%s was retired and has been registered again with a new meaning", c)
		}
	}

	// The register is append-only in practice, so the first codes must still be
	// what they always were. Pinning a few by meaning catches a renumbering
	// that a count would not.
	for _, c := range []struct {
		code  diag.Code
		title string
	}{
		{diag.E001, "Go field has no matching column"},
		{diag.E017, "multiple Go entities map to one PostgreSQL table"},
		{diag.E024, "relation target is in another package"},
	} {
		if got := c.code.Title(); got != c.title {
			t.Errorf("%s now means %q; it meant %q, and a published code may be retired but never reassigned",
				c.code, got, c.title)
		}
	}
}
