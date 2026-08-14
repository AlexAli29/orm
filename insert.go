package orm

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlexAli29/orm/observe"
	"slices"

	"github.com/AlexAli29/orm/internal/expr"
)

// maxInsertParams caps how many bind parameters one INSERT may carry.
//
// The protocol allows 65535. The ceiling here is lower on purpose: a statement
// that fits exactly is one that breaks the moment a column is added, and a
// write that starts failing because of a schema change nobody connected to it
// is a bad afternoon. Tests lower it further to exercise chunking.
var maxInsertParams = 60000

// InsertOpt configures an insert. The options are typed by entity, so an option
// naming one entity's columns cannot be passed to another entity's insert.
type InsertOpt[E any] interface {
	applyInsert(*insertConfig[E])
}

// insertConfig is what the options build up.
type insertConfig[E any] struct {
	// defaults holds the columns left out so PostgreSQL supplies them.
	defaults []expr.Column
	conflict *conflictConfig[E]
	errs     []error
}

func (c *insertConfig[E]) fail(err error) {
	if err != nil {
		c.errs = append(c.errs, err)
	}
}

// defaultOpt omits columns from the INSERT so the database fills them in.
type defaultOpt[E any] struct {
	cols []expr.Column
}

func (o defaultOpt[E]) applyInsert(c *insertConfig[E]) {
	for _, col := range o.cols {
		if slices.ContainsFunc(c.defaults, func(x expr.Column) bool { return x.Name == col.Name }) {
			c.fail(fmt.Errorf("%w: %s is listed twice", ErrInvalidDefault, col.Name))
			continue
		}
		c.defaults = append(c.defaults, col)
	}
}

// Default asks PostgreSQL to supply the values for these columns.
//
// The columns are left out of the INSERT entirely, so the column's DEFAULT
// applies — or its sequence, or NULL for a nullable column with no default.
//
// This is the only way to get a database default, and that is the point. A Go
// zero value is a value:
//
//	db.Users.Insert(ctx, User{Active: false})              // active = FALSE
//	db.Users.Insert(ctx, User{}, orm.Default(Users.Active)) // active = its default
//
// The first writes false. It does not mean "Active is unset, so let the
// database decide", because there is no way to tell that apart from an author
// who meant false — and guessing wrong writes the opposite of what was asked.
func Default[E any](cols ...InsertColumn[E]) InsertOpt[E] {
	out := defaultOpt[E]{cols: make([]expr.Column, 0, len(cols))}
	for _, c := range cols {
		out.cols = append(out.cols, c.insertColumn())
	}
	return out
}

// conflictConfig is an ON CONFLICT clause under construction.
type conflictConfig[E any] struct {
	target []expr.Column
	// update names the columns DO UPDATE writes from the rejected row. An
	// empty slice with doUpdate false is DO NOTHING.
	update []expr.Column
	// sets are explicit assignments, which is how a conflict computes a new
	// value rather than merely taking the rejected one.
	sets     []Assign[E]
	where    []Predicate[E]
	doUpdate bool
}

// ConflictBuilder chooses what happens when an insert collides with an existing
// row. It is produced by [OnConflict] and becomes an option through DoNothing
// or DoUpdate.
type ConflictBuilder[E any] struct {
	cfg conflictConfig[E]
}

// OnConflict names the columns PostgreSQL matches a conflict against, which
// must correspond to a unique constraint or index:
//
//	orm.OnConflict(Users.Email).DoNothing()
//	orm.OnConflict(Users.TenantID, Users.Email).DoUpdate(Users.Name)
func OnConflict[E any](target ...InsertColumn[E]) *ConflictBuilder[E] {
	b := &ConflictBuilder[E]{}
	for _, c := range target {
		b.cfg.target = append(b.cfg.target, c.insertColumn())
	}
	return b
}

// DoNothing leaves the existing row alone.
//
// PostgreSQL returns no row for a conflict it ignored, so [Repo.Insert] reports
// [ErrConflictIgnored] rather than inventing an entity. Fetching the row that
// was already there would be a query the caller did not ask for.
func (b *ConflictBuilder[E]) DoNothing() InsertOpt[E] {
	return conflictOpt[E]{cfg: b.cfg}
}

