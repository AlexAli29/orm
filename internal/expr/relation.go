package expr

import (
	"fmt"
	"strconv"
)

// The batched relation statement.
//
// A to-many relation is loaded for every parent at once, in one statement, so
// the number of statements does not depend on the number of rows. The question
// that makes it work is how the returned children get back to the right parent.
//
// The obvious answer — read the foreign key into Go and group by it — is wrong.
// Go equality is not PostgreSQL equality: citext compares case-insensitively,
// numeric ignores trailing zeroes, a domain carries its base type's semantics,
// and a configured type compares however its author decided. Grouping in Go
// would quietly mis-attach rows for any of them.
//
// So PostgreSQL does the matching. The parent keys go in as arrays, WITH
// ORDINALITY numbers them, and each returned child carries the ordinal of the
// parent it matched. Go only has to put row n with parent n-1, which is
// arithmetic rather than comparison.
//
//	SELECT "_k"."ord", "_c"."id", "_c"."title"
//	FROM unnest($1::int8[]) WITH ORDINALITY AS "_k"("k0", "ord")
//	JOIN "public"."posts" AS "_c" ON "_c"."author_id" = "_k"."k0"
//
// Note that the parent keys travel as one array parameter per key column, not
// one parameter per parent. A thousand parents cost the same one parameter as
// two, so there is no bind-parameter ceiling to chunk against.

// Reserved aliases for the batched relation statement. They carry the prefix
// the compiler keeps for itself, so a caller's alias can never collide.
const (
	keysAlias    = "_k"
	childAlias   = "_c"
	lateralAlias = "_l"
	ordColumn    = "ord"
)

// RelationSelect loads the children of many parents in one statement.
type RelationSelect struct {
	// Child is the table the related rows come from.
	//
	// It is the target's own occurrence rather than a fresh alias, because the
	// predicates and orderings a caller configures for the relation are built
	// from that target's generated descriptors. An alias would put every one of
	// them out of scope, and a raw fragment naming the table would break
	// outright.
	Child *Source
	// Columns are the child columns to read, after the ordinal.
	Columns []Column
	// KeyTypes are the PostgreSQL types of the parent key columns, in
	// constraint order, used to cast the input arrays.
	KeyTypes []string
	// ChildKeys are the child columns matched against the parent keys, in the
	// same order.
	ChildKeys []string
	// Args holds one array per key column.
	Args []any
	// Where restricts which related rows are loaded. It never restricts the
	// parents: a parent none of whose children match is still a parent, and
	// still gets its relation loaded, empty.
	Where Node
	// OrderBy orders the related rows within each parent.
	OrderBy []Order
	// Limit, when set, caps the rows loaded for each parent — not the rows the
	// statement returns. That is the whole reason the LATERAL form exists: a
	// LIMIT on the flat join would count rows across every parent at once and
	// give the first parent everything.
	Limit *int
}

// Compile renders the statement and its parameters.
func (s *RelationSelect) Compile() (string, []any, error) {
	if err := s.validate(); err != nil {
		return "", nil, err
	}
	w := &writer{}
	keys := &Source{table: keysAlias, alias: keysAlias}

	var err error
	if s.Limit != nil {
		err = s.writeLateral(w, keys)
	} else {
		err = s.writeJoined(w, keys)
	}
	if err != nil {
		return "", nil, err
	}
	if w.err != nil {
		return "", nil, w.err
	}
	return w.b.String(), w.args, nil
}

func (s *RelationSelect) validate() error {
	switch {
	case s.Child == nil:
		return fmt.Errorf("relation select has no child table")
	case len(s.Columns) == 0:
		return fmt.Errorf("relation select has no columns")
	case len(s.KeyTypes) == 0:
		return fmt.Errorf("relation select has no key columns")
	case len(s.KeyTypes) != len(s.ChildKeys):
		return fmt.Errorf("relation select has %d key types for %d child columns", len(s.KeyTypes), len(s.ChildKeys))
	case len(s.Args) != len(s.KeyTypes):
		return fmt.Errorf("relation select has %d key arrays for %d key columns", len(s.Args), len(s.KeyTypes))
	case s.Limit != nil && *s.Limit < 0:
		return fmt.Errorf("negative relation limit %d", *s.Limit)
	}
	return nil
}

