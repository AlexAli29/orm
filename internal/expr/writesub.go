package expr

// Data-modifying statements as WITH items.
//
// PostgreSQL lets an INSERT, UPDATE or DELETE with a RETURNING clause be a WITH
// item, and the rows it returns are then a source like any other. It is not a
// separate feature here either: the three statements already share this
// package's sources, columns, scope and argument writer, so all that was
// missing was the ability to render into an enclosing writer rather than into
// one of their own.
//
// The semantics are PostgreSQL's and worth stating. A data-modifying WITH item
// executes exactly once, whatever the main query does with its rows — including
// not reading them — and every part of the statement sees the same snapshot, so
// the modified rows are not visible to the other parts except through the WITH
// item's own output.

// compileAlone renders a statement that is not nested inside another.
func compileAlone(s Subquery) (string, []any, error) {
	w := &writer{}
	if err := s.write(w); err != nil {
		return "", nil, err
	}
	if w.err != nil {
		return "", nil, w.err
	}
	return w.b.String(), w.args, nil
}

// The source accounting the three writes share.
//
// A write binds exactly one source — the table it writes — and is free in
// whatever its expressions name beyond that. In practice that is nothing: a
// write's scope is its own table and EXCLUDED, and the builder above refuses
// anything else. Computing it rather than assuming it is what keeps a WITH item
// containing a write honest when a later milestone widens what a write can say.

func (i *Insert) bound(add func(*Source)) { add(i.Into) }

func (i *Insert) free(add func(*Source)) { freeOf(i, add) }

func (i *Insert) each(add func(*Source)) { eachOf(i, add) }

func (i *Insert) walk(visit func(Node)) {
	for _, row := range i.Rows {
		for _, v := range row {
			visit(v)
		}
	}
	if i.Conflict != nil {
		for _, a := range i.Conflict.Set {
			visit(a.Value)
		}
		if i.Conflict.Where != nil {
			visit(i.Conflict.Where)
		}
	}
	for _, it := range i.ReturningItems {
		visit(it.Node)
	}
}

func (i *Insert) resultArity() int { return returningArity(i.Returning, i.ReturningItems) }

func (u *Update) bound(add func(*Source)) { add(u.Table) }

func (u *Update) free(add func(*Source)) { freeOf(u, add) }

func (u *Update) each(add func(*Source)) { eachOf(u, add) }

func (u *Update) walk(visit func(Node)) {
	for _, a := range u.Set {
		visit(a.Value)
	}
	if u.Where != nil {
		visit(u.Where)
	}
	for _, it := range u.ReturningItems {
		visit(it.Node)
	}
}

func (u *Update) resultArity() int { return returningArity(u.Returning, u.ReturningItems) }

func (d *Delete) bound(add func(*Source)) { add(d.From) }

func (d *Delete) free(add func(*Source)) { freeOf(d, add) }

func (d *Delete) each(add func(*Source)) { eachOf(d, add) }

func (d *Delete) walk(visit func(Node)) {
	if d.Where != nil {
		visit(d.Where)
	}
	for _, it := range d.ReturningItems {
		visit(it.Node)
	}
}

func (d *Delete) resultArity() int { return returningArity(d.Returning, d.ReturningItems) }

// walker is a statement whose expressions can be visited in render order.
type walker interface {
	Subquery
	walk(visit func(Node))
}

func freeOf(s walker, add func(*Source)) {
	own := make(map[*Source]bool)
	s.bound(func(src *Source) {
		if src != nil {
			own[src] = true
		}
	})
	s.walk(func(n Node) {
		walkSources(n, func(src *Source) {
			if !own[src] {
				add(src)
			}
		})
	})
}

func eachOf(s walker, add func(*Source)) {
	s.bound(func(src *Source) {
		if src != nil {
			add(src)
		}
	})
	s.walk(func(n Node) { walkEvery(n, add) })
}

// returningArity reports how many columns the statement hands back, which is
// zero when it hands back none — a write with no RETURNING is a legal WITH item
// and simply produces no rows to select from.
func returningArity(cols []Column, items []SelectItem) int {
	if len(items) > 0 {
		return len(items)
	}
	return len(cols)
}
