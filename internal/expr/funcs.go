package expr

import (
	"fmt"
	"strings"
)

// The expression nodes M11.4 adds.
//
// Every one of them is a node like any other, so each is selectable, comparable,
// groupable and nestable without anything downstream learning about it. What
// they have in common is where their syntax comes from: a function name, an
// operator spelling, a type name or a date field is written into the statement
// as text, so none of those ever comes from a caller. They come from
// constructors in this package's public layer, which is a closed set.

// Case is a searched CASE.
//
// PostgreSQL evaluates the branches in order and returns the first whose
// condition is TRUE. With no branch matching and no ELSE the result is NULL,
// which is why the typed layer above makes an ELSE-less CASE nullable whatever
// its branches produce.
type Case struct {
	When []CaseBranch
	// Else is the fallback. Nil means the CASE has none.
	Else Node
}

// CaseBranch is one WHEN ... THEN pair.
type CaseBranch struct {
	Cond Node
	Then Node
}

func (Case) node() {}

// Call is a function call.
//
// The name is written literally because it comes from this package's own
// constructors: quoting it would produce "lower"(x), which PostgreSQL reads as
// a call to a user-defined function of that name.
type Call struct {
	Func string
	Args []Node
}

func (Call) node() {}

// Cast is an explicit type conversion.
//
// The type name is syntax rather than a value, so it comes from the type
// descriptors above rather than from a caller, and its parts are quoted as
// identifiers — which is what keeps a schema-qualified type legal and a name
// containing a quote impossible.
type Cast struct {
	X    Node
	Type string
}

func (Cast) node() {}

// Extract pulls a field out of a date or timestamp.
//
// It is not a [Call] because PostgreSQL's grammar is not a call: the field is a
// keyword in the syntax rather than a value, so it cannot be a parameter and is
// written from the closed set the layer above offers.
type Extract struct {
	Field string
	X     Node
}

func (Extract) node() {}

// Infix is a binary operator this package writes.
//
// It is separate from [Binary] and [Arith] because those two carry operator
// enumerations whose members the compiler knows; this one carries the spelling
// itself, for the PostgreSQL operators that have no place in a comparison or an
// arithmetic lattice — the JSON and array operators, mainly. The spelling still
// never comes from a caller.
type Infix struct {
	Op          string
	Left, Right Node
}

func (Infix) node() {}

// Quantified is a comparison against every or any element of an array.
type Quantified struct {
	Op Op
	// All selects ALL rather than ANY.
	All         bool
	Left, Right Node
}

func (Quantified) node() {}

// RowValue is a tuple, which PostgreSQL compares element by element.
//
// It is what makes a composite key one comparison rather than a conjunction of
// several, and it is the shape an index on those columns is built for.
type RowValue struct {
	Items []Node
}

func (RowValue) node() {}

func (w *writer) writeCase(n Case) {
	if len(n.When) == 0 {
		w.fail(fmt.Errorf("a CASE has no WHEN branch"))
		return
	}
	w.b.WriteString("CASE")
	for _, br := range n.When {
		if br.Cond == nil || br.Then == nil {
			w.fail(fmt.Errorf("a CASE branch is incomplete"))
			return
		}
		w.b.WriteString(" WHEN ")
		w.node(br.Cond, false)
		w.b.WriteString(" THEN ")
		w.node(br.Then, false)
	}
	if n.Else != nil {
		w.b.WriteString(" ELSE ")
		w.node(n.Else, false)
	}
	w.b.WriteString(" END")
}

func (w *writer) writeCall(n Call) {
	if n.Func == "" {
		w.fail(fmt.Errorf("a function call has no name"))
		return
	}
	w.b.WriteString(n.Func)
	w.b.WriteByte('(')
	for i, a := range n.Args {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.node(a, false)
	}
	w.b.WriteByte(')')
}

func (w *writer) writeCast(n Cast) {
	if n.Type == "" {
		w.fail(fmt.Errorf("a cast has no target type"))
		return
	}
	w.b.WriteString("CAST(")
	w.node(n.X, false)
	w.b.WriteString(" AS ")
	if err := w.typeName(n.Type); err != nil {
		w.fail(err)
		return
	}
	w.b.WriteByte(')')
}

