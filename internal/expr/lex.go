package expr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A raw fragment carries its own $1, $2 placeholders, and they have to be
// renumbered when the fragment joins a larger statement. Finding them is not a
// search-and-replace problem: $1 is only a parameter where PostgreSQL would
// read it as one, and there are six constructions where it would not.
//
//	'$1'          a string literal
//	E'\\$1'       an escape string literal
//	"$1"          a quoted identifier
//	$$ $1 $$      a dollar-quoted string
//	$tag$ $1 $tag$
//	-- $1         a line comment
//	/* $1 */      a block comment, which nests
//
// A regular expression cannot tell those apart from the real thing, and one
// that appeared to would fail on the first fragment containing a LIKE pattern.
// So this is a lexer: small, but one that reads the fragment the way the server
// will.

// PlaceholderRef is one parameter reference found in a fragment.
type PlaceholderRef struct {
	// Start and End bound the reference within the fragment, so a rewriter can
	// splice without searching again.
	Start, End int
	// Index is the number written, which is 1-based as PostgreSQL numbers them.
	Index int
}

// ScanPlaceholders returns every parameter reference in sql, in the order they
// appear, ignoring the ones PostgreSQL would not read as parameters.
func ScanPlaceholders(sql string) ([]PlaceholderRef, error) {
	var refs []PlaceholderRef
	for i := 0; i < len(sql); {
		switch {
		case sql[i] == '\'':
			next, err := skipQuoted(sql, i, '\'', false)
			if err != nil {
				return nil, err
			}
			i = next

		case isEscapeStringStart(sql, i):
			next, err := skipQuoted(sql, i+1, '\'', true)
			if err != nil {
				return nil, err
			}
			i = next

		case sql[i] == '"':
			next, err := skipQuoted(sql, i, '"', false)
			if err != nil {
				return nil, err
			}
			i = next

		case strings.HasPrefix(sql[i:], "--"):
			i = skipLineComment(sql, i)

		case strings.HasPrefix(sql[i:], "/*"):
			next, err := skipBlockComment(sql, i)
			if err != nil {
				return nil, err
			}
			i = next

		case sql[i] == '$':
			ref, next, err := scanDollar(sql, i)
			if err != nil {
				return nil, err
			}
			if ref != nil {
				refs = append(refs, *ref)
			}
			i = next

		default:
			i++
		}
	}
	return refs, nil
}

// isEscapeStringStart reports whether position i begins an E'...' literal.
//
// The E only introduces one when it is not the tail of an identifier: in
// valueE'x' PostgreSQL reads the identifier valueE and then an ordinary
// string, and treating the E as a prefix there would change how backslashes in
// that string are read.
func isEscapeStringStart(sql string, i int) bool {
	if sql[i] != 'E' && sql[i] != 'e' {
		return false
	}
	if i+1 >= len(sql) || sql[i+1] != '\'' {
		return false
	}
	return i == 0 || !isIdentChar(sql[i-1])
}

