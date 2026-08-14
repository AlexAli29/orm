package orm

import (
	"context"
	"fmt"
	"iter"

	"github.com/AlexAli29/orm/observe"
)

// Raw is the whole-statement escape hatch.
//
// [Expr] lets a caller write a fragment inside a query this package builds;
// Raw lets them write the statement and keep the generated scanner:
//
//	users, err := orm.Raw(db.Users, `
//	    SELECT id, email, age, created_at
//	    FROM users
//	    WHERE similarity(email, $1) > 0.4
//	    ORDER BY similarity(email, $1) DESC
//	`, term).All(ctx)
//
// It exists because PostgreSQL is larger than any typed API, and a query
// builder people cannot escape is a query builder people abandon. What is given
// up is the type checking of the statement — nothing here validates the SQL,
// rewrites its select list, or infers anything from it. What is kept is
// scanning: rows land in E through the same generated destinations an ordinary
// query uses, with no reflection.
//
// # The column contract
//
// The statement must return every mapped column of E, in the order the
// generated metadata declares them. That order is the one the entity's fields
// declare, which [Query.SQL] will print for any entity. Column *names* are not
// matched — an alias is legal and ignored — so the contract is positional, and
// the count is checked before any row is read.
//
// # Values are still parameters
//
// Placeholders are PostgreSQL's own and are passed through untouched: $1 is $1,
// because Raw owns the whole statement and has nothing to renumber into. Pass
// values as arguments:
//
//	orm.Raw(db.Users, `SELECT ... WHERE email = $1`, email)
//
// rather than formatting them into the text, which is the one thing this
// package never does on a caller's behalf.
//
// The repository decides which executor runs it, so a Raw query built from a
// repository inside a transaction runs inside that transaction.
func Raw[E any](r *Repo[E], sql string, args ...any) *RawQuery[E] {
	q := &RawQuery[E]{sql: sql, args: args}
	switch {
	case r == nil:
		q.err = fmt.Errorf("raw query: no repository")
	default:
		q.repo = r
		q.err = r.meta.validate()
	}
	return q
}

// RawQuery is a statement a caller wrote, scanned into entity E.
//
// Its terminals mirror the typed query's — [RawQuery.All], [RawQuery.One] and
// [RawQuery.Rows] — and return the same errors for the same reasons. There is
// no Count or Exists: a caller who is writing their own SQL can write those
// themselves, and a wrapper that tried to would have to rewrite a statement
// this type promises not to read.
type RawQuery[E any] struct {
	repo *Repo[E]
	sql  string
	args []any
	err  error
}

// SQL returns the statement and its arguments, unchanged.
//
// It is here so that a raw query logs and tests like any other, and so that the
// promise that nothing is rewritten is one a caller can check.
func (q *RawQuery[E]) SQL() (string, []any, error) {
	if q.err != nil {
		return "", nil, q.err
	}
	return q.sql, q.args, nil
}

// exec runs the statement and checks that it returns the columns E needs.
// startSpan begins tracing this statement.
//
// The SQL is the caller's own, and the event says so: [observe.StartEvent.Raw]
// is what lets an adapter treat it differently from SQL the ORM built. It has
// to, because a caller's SQL may contain literals the ORM cannot redact — that
// would mean parsing SQL — so ormslog and ormotel keep raw SQL behind a
// separate switch that is off by default.
//
// Without this the switch could never fire: the raw path emitted no event at
// all, so a raw query was invisible to tracing and the option that exists to
// govern it governed nothing.
func (q *RawQuery[E]) startSpan(ctx context.Context) (context.Context, span) {
	if q.err != nil || q.repo == nil || !tracing(q.repo.ex) {
		return ctx, span{}
	}
	return startSpan(ctx, q.repo.ex, observe.StartEvent{
		Op:    observe.OpQuery,
		SQL:   q.sql,
		Args:  len(q.args),
		Table: q.repo.meta.Table.String(),
		Raw:   true,
	})
}

