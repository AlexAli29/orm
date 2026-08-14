package schema

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/indexcmp"
)

// Semantic equality.
//
// Two schemas are equal when they describe the same database, not when they
// were built the same way. Order that carries meaning is compared as a sequence
// — a composite key's columns, an index's keys, an enum's labels — and order
// that does not is compared as a set.
//
// The differences are returned rather than a bare boolean, because the only
// time anybody asks this question is when the answer is no and they need to
// know why.

// Equal reports whether two schemas describe the same database.
func Equal(a, b *Schema) bool { return len(Diff(a, b)) == 0 }

// Diff returns the semantic differences between two schemas, in a deterministic
// order. An empty result means they are equal.
func Diff(a, b *Schema) []string {
	if a == nil {
		a = &Schema{}
	}
	if b == nil {
		b = &Schema{}
	}
	x, y := a.Clone(), b.Clone()
	x.Normalize()
	y.Normalize()

	var out []string
	out = append(out, diffEnums(x.Enums, y.Enums)...)
	out = append(out, diffTables(x.Tables, y.Tables)...)
	return out
}

func diffEnums(a, b []Enum) []string {
	var out []string
	byName := func(list []Enum) map[string]Enum {
		m := make(map[string]Enum, len(list))
		for _, e := range list {
			m[e.Qualified()] = e
		}
		return m
	}
	x, y := byName(a), byName(b)
	for _, name := range sortedKeys(x, y) {
		ex, okx := x[name]
		ey, oky := y[name]
		switch {
		case !oky:
			out = append(out, "enum "+name+" is only in the first schema")
		case !okx:
			out = append(out, "enum "+name+" is only in the second schema")
		// Label order is the type's sort order, so it is compared as a
		// sequence rather than as a set.
		case !slices.Equal(ex.Labels, ey.Labels):
			out = append(out, fmt.Sprintf("enum %s labels differ: [%s] and [%s]",
				name, strings.Join(ex.Labels, " "), strings.Join(ey.Labels, " ")))
		}
	}
	return out
}

func diffTables(a, b []Table) []string {
	var out []string
	byName := func(list []Table) map[string]Table {
		m := make(map[string]Table, len(list))
		for _, t := range list {
			m[t.Qualified()] = t
		}
		return m
	}
	x, y := byName(a), byName(b)
	for _, name := range sortedKeys(x, y) {
		tx, okx := x[name]
		ty, oky := y[name]
		switch {
		case !oky:
			out = append(out, "table "+name+" is only in the first schema")
		case !okx:
			out = append(out, "table "+name+" is only in the second schema")
		default:
			out = append(out, diffTable(tx, ty)...)
		}
	}
	return out
}

func diffTable(a, b Table) []string {
	var out []string
	name := a.Qualified()

	out = append(out, diffColumns(name, a.Columns, b.Columns)...)

	switch {
	case a.PrimaryKey == nil && b.PrimaryKey != nil:
		out = append(out, "table "+name+" has no primary key in the first schema")
	case a.PrimaryKey != nil && b.PrimaryKey == nil:
		out = append(out, "table "+name+" has no primary key in the second schema")
	case a.PrimaryKey != nil && !slices.Equal(a.PrimaryKey.Columns, b.PrimaryKey.Columns):
		out = append(out, fmt.Sprintf("table %s primary key columns differ: (%s) and (%s)",
			name, strings.Join(a.PrimaryKey.Columns, ", "), strings.Join(b.PrimaryKey.Columns, ", ")))
	}

	out = append(out, diffNamed(name, "unique", a.Uniques, b.Uniques,
		func(u Unique) string { return u.Name }, sameUnique)...)
	out = append(out, diffNamed(name, "foreign key", a.ForeignKeys, b.ForeignKeys,
		func(f ForeignKey) string { return f.Name }, sameForeignKey)...)
	out = append(out, diffNamed(name, "check", a.Checks, b.Checks,
		func(c Check) string { return c.Name }, sameCheck)...)
	out = append(out, diffNamed(name, "index", a.Indexes, b.Indexes,
		func(i Index) string { return i.Name }, sameIndex)...)
	return out
}

