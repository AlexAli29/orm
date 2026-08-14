package expr

import "fmt"

// JoinKind is the SQL join operator.
type JoinKind uint8

// The joins PostgreSQL performs. The zero value is invalid on purpose: a join
// with no kind is a mistake the compiler should name, not a LEFT JOIN nobody
// asked for.
const (
	joinInvalid JoinKind = iota
	JoinInner
	JoinLeft
	JoinRight
	JoinFull
	JoinCross
)

var joinSQL = map[JoinKind]string{
	JoinInner: "JOIN",
	JoinLeft:  "LEFT JOIN",
	JoinRight: "RIGHT JOIN",
	JoinFull:  "FULL JOIN",
	JoinCross: "CROSS JOIN",
}

// String returns the join's PostgreSQL spelling.
func (k JoinKind) String() string {
	if s, ok := joinSQL[k]; ok {
		return s
	}
	return "?"
}

// Join is a source brought into a statement alongside the ones before it.
//
// Before M11 there was one construction site — the relation planner, which
// only ever joined LEFT — and the type carried no operator at all. Now that
// joins are something a caller writes, the operator is part of the node, and
// the nullability the operator induces is decided above this package: nothing
// here knows what a Go type is, so nothing here can widen one.
type Join struct {
	Kind   JoinKind
	Source *Source
	// On is the join condition. CROSS JOIN has none; every other kind must,
	// because a join with no condition and an operator that is not CROSS is
	// either a syntax error or an accident.
	On Node
	// Lateral marks a subquery source that may refer to the FROM items
	// introduced before it. Without it PostgreSQL evaluates a FROM subquery
	// once, independently of its siblings, which is why a plain derived table
	// cannot correlate and a LATERAL one can.
	Lateral bool
}

// writeJoin renders one join clause.
func (w *writer) writeJoin(j Join) {
	op, ok := joinSQL[j.Kind]
	if !ok {
		w.fail(fmt.Errorf("cannot compile join operator %d", j.Kind))
		return
	}
	w.b.WriteByte(' ')
	w.b.WriteString(op)
	if j.Lateral {
		w.b.WriteString(" LATERAL")
	}
	w.b.WriteByte(' ')
	w.source(j.Source)

	// PostgreSQL's grammar has no ON for a CROSS JOIN, and the builder above
	// this package refuses one, so reaching here with both is a tree nobody
	// could have built through the public API.
	if j.Kind == JoinCross {
		if j.On != nil {
			w.fail(fmt.Errorf("a CROSS JOIN has no ON condition"))
		}
		return
	}
	if j.On == nil {
		w.fail(fmt.Errorf("%s has no ON condition", op))
		return
	}
	w.b.WriteString(" ON ")
	w.node(j.On, false)
}
