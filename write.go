package orm

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlexAli29/orm/observe"
	"slices"

	"github.com/AlexAli29/orm/internal/expr"
)

// scope is the WHERE-or-everything decision a write builder has to make.
//
// The absence of a WHERE clause cannot mean "every row", because it is also
// what a forgotten one looks like, and the two have very different
// consequences. So neither is assumed: a write says which it meant.
type writeScope uint8

const (
	scopeUnset writeScope = iota
	scopeFiltered
	scopeAll
)

// Update changes rows of one table.
//
// Its methods mutate the builder and return it. Mistakes are recorded rather
// than raised and surface together from [Update.SQL] or [Update.Exec], and
// while any is recorded neither touches PostgreSQL.
type Update[E any] struct {
	repo  *Repo[E]
	sets  []Assign[E]
	where []Predicate[E]
	scope writeScope
	errs  []error
}

// Update starts an update over the entity's table.
func (r *Repo[E]) Update() *Update[E] {
	u := &Update[E]{repo: r}
	if err := r.meta.validate(); err != nil {
		u.fail(err)
	}
	return u
}

func (u *Update[E]) fail(err error) {
	if err != nil {
		u.errs = append(u.errs, err)
	}
}

// Set assigns values to columns.
//
// Assigning the same column twice is an error rather than last-one-wins. The
// two assignments came from somewhere, and quietly dropping one hides whichever
// piece of code was wrong.
func (u *Update[E]) Set(assignments ...Assign[E]) *Update[E] {
	for _, a := range assignments {
		if a.IsZero() {
			u.fail(a.Err())
			u.fail(errors.New("assignment names no column"))
			continue
		}
		if slices.ContainsFunc(u.sets, func(x Assign[E]) bool { return x.Column() == a.Column() }) {
			u.fail(fmt.Errorf("%w: %s is assigned more than once", ErrDuplicateAssignment, a.Column()))
			continue
		}
		if err := u.repo.checkAssignment(a, false); err != nil {
			u.fail(err)
			continue
		}
		u.sets = append(u.sets, a)
	}
	return u
}

// Where restricts which rows are updated. Several predicates, and several
// calls, combine with AND — the same predicates the read side uses.
func (u *Update[E]) Where(ps ...Predicate[E]) *Update[E] {
	if u.scope == scopeAll {
		u.fail(errors.New("Where was called after All, which already said every row"))
		return u
	}
	for _, p := range ps {
		u.fail(p.Err())
		if !p.IsZero() {
			u.where = append(u.where, p)
		}
	}
	u.scope = scopeFiltered
	return u
}

// All says the update is meant to affect every row.
//
// It is verbose on purpose. Without it an update carrying no WHERE is refused,
// because a full-table write is one of the few mistakes a person cannot undo
// and the shape that causes it — a predicate that was supposed to be there —
// looks exactly like the deliberate case.
func (u *Update[E]) All() *Update[E] {
	if u.scope == scopeFiltered {
		u.fail(errors.New("All was called after Where, which already restricted the rows"))
		return u
	}
	u.scope = scopeAll
	return u
}

// SQL renders the statement and its parameters without executing anything.
func (u *Update[E]) SQL() (string, []any, error) {
	stmt, err := u.build()
	if err != nil {
		return "", nil, err
	}
	sql, args, err := stmt.Compile()
	if err != nil {
		return "", nil, fmt.Errorf("compiling the update of %s: %w", u.repo.meta.Table, err)
	}
	return sql, args, nil
}

func (u *Update[E]) build() (*expr.Update, error) {
	if len(u.errs) > 0 {
		return nil, errors.Join(u.errs...)
	}
	if len(u.sets) == 0 {
		return nil, fmt.Errorf("update %s: %w", u.repo.meta.Table, ErrMissingSet)
	}
	if u.scope == scopeUnset {
		return nil, fmt.Errorf("update %s: %w", u.repo.meta.Table, ErrMissingWhere)
	}

	stmt := &expr.Update{Table: u.repo.source}
	for _, a := range u.sets {
		stmt.Set = append(stmt.Set, a.assignment)
	}
	if where := And(u.where...); !expr.IsTrue(where.node) {
		stmt.Where = where.node
	}
	return stmt, nil
}

// Exec runs the update and returns how many rows it changed, as PostgreSQL
// reported it. No second statement is issued to find that out.
func (u *Update[E]) Exec(ctx context.Context) (int64, error) {
	stmt, err := u.build()
	if err != nil {
		return 0, err
	}
	return u.repo.execWrite(ctx, stmt, "updating "+u.repo.meta.Table.String(), observe.OpUpdate)
}

// Delete removes rows of one table.
//
// It carries the same guard as [Update]: a delete with no WHERE is refused
// unless [Delete.All] says every row was meant.
type Delete[E any] struct {
	repo  *Repo[E]
	where []Predicate[E]
	scope writeScope
	errs  []error
}