// diffColumns compares two column lists by name first and by position second.
//
// By name, because "the column is missing" is what somebody reading a drift
// report needs to be told; a positional comparison would report a dropped
// column as three columns that disagree with each other. By position as well,
// because column order is part of a table: it decides what SELECT * returns and
// what a positional scan reads.
func diffColumns(table string, a, b []Column) []string {
	var out []string
	x := make(map[string]Column, len(a))
	for _, c := range a {
		x[c.Name] = c
	}
	y := make(map[string]Column, len(b))
	for _, c := range b {
		y[c.Name] = c
	}
	for _, name := range sortedKeys(x, y) {
		cx, okx := x[name]
		cy, oky := y[name]
		switch {
		case !oky:
			out = append(out, fmt.Sprintf("table %s column %s is only in the first schema", table, name))
		case !okx:
			out = append(out, fmt.Sprintf("table %s column %s is only in the second schema", table, name))
		default:
			if d := diffColumn(table, cx, cy); d != "" {
				out = append(out, d)
			}
		}
	}
	if len(out) > 0 || len(a) != len(b) {
		return out
	}
	for i := range a {
		if a[i].Name != b[i].Name {
			return append(out, fmt.Sprintf("table %s declares its columns in a different order: (%s) and (%s)",
				table, joinColumnNames(a), joinColumnNames(b)))
		}
	}
	return out
}

