package expr_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/expr"
)

// The placeholder lexer is the one part of this package that reads text a caller
// wrote rather than a tree the package built, so it is the one part that can be
// handed something nobody anticipated. What is being fuzzed is not whether it
// finds the right placeholders — the table tests cover that — but that no input
// makes it panic, run off the end of a string, or fail to terminate.

func FuzzScanPlaceholders(f *testing.F) {
	for _, seed := range []string{
		"",
		"$1",
		"$$1",
		"$1$2$3",
		"'$1'",
		`"$1"`,
		"$tag$ $1 $tag$",
		"-- $1\n$2",
		"/* $1 */ $2",
		"E'\\'$1'",
		"$1::int8[]",
		"$",
		"$0",
		"$99999999999999999999",
		"'unterminated",
		"$tag$unterminated",
		"/* unterminated",
		"$$",
		"U&'\\0441$1'",
		"/*\n  nested /* $2 */\n  $3\n*/ $1",
		"/*/ $1",
		"/**/$1",
		"$1$$x$$$2",
		"$_tag$ $1 $_tag$ $2",
		"E'$1\\\\' $2",
		"e'\\\\'$1'",
		"valueE'$1'",
		"$999999999",
		"\u00e9$1\u00e9",
		strings.Repeat("$1 ", 2000),
		strings.Repeat("/*", 200) + "$1" + strings.Repeat("*/", 200),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, sql string) {
		refs, err := expr.ScanPlaceholders(sql)
		if err != nil {
			// A refusal is a legitimate outcome; what matters is that it is a
			// refusal rather than a crash.
			return
		}
		for _, r := range refs {
			switch {
			case r.Index < 1:
				t.Fatalf("placeholder index %d in %q", r.Index, sql)
			case r.Start < 0 || r.End > len(sql) || r.Start >= r.End:
				t.Fatalf("placeholder span [%d,%d) is outside %q", r.Start, r.End, sql)
			}
			// The span has to be the placeholder it claims to be, or
			// renumbering would rewrite the wrong bytes.
			if got := sql[r.Start:r.End]; !strings.HasPrefix(got, "$") {
				t.Fatalf("span %q is not a placeholder in %q", got, sql)
			}
		}
		// Rewriting is the operation the spans exist for, and it must not
		// depend on them being in any particular order or on the text being
		// valid SQL.
		out := expr.RewritePlaceholders(sql, refs, func(int) string { return "$1" })
		if len(refs) == 0 && out != sql {
			t.Fatalf("rewriting with no placeholders changed %q into %q", sql, out)
		}
	})
}

// Identifier quoting is the other place a string becomes syntax. Nothing a
// caller writes reaches it — identifiers come from the catalog — but it is the
// last line between a name and the statement, so it is worth proving that no
// input escapes the quotes.
func FuzzIdentifier(f *testing.F) {
	for _, seed := range []string{"users", `we"ird`, "", "\x00", "ünïcode", `"`, `""`, "a.b"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, name string) {
		out, err := expr.QuoteIdentifier(name)
		if err != nil {
			return
		}
		if len(out) < 2 || out[0] != '"' || out[len(out)-1] != '"' {
			t.Fatalf("QuoteIdentifier(%q) = %q, want it quoted", name, out)
		}
		// Every quote inside has to be doubled, or the identifier ends early
		// and the rest becomes syntax.
		inner := out[1 : len(out)-1]
		for i := 0; i < len(inner); i++ {
			if inner[i] != '"' {
				continue
			}
			if i+1 >= len(inner) || inner[i+1] != '"' {
				t.Fatalf("QuoteIdentifier(%q) = %q, which ends the identifier early", name, out)
			}
			i++
		}
	})
}
