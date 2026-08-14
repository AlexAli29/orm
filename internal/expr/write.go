package expr

import (
	"fmt"
)

// The write statements.
//
// They share everything with the read side that can be shared: the same
// sources, the same columns, the same predicate nodes, the same argument
// writer, the same identifier quoting and the same scope. A second SQL engine
// for writes would be a second place for a quoting mistake to live.
//
// What differs is where a column may be qualified. PostgreSQL forbids
// qualifying the target of an UPDATE ... SET, and an INSERT's column list,
// conflict target and RETURNING list are all bare by grammar. So writes render
// column names bare in those positions while still checking that each column
// belongs to the table being written — the check is about correctness, the
// qualification about syntax.

// Insert adds rows to a table.
type Insert struct {
	Into    *Source
	Columns []Column
	// Rows holds one slice of values per row, aligned with Columns.
	Rows [][]Node
	// Returning is the column list read back. It is always explicit: a
	// RETURNING * would decide the scan order at the server, where the
	// generated scanner cannot see it.
	Returning []Column
	// ReturningItems is the projected form, used when a caller asked for a
	// result shape rather than the whole entity. One or the other is set.
	ReturningItems []SelectItem
	Conflict       *Conflict
}

// Conflict is an ON CONFLICT clause.
type Conflict struct {
	// Target is the column list PostgreSQL matches a conflict against.
	Target []Column
	// Set is the assignment list of DO UPDATE. An empty Set is DO NOTHING.
	Set []Assignment
	// Where restricts which conflicting rows the update touches. PostgreSQL
	// scopes it to the target table and EXCLUDED, which is a different scope
	// from the INSERT's own — there is no row to filter before the conflict.
	Where Node
}

// Compile renders the statement and its parameters.
func (i *Insert) Compile() (string, []any, error) { return compileAlone(i) }

// write renders the statement into an existing writer, which is what lets an
// INSERT ... RETURNING stand as a WITH item of a larger statement.
func (i *Insert) write(w *writer) error {
	switch {
	case i.Into == nil:
		return fmt.Errorf("insert has no table")
	case len(i.Columns) == 0:
		return fmt.Errorf("insert has no columns")
	case len(i.Rows) == 0:
		return fmt.Errorf("insert has no rows")
	}

	w.scope.Push()
	defer w.scope.Pop()
	if err := w.scope.Add(i.Into); err != nil {
		return err
	}

	w.b.WriteString("INSERT INTO ")
	w.source(i.Into)
	w.b.WriteString(" (")
	w.columnList(i.Columns)
	w.b.WriteString(") VALUES ")

	for r, row := range i.Rows {
		if len(row) != len(i.Columns) {
			return fmt.Errorf("row %d has %d values for %d columns", r, len(row), len(i.Columns))
		}
		if r > 0 {
			w.b.WriteString(", ")
		}
		w.b.WriteByte('(')
		for c, v := range row {
			if c > 0 {
				w.b.WriteString(", ")
			}
			w.node(v, true)
		}
		w.b.WriteByte(')')
	}

	if i.Conflict != nil {
		if err := w.conflict(i.Conflict); err != nil {
			return err
		}
	}

	w.returning(i.Returning, i.ReturningItems)
	return nil
}

func (w *writer) conflict(c *Conflict) error {
	if len(c.Target) == 0 {
		return fmt.Errorf("on conflict has no target columns")
	}
	w.b.WriteString(" ON CONFLICT (")
	w.columnList(c.Target)
	w.b.WriteString(")")

	if len(c.Set) == 0 {
		w.b.WriteString(" DO NOTHING")
		return nil
	}
	w.b.WriteString(" DO UPDATE SET ")
	w.assignments(c.Set)
	if c.Where != nil && !IsTrue(c.Where) {
		w.b.WriteString(" WHERE ")
		w.node(c.Where, false)
	}
	return nil
}

