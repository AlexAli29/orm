package orm

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlexAli29/orm/observe"
	"iter"
	"slices"

	"github.com/AlexAli29/orm/internal/expr"
)

// Repo reads entities of type E through an Executor.
//
// There is one Repo implementation for every entity in every project, because
// nothing about reading a row depends on which row it is: the differences live
// in generated metadata, not in generated behaviour. Generated code wires the
// two together and adds no logic of its own.
type Repo[E any] struct {
	ex     Executor
	meta   *EntityMeta[E]
	source *Source
}

// NewRepo binds an executor to generated entity metadata. Generated code calls
// it; the metadata argument is the generated value for E.
func NewRepo[E any](ex Executor, meta *EntityMeta[E]) *Repo[E] {
	r := &Repo[E]{ex: ex, meta: meta}
	switch {
	case meta == nil:
	case meta.Source != nil:
		r.source = meta.Source
	default:
		r.source = NewSource(meta.Table.Schema, meta.Table.Name)
	}
	return r
}

// Query starts a new query over the entity's table.
//
// The returned builder is mutable and is not safe for concurrent use. Build one
// per query, or [Query.Clone] before branching.
func (r *Repo[E]) Query() *Query[E] {
	q := &Query[E]{repo: r, source: r.source}
	// The metadata is checked here because everything a query does depends on
	// it. The executor is not, because rendering SQL does not need one: a
	// query you can print without a database is a query you can inspect in a
	// test, or log before running.
	if err := r.meta.validate(); err != nil {
		q.fail(err)
	}
	return q
}

// QueryFrom starts a query reading from a particular occurrence of the entity's
// table, which is how a query selects through an alias:
//
//	manager := Users.As("manager")
//	q := db.Users.QueryFrom(manager.Source()).Where(manager.Email.Eq(addr))
//
// The occurrence must be of the entity's own table. Reading a User out of the
// posts table would produce a statement naming columns that table does not
// have, so the mismatch is reported here rather than discovered by PostgreSQL.
func (r *Repo[E]) QueryFrom(src *Source) *Query[E] {
	q := r.Query()
	switch {
	case src == nil:
		q.fail(errors.New("QueryFrom was given no source"))
	case r.meta != nil && !sourceIsTable(src, r.meta.Table):
		q.fail(fmt.Errorf("QueryFrom: %s is not an occurrence of %s", src, r.meta.Table))
	default:
		q.source = src
	}
	return q
}

// Query builds a SELECT over entity E.
//
// Its methods mutate the builder and return it, so a chain reads as one
// statement. Mistakes are recorded rather than raised: a builder method never
// panics and never returns an error, every mistake is kept, and all of them
// surface together from a terminal operation — [Query.SQL], [Query.All],
// [Query.One], [Query.Count], [Query.Exists] or [Query.Rows]. When any has been
// recorded, no terminal operation touches PostgreSQL.
//
// A Query is mutable and is not safe for concurrent use. Reusing one as the
// base for several queries needs [Query.Clone]; without it, each branch would
// see the others' conditions.
type Query[E any] struct {
	repo      *Repo[E]
	source    *Source
	wheres    []Predicate[E]
	orderBy   []Order[E]
	limit     *int
	offset    *int
	forUpdate bool
	lock      expr.Lock
	with      []relation[E]
	errs      []error
}

// fail records a construction mistake. Every one is kept: a caller who wrote
// two of them is better served seeing both than fixing one and running again.
func (q *Query[E]) fail(err error) {
	if err != nil {
		q.errs = append(q.errs, err)
	}
}

