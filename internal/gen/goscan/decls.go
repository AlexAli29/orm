package goscan

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/model"
)

// Struct-level schema declarations.
//
// A field tag is the right place for a property of one column. It is the wrong
// place for a two-column partial covering index: a tag that could describe one
// would be a small unreadable language living inside a string literal, and the
// thing being described is not a property of any single field anyway.
//
// So they are directives on the type:
//
//	//orm:table posts
//	//orm:index posts_feed_idx (author_id, created_at desc) include (title) where "published = true"
//	//orm:check posts_score_positive "score >= 0"
//	//orm:unique posts_slug_key (slug)
//	type Post struct { ... }
//
// The columns are named as columns rather than as generated descriptors, and
// the scanner resolves each one against the struct it is written on. That is
// what makes a fresh project work: a schema declaration cannot depend on code
// that only exists after the schema has been created.

const (
	indexDirective     = "//orm:index"
	uniqueDirective    = "//orm:unique"
	checkDirective     = "//orm:check"
	enumDirective      = "//orm:enum"
	extensionDirective = "//orm:extension"
)

// schemaDirectives are the prefixes a declaration line may start with.
var schemaDirectives = []string{
	indexDirective, uniqueDirective, checkDirective, enumDirective, extensionDirective,
}

// parseSchemaDecl reads one declaration line.
//
// The grammar is small and positional, which is what keeps it readable:
//
//	index  <name> (<key>, ...) [include (<col>, ...)] [where <expr>] [using <method>] [unique] [concurrently]
//	unique <name> (<col>, ...)
//	check  <name> <expr>
//	enum   <schema.type> (<label>, ...)
//	extension <name>
//
// A key is a column name with an optional "desc", "nulls first", "nulls last"
// and operator class. An expression is a quoted string, because it is SQL and
// contains everything a grammar would otherwise have to escape.
func parseSchemaDecl(text string, pos model.Position) (model.SchemaDecl, bool, error) {
	line := strings.TrimRight(text, " \t")
	var kind model.SchemaDeclKind
	var rest string
	switch {
	case strings.HasPrefix(line, indexDirective):
		kind, rest = model.DeclIndex, strings.TrimPrefix(line, indexDirective)
	case strings.HasPrefix(line, uniqueDirective):
		kind, rest = model.DeclUnique, strings.TrimPrefix(line, uniqueDirective)
	case strings.HasPrefix(line, checkDirective):
		kind, rest = model.DeclCheck, strings.TrimPrefix(line, checkDirective)
	case strings.HasPrefix(line, enumDirective):
		kind, rest = model.DeclEnum, strings.TrimPrefix(line, enumDirective)
	case strings.HasPrefix(line, extensionDirective):
		kind, rest = model.DeclExtension, strings.TrimPrefix(line, extensionDirective)
	default:
		return model.SchemaDecl{}, false, nil
	}

	d := model.SchemaDecl{Kind: kind, Raw: strings.TrimSpace(line), Pos: pos}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return d, true, fmt.Errorf("%s needs a name", d.Kind)
	}

	name, rest := takeWord(rest)
	d.Name = name

	switch kind {
	case model.DeclExtension:
		if strings.TrimSpace(rest) != "" {
			return d, true, fmt.Errorf("extension takes one name, and %q follows it", strings.TrimSpace(rest))
		}
		return d, true, nil

	case model.DeclCheck:
		expr, remainder, err := takeQuoted(rest)
		if err != nil {
			return d, true, fmt.Errorf("check %s: %w", name, err)
		}
		if strings.TrimSpace(remainder) != "" {
			return d, true, fmt.Errorf("check %s: %q follows the condition", name, strings.TrimSpace(remainder))
		}
		d.Expr = expr
		return d, true, nil

	case model.DeclEnum:
		labels, remainder, err := takeList(rest)
		if err != nil {
			return d, true, fmt.Errorf("enum %s: %w", name, err)
		}
		if len(labels) == 0 {
			return d, true, fmt.Errorf("enum %s has no labels", name)
		}
		if strings.TrimSpace(remainder) != "" {
			return d, true, fmt.Errorf("enum %s: %q follows the labels", name, strings.TrimSpace(remainder))
		}
		for _, l := range labels {
			d.Labels = append(d.Labels, strings.Trim(l, `'"`))
		}
		return d, true, nil
	}

	// Index and unique both start with a parenthesised key list.
	keys, remainder, err := takeList(rest)
	if err != nil {
		return d, true, fmt.Errorf("%s %s: %w", d.Kind, name, err)
	}
	if len(keys) == 0 {
		return d, true, fmt.Errorf("%s %s names no columns", d.Kind, name)
	}
	for _, k := range keys {
		f, err := parseKey(k)
		if err != nil {
			return d, true, fmt.Errorf("%s %s: %w", d.Kind, name, err)
		}
		d.Fields = append(d.Fields, f)
	}
	if kind == model.DeclUnique {
		if strings.TrimSpace(remainder) != "" {
			return d, true, fmt.Errorf("unique %s: %q follows the columns; a unique constraint takes no other options", name, strings.TrimSpace(remainder))
		}
		return d, true, nil
	}
	return parseIndexOptions(d, remainder)
}