// returning writes a RETURNING clause from whichever form the caller supplied.
//
// The two are never both set: a statement returns the entity's columns or a
// projection's expressions. Neither is ever RETURNING *, because the scan order
// would then be decided at the server, where the generated scanner cannot see
// it.
func (w *writer) returning(cols []Column, items []SelectItem) {
	switch {
	case len(items) > 0:
		w.b.WriteString(" RETURNING ")
		w.selectList(items)
	case len(cols) > 0:
		w.b.WriteString(" RETURNING ")
		w.columnList(cols)
	}
}

// Update changes rows already in a table.
type Update struct {
	Table          *Source
	Set            []Assignment
	Where          Node
	Returning      []Column
	ReturningItems []SelectItem
}

// Compile renders the statement and its parameters.
//
// A statement with no WHERE is compiled without one: refusing to update every
// row is the builder's job, because only the builder can tell an author who
// forgot from one who meant it.
func (u *Update) Compile() (string, []any, error) { return compileAlone(u) }

// write renders the statement into an existing writer.
func (u *Update) write(w *writer) error {
	switch {
	case u.Table == nil:
		return fmt.Errorf("update has no table")
	case len(u.Set) == 0:
		return fmt.Errorf("update assigns nothing")
	}

	w.scope.Push()
	defer w.scope.Pop()
	if err := w.scope.Add(u.Table); err != nil {
		return err
	}

	w.b.WriteString("UPDATE ")
	w.source(u.Table)
	w.b.WriteString(" SET ")
	w.assignments(u.Set)

	if u.Where != nil && !IsTrue(u.Where) {
		w.b.WriteString(" WHERE ")
		w.node(u.Where, false)
	}
	w.returning(u.Returning, u.ReturningItems)
	return nil
}

// Delete removes rows from a table.
type Delete struct {
	From           *Source
	Where          Node
	Returning      []Column
	ReturningItems []SelectItem
}

// Compile renders the statement and its parameters.
func (d *Delete) Compile() (string, []any, error) { return compileAlone(d) }

// write renders the statement into an existing writer.
func (d *Delete) write(w *writer) error {
	if d.From == nil {
		return fmt.Errorf("delete has no table")
	}

	w.scope.Push()
	defer w.scope.Pop()
	if err := w.scope.Add(d.From); err != nil {
		return err
	}

	w.b.WriteString("DELETE FROM ")
	w.source(d.From)

	if d.Where != nil && !IsTrue(d.Where) {
		w.b.WriteString(" WHERE ")
		w.node(d.Where, false)
	}
	w.returning(d.Returning, d.ReturningItems)
	return nil
}

// columnList writes bare column names, which is what an INSERT column list, a
// conflict target and a RETURNING list all take.
func (w *writer) columnList(cols []Column) {
	for i, c := range cols {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.bareColumn(c)
	}
}

// bareColumn writes a column name unqualified, having first checked that the
// column belongs to the table the statement writes.
func (w *writer) bareColumn(c Column) {
	if c.Source == nil {
		w.fail(fmt.Errorf("column %q refers to no source", c.Name))
		return
	}
	if !w.scope.Visible(c.Source) {
		w.fail(&ScopeError{Column: c.Name, Source: c.Source, Visible: w.scope.Sources()})
		return
	}
	w.ident(c.Name)
}

// assignments writes a SET list. The targets are bare because PostgreSQL
// rejects a qualified one.
func (w *writer) assignments(as []Assignment) {
	for i, a := range as {
		if i > 0 {
			w.b.WriteString(", ")
		}
		w.bareColumn(a.Column)
		w.b.WriteString(" = ")
		if a.Value == nil {
			w.fail(fmt.Errorf("column %q is assigned nothing", a.Column.Name))
			continue
		}
		// The right-hand side of an assignment stands alone: there is no
		// operator beside it for precedence to be decided against, so it is
		// written unnested and "value" + $1 stays readable.
		w.node(a.Value, false)
	}
}