// Clone returns an independent copy, so that one query can be the base for
// several:
//
//	base := db.Users.Query().Where(Users.Active.Eq(true))
//	admins := base.Clone().Where(Users.Role.Eq(RoleAdmin))
//	recent := base.Clone().Where(Users.CreatedAt.Gte(cutoff))
//
// Without the clones, admins and recent would accumulate onto base and onto
// each other. Every slice the builder owns is copied, and the numbers behind
// limit and offset are copied rather than shared; the generated metadata is
// not, because nothing ever writes to it.
//
// Requested relations are copied all the way down, so a clone shares no part of
// the other's relation tree — not the options at any level, and not the list of
// relations nested under any of them.
func (q *Query[E]) Clone() *Query[E] {
	out := &Query[E]{
		repo:      q.repo,
		source:    q.source,
		wheres:    slices.Clone(q.wheres),
		orderBy:   slices.Clone(q.orderBy),
		forUpdate: q.forUpdate,
		lock:      q.lock,
		with:      slices.Clone(q.with),
		errs:      slices.Clone(q.errs),
	}
	for i := range out.with {
		out.with[i] = out.with[i].clone()
	}
	if q.limit != nil {
		n := *q.limit
		out.limit = &n
	}
	if q.offset != nil {
		n := *q.offset
		out.offset = &n
	}
	return out
}

// Where restricts the query. Several predicates in one call combine with AND,
// and so do several calls, so these three are the same query:
//
//	q.Where(a, b)
//	q.Where(And(a, b))
//	q.Where(a).Where(b)
//
// Passing no predicates, or only zero ones, restricts nothing.
func (q *Query[E]) Where(ps ...Predicate[E]) *Query[E] {
	for _, p := range ps {
		q.fail(p.err)
		if !p.IsZero() {
			q.wheres = append(q.wheres, p)
		}
	}
	return q
}

// With loads the named relations alongside the root rows:
//
//	db.Users.Query().With(Users.Profile, Users.Posts).Limit(50).All(ctx)
//
// Nothing is loaded that is not named here. There is no lazy loading, so a
// relation left out stays unloaded rather than fetching itself the first time
// somebody reads it — which is the behaviour that turns a loop over fifty rows
// into fifty-one queries without anybody writing one.
//
// Loading never changes which root rows come back. A to-one relation folds into
// the root statement, where it can add at most one row per row and so cannot
// disturb a limit; a to-many relation is loaded in one further statement after
// the root rows are known. The number of statements depends on how many
// relations were asked for, never on how many rows came back.
//
// Relations may be configured, which restricts what is loaded and never which
// roots come back:
//
//	db.Users.Query().With(
//	    Users.Posts.
//	        Where(Posts.Published.Eq(true)).
//	        OrderBy(Posts.CreatedAt.Desc()).
//	        Limit(5),
//	)
//
// gives every user their five most recent published posts, and gives a user
// with none a loaded, empty relation. See [Rel.Where], [Rel.OrderBy] and
// [Rel.Limit]. Filtering the roots by their relations is [Rel.Any] and
// [Rel.None], which belong in Where.
//
// Asking for the same relation twice is an error rather than a second identical
// query, and configuring the two copies differently does not make them two
// relations.
func (q *Query[E]) With(loaders ...Loader[E]) *Query[E] {
	for _, l := range loaders {
		rel := l.relation()
		// A mistake made configuring the relation is recorded here, so that it
		// surfaces from every terminal operation rather than only from the ones
		// that would have loaded it.
		for _, err := range rel.cfg.errs {
			q.fail(err)
		}
		if slices.ContainsFunc(q.with, func(x relation[E]) bool { return x.name == rel.name }) {
			q.fail(fmt.Errorf("relation %s was requested more than once", rel.name))
			continue
		}
		q.with = append(q.with, rel)
	}
	return q
}

// OrderBy sorts the result. Terms apply in the order given, and repeated calls
// append.
func (q *Query[E]) OrderBy(os ...Order[E]) *Query[E] {
	for _, o := range os {
		if o.IsZero() {
			q.fail(errors.New("order term has no column"))
			continue
		}
		q.orderBy = append(q.orderBy, o)
	}
	return q
}

// Limit caps the number of rows. A limit of zero is a query that returns
// nothing; a negative one is a mistake.
func (q *Query[E]) Limit(n int) *Query[E] {
	if n < 0 {
		q.fail(fmt.Errorf("negative limit %d", n))
		return q
	}
	q.limit = &n
	return q
}

// Offset skips rows before returning any. Pair it with an ordering: without
// one, which rows are skipped is whatever the server happened to produce first.
func (q *Query[E]) Offset(n int) *Query[E] {
	if n < 0 {
		q.fail(fmt.Errorf("negative offset %d", n))
		return q
	}
	q.offset = &n
	return q
}

