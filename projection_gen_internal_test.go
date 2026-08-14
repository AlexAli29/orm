package orm

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The generated-projection tests.
//
// Project9..Project50 are written by a generator, which changes what is worth
// testing. A hand-written constructor is read by whoever writes it; a generated
// one is read by nobody, and the failure it can carry is silent: a scanner
// whose destinations are in a different order than its items, so every row
// scans without error into the wrong fields. Two int64 columns transposed do
// not fail — they just report the wrong number for the rest of the program's
// life.
//
// So there are two checks. The runtime ones drive the widest and narrowest
// generated arities with fifty distinguishable values and demand them back in
// order. The source one reads every arity the generator emitted and insists the
// positions line up, because writing out forty-two calls by hand to catch a
// generator bug would be doing the generator's job badly.

type seqEntity struct{ N int64 }

var seqSrc = NewSource("public", "seq_entity")

// seqCols is fifty distinct int64 columns, c1..c50.
var seqCols = func() []OrdCol[seqEntity, int64] {
	cols := make([]OrdCol[seqEntity, int64], 50)
	for i := range cols {
		cols[i] = NewOrdCol[seqEntity, int64](seqSrc, fmt.Sprintf("c%d", i+1))
	}
	return cols
}()

// countingRows writes 1, 2, 3... into successive destinations, so a scanner
// that binds its destinations out of order produces values out of order rather
// than an error.
type countingRows struct{ pgx.Rows }

func (countingRows) Scan(dest ...any) error {
	for i, d := range dest {
		p, ok := d.(*int64)
		if !ok {
			return fmt.Errorf("destination %d is %T, want *int64", i, d)
		}
		*p = int64(i + 1)
	}
	return nil
}

func TestProjectGenerated_scansInOrder(t *testing.T) {
	c := seqCols
	p := Project50(
		c[0], c[1], c[2], c[3], c[4], c[5], c[6], c[7], c[8], c[9],
		c[10], c[11], c[12], c[13], c[14], c[15], c[16], c[17], c[18], c[19],
		c[20], c[21], c[22], c[23], c[24], c[25], c[26], c[27], c[28], c[29],
		c[30], c[31], c[32], c[33], c[34], c[35], c[36], c[37], c[38], c[39],
		c[40], c[41], c[42], c[43], c[44], c[45], c[46], c[47], c[48], c[49],
		func(v1, v2, v3, v4, v5, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16, v17, v18, v19, v20, v21, v22, v23, v24, v25, v26, v27, v28, v29, v30, v31, v32, v33, v34, v35, v36, v37, v38, v39, v40, v41, v42, v43, v44, v45, v46, v47, v48, v49, v50 int64) []int64 {
			return []int64{v1, v2, v3, v4, v5, v6, v7, v8, v9, v10, v11, v12, v13, v14, v15, v16, v17, v18, v19, v20, v21, v22, v23, v24, v25, v26, v27, v28, v29, v30, v31, v32, v33, v34, v35, v36, v37, v38, v39, v40, v41, v42, v43, v44, v45, v46, v47, v48, v49, v50}
		},
	)

	if p.Columns() != 50 {
		t.Fatalf("Columns() = %d, want 50", p.Columns())
	}

	got, err := p.newScan()(countingRows{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for i, v := range got {
		if v != int64(i+1) {
			t.Fatalf("position %d scanned %d: destinations are bound out of order", i, v)
		}
	}
}

// TestProjectGenerated_sourceIsPositionallyConsistent reads the generated file
// and checks, for every constructor in it, that the four positional lists agree:
// the parameters, the select items, the shape slots, and the scan destinations
// followed by the build call. A generator that transposed any pair would emit
// code that compiles and scans and is wrong, and this is the check that sees it.
func TestProjectGenerated_sourceIsPositionallyConsistent(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "projection_gen.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing projection_gen.go: %v", err)
	}

	seen := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(fn.Name.Name, "Project%d", &n); err != nil {
			t.Errorf("unexpected declaration %s in generated file", fn.Name.Name)
			continue
		}
		seen++

		// Type parameters are E, T1..Tn, R.
		want := []string{"E"}
		for i := 1; i <= n; i++ {
			want = append(want, fmt.Sprintf("T%d", i))
		}
		want = append(want, "R")
		if got := names(fn.Type.TypeParams); !sameStrings(got, want) {
			t.Errorf("%s type parameters = %v, want %v", fn.Name.Name, got, want)
		}

		// Value parameters are e1..en then build.
		want = nil
		for i := 1; i <= n; i++ {
			want = append(want, fmt.Sprintf("e%d", i))
		}
		want = append(want, "build")
		if got := names(fn.Type.Params); !sameStrings(got, want) {
			t.Errorf("%s parameters = %v, want %v", fn.Name.Name, got, want)
		}

		// Every e_i and v_i in the body appears in ascending order, restarting
		// at each of the four lists that mention them.
		checkRuns(t, fn.Name.Name, fn.Body, n)
	}

	if seen == 0 {
		t.Fatal("no generated constructors found")
	}
	t.Logf("checked %d generated constructors", seen)
}

// checkRuns walks the body in source order collecting every identifier of the
// form e<i> or v<i>, splitting them into runs wherever the index stops
// ascending, and requires every run to be exactly 1..n. The body holds five
// such runs: e1..en twice, for the select items and the shape slots, and
// v1..vn three times, for the var block, the destinations and the build call.
// A transposition anywhere breaks the run it occurs in, and a missing or
// duplicated position changes a run's length.
func checkRuns(t *testing.T, fn string, body *ast.BlockStmt, n int) {
	t.Helper()

	var run []int
	var prefix byte
	runs := map[byte]int{}
	flush := func() {
		if len(run) == 0 {
			return
		}
		runs[prefix]++
		if len(run) != n {
			t.Errorf("%s: a run of %c has %d entries, want %d", fn, prefix, len(run), n)
		}
		for i, v := range run {
			if v != i+1 {
				t.Errorf("%s: a run of %c is out of order at position %d: got %c%d", fn, prefix, i, prefix, v)
				break
			}
		}
		run = nil
	}
	add := func(p byte, i int) {
		// A run ends where the index stops ascending by one, which is exactly
		// where the generator moved on to the next list.
		if p != prefix || (len(run) > 0 && i != run[len(run)-1]+1) {
			flush()
			prefix = p
		}
		run = append(run, i)
	}

	ast.Inspect(body, func(node ast.Node) bool {
		id, ok := node.(*ast.Ident)
		if !ok {
			return true
		}
		var i int
		for _, p := range []byte{'e', 'v'} {
			if _, err := fmt.Sscanf(id.Name, string(p)+"%d", &i); err == nil && id.Name == fmt.Sprintf("%c%d", p, i) {
				add(p, i)
				break
			}
		}
		return true
	})
	flush()

	// Two lists mention the parameters, three mention the locals. Fewer would
	// mean the generator dropped one; more would mean it emitted a list twice.
	if runs['e'] != 2 {
		t.Errorf("%s: %d runs of e, want 2 (select items, shape slots)", fn, runs['e'])
	}
	if runs['v'] != 3 {
		t.Errorf("%s: %d runs of v, want 3 (var block, destinations, build call)", fn, runs['v'])
	}
}

func names(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, f := range fl.List {
		for _, n := range f.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