// DoUpdate overwrites the named columns with the values the insert carried.
//
// Only the named columns change. Updating every column would be the convenient
// default and the wrong one: an upsert usually means "these fields are new, the
// rest belong to the existing row", and a caller who wants everything can say
// so.
func (b *ConflictBuilder[E]) DoUpdate(cols ...InsertColumn[E]) InsertOpt[E] {
	cfg := b.cfg
	cfg.doUpdate = true
	for _, c := range cols {
		cfg.update = append(cfg.update, c.insertColumn())
	}
	return conflictOpt[E]{cfg: cfg}
}

// DoUpdateSet overwrites columns with expressions of your own.
//
//	orm.OnConflict(Users.Email).DoUpdateSet(
//	    Users.Name.SetExpr(orm.Excluded(Users.Name)),
//	    Users.LoginCount.SetExpr(Users.LoginCount.Add(1)),
//	)
//
// Both sources PostgreSQL makes visible here are usable: the target table,
// which holds the row that was already there, and EXCLUDED, which holds the one
// the insert could not add. [DoUpdate] is the common case of this — every named
// column taken from EXCLUDED — and the two may be combined.
func (b *ConflictBuilder[E]) DoUpdateSet(assignments ...Assign[E]) InsertOpt[E] {
	cfg := b.cfg
	cfg.doUpdate = true
	cfg.sets = append(cfg.sets, assignments...)
	return conflictOpt[E]{cfg: cfg}
}

// Where restricts which conflicting rows the update touches.
//
//	orm.OnConflict(Users.Email).
//	    Where(orm.Excluded(Users.UpdatedAt).GtCol(Users.UpdatedAt)).
//	    DoUpdate(Users.Name)
//
// PostgreSQL scopes this to the target table and EXCLUDED, which is not the
// INSERT's own scope: there is no row to filter before the conflict happens, so
// this is a condition on the update rather than on the insert.
func (b *ConflictBuilder[E]) Where(ps ...Predicate[E]) *ConflictBuilder[E] {
	b.cfg.where = append(b.cfg.where, ps...)
	return b
}

type conflictOpt[E any] struct {
	cfg conflictConfig[E]
}

func (o conflictOpt[E]) applyInsert(c *insertConfig[E]) {
	if c.conflict != nil {
		c.fail(errors.New("insert was given more than one OnConflict"))
		return
	}
	cfg := o.cfg
	c.conflict = &cfg
}

// Insert adds one row and returns it as PostgreSQL stored it.
//
// Every mapped writable column is written, zero values included. Identity and
// generated columns are left to PostgreSQL, and so is any column named by
// [Default]. The row comes back through an explicit RETURNING list in the
// generated column order, so the values the database computed — a key, a
// default, a generated column — are on the returned entity.
//
// The entity passed in is not modified. Insert takes it by value and scans into
// a fresh one, so the caller's copy still holds exactly what they built.
func (r *Repo[E]) Insert(ctx context.Context, entity E, opts ...InsertOpt[E]) (E, error) {
	var zero E

	stmt, err := r.insertStatement([]E{entity}, opts)
	if err != nil {
		return zero, err
	}
	out, err := r.runInsert(ctx, stmt)
	if err != nil {
		return zero, err
	}
	if len(out) == 0 {
		// The only way an insert returns nothing is a conflict that DO NOTHING
		// swallowed. Reporting it beats handing back a zero entity that looks
		// like a row.
		return zero, fmt.Errorf("inserting into %s: %w", r.meta.Table, ErrConflictIgnored)
	}
	return out[0], nil
}