func (q *RawQuery[E]) exec(ctx context.Context) (pgxRows, error) {
	if q.err != nil {
		return nil, q.err
	}
	if q.repo.ex == nil {
		return nil, fmt.Errorf("raw %s query: the repository has no executor", q.repo.meta.Table)
	}
	rows, err := q.repo.ex.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, fmt.Errorf("raw %s query: %w", q.repo.meta.Table, err)
	}
	// Checked before a row is read, so a statement selecting the wrong shape
	// fails with what it did rather than with a scan error naming a column the
	// caller never mentioned.
	if got, want := len(rows.FieldDescriptions()), len(q.repo.meta.Columns); got != want {
		rows.Close()
		return nil, fmt.Errorf("%w: the statement returns %d columns and %s has %d mapped; select every mapped column, in the order the entity declares them",
			ErrRawColumns, got, q.repo.meta.Table, want)
	}
	return rows, nil
}

// All runs the statement and returns every row.
func (q *RawQuery[E]) All(ctx context.Context) (out []E, err error) {
	ctx, sp := q.startSpan(ctx)
	defer func() { sp.end(err, int64(len(out)), err == nil) }()

	rows, err := q.exec(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scan := q.repo.rawScanner()
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading raw %s rows: %w", q.repo.meta.Table, err)
	}
	return out, nil
}

// One runs the statement and returns the single row it matched, reporting
// [ErrNotFound] when there was none and [ErrMultipleRows] when there was more
// than one.
//
// Unlike the typed [Query.One] it cannot limit the statement to two rows —
// there is nothing here that rewrites SQL — so it reads until it has an answer.
// A statement that could match many rows should say so itself, with its own
// LIMIT.
func (q *RawQuery[E]) One(ctx context.Context) (_ E, err error) {
	var zero E

	ctx, sp := q.startSpan(ctx)
	defer func() { sp.end(err, 1, err == nil) }()

	rows, err := q.exec(ctx)
	if err != nil {
		return zero, err
	}
	defer rows.Close()

	scan := q.repo.rawScanner()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, fmt.Errorf("reading raw %s rows: %w", q.repo.meta.Table, err)
		}
		return zero, fmt.Errorf("%s: %w", q.repo.meta.Table, ErrNotFound)
	}
	out, err := scan(rows)
	if err != nil {
		return zero, err
	}
	if rows.Next() {
		return zero, fmt.Errorf("%s: %w", q.repo.meta.Table, ErrMultipleRows)
	}
	if err := rows.Err(); err != nil {
		return zero, fmt.Errorf("reading raw %s rows: %w", q.repo.meta.Table, err)
	}
	return out, nil
}

// Rows runs the statement and yields entities one at a time, buffering nothing.
// Stopping early closes the result set, so a break is safe.
func (q *RawQuery[E]) Rows(ctx context.Context) iter.Seq2[E, error] {
	return func(yield func(E, error) bool) {
		var zero E

		// A stream's span ends when the iteration does, however it does:
		// exhausted, abandoned by a break, or stopped by an error. The row
		// count is what was actually delivered.
		ctx, sp := q.startSpan(ctx)
		var (
			delivered int64
			failure   error
		)
		defer func() { sp.end(failure, delivered, failure == nil) }()

		rows, err := q.exec(ctx)
		if err != nil {
			failure = err
			yield(zero, err)
			return
		}
		defer rows.Close()

		scan := q.repo.rawScanner()
		for rows.Next() {
			e, err := scan(rows)
			if err != nil {
				failure = err
				yield(zero, err)
				return
			}
			delivered++
			if !yield(e, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			failure = fmt.Errorf("reading raw %s rows: %w", q.repo.meta.Table, err)
			yield(zero, failure)
		}
	}
}

// rawScanner reads one row into a new entity through the generated
// destinations, which is what a raw query keeps that a hand-written scan gives
// up.
func (r *Repo[E]) rawScanner() func(pgxRows) (E, error) {
	meta := r.meta
	dest := make([]any, len(meta.Columns))
	return func(rows pgxRows) (E, error) {
		var e E
		for i := range dest {
			p := meta.Dest(&e, i)
			if p == nil {
				return e, fmt.Errorf("scanning %s: metadata has no destination for column %d (%s)", meta.Table, i, meta.Columns[i].Name)
			}
			dest[i] = p
		}
		if err := rows.Scan(dest...); err != nil {
			return e, fmt.Errorf("scanning raw %s row: %w", meta.Table, err)
		}
		return e, nil
	}
}