// typeName writes a possibly schema-qualified type, quoting each part and
// keeping an array suffix outside the quotes where PostgreSQL's grammar wants
// it.
func (w *writer) typeName(typ string) error {
	array := strings.HasSuffix(typ, "[]")
	base := strings.TrimSuffix(typ, "[]")
	schema, name := "", base
	if i := strings.IndexByte(base, '.'); i >= 0 {
		schema, name = base[:i], base[i+1:]
	}
	if name == "" {
		return fmt.Errorf("type name %q has no name part", typ)
	}
	if schema != "" {
		w.ident(schema)
		w.b.WriteByte('.')
	}
	w.ident(name)
	if array {
		w.b.WriteString("[]")
	}
	return nil
}

func (w *writer) writeExtract(n Extract) {
	if n.Field == "" {
		w.fail(fmt.Errorf("extract has no field"))
		return
	}
	// The field is a keyword in PostgreSQL's grammar rather than a value, so
	// it cannot be a parameter. It comes from a closed set of constants this
	// package defines, which is checked here as well as there.
	if !validDateField(n.Field) {
		w.fail(fmt.Errorf("extract field %q is not one this package writes", n.Field))
		return
	}
	w.b.WriteString("extract(")
	w.b.WriteString(n.Field)
	w.b.WriteString(" FROM ")
	w.node(n.X, false)
	w.b.WriteByte(')')
}

// dateFields is the set of field names extract may be given. It is a closed set
// because the name becomes syntax.
var dateFields = map[string]bool{
	"century": true, "day": true, "decade": true, "dow": true, "doy": true,
	"epoch": true, "hour": true, "isodow": true, "isoyear": true,
	"microseconds": true, "millennium": true, "milliseconds": true,
	"minute": true, "month": true, "quarter": true, "second": true,
	"timezone": true, "timezone_hour": true, "timezone_minute": true,
	"week": true, "year": true,
}

func validDateField(f string) bool { return dateFields[f] }

func (w *writer) writeInfix(n Infix) {
	if n.Op == "" {
		w.fail(fmt.Errorf("an operator expression has no operator"))
		return
	}
	// Parenthesised because these operators sit at precedences a reader should
	// not have to recall, and because the tree the caller built is the tree
	// PostgreSQL should evaluate.
	w.b.WriteByte('(')
	w.node(n.Left, true)
	w.b.WriteByte(' ')
	w.b.WriteString(n.Op)
	w.b.WriteByte(' ')
	w.node(n.Right, true)
	w.b.WriteByte(')')
}

func (w *writer) writeQuantified(n Quantified) {
	op, ok := opSQL[n.Op]
	if !ok {
		w.fail(fmt.Errorf("cannot compile quantified operator %d", n.Op))
		return
	}
	w.node(n.Left, true)
	w.b.WriteByte(' ')
	w.b.WriteString(op)
	if n.All {
		w.b.WriteString(" ALL(")
	} else {
		w.b.WriteString(" ANY(")
	}
	w.node(n.Right, false)
	w.b.WriteByte(')')
}

func (w *writer) writeRowValue(n RowValue) {
	if len(n.Items) < 2 {
		w.fail(fmt.Errorf("a row value needs at least two expressions"))
		return
	}
	w.b.WriteByte('(')
	for i, it := range n.Items {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.node(it, false)
	}
	w.b.WriteByte(')')
}

// Prefix is a unary operator this package writes.
//
// It exists for the handful PostgreSQL spells as an operator rather than as a
// function — tsquery negation is the one M12 needs — and, like [Infix], it
// carries the spelling because the spelling never comes from a caller.
type Prefix struct {
	Op string
	X  Node
}

func (Prefix) node() {}

func (w *writer) writePrefix(n Prefix) {
	if n.Op == "" {
		w.fail(fmt.Errorf("a prefix expression has no operator"))
		return
	}
	w.b.WriteByte('(')
	w.b.WriteString(n.Op)
	w.b.WriteByte(' ')
	w.node(n.X, true)
	w.b.WriteByte(')')
}