// ForUpdate locks the rows the query returns until the surrounding transaction
// ends. Outside a transaction the lock is released immediately and buys
// nothing, so this belongs in one.
func (q *Query[E]) ForUpdate() *Query[E] {
	q.forUpdate = true
	return q
}

// SQL renders the statement and its parameters without executing anything.
//
// It is the canonical build path: every terminal operation compiles through the
// same code, so a query that fails here fails everywhere, and one that succeeds
// runs exactly the statement this returns. Every value the caller supplied is
// in args, never in the text.
func (q *Query[E]) SQL() (string, []any, error) {
	plan, err := q.plan()
	if err != nil {
		return "", nil, err
	}
	return q.compile(plan.sel)
}

// build assembles the statement, refusing before any of it if the builder
// recorded a mistake.
func (q *Query[E]) build() (*expr.Select, error) {
	if len(q.errs) > 0 {
		return nil, errors.Join(q.errs...)
	}
	meta := q.repo.meta

	cols := make([]expr.Column, 0, len(meta.Columns))
	for _, c := range meta.Columns {
		cols = append(cols, expr.Column{Source: q.source, Name: c.Name})
	}

	sel := &expr.Select{
		From:      q.source,
		Columns:   cols,
		Limit:     q.limit,
		Offset:    q.offset,
		ForUpdate: q.forUpdate,
		Lock:      q.lock,
	}
	if where := And(q.wheres...); !expr.IsTrue(where.node) {
		sel.Where = where.node
	}
	for _, o := range q.orderBy {
		sel.OrderBy = append(sel.OrderBy, o.order)
	}
	return sel, nil
}

// valueTerm makes an entity query usable where a value is expected.
//
// The arity is the generated descriptor's column count, which is what the
// statement selects: build writes one column per entry of meta.Columns, in that
// order. An entity query is a valid subquery and almost never a valid value one,
// because an entity of one column is rare — but that is a fact about the entity
// rather than about the kind of query, so the arity says it instead of the type.
func (q *Query[E]) valueTerm() (expr.Subquery, int, error) {
	sel, err := q.build()
	if err != nil {
		return nil, 0, err
	}
	if q.repo == nil || q.repo.meta == nil {
		return nil, 0, errors.New("the entity query has no metadata, so how many columns it selects is unknown")
	}
	return sel, len(q.repo.meta.Columns), nil
}

// compile renders a statement, naming the table in anything that goes wrong.
func (q *Query[E]) compile(stmt expr.Statement) (string, []any, error) {
	sql, args, err := stmt.Compile()
	if err != nil {
		return "", nil, fmt.Errorf("compiling the query for %s: %w", q.repo.meta.Table, err)
	}
	return sql, args, nil
}

// exec compiles and runs a statement.
func (q *Query[E]) exec(ctx context.Context, stmt expr.Statement) (pgxRows, error) {
	sql, args, err := q.compile(stmt)
	if err != nil {
		return nil, err
	}
	if q.repo.ex == nil {
		return nil, fmt.Errorf("querying %s: the repository has no executor", q.repo.meta.Table)
	}
	rows, err := q.repo.ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", q.repo.meta.Table, err)
	}
	return rows, nil
}

// All runs the query and returns every row.
//
// It builds and validates the statement before touching PostgreSQL, so a
// malformed query costs no round trip. Rows are scanned straight into the
// entity through generated metadata, with no reflection on the path.
func (q *Query[E]) All(ctx context.Context) ([]E, error) {
	plan, err := q.plan()
	if err != nil {
		return nil, err
	}
	// One event for the operation the caller asked for. Loading relations
	// issues more statements, and each of those gets its own event tagged with
	// the relation path — so a trace shows both that this was one call and how
	// many statements it took.
	ctx, sp := q.startSpan(ctx, observe.OpQuery, plan.sel)
	out, err := q.readAll(ctx, plan)
	if err != nil {
		sp.end(err, 0, false)
		return nil, err
	}
	// A relation that was asked for and could not be loaded is a failure of the
	// call, not a detail of the result. Returning the roots with that relation
	// quietly unloaded would look exactly like never having asked for it.
	if err := plan.loadRelations(ctx, q, out); err != nil {
		sp.end(err, 0, false)
		return nil, err
	}
	sp.end(nil, int64(len(out)), true)
	return out, nil
}

