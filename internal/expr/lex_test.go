package expr

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// indices is what the lexer found, in order, as a comparable string.
func indices(refs []PlaceholderRef) string {
	var b strings.Builder
	for i, r := range refs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(r.Index))
	}
	return b.String()
}

func TestScanPlaceholders(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "one", sql: `x = $1`, want: "1"},
		{name: "two", sql: `x = $1 AND y = $2`, want: "1,2"},
		{name: "one referenced twice", sql: `x = $1 OR y = $1`, want: "1,1"},
		{name: "none", sql: `x = y`, want: ""},
		{name: "adjacent to punctuation", sql: `f($1,$2)`, want: "1,2"},
		{name: "multi-digit", sql: `x = $12`, want: "12"},

		// A parameter inside any of these is not a parameter. Each of them is
		// a place where a regular expression would be wrong.
		{name: "single-quoted string", sql: `x = '$1'`, want: ""},
		{name: "doubled quote inside a string", sql: `x = 'it''s $1'`, want: ""},
		{name: "escape string", sql: `x = E'\\$1'`, want: ""},
		{name: "escape string escaping its own quote", sql: `x = E'\'$1'`, want: ""},
		{name: "quoted identifier", sql: `"$1" = 1`, want: ""},
		{name: "doubled quote inside an identifier", sql: `"a""$1" = 1`, want: ""},
		{name: "anonymous dollar quote", sql: `x = $$ $1 $$`, want: ""},
		{name: "tagged dollar quote", sql: `x = $body$ $1 $body$`, want: ""},
		{name: "line comment", sql: "-- $1\nx = $2", want: "2"},
		{name: "block comment", sql: `/* $1 */ x = $2`, want: "2"},
		{
			name: "nested block comment",
			sql:  "/*\n outer\n /* $1 */\n*/\nx = $2",
			want: "2",
		},

		// The interesting cases are the ones that mix them.
		{name: "a string then a parameter", sql: `a = '$9' AND b = $1`, want: "1"},
		{name: "a parameter then a string", sql: `a = $1 AND b = '$9'`, want: "1"},
		{name: "a parameter between comments", sql: "/* $8 */ a = $1 -- $9", want: "1"},
		{name: "a dollar quote between parameters", sql: `a = $1 AND b = $tag$ $8 $tag$ AND c = $2`, want: "1,2"},
		{name: "a LIKE pattern holding a dollar", sql: `a LIKE '%$1%' AND b = $1`, want: "1"},

		// An E that ends an identifier does not introduce an escape string, so
		// the backslash in the string that follows is an ordinary character
		// and does not hide the closing quote.
		{name: "an identifier ending in E", sql: `valueE'\' AND x = $1`, want: "1"},

		// A lone dollar with no tag and no digits is just text.
		{name: "a stray dollar", sql: `cost = 5$ AND x = $1`, want: "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := ScanPlaceholders(tt.sql)
			if err != nil {
				t.Fatalf("ScanPlaceholders(%q): %v", tt.sql, err)
			}
			if got := indices(refs); got != tt.want {
				t.Errorf("ScanPlaceholders(%q) found %q, want %q", tt.sql, got, tt.want)
			}
			for _, r := range refs {
				if got := tt.sql[r.Start:r.End]; got[0] != '$' {
					t.Errorf("reference spans %q, which does not begin with $", got)
				}
			}
		})
	}
}

func TestScanPlaceholders_malformed(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "unterminated string", sql: `x = 'abc`, want: "unterminated"},
		{name: "unterminated identifier", sql: `"abc = 1`, want: "unterminated"},
		{name: "unterminated block comment", sql: `/* abc`, want: "unterminated block comment"},
		{name: "unterminated nested block comment", sql: `/* /* abc */`, want: "unterminated block comment"},
		{name: "unterminated dollar quote", sql: `x = $tag$ abc`, want: "unterminated dollar-quoted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ScanPlaceholders(tt.sql)
			if err == nil {
				t.Fatalf("ScanPlaceholders(%q) succeeded, want an error", tt.sql)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRewritePlaceholders(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want string
	}{
		{name: "one", sql: `x = $1`, want: `x = $11`},
		{name: "two", sql: `x = $1 AND y = $2`, want: `x = $11 AND y = $12`},
		{name: "repeated", sql: `x = $1 OR y = $1`, want: `x = $11 OR y = $11`},
		{name: "none", sql: `x = y`, want: `x = y`},
		{name: "literals are left alone", sql: `a = '$1' AND b = $1`, want: `a = '$1' AND b = $11`},
		{name: "comments are left alone", sql: `/* $1 */ b = $1`, want: `/* $1 */ b = $11`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := ScanPlaceholders(tt.sql)
			if err != nil {
				t.Fatalf("ScanPlaceholders: %v", err)
			}
			// Shift every local index by ten, so the rewrite is visible.
			got := RewritePlaceholders(tt.sql, refs, func(i int) string {
				return "$" + strconv.Itoa(i+10)
			})
			if got != tt.want {
				t.Errorf("rewrote %q as %q, want %q", tt.sql, got, tt.want)
			}
		})
	}
}

func TestValidatePlaceholders(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		args int
		want string
	}{
		{name: "one for one", sql: `x = $1`, args: 1},
		{name: "two for two", sql: `x = $1 AND y = $2`, args: 2},
		{name: "one referenced twice", sql: `x = $1 OR y = $1`, args: 1},
		{name: "none for none", sql: `x IS NULL`, args: 0},
		{name: "out of range", sql: `x = $2`, args: 1, want: "refers to $2 but was given 1 argument"},
		{name: "far out of range", sql: `x = $9`, args: 2, want: "refers to $9 but was given 2 arguments"},
		{name: "an unused argument", sql: `x = $1`, args: 2, want: "argument 2 is never referred to"},
		{name: "no placeholders but arguments", sql: `x IS NULL`, args: 1, want: "argument 1 is never referred to"},
		{name: "a gap in the middle", sql: `x = $1 AND y = $3`, args: 3, want: "argument 2 is never referred to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, err := ScanPlaceholders(tt.sql)
			if err != nil {
				t.Fatalf("ScanPlaceholders: %v", err)
			}
			err = ValidatePlaceholders(refs, tt.args)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ValidatePlaceholders: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidatePlaceholders succeeded, want an error containing %q", tt.want)
			}
			if !errors.Is(err, ErrRawPlaceholder) {
				t.Errorf("error = %v, want it to wrap ErrRawPlaceholder", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestScanPlaceholders_zeroIsNotAParameter(t *testing.T) {
	// PostgreSQL numbers parameters from one. $0 is refused where it is read,
	// so no caller is ever handed a reference that cannot address an argument
	// and could index args at -1.
	if _, err := ScanPlaceholders(`x = $0`); err == nil {
		t.Error("ScanPlaceholders accepted $0")
	} else if !strings.Contains(err.Error(), "start at 1") {
		t.Errorf("error = %v, want it to say where parameter numbers begin", err)
	}
	// Validation refuses it too, for a reference built by hand rather than
	// scanned.
	err := ValidatePlaceholders([]PlaceholderRef{{Index: 0}}, 1)
	if err == nil {
		t.Error("ValidatePlaceholders accepted index 0")
	} else if !errors.Is(err, ErrRawPlaceholder) {
		t.Errorf("error = %v, want it to wrap ErrRawPlaceholder", err)
	}
}