// writeJoined renders the flat form: one join between the parent keys and the
// child table.
//
// This is the shape every relation without a per-parent limit uses, filtered or
// not. A filter is a WHERE, an ordering is an ORDER BY, and neither needs the
// server to evaluate a subquery once per parent.
func (s *RelationSelect) writeJoined(w *writer, keys *Source) error {
	w.scope.Push()
	defer w.scope.Pop()
	if err := w.scope.Add(s.Child); err != nil {
		return err
	}
	if err := w.scope.Add(keys); err != nil {
		return err
	}

	s.writeSelectList(w, keys, s.Columns)
	s.writeKeySource(w)

	w.b.WriteString(" JOIN ")
	w.source(s.Child)
	w.b.WriteString(" ON ")
	s.writeCorrelation(w, keys, s.Child)

	if s.Where != nil && !IsTrue(s.Where) {
		w.b.WriteString(" WHERE ")
		w.node(s.Where, false)
	}
	s.writeOrder(w, keys, s.OrderBy)
	return nil
}

// writeLateral renders the per-parent form.
//
// The subquery runs once for each parent key, so its LIMIT counts that parent's
// rows and nobody else's. Written flat, the same limit would be reached by the
// first parent with enough children and every later parent would come back
// empty — which looks like missing data rather than a mistake in the query.
func (s *RelationSelect) writeLateral(w *writer, keys *Source) error {
	// The subquery projects the child's columns, so outside it they are read
	// from the join's own name rather than from the child table, which is not
	// in scope out there.
	lateral := &Source{table: lateralAlias, alias: lateralAlias}
	outer := make([]Column, 0, len(s.Columns))
	for _, c := range s.Columns {
		outer = append(outer, Column{Source: lateral, Name: c.Name})
	}

	w.scope.Push()
	defer w.scope.Pop()
	if err := w.scope.Add(keys); err != nil {
		return err
	}
	if err := w.scope.Add(lateral); err != nil {
		return err
	}

	s.writeSelectList(w, keys, outer)
	s.writeKeySource(w)

	// The correlation moves inside, which is what LATERAL is for: the subquery
	// may refer to the row of the FROM item to its left.
	inner := &Select{
		From:    s.Child,
		Columns: s.Columns,
		OrderBy: s.OrderBy,
		Limit:   s.Limit,
		Where:   allOf(s.correlation(keys, s.Child), s.Where),
	}
	w.b.WriteString(" CROSS JOIN LATERAL (")
	if err := inner.write(w); err != nil {
		return err
	}
	w.b.WriteString(") AS ")
	w.ident(lateralAlias)

	// The subquery's ordering decides which rows each parent gets; it does not
	// survive into the result. Repeating it outside, over the projected
	// columns, is what makes the rows arrive in the order they were asked for
	// rather than in whatever order the join happened to produce them.
	s.writeOrder(w, keys, ordersOver(lateral, s.OrderBy))
	return nil
}

// writeSelectList writes the ordinal followed by the relation's columns.
func (s *RelationSelect) writeSelectList(w *writer, keys *Source, cols []Column) {
	w.b.WriteString("SELECT ")
	w.column(Column{Source: keys, Name: ordColumn})
	for _, c := range cols {
		w.b.WriteString(", ")
		w.column(c)
	}
}

// writeKeySource writes the numbered parent keys.
func (s *RelationSelect) writeKeySource(w *writer) {
	w.b.WriteString(" FROM unnest(")
	for i, t := range s.KeyTypes {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.arg(s.Args[i])
		w.b.WriteString("::")
		// The cast comes from the catalog by way of the generator, never from
		// a caller, so it is the one place a type name is written rather than
		// quoted as an identifier.
		if err := writeTypeCast(w, t); err != nil {
			w.fail(err)
			return
		}
	}
	w.b.WriteString(") WITH ORDINALITY AS ")
	w.ident(keysAlias)
	w.b.WriteString("(")
	for i := range s.KeyTypes {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.ident(keyColumn(i))
	}
	w.b.WriteString(", ")
	w.ident(ordColumn)
	w.b.WriteString(")")
}

