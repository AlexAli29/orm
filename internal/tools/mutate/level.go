package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
)

// Where a mutation lives.
//
// A mutation is refuted at two levels: a narrow probe at the level of the code
// that changed, and a killer at the level of the guarantee a caller loses. The
// campaign checks that both fail under the mutant, which catches a probe that is
// simply wrong — but it cannot catch a probe aimed at the wrong function, if
// that function happens to fail too.
//
// It has been aimed at the wrong function twice. In one batch three probes of
// seven were pointed at a function the mutation did not touch; in another, four
// killers would have proved a neighbouring guard rather than the one under test.
// Both times the campaign refused the class rather than recording it, which is
// the model working — and both times the fix was attention rather than
// structure.
//
// So a class names the function its anchor is inside, and this checks that the
// anchor really is inside it. That does not prove the probe is at the right
// level; nothing mechanical can. What it does is make the level something the
// author writes down and the tool agrees with, which is where the mistake was
// being made.
//
// The field is optional. The manifests written before it exist have closed
// accounting, and reopening them to add a field would mean rerunning campaigns
// whose results are already evidence.

// checkSiteFunction reports whether the anchor lies inside the named function of
// the file. An empty name is not checked.
func checkSiteFunction(path, function, anchor string) error {
	if function == "" {
		return nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	idx := strings.Index(string(src), anchor)
	if idx < 0 {
		// The anchor not matching is applyMutation's refusal to make, with its
		// own message; this one has nothing to add.
		return nil
	}
	if strings.Index(string(src)[idx+1:], anchor) >= 0 {
		return fmt.Errorf("the anchor occurs more than once in %s, so which function it is in is not decided", path)
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	holder := enclosingFunc(fset, file, idx)
	if holder == "" {
		return fmt.Errorf("the anchor in %s is not inside any function, and the class says it is inside %s", path, function)
	}
	if holder != function {
		return fmt.Errorf("the class says the mutation is in %s and the anchor is in %s; "+
			"the name is what the semantic probe is chosen against, so a wrong one makes the probe's level a guess",
			function, holder)
	}
	return nil
}

// enclosingFunc names the function or method whose body contains the offset.
func enclosingFunc(fset *token.FileSet, file *ast.File, offset int) string {
	base := fset.File(file.Pos()).Base()
	pos := token.Pos(base + offset)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if pos < fn.Pos() || pos >= fn.End() {
			continue
		}
		if fn.Recv == nil || len(fn.Recv.List) == 0 {
			return fn.Name.Name
		}
		return receiverName(fn.Recv.List[0].Type) + "." + fn.Name.Name
	}
	return ""
}

// receiverName renders a receiver type the way a class names it: the type's own
// name, without a pointer star or type parameters.
func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	}
	return ""
}