// startSpan begins tracing an operation rooted at this query's table.
//
// The statement is compiled here rather than reusing the one the caller will
// run, because an untraced program must not pay for compiling twice — and it
// does not: the compile happens only when a tracer is attached.
func (q *Query[E]) startSpan(ctx context.Context, op observe.Op, stmt expr.Statement) (context.Context, span) {
	if !tracing(q.repo.ex) {
		return ctx, span{}
	}
	sql, args, err := stmt.Compile()
	if err != nil {
		sql, args = "", nil
	}
	return startSpan(ctx, q.repo.ex, observe.StartEvent{
		Op:    op,
		SQL:   sql,
		Args:  len(args),
		Table: q.repo.meta.Table.String(),
	})
}

// readAll runs the root statement and scans it, folded relations included.
func (q *Query[E]) readAll(ctx context.Context, plan *rootPlan[E]) ([]E, error) {
	rows, err := q.exec(ctx, plan.sel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []E
	scan := plan.scanner(q)
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	// A failure part way through streaming shows up here and nowhere else, so
	// skipping this check would turn a truncated result into a short one.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", q.repo.meta.Table, err)
	}
	return out, nil
}

// One runs the query and returns the single row it matched.
//
// It returns [ErrNotFound] when nothing matched and [ErrMultipleRows] when more
// than one row did. The second is not pedantry: a query that matches two rows
// is one whose author believed something about the data that is not true, and
// quietly returning the first would hide that until it caused damage
// elsewhere.
//
// At most two rows are fetched, since two is enough to tell the three cases
// apart. That limit is applied to a copy — the builder a caller holds is not
// touched, so this stays safe:
//
//	base := db.Users.Query().Where(Users.Active.Eq(true))
//	_, _ = base.Clone().Where(Users.Email.Eq(addr)).One(ctx)
//	users, _ := base.All(ctx)
func (q *Query[E]) One(ctx context.Context) (E, error) {
	var zero E

	plan, err := q.plan()
	if err != nil {
		return zero, err
	}
	// A caller who asked for fewer rows than that meant it, so their limit
	// stands and no ambiguity can arise from it.
	probe := *plan.sel
	n := 2
	if plan.sel.Limit != nil && *plan.sel.Limit < n {
		n = *plan.sel.Limit
	}
	probe.Limit = &n

	ctx, sp := q.startSpan(ctx, observe.OpQuery, &probe)
	out, err := q.readOne(ctx, plan, &probe)
	if err != nil {
		sp.end(err, 0, false)
		return zero, err
	}
	// Relations load only once the root is known to be exactly one row. Loading
	// them first would spend a statement on a query that turns out to have
	// found nothing, or too much.
	roots := []E{out}
	if err := plan.loadRelations(ctx, q, roots); err != nil {
		sp.end(err, 0, false)
		return zero, err
	}
	sp.end(nil, 1, true)
	return roots[0], nil
}

// readOne runs the root statement and insists on exactly one row.
func (q *Query[E]) readOne(ctx context.Context, plan *rootPlan[E], probe *expr.Select) (E, error) {
	var zero E

	rows, err := q.exec(ctx, probe)
	if err != nil {
		return zero, err
	}
	defer rows.Close()

	scan := plan.scanner(q)
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return zero, fmt.Errorf("reading %s: %w", q.repo.meta.Table, err)
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
		return zero, fmt.Errorf("reading %s: %w", q.repo.meta.Table, err)
	}
	return out, nil
}

// Count returns how many rows the query matches.
//
// It counts exactly what [Query.All] would return, which means it respects
// Where, Limit and Offset and ignores ordering. The statement wraps the query
// rather than replacing its column list, because a bare count with a LIMIT
// would count the limit away and report the whole table.
//
// Relations are ignored. Count answers a question about the root rows, and
// loading their relations would be work whose result is discarded.
//
// No entity is materialised: the server sends one number.
func (q *Query[E]) Count(ctx context.Context) (int64, error) {
	sel, err := q.build()
	if err != nil {
		return 0, err
	}
	rows, err := q.exec(ctx, expr.CountFrom(sel))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var n int64
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("counting %s: %w", q.repo.meta.Table, err)
		}
		return 0, fmt.Errorf("counting %s: the server returned no row", q.repo.meta.Table)
	}
	if err := rows.Scan(&n); err != nil {
		return 0, fmt.Errorf("counting %s: %w", q.repo.meta.Table, err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("counting %s: %w", q.repo.meta.Table, err)
	}
	return n, nil
}