func joinColumnNames(cols []Column) string {
	names := make([]string, 0, len(cols))
	for _, c := range cols {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

func diffColumn(table string, a, b Column) string {
	switch {
	case a.Name != b.Name:
		return fmt.Sprintf("table %s column names differ: %s and %s", table, a.Name, b.Name)
	case a.Type.Canonical() != b.Type.Canonical():
		return fmt.Sprintf("column %s.%s types differ: %s and %s", table, a.Name, a.Type, b.Type)
	case a.Nullable != b.Nullable:
		return fmt.Sprintf("column %s.%s nullability differs: %t and %t", table, a.Name, a.Nullable, b.Nullable)
	case a.Identity != b.Identity:
		return fmt.Sprintf("column %s.%s identity differs: %q and %q", table, a.Name, a.Identity, b.Identity)
	case !sameExpr(a.Default, b.Default):
		return fmt.Sprintf("column %s.%s defaults differ: %q and %q", table, a.Name, a.Default, b.Default)
	case !sameExpr(a.Generated, b.Generated):
		return fmt.Sprintf("column %s.%s generated expressions differ: %q and %q", table, a.Name, a.Generated, b.Generated)
	}
	return ""
}

// diffNamed compares two collections whose order carries no meaning.
func diffNamed[T any](table, kind string, a, b []T, name func(T) string, same func(T, T) bool) []string {
	byName := func(list []T) map[string]T {
		m := make(map[string]T, len(list))
		for _, v := range list {
			m[name(v)] = v
		}
		return m
	}
	x, y := byName(a), byName(b)
	var out []string
	for _, n := range sortedKeys(x, y) {
		vx, okx := x[n]
		vy, oky := y[n]
		switch {
		case !oky:
			out = append(out, fmt.Sprintf("%s %s on %s is only in the first schema", kind, n, table))
		case !okx:
			out = append(out, fmt.Sprintf("%s %s on %s is only in the second schema", kind, n, table))
		case !same(vx, vy):
			out = append(out, fmt.Sprintf("%s %s on %s differs", kind, n, table))
		}
	}
	return out
}

func sameUnique(a, b Unique) bool { return SameUnique(a, b) }

// SameUnique reports whether two unique objects are the same object.
//
// It is exported for the same reason [SameIndex] is: the migration planner has to
// answer the question this package's drift comparison answers, and for a while it
// answered it with its own copy. The two agreed and nothing kept them agreeing —
// the shape that let GiST comparison disappear from one half of index equality
// while every test of the other half stayed green.
//
// Nothing is hoisted into a package of its own here, unlike index equality. That
// split existed because comparing two indexes needs the default access method
// resolved and expressions reduced to a normal form first, and doing either in a
// caller would have put half the rule back in two places. A unique object has no
// such preliminary: the four fields that decide it are compared directly, and the
// expression comparison it uses already lives here. One implementation both
// callers reach is the whole of what was missing.
//
// What decides identity: the key columns as a sequence, because (a, b) and (b, a)
// are different objects; whether it is a constraint or a bare unique index,
// because only a constraint can be referenced by a foreign key; the partial
// predicate, because an index over some rows proves nothing about the others; and
// NULLS NOT DISTINCT, because it decides whether two NULLs collide. The name is
// not part of it — callers pair by name before asking.
func SameUnique(a, b Unique) bool {
	return slices.Equal(a.Columns, b.Columns) && a.Constraint == b.Constraint &&
		sameExpr(a.Where, b.Where) && a.NullsNotDistinct == b.NullsNotDistinct
}

func sameForeignKey(a, b ForeignKey) bool {
	return slices.Equal(a.Columns, b.Columns) && slices.Equal(a.RefColumns, b.RefColumns) &&
		a.RefSchema == b.RefSchema && a.RefTable == b.RefTable &&
		defaultAction(a.OnDelete) == defaultAction(b.OnDelete) &&
		defaultAction(a.OnUpdate) == defaultAction(b.OnUpdate) &&
		a.Deferrable == b.Deferrable && a.InitiallyDeferred == b.InitiallyDeferred &&
		a.NotValid == b.NotValid
}

func sameCheck(a, b Check) bool {
	return sameExpr(a.Expression, b.Expression) && a.NotValid == b.NotValid
}

// sameIndex compares everything that makes an index the object it is.
//
// Concurrently is deliberately absent. It is how an index was built, not a
// property the catalog keeps: PostgreSQL cannot report that an index was once
// created concurrently, so comparing it would make a live schema permanently unequal to
// the state that produced it.
func sameIndex(a, b Index) bool { return indexcmp.Equal(canonicalIndex(a), canonicalIndex(b)) }

// SameIndex reports whether two indexes are the same index.
//
// It is exported because the migration planner has to answer the same question
// this package's drift comparison does, and for a while it answered it with its
// own copy. The two agreed and nothing kept them agreeing: removing one axis
// from either left the other's tests green, so a project would either stop
// being told about an index changed by hand or stop being offered the migration
// for one it declared, with nothing red in between.
//
// So there is one rule, in [indexcmp], and one adapter into it, here. A caller
// that wants index equality calls this; a caller that writes its own field
// comparison is reintroducing the defect.
func SameIndex(a, b Index) bool { return sameIndex(a, b) }

// canonicalIndex reduces an index to what decides its identity.
//
// This is the only place a schema.Index becomes comparable, so the two things
// that have to be settled before comparison — the default access method, and
// the normal form expressions are compared in — are settled once. Doing either
// in a caller would put half the rule back where it was.
func canonicalIndex(i Index) indexcmp.Index {
	out := indexcmp.Index{
		Unique:  i.Unique,
		Method:  method(i.Method),
		Where:   normalizeExpr(i.Where),
		Include: i.Include,
		Keys:    make([]indexcmp.Key, 0, len(i.Columns)),
	}
	for _, c := range i.Columns {
		out.Keys = append(out.Keys, indexcmp.Key{
			Name:       c.Name,
			Expression: normalizeExpr(c.Expression),
			Direction:  int(c.Direction),
			Nulls:      int(c.Nulls),
			OpClass:    c.OpClass,
		})
	}
	return out
}

// method resolves the default access method.
//
// PostgreSQL stores btree for an index whose declaration named no method, so a
// declaration and a catalog reading of the same index differ in this field and
// in nothing else. Without this they would differ forever.
func method(m string) string {
	if m == "" {
		return "btree"
	}
	return m
}

// IndexMethod resolves an index's access method, defaulting an unset one to
// btree.
//
// It is exported for the migration checksum, which is not index equality — it
// deliberately covers whether the index was built CONCURRENTLY, which equality
// deliberately does not — but which has to resolve the default the same way.
// Two spellings of the default would make one index produce two checksums.
func IndexMethod(m string) string { return method(m) }

func defaultAction(a Action) Action {
	if a == "" {
		return NoAction
	}
	return a
}

// sameExpr compares two schema expressions after normalising the differences
// PostgreSQL introduces when it stores and re-renders one.
//
// The catalog does not keep the text somebody typed: it keeps a parse tree and
// renders it back, which adds parentheses, qualifies types and casts literals.
// Comparing the raw strings would report a change on every run for a default
// nobody touched, which is the churn that makes a migration tool untrustworthy.
func sameExpr(a, b Expr) bool {
	return normalizeExpr(a) == normalizeExpr(b)
}

// SameExpr reports whether two schema expressions mean the same thing.
//
// It is exported because the diff has to answer the same question the drift
// check does. If they answered it differently, a database adopted as a baseline
// would immediately need a migration to change a default into the identical
// default PostgreSQL had already rendered back at us.
func SameExpr(a, b Expr) bool { return sameExpr(a, b) }

// normalizeExpr reduces an expression to a form two spellings of the same thing
// share.
//
// It is deliberately conservative: it removes whitespace differences, outer
// parentheses and the type casts PostgreSQL adds to literals. It does not try
// to understand the expression, because an expression comparison that guessed
// would be worse than one that occasionally reports a difference a human then
// looks at.
func normalizeExpr(e Expr) string {
	s := strings.TrimSpace(string(e))
	if s == "" {
		return ""
	}
	// Each pass can expose work for the next: stripping a cast leaves the
	// parentheses that wrapped its operand, and unwrapping those can leave the
	// whole expression parenthesised.
	for range 8 {
		before := s
		// PostgreSQL renders a stored expression fully parenthesised.
		for len(s) > 1 && s[0] == '(' {
			inner, rest, ok := cutParen(s)
			if !ok || strings.TrimSpace(rest) != "" {
				break
			}
			s = strings.TrimSpace(inner)
		}
		// A literal comes back with the cast that gives it its type, which the
		// declaration did not have to write.
		s = stripCasts(s)
		s = unwrapAtoms(s)
		if s == before {
			break
		}
	}
	return strings.Join(strings.Fields(s), " ")
}

// unwrapAtoms removes the parentheses PostgreSQL puts round a bare name.
//
// Widening a column re-renders every expression mentioning it: email <> ”
// becomes (email)::text <> ”::text, and once the cast is stripped the
// parentheses are all that is left of it. Without this, changing a column's
// type would make every check and default over that column look different for
// ever — a migration proposed on every run for a change nobody made.
//
// It is deliberately narrow. Only a parenthesised run that is a plain name is
// unwrapped; anything with an operator, a call or a space inside keeps its
// parentheses, because there they may be what decides the meaning.
func unwrapAtoms(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '(' {
			// A quoted run is copied whole: parentheses inside a literal are
			// data, and an identifier's quotes are part of its name.
			if q := skipLiteral(s, i); q > i {
				b.WriteString(s[i:q])
				i = q
				continue
			}
			b.WriteByte(s[i])
			i++
			continue
		}
		inner, rest, ok := cutParen(s[i:])
		if !ok || !isPlainName(inner) {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString(inner)
		i = len(s) - len(rest)
	}
	return b.String()
}

// skipLiteral returns the offset past a quoted run beginning at i, or i itself
// when nothing starts there.
func skipLiteral(s string, i int) int {
	q := s[i]
	if q != '\'' && q != '"' {
		return i
	}
	for j := i + 1; j < len(s); j++ {
		if s[j] != q {
			continue
		}
		if j+1 < len(s) && s[j+1] == q {
			j++
			continue
		}
		return j + 1
	}
	return len(s)
}

// isPlainName reports whether s is an unqualified or qualified identifier and
// nothing else.
func isPlainName(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	start := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if start {
				return false
			}
			start = true
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80:
			start = false
		case c >= '0' && c <= '9':
			if start {
				return false
			}
		default:
			return false
		}
	}
	return !start
}

// stripCasts removes trailing ::type annotations from a rendered expression.
func stripCasts(s string) string {
	var (
		b       strings.Builder
		inQuote bool
	)
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			inQuote = !inQuote
			b.WriteByte(s[i])
			continue
		}
		if !inQuote && i+1 < len(s) && s[i] == ':' && s[i+1] == ':' {
			// Skip the cast: everything up to the next character that cannot be
			// part of a type name.
			i += 2
			for i < len(s) && (isTypeChar(s[i])) {
				i++
			}
			i--
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isTypeChar(c byte) bool {
	return c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '[' || c == ']'
}

func cutParen(s string) (inside, rest string, ok bool) {
	depth, inQuote := 0, false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\'':
			inQuote = !inQuote
		case inQuote:
		case s[i] == '(':
			depth++
		case s[i] == ')':
			depth--
			if depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// sortedKeys returns the union of two maps' keys, sorted.
func sortedKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}