// InsertMany adds rows in as few statements as it can and returns them in the
// order given.
//
// Passing no entities returns an empty slice and runs nothing.
//
// Large inserts are split into chunks, because one statement can only carry so
// many bind parameters. That makes the operation several statements, and M4 has
// no transaction of its own to wrap them in: if a later chunk fails, the
// earlier ones are already committed. Pass a pgx.Tx as the executor when the
// whole insert has to succeed or fail together — an implicit transaction here
// would be a write the caller never asked for.
//
// With DoNothing, rows that conflicted are absent from the result, so the
// returned slice may be shorter than the one passed in.
func (r *Repo[E]) InsertMany(ctx context.Context, entities []E, opts ...InsertOpt[E]) ([]E, error) {
	if len(entities) == 0 {
		return []E{}, nil
	}

	chunk, err := r.insertChunkSize(opts)
	if err != nil {
		return nil, err
	}

	out := make([]E, 0, len(entities))
	for start := 0; start < len(entities); start += chunk {
		end := min(start+chunk, len(entities))
		stmt, err := r.insertStatement(entities[start:end], opts)
		if err != nil {
			return nil, err
		}
		rows, err := r.runInsert(ctx, stmt)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// insertChunkSize works out how many rows fit in one statement.
func (r *Repo[E]) insertChunkSize(opts []InsertOpt[E]) (int, error) {
	cols, _, err := r.insertColumns(opts)
	if err != nil {
		return 0, err
	}
	if len(cols) == 0 {
		return 0, fmt.Errorf("inserting into %s: no writable column is left to insert", r.meta.Table)
	}
	n := maxInsertParams / len(cols)
	if n < 1 {
		return 0, fmt.Errorf("inserting into %s: a single row needs %d parameters, more than the %d one statement may carry",
			r.meta.Table, len(cols), maxInsertParams)
	}
	return n, nil
}

// insertColumns resolves which columns an insert writes, and the configuration
// the options built.
func (r *Repo[E]) insertColumns(opts []InsertOpt[E]) ([]ColumnMeta, *insertConfig[E], error) {
	if err := r.meta.validate(); err != nil {
		return nil, nil, err
	}
	if r.meta.Value == nil {
		return nil, nil, fmt.Errorf("inserting into %s: the metadata has no value accessor", r.meta.Table)
	}

	cfg := &insertConfig[E]{}
	for _, o := range opts {
		o.applyInsert(cfg)
	}

	byName := make(map[string]ColumnMeta, len(r.meta.Columns))
	for _, c := range r.meta.Columns {
		byName[c.Name] = c
	}

	for _, d := range cfg.defaults {
		c, ok := byName[d.Name]
		switch {
		case !ok:
			cfg.fail(fmt.Errorf("%w: %s is not a mapped column of %s", ErrInvalidDefault, d.Name, r.meta.Table))
		case !c.Defaultable():
			// Omitting it would make PostgreSQL reject the row, and the
			// message it would send names a constraint rather than the call
			// that caused it.
			cfg.fail(fmt.Errorf("%w: %s.%s is NOT NULL with no default, no identity and no generation expression, so leaving it out would insert nothing for it",
				ErrInvalidDefault, r.meta.Table, d.Name))
		}
	}
	if cfg.conflict != nil {
		r.validateConflict(cfg, byName)
	}
	if len(cfg.errs) > 0 {
		return nil, nil, errors.Join(cfg.errs...)
	}

	var cols []ColumnMeta
	for _, c := range r.meta.Columns {
		if !c.Writable() {
			continue
		}
		if slices.ContainsFunc(cfg.defaults, func(x expr.Column) bool { return x.Name == c.Name }) {
			continue
		}
		cols = append(cols, c)
	}
	return cols, cfg, nil
}

// validateConflict checks the conflict clause against what the schema allows.
func (r *Repo[E]) validateConflict(cfg *insertConfig[E], byName map[string]ColumnMeta) {
	c := cfg.conflict
	if len(c.target) == 0 {
		cfg.fail(errors.New("OnConflict names no columns"))
	}
	for _, t := range c.target {
		if _, ok := byName[t.Name]; !ok {
			cfg.fail(fmt.Errorf("OnConflict names %s, which is not a mapped column of %s", t.Name, r.meta.Table))
		}
	}
	seen := make(map[string]bool, len(c.update))
	for _, u := range c.update {
		col, ok := byName[u.Name]
		switch {
		case !ok:
			cfg.fail(fmt.Errorf("DoUpdate names %s, which is not a mapped column of %s", u.Name, r.meta.Table))
		case !col.Writable():
			cfg.fail(fmt.Errorf("DoUpdate names %s.%s, which %s", r.meta.Table, u.Name, whyNotWritable(col)))
		case seen[u.Name]:
			cfg.fail(fmt.Errorf("%w: DoUpdate names %s twice", ErrDuplicateAssignment, u.Name))
		}
		seen[u.Name] = true
	}
	for _, a := range c.sets {
		if err := r.checkAssignment(a, true); err != nil {
			cfg.fail(err)
			continue
		}
		if seen[a.Column()] {
			cfg.fail(fmt.Errorf("%w: DoUpdate assigns %s twice", ErrDuplicateAssignment, a.Column()))
		}
		seen[a.Column()] = true
	}
	for _, p := range c.where {
		cfg.fail(p.Err())
	}
	if c.doUpdate && len(c.update) == 0 && len(c.sets) == 0 {
		cfg.fail(errors.New("DoUpdate names no columns; use DoNothing to leave the existing row alone"))
	}
	if !c.doUpdate && len(c.where) > 0 {
		cfg.fail(errors.New("OnConflict.Where restricts a DO UPDATE, and this conflict does nothing"))
	}
}

func whyNotWritable(c ColumnMeta) string {
	if c.Generated {
		return "PostgreSQL computes and will not accept a value for"
	}
	return "is an identity column, which PostgreSQL supplies"
}

// insertStatement builds the statement for a batch of entities.
func (r *Repo[E]) insertStatement(entities []E, opts []InsertOpt[E]) (*expr.Insert, error) {
	cols, cfg, err := r.insertColumns(opts)
	if err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("inserting into %s: no writable column is left to insert", r.meta.Table)
	}

	stmt := &expr.Insert{Into: r.source}
	for _, c := range cols {
		stmt.Columns = append(stmt.Columns, expr.Column{Source: r.source, Name: c.Name})
	}
	// The result is read back in the generated column order, which is the
	// order the scanner indexes into.
	for _, c := range r.meta.Columns {
		stmt.Returning = append(stmt.Returning, expr.Column{Source: r.source, Name: c.Name})
	}

	index := make(map[string]int, len(r.meta.Columns))
	for i, c := range r.meta.Columns {
		index[c.Name] = i
	}
	for i := range entities {
		row := make([]expr.Node, 0, len(cols))
		for _, c := range cols {
			row = append(row, expr.Arg{Value: r.meta.Value(&entities[i], index[c.Name])})
		}
		stmt.Rows = append(stmt.Rows, row)
	}

	if cfg.conflict != nil {
		conflict := &expr.Conflict{}
		for _, t := range cfg.conflict.target {
			conflict.Target = append(conflict.Target, expr.Column{Source: r.source, Name: t.Name})
		}
		for _, u := range cfg.conflict.update {
			conflict.Set = append(conflict.Set, expr.Assignment{
				Column: expr.Column{Source: r.source, Name: u.Name},
				Value:  expr.Excluded{Name: u.Name},
			})
		}
		for _, a := range cfg.conflict.sets {
			conflict.Set = append(conflict.Set, a.assignment)
		}
		if where := And(cfg.conflict.where...); !expr.IsTrue(where.node) {
			conflict.Where = where.node
		}
		stmt.Conflict = conflict
	}
	return stmt, nil
}

// runInsert executes a statement and scans the rows it returns.
func (r *Repo[E]) runInsert(ctx context.Context, stmt *expr.Insert) ([]E, error) {
	sql, args, err := stmt.Compile()
	if err != nil {
		return nil, fmt.Errorf("compiling the insert into %s: %w", r.meta.Table, err)
	}
	if r.ex == nil {
		return nil, fmt.Errorf("inserting into %s: the repository has no executor", r.meta.Table)
	}

	ctx, sp := r.startSpan(ctx, observe.OpInsert, sql, args)
	out, err := r.runInsertRows(ctx, sql, args)
	sp.end(err, int64(len(out)), err == nil)
	return out, err
}

func (r *Repo[E]) runInsertRows(ctx context.Context, sql string, args []any) ([]E, error) {
	rows, err := r.ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("inserting into %s: %w", r.meta.Table, err)
	}
	defer rows.Close()

	scan := (&Query[E]{repo: r}).scanner()
	var out []E
	for rows.Next() {
		e, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the insert into %s: %w", r.meta.Table, err)
	}
	return out, nil
}

// InsertSQL renders the statement Insert would run, without executing it.
//
// It exists so a write can be inspected the way a read can. The options are the
// ones Insert takes, so what this prints is what that would run.
func (r *Repo[E]) InsertSQL(entities []E, opts ...InsertOpt[E]) (string, []any, error) {
	if len(entities) == 0 {
		return "", nil, fmt.Errorf("inserting into %s: no rows", r.meta.Table)
	}
	stmt, err := r.insertStatement(entities, opts)
	if err != nil {
		return "", nil, err
	}
	sql, args, err := stmt.Compile()
	if err != nil {
		return "", nil, fmt.Errorf("compiling the insert into %s: %w", r.meta.Table, err)
	}
	return sql, args, nil
}