// Exists reports whether the query matches anything.
//
// Like [Query.Count] it respects Where, Limit and Offset and ignores ordering,
// so Limit(0) is false rather than a test against an unrestricted table.
// Relations are ignored, for the same reason Count ignores them. No entity is
// materialised: the server sends one boolean.
func (q *Query[E]) Exists(ctx context.Context) (bool, error) {
	sel, err := q.build()
	if err != nil {
		return false, err
	}
	rows, err := q.exec(ctx, expr.ExistsFrom(sel))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	var found bool
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, fmt.Errorf("testing %s: %w", q.repo.meta.Table, err)
		}
		return false, fmt.Errorf("testing %s: the server returned no row", q.repo.meta.Table)
	}
	if err := rows.Scan(&found); err != nil {
		return false, fmt.Errorf("testing %s: %w", q.repo.meta.Table, err)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("testing %s: %w", q.repo.meta.Table, err)
	}
	return found, nil
}

// Rows runs the query and yields entities one at a time:
//
//	for user, err := range db.Users.Query().Rows(ctx) {
//	    if err != nil {
//	        return err
//	    }
//	    process(user)
//	}
//
// Nothing is buffered. Each row is scanned as the loop reaches it, which is
// what makes this usable over a result too large to hold — and what
// distinguishes it from All. Stopping early closes the rows and releases the
// connection, so a break is safe.
//
// Errors arrive through the loop rather than a second return: a build mistake
// or a failed query yields once with the zero entity and the error, and a scan
// failure ends the iteration the same way. A loop that checks err on every step
// misses nothing.
func (q *Query[E]) Rows(ctx context.Context) iter.Seq2[E, error] {
	return func(yield func(E, error) bool) {
		var zero E

		// A relation needing a statement of its own would have to see every
		// row before it could run, which means buffering the result — the one
		// thing this method exists to avoid. It is refused rather than
		// silently turned into All.
		if err := q.streamable(); err != nil {
			yield(zero, err)
			return
		}
		plan, err := q.plan()
		if err != nil {
			yield(zero, err)
			return
		}
		// A stream's event measures the lifetime of the iteration rather than
		// the query: the rows arrive as the caller asks for them, so however
		// long the caller spends between two of them is inside this. That is
		// what OpStream means, and it is why it is a different Op from a
		// buffered read.
		ctx, sp := q.startSpan(ctx, observe.OpStream, plan.sel)
		var taken int64
		var failed error
		defer func() { sp.end(failed, taken, failed == nil) }()

		rows, err := q.exec(ctx, plan.sel)
		if err != nil {
			failed = err
			yield(zero, err)
			return
		}
		// This runs whether the loop finishes, returns, or breaks, which is
		// what keeps an early stop from holding the connection.
		defer rows.Close()

		scan := plan.scanner(q)
		for rows.Next() {
			e, err := scan(rows)
			if err != nil {
				failed = err
				yield(zero, err)
				return
			}
			taken++
			if !yield(e, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			failed = fmt.Errorf("reading %s: %w", q.repo.meta.Table, err)
			yield(zero, failed)
		}
	}
}

// scanner returns a function that reads one row into a new entity.
//
// The destination slice is allocated once and refilled per row, and every
// destination is a pointer into the entity being built, so scanning costs no
// reflection and no allocation beyond the entity itself.
func (q *Query[E]) scanner() func(pgxRows) (E, error) {
	meta := q.repo.meta
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
			return e, fmt.Errorf("scanning %s: %w", meta.Table, err)
		}
		return e, nil
	}
}

// sourceIsTable reports whether a source is an occurrence of a particular
// table, which is what makes a hand-supplied source usable with a repository's
// metadata.
func sourceIsTable(src *Source, id TableID) bool {
	schema, table := src.Relation()
	return schema == id.Schema && table == id.Name
}