// writeCorrelation writes the equality between each key column and its child
// column, in constraint order.
func (s *RelationSelect) writeCorrelation(w *writer, keys, child *Source) {
	for i, name := range s.ChildKeys {
		if i > 0 {
			w.b.WriteString(" AND ")
		}
		w.column(Column{Source: child, Name: name})
		w.b.WriteString(" = ")
		w.column(Column{Source: keys, Name: keyColumn(i)})
	}
}

// correlation builds the same equality as a node, for the LATERAL subquery's
// WHERE clause.
func (s *RelationSelect) correlation(keys, child *Source) Node {
	items := make([]Node, 0, len(s.ChildKeys))
	for i, name := range s.ChildKeys {
		items = append(items, Binary{
			Op:    OpEq,
			Left:  Column{Source: child, Name: name},
			Right: Column{Source: keys, Name: keyColumn(i)},
		})
	}
	if len(items) == 1 {
		return items[0]
	}
	return Group{Op: OpAnd, Items: items}
}

// writeOrder writes the relation's ordering, with the parent ordinal first.
//
// The ordinal is not decoration. Without it the statement's row order is only
// the ordering the caller asked for, and rows belonging to different parents
// interleave; each parent still receives its rows in the right order, because
// they are appended in arrival order, but the statement itself has no
// determinate output and a golden test of it would be testing the planner.
// Nothing is added when no ordering was requested: inventing one would make the
// server sort rows for a caller who never said the order mattered.
func (s *RelationSelect) writeOrder(w *writer, keys *Source, orders []Order) {
	if len(orders) == 0 {
		return
	}
	w.b.WriteString(" ORDER BY ")
	w.column(Column{Source: keys, Name: ordColumn})
	w.b.WriteString(" ASC")
	for _, o := range orders {
		w.b.WriteString(", ")
		w.column(o.Column)
		if o.Desc {
			w.b.WriteString(" DESC")
		} else {
			w.b.WriteString(" ASC")
		}
	}
}

// ordersOver re-expresses an ordering against another occurrence, which is what
// lets the outer statement repeat the ordering the LATERAL subquery applied.
//
// It is safe only because an Order is always a plain column: there is no
// fragment to rewrite and no expression to re-resolve, and the column is one the
// subquery projects, since a relation reads every mapped column of its target.
func ordersOver(src *Source, orders []Order) []Order {
	if len(orders) == 0 {
		return nil
	}
	out := make([]Order, 0, len(orders))
	for _, o := range orders {
		out = append(out, Order{Column: Column{Source: src, Name: o.Column.Name}, Desc: o.Desc})
	}
	return out
}

// allOf conjoins the nodes that carry a condition.
func allOf(nodes ...Node) Node {
	items := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil || IsTrue(n) {
			continue
		}
		items = append(items, n)
	}
	switch len(items) {
	case 0:
		return nil
	case 1:
		return items[0]
	default:
		return Group{Op: OpAnd, Items: items}
	}
}

// keyColumn names the nth key column of the ordinality table.
func keyColumn(i int) string { return "k" + strconv.Itoa(i) }

// writeTypeCast writes an array cast for a PostgreSQL type name, quoting the
// parts that are identifiers.
func writeTypeCast(w *writer, typ string) error {
	if typ == "" {
		return fmt.Errorf("relation select has an unnamed key type")
	}
	schema, name := "", typ
	if i := indexByte(typ, '.'); i >= 0 {
		schema, name = typ[:i], typ[i+1:]
	}
	if schema != "" {
		w.ident(schema)
		w.b.WriteByte('.')
	}
	w.ident(name)
	w.b.WriteString("[]")
	return nil
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}