// skipQuoted consumes a quoted run beginning at i, whose opening delimiter is
// quote. A doubled delimiter is an escaped one in every PostgreSQL quoted
// construction; a backslash escapes the next byte only in an escape string.
func skipQuoted(sql string, i int, quote byte, backslashEscapes bool) (int, error) {
	for j := i + 1; j < len(sql); j++ {
		switch {
		case backslashEscapes && sql[j] == '\\':
			j++
		case sql[j] == quote:
			if j+1 < len(sql) && sql[j+1] == quote {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated %c-quoted text at offset %d", quote, i)
}

func skipLineComment(sql string, i int) int {
	if n := strings.IndexByte(sql[i:], '\n'); n >= 0 {
		return i + n + 1
	}
	return len(sql)
}

// skipBlockComment consumes /* ... */, which nests in PostgreSQL: the first */
// does not necessarily end the comment.
func skipBlockComment(sql string, i int) (int, error) {
	depth := 0
	for j := i; j < len(sql); {
		switch {
		case strings.HasPrefix(sql[j:], "/*"):
			depth++
			j += 2
		case strings.HasPrefix(sql[j:], "*/"):
			depth--
			j += 2
			if depth == 0 {
				return j, nil
			}
		default:
			j++
		}
	}
	return 0, fmt.Errorf("unterminated block comment at offset %d", i)
}

// scanDollar resolves what a $ begins.
//
// A digit after it makes a parameter. Otherwise it may open a dollar-quoted
// string, whose tag runs to the next $ — and if there is no well-formed tag, it
// is just a dollar sign in the text.
func scanDollar(sql string, i int) (*PlaceholderRef, int, error) {
	if j := i + 1; j < len(sql) && isDigit(sql[j]) {
		k := j
		for k < len(sql) && isDigit(sql[k]) {
			k++
		}
		n, err := strconv.Atoi(sql[j:k])
		if err != nil {
			return nil, 0, fmt.Errorf("placeholder %q at offset %d is not a number: %w", sql[i:k], i, err)
		}
		// PostgreSQL numbers parameters from one. Returning $0 as a reference
		// would hand every caller an index that cannot address an argument, and
		// the one that forgot to validate it would index a slice at -1.
		if n < 1 {
			return nil, 0, fmt.Errorf("placeholder %q at offset %d is not a valid parameter number, which start at 1", sql[i:k], i)
		}
		return &PlaceholderRef{Start: i, End: k, Index: n}, k, nil
	}

	tag, ok := dollarTag(sql, i)
	if !ok {
		return nil, i + 1, nil
	}
	closer := "$" + tag + "$"
	rest := sql[i+len(closer):]
	n := strings.Index(rest, closer)
	if n < 0 {
		return nil, 0, fmt.Errorf("unterminated dollar-quoted string %s at offset %d", closer, i)
	}
	return nil, i + len(closer) + n + len(closer), nil
}

// dollarTag reads the tag of a dollar quote opening at i, which may be empty
// for $$. It reports false when what follows is not a tag at all.
func dollarTag(sql string, i int) (string, bool) {
	j := i + 1
	for j < len(sql) && isTagChar(sql[j], j == i+1) {
		j++
	}
	if j < len(sql) && sql[j] == '$' {
		return sql[i+1 : j], true
	}
	return "", false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isIdentChar(b byte) bool {
	return b == '_' || isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
}

// isTagChar reports whether b may appear in a dollar-quote tag. A tag may not
// start with a digit, which is what keeps $1 a parameter.
func isTagChar(b byte, first bool) bool {
	if first {
		return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b >= 0x80
	}
	return isIdentChar(b)
}

// ErrRawPlaceholder reports a fragment whose placeholders and arguments do not
// agree.
var ErrRawPlaceholder = errors.New("orm: raw fragment placeholders do not match its arguments")

// ValidatePlaceholders checks a fragment's references against the arguments it
// was given.
//
// An unreferenced argument is an error rather than a courtesy: it means the
// fragment and the call have drifted apart, and the reading that would silently
// work — dropping it — is the one most likely to be wrong.
func ValidatePlaceholders(refs []PlaceholderRef, args int) error {
	seen := make(map[int]bool, args)
	for _, r := range refs {
		if r.Index < 1 {
			return fmt.Errorf("%w: $%d is not a valid parameter number, which start at 1", ErrRawPlaceholder, r.Index)
		}
		if r.Index > args {
			return fmt.Errorf("%w: the fragment refers to $%d but was given %s", ErrRawPlaceholder, r.Index, plural(args, "argument"))
		}
		seen[r.Index] = true
	}
	for i := 1; i <= args; i++ {
		if !seen[i] {
			return fmt.Errorf("%w: argument %d is never referred to; the fragment uses %s", ErrRawPlaceholder, i, plural(len(seen), "placeholder"))
		}
	}
	return nil
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// RewritePlaceholders returns sql with every parameter reference replaced by
// the text remap returns for it. The refs must be the ones ScanPlaceholders
// found in that same string.
func RewritePlaceholders(sql string, refs []PlaceholderRef, remap func(index int) string) string {
	if len(refs) == 0 {
		return sql
	}
	var b strings.Builder
	b.Grow(len(sql))
	prev := 0
	for _, r := range refs {
		b.WriteString(sql[prev:r.Start])
		b.WriteString(remap(r.Index))
		prev = r.End
	}
	b.WriteString(sql[prev:])
	return b.String()
}