// Delete starts a delete over the entity's table.
func (r *Repo[E]) Delete() *Delete[E] {
	d := &Delete[E]{repo: r}
	if err := r.meta.validate(); err != nil {
		d.fail(err)
	}
	return d
}

func (d *Delete[E]) fail(err error) {
	if err != nil {
		d.errs = append(d.errs, err)
	}
}

// Where restricts which rows are deleted. Several predicates, and several
// calls, combine with AND.
func (d *Delete[E]) Where(ps ...Predicate[E]) *Delete[E] {
	if d.scope == scopeAll {
		d.fail(errors.New("Where was called after All, which already said every row"))
		return d
	}
	for _, p := range ps {
		d.fail(p.Err())
		if !p.IsZero() {
			d.where = append(d.where, p)
		}
	}
	d.scope = scopeFiltered
	return d
}

// All says the delete is meant to empty the table.
func (d *Delete[E]) All() *Delete[E] {
	if d.scope == scopeFiltered {
		d.fail(errors.New("All was called after Where, which already restricted the rows"))
		return d
	}
	d.scope = scopeAll
	return d
}

// SQL renders the statement and its parameters without executing anything.
func (d *Delete[E]) SQL() (string, []any, error) {
	stmt, err := d.build()
	if err != nil {
		return "", nil, err
	}
	sql, args, err := stmt.Compile()
	if err != nil {
		return "", nil, fmt.Errorf("compiling the delete from %s: %w", d.repo.meta.Table, err)
	}
	return sql, args, nil
}

func (d *Delete[E]) build() (*expr.Delete, error) {
	if len(d.errs) > 0 {
		return nil, errors.Join(d.errs...)
	}
	if d.scope == scopeUnset {
		return nil, fmt.Errorf("delete from %s: %w", d.repo.meta.Table, ErrMissingWhere)
	}

	stmt := &expr.Delete{From: d.repo.source}
	if where := And(d.where...); !expr.IsTrue(where.node) {
		stmt.Where = where.node
	}
	return stmt, nil
}

// Exec runs the delete and returns how many rows it removed.
func (d *Delete[E]) Exec(ctx context.Context) (int64, error) {
	stmt, err := d.build()
	if err != nil {
		return 0, err
	}
	return d.repo.execWrite(ctx, stmt, "deleting from "+d.repo.meta.Table.String(), observe.OpDelete)
}

// checkWritable refuses an assignment PostgreSQL would reject.
func (r *Repo[E]) checkWritable(name string) error {
	for _, c := range r.meta.Columns {
		if c.Name != name {
			continue
		}
		if !c.Writable() {
			return fmt.Errorf("%s.%s %s", r.meta.Table, name, whyNotWritable(c))
		}
		return nil
	}
	return fmt.Errorf("%s is not a mapped column of %s", name, r.meta.Table)
}

// execWrite runs a statement that returns no rows and reports how many it
// affected.
//
// The count comes from the command tag PostgreSQL already sent, so nothing here
// asks the database a second question. Query rather than Exec is used because
// Executor is one method: pgx delivers the tag either way, and widening the
// interface would break every implementation of it for no gain.
func (r *Repo[E]) execWrite(ctx context.Context, stmt expr.Statement, what string, op observe.Op) (int64, error) {
	sql, args, err := stmt.Compile()
	if err != nil {
		return 0, fmt.Errorf("compiling while %s: %w", what, err)
	}
	if r.ex == nil {
		return 0, fmt.Errorf("%s: the repository has no executor", what)
	}

	// One event for one thing the caller asked for. It is emitted here rather
	// than around Exec so that a statement that fails to compile produces no
	// event at all: nothing ran.
	ctx, sp := r.startSpan(ctx, op, sql, args)
	n, err := r.execWriteRows(ctx, sql, args, what)
	sp.end(err, n, err == nil)
	return n, err
}

func (r *Repo[E]) execWriteRows(ctx context.Context, sql string, args []any, what string) (int64, error) {
	rows, err := r.ex.Query(ctx, sql, args...)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	// The command tag is only complete once the result is drained, so the
	// close has to happen before it is read rather than in a defer.
	for rows.Next() {
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("%s: %w", what, err)
	}
	return rows.CommandTag().RowsAffected(), nil
}

// startSpan begins tracing an operation rooted at this repository's table.
//
// It is a method on Repo because the entity name and the table are the
// repository's, and an event without them would be an event nobody can group.
func (r *Repo[E]) startSpan(ctx context.Context, op observe.Op, sql string, args []any) (context.Context, span) {
	if !tracing(r.ex) {
		return ctx, span{}
	}
	return startSpan(ctx, r.ex, observe.StartEvent{
		Op:    op,
		SQL:   sql,
		Args:  len(args),
		Table: r.meta.Table.String(),
	})
}
