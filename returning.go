package orm

import (
	"context"
	"errors"
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
)

// RETURNING.
//
// An update or a delete that returns rows is the only way to see what it did
// without a second statement — and for a delete it is the only way at all,
// because afterwards the rows are gone. So the values come back from the write
// itself; nothing here issues a SELECT before or after.
//
// The terminal set is deliberately smaller than a query's, and the reason is
// [Returning.One]. A read can be limited to two rows to detect a second one; a
// write cannot. LIMIT is not part of UPDATE, and adding one would change which
// rows the statement modified — the mutation would become smaller because
// somebody asked for one row back. So One runs the whole write and reports
// ErrMultipleRows afterwards, when the rows it returned have already been
// changed. That is stated rather than hidden, and it is why Rows is absent: a
// stream implies the caller may stop early and the work may stop with it, which
// is exactly what a mutation cannot promise.

// Returning is a write whose rows come back through a result shape.
type Returning[E, R any] struct {
	repo *Repo[E]
	proj Projection[E, R]
	// stmt is built by whichever write produced this, already carrying its
	// RETURNING list.
	stmt expr.Statement
	what string
	errs []error
}

func (t *Returning[E, R]) fail(err error) {
	if err != nil {
		t.errs = append(t.errs, err)
	}
}

// All runs the write and returns every row it touched.
//
// The rows are buffered. A write returning a great many rows therefore holds
// them all, which is stated rather than hidden: there is no streaming form,
// because a stream implies that stopping early stops the work, and the write
// has already happened by the time the first row is read.
func (t *Returning[E, R]) All(ctx context.Context) ([]R, error) {
	if len(t.errs) > 0 {
		return nil, errors.Join(t.errs...)
	}
	sql, args, err := t.stmt.Compile()
	if err != nil {
		return nil, fmt.Errorf("compiling while %s: %w", t.what, err)
	}
	if t.repo.ex == nil {
		return nil, fmt.Errorf("%s: the repository has no executor", t.what)
	}
	rows, err := t.repo.ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.what, err)
	}
	defer rows.Close()

	scan := t.proj.newScan()
	var out []R
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t.what, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", t.what, err)
	}
	return out, nil
}

// One runs the write and returns the single row it touched.
//
// The write is not narrowed to one row. It affects exactly the rows the builder
// described, and the count is checked afterwards — so a call that returns
// [ErrMultipleRows] has already changed every row it matched. That is the only
// honest way to offer this on a mutation: the alternative would be to add a
// limit the caller did not ask for and modify fewer rows than they described.
//
// Use it when the WHERE clause already identifies one row, such as a primary
// key. When it does not, All says what happened.
//
// An error from this method never means the write was undone. Outside a
// transaction nothing rolls it back; inside one, the transaction does.
func (t *Returning[E, R]) One(ctx context.Context) (R, error) {
	var zero R
	out, err := t.All(ctx)
	switch {
	case err != nil:
		return zero, err
	case len(out) == 0:
		return zero, ErrNotFound
	case len(out) > 1:
		return zero, fmt.Errorf("%w: the write affected %d rows", ErrMultipleRows, len(out))
	}
	return out[0], nil
}

// SQL renders the statement and its parameters without executing anything.
func (t *Returning[E, R]) SQL() (string, []any, error) {
	if len(t.errs) > 0 {
		return "", nil, errors.Join(t.errs...)
	}
	return t.stmt.Compile()
}

// entityProjection is the result shape that reads a whole entity back.
//
// It reuses the generated metadata rather than reflecting over the struct: the
// column list is the one the entity declares, in the order it declares it, and
// Dest is the same generated switch the read path scans through. So a
// ReturningEntity costs exactly what a SELECT of that entity costs.
func entityProjection[E any](meta *EntityMeta[E]) (Projection[E, E], []expr.Column) {
	cols := make([]expr.Column, 0, len(meta.Columns))
	for _, c := range meta.Columns {
		cols = append(cols, expr.Column{Source: meta.Source, Name: c.Name})
	}
	items := make([]expr.SelectItem, 0, len(cols))
	for _, c := range cols {
		items = append(items, expr.SelectItem{Node: c})
	}
	return Projection[E, E]{
		items: items,
		newScan: func() func(pgxRows) (E, error) {
			dest := make([]any, len(meta.Columns))
			return func(rows pgxRows) (E, error) {
				var e E
				for i := range dest {
					p := meta.Dest(&e, i)
					if p == nil {
						return e, fmt.Errorf("metadata has no destination for column %d (%s)", i, meta.Columns[i].Name)
					}
					dest[i] = p
				}
				if err := rows.Scan(dest...); err != nil {
					return e, err
				}
				return e, nil
			}
		},
	}, cols
}

// Returning runs the update and reads the projected values of the rows it
// changed.
//
//	updated, err := orm.UpdateReturning(
//	    db.Users.Update().Set(Users.Active.Set(true)).Where(Users.ID.Eq(id)),
//	    UserSummaries,
//	).All(ctx)
//
// The values come from the UPDATE itself. No SELECT is issued before or after.
func UpdateReturning[E, R any](u *Update[E], p Projection[E, R]) *Returning[E, R] {
	t := &Returning[E, R]{repo: u.repo, proj: p, what: "updating " + u.repo.meta.Table.String()}
	if err := p.validate(); err != nil {
		t.fail(err)
	}
	stmt, err := u.build()
	if err != nil {
		t.fail(err)
		return t
	}
	stmt.ReturningItems = p.items
	t.stmt = stmt
	return t
}

// UpdateReturningEntity runs the update and reads back whole entities.
func UpdateReturningEntity[E any](u *Update[E]) *Returning[E, E] {
	p, cols := entityProjection(u.repo.meta)
	t := &Returning[E, E]{repo: u.repo, proj: p, what: "updating " + u.repo.meta.Table.String()}
	stmt, err := u.build()
	if err != nil {
		t.fail(err)
		return t
	}
	// The entity form renders bare column names, which is what PostgreSQL's
	// RETURNING grammar takes and what the existing insert path already emits.
	stmt.Returning = cols
	t.stmt = stmt
	return t
}

// DeleteReturning runs the delete and reads the projected values of the rows it
// removed.
//
// This is the case RETURNING exists for: after the statement the rows are gone,
// so there is nothing left to select.
func DeleteReturning[E, R any](d *Delete[E], p Projection[E, R]) *Returning[E, R] {
	t := &Returning[E, R]{repo: d.repo, proj: p, what: "deleting from " + d.repo.meta.Table.String()}
	if err := p.validate(); err != nil {
		t.fail(err)
	}
	stmt, err := d.build()
	if err != nil {
		t.fail(err)
		return t
	}
	stmt.ReturningItems = p.items
	t.stmt = stmt
	return t
}

// DeleteReturningEntity runs the delete and reads back the whole entities it
// removed.
func DeleteReturningEntity[E any](d *Delete[E]) *Returning[E, E] {
	p, cols := entityProjection(d.repo.meta)
	t := &Returning[E, E]{repo: d.repo, proj: p, what: "deleting from " + d.repo.meta.Table.String()}
	stmt, err := d.build()
	if err != nil {
		t.fail(err)
		return t
	}
	stmt.Returning = cols
	t.stmt = stmt
	return t
}