// parseIndexOptions reads what may follow an index's key list.
func parseIndexOptions(d model.SchemaDecl, rest string) (model.SchemaDecl, bool, error) {
	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return d, true, nil
		}
		word, after := takeWord(rest)
		switch strings.ToLower(word) {
		case "unique":
			d.Unique, rest = true, after
		case "concurrently":
			d.Concurrently, rest = true, after
		case "using":
			method, tail := takeWord(strings.TrimSpace(after))
			if method == "" {
				return d, true, fmt.Errorf("index %s: using needs a method", d.Name)
			}
			d.Method, rest = method, tail
		case "include":
			cols, tail, err := takeList(after)
			if err != nil {
				return d, true, fmt.Errorf("index %s: include: %w", d.Name, err)
			}
			d.Include, rest = cols, tail
		case "where":
			expr, tail, err := takeQuoted(after)
			if err != nil {
				return d, true, fmt.Errorf("index %s: where: %w", d.Name, err)
			}
			d.Expr, rest = expr, tail
		default:
			return d, true, fmt.Errorf("index %s: unknown option %q; the options are unique, concurrently, using, include and where", d.Name, word)
		}
	}
}

// parseKey reads one index key: a column, or an expression in quotes, with its
// ordering and operator class.
func parseKey(s string) (model.SchemaDeclField, error) {
	f := model.SchemaDeclField{}
	rest := strings.TrimSpace(s)
	if rest == "" {
		return f, fmt.Errorf("empty key")
	}

	if rest[0] == '"' || rest[0] == '\'' {
		expr, after, err := takeQuoted(rest)
		if err != nil {
			return f, err
		}
		f.Expression, rest = expr, after
	} else {
		f.Field, rest = takeWord(rest)
	}

	for {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return f, nil
		}
		word, after := takeWord(rest)
		switch strings.ToLower(word) {
		case "asc":
			rest = after
		case "desc":
			f.Desc, rest = true, after
		case "nulls":
			which, tail := takeWord(strings.TrimSpace(after))
			switch strings.ToLower(which) {
			case "first":
				f.NullsFirst, rest = true, tail
			case "last":
				f.NullsLast, rest = true, tail
			default:
				return f, fmt.Errorf("key %q: nulls takes first or last", s)
			}
		default:
			// Anything else in a key position is an operator class, which is
			// the only remaining thing PostgreSQL accepts there.
			if f.OpClass != "" {
				return f, fmt.Errorf("key %q: %q is not an ordering or an operator class", s, word)
			}
			f.OpClass, rest = word, after
		}
	}
}

// takeWord splits off the first whitespace-delimited word.
func takeWord(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// takeQuoted reads a quoted string, which is how SQL enters a directive.
func takeQuoted(s string) (value, rest string, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", "", fmt.Errorf("expected a quoted expression")
	}
	quote := s[0]
	if quote != '"' && quote != '\'' {
		return "", "", fmt.Errorf("expected a quoted expression, found %q", s)
	}
	if quote == '"' {
		// A Go-quoted string, so that a condition containing a quote can be
		// written the way Go writes one.
		unquoted, remainder, err := unquotePrefix(s)
		if err != nil {
			return "", "", err
		}
		return unquoted, remainder, nil
	}
	end := strings.IndexByte(s[1:], '\'')
	if end < 0 {
		return "", "", fmt.Errorf("unterminated expression: %s", s)
	}
	return s[1 : 1+end], s[2+end:], nil
}

// unquotePrefix reads a Go string literal from the front of s.
func unquotePrefix(s string) (string, string, error) {
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == '"' {
			v, err := strconv.Unquote(s[:i+1])
			if err != nil {
				return "", "", fmt.Errorf("%s: %w", s[:i+1], err)
			}
			return v, s[i+1:], nil
		}
	}
	return "", "", fmt.Errorf("unterminated expression: %s", s)
}

// takeList reads a parenthesised, comma-separated list.
func takeList(s string) ([]string, string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s[0] != '(' {
		return nil, "", fmt.Errorf("expected a parenthesised list, found %q", s)
	}
	depth, inQuote := 0, byte(0)
	for i := 0; i < len(s); i++ {
		switch {
		case inQuote != 0:
			if s[i] == inQuote {
				inQuote = 0
			}
		case s[i] == '"' || s[i] == '\'':
			inQuote = s[i]
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
			if depth == 0 {
				return splitList(s[1:i]), s[i+1:], nil
			}
		}
	}
	return nil, "", fmt.Errorf("unterminated list: %s", s)
}

// splitList splits a list body on commas outside parentheses and quotes.
func splitList(s string) []string {
	var (
		out     []string
		depth   int
		inQuote byte
		start   int
	)
	for i := 0; i < len(s); i++ {
		switch {
		case inQuote != 0:
			if s[i] == inQuote {
				inQuote = 0
			}
		case s[i] == '"' || s[i] == '\'':
			inQuote = s[i]
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
		case s[i] == ',' && depth == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if tail := strings.TrimSpace(s[start:]); tail != "" || len(out) > 0 {
		out = append(out, tail)
	}
	// A trailing comma leaves an empty entry, which is a typo rather than a
	// column.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}
