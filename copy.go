package orm

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlexAli29/orm/observe"
	"iter"
	"slices"

	"github.com/jackc/pgx/v5"
)

// COPY.
//
// [Repo.InsertMany] sends an INSERT with one placeholder per value, which is
// what you want when the rows have to come back: RETURNING gives you the
// identities PostgreSQL assigned, ON CONFLICT decides what happens to the ones
// that clash, and every value is a bind parameter the server type-checks
// against the column.
//
// COPY is the other tool. It streams rows over PostgreSQL's copy protocol in
// its binary format, with no statement to parse and no per-row round trip, and
// it is the fastest way to get a lot of rows into a table. What it gives up is
// everything the statement bought: no RETURNING, no ON CONFLICT, no per-row
// error telling you which row was wrong.
//
// So neither replaces the other, and this package does not pretend one does.
// COPY is for ingestion. Insert is for writing rows your program then uses.

// CopyExecutor is the part of pgx a COPY needs.
//
// It is separate from [Executor] because COPY is not a query: it takes a
// destination and a source of rows rather than SQL and arguments, and it exists
// on the pgx types that can hold a connection open for the duration —
// *pgxpool.Pool, *pgx.Conn and pgx.Tx, all of which satisfy this as they are.
//
// A repository whose executor does not implement it can still do everything
// else; only COPY refuses, and it says so rather than falling back to an
// INSERT that would have different semantics.
type CopyExecutor interface {
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error)
}

// CopyFrom streams entities into the table over PostgreSQL's copy protocol.
//
//	n, err := db.Events.CopyFrom(ctx, events)
//
// The columns are the entity's writable ones — everything except generated and
// identity columns, which PostgreSQL supplies itself. Values come from the
// generated accessors, so the row path costs no reflection.
//
// # What it does not do
//
// There is no RETURNING and no ON CONFLICT: PostgreSQL's copy protocol has
// neither, so a value the database generated is not read back and a row that
// violates a constraint fails the whole COPY rather than being skipped.
// [Repo.InsertMany] is the call for either of those.
//
// # Atomicity
//
// A failing COPY fails as one statement — no part of it is applied. That is the
// whole of the guarantee: if the COPY is one of several operations that have to
// succeed together, run them in a transaction, and the repository bound to that
// transaction is the one to call this on.
func (r *Repo[E]) CopyFrom(ctx context.Context, entities []E) (int64, error) {
	return r.copyFrom(ctx, sliceSource(entities), nil)
}

// CopyFromSeq streams entities from an iterator, so that the rows never all
// exist at once.
//
//	n, err := db.Events.CopyFromSeq(ctx, func(yield func(Event, error) bool) {
//	    for scanner.Scan() {
//	        ev, err := parse(scanner.Text())
//	        if !yield(ev, err) {
//	            return
//	        }
//	    }
//	})
//
// The sequence is pulled one row at a time as PostgreSQL consumes them, so a
// file larger than memory is a file this can ingest. An error yielded by the
// sequence stops the COPY and is returned; the rows already sent are part of
// the statement PostgreSQL then discards.
func (r *Repo[E]) CopyFromSeq(ctx context.Context, rows iter.Seq2[E, error]) (int64, error) {
	if rows == nil {
		return 0, errors.New("CopyFromSeq was given no sequence")
	}
	src, stop := seqSource(rows)
	defer stop()
	return r.copyFrom(ctx, src, nil)
}

// CopyColumns streams a subset of the entity's writable columns.
//
//	n, err := orm.CopyColumns(ctx, db.Events, events,
//	    Events.UserID, Events.Kind, Events.Payload)
//
// Every column omitted takes whatever the table says — its DEFAULT, its
// identity sequence, or NULL — because the COPY simply does not mention it.
// Nothing here substitutes a Go zero for a column the caller left out, and a
// zero the caller did include is a value like any other.
//
// It is a function rather than a method because the columns are descriptors of
// E, and taking them as [InsertColumn] is what makes a column of another entity
// a compile error.
func CopyColumns[E any](ctx context.Context, r *Repo[E], entities []E, cols ...InsertColumn[E]) (int64, error) {
	if r == nil {
		return 0, errors.New("CopyColumns was given no repository")
	}
	return r.copyFrom(ctx, sliceSource(entities), cols)
}

// CopyColumnsFromSeq is [CopyColumns] over an iterator.
func CopyColumnsFromSeq[E any](ctx context.Context, r *Repo[E], rows iter.Seq2[E, error], cols ...InsertColumn[E]) (int64, error) {
	switch {
	case r == nil:
		return 0, errors.New("CopyColumnsFromSeq was given no repository")
	case rows == nil:
		return 0, errors.New("CopyColumnsFromSeq was given no sequence")
	}
	src, stop := seqSource(rows)
	defer stop()
	return r.copyFrom(ctx, src, cols)
}

// copyFrom is the one implementation the four entry points share.
func (r *Repo[E]) copyFrom(ctx context.Context, next func() (E, bool, error), cols []InsertColumn[E]) (int64, error) {
	if r == nil {
		return 0, errors.New("copy: no repository")
	}
	idx, names, err := r.copyColumns(cols)
	if err != nil {
		return 0, err
	}
	// The capability is asked of the executor underneath any tracing wrapper:
	// a wrapper that hid COPY would make attaching a tracer change what a
	// repository can do.
	ex, ok := unwrapExecutor(r.ex).(CopyExecutor)
	if !ok {
		if r.ex == nil {
			return 0, fmt.Errorf("copying into %s: the repository has no executor", r.meta.Table)
		}
		return 0, fmt.Errorf("copying into %s: %T cannot COPY; a *pgxpool.Pool, a *pgx.Conn or a pgx.Tx can",
			r.meta.Table, unwrapExecutor(r.ex))
	}

	// A COPY has no SQL, so its identity is what it copies: the table and the
	// ordered column set, which is what the wire actually differs by. The rows
	// are not counted here — the source is a stream, and materialising it to
	// report a number would defeat the point of streaming it.
	ctx, sp := startSpan(ctx, r.ex, observe.StartEvent{
		Op:          observe.OpCopy,
		Table:       r.meta.Table.String(),
		Columns:     names,
		Fingerprint: CopyFingerprint(r.meta.Table.Schema, r.meta.Table.Name, names).String(),
	})

	src := &entityCopySource[E]{meta: r.meta, idx: idx, next: next, values: make([]any, len(idx))}
	n, err := ex.CopyFrom(ctx, pgx.Identifier{r.meta.Table.Schema, r.meta.Table.Name}, names, src)
	defer func() {
		if src.err != nil {
			sp.end(src.err, n, true)
			return
		}
		sp.end(err, n, true)
	}()
	switch {
	case src.err != nil:
		// The source failed, which is the caller's error rather than
		// PostgreSQL's. pgx reports its own on the way out; this one is the
		// cause and is what the caller can act on.
		return n, fmt.Errorf("copying into %s: %w", r.meta.Table, src.err)
	case err != nil:
		// PostgreSQL's error is passed through with the table added and
		// nothing else. It does not name a row: the copy protocol reports the
		// failure against the stream rather than against an index, and
		// inventing one would be inventing information.
		return n, fmt.Errorf("copying into %s: %w", r.meta.Table, err)
	}
	return n, nil
}

// copyColumns resolves the column set into metadata indices and SQL names.
//
// The indices are what makes the row path cheap: the source reads
// meta.Value(&e, idx) for each of them, which is the generated switch the
// insert path already uses.
func (r *Repo[E]) copyColumns(cols []InsertColumn[E]) ([]int, []string, error) {
	if err := r.meta.validate(); err != nil {
		return nil, nil, err
	}
	if r.meta.Value == nil {
		return nil, nil, fmt.Errorf("copying into %s: the metadata has no value accessor", r.meta.Table)
	}

	if len(cols) == 0 {
		var idx []int
		var names []string
		for i, c := range r.meta.Columns {
			// A generated column PostgreSQL computes and an identity it
			// supplies are both columns COPY must not mention.
			if !c.Writable() {
				continue
			}
			idx = append(idx, i)
			names = append(names, c.Name)
		}
		if len(idx) == 0 {
			return nil, nil, fmt.Errorf("copying into %s: no column of it can be written", r.meta.Table)
		}
		return idx, names, nil
	}

	byName := make(map[string]int, len(r.meta.Columns))
	for i, c := range r.meta.Columns {
		byName[c.Name] = i
	}
	var (
		idx   []int
		names []string
		errs  []error
	)
	for _, col := range cols {
		name := col.insertColumn().Name
		i, ok := byName[name]
		switch {
		case !ok:
			errs = append(errs, fmt.Errorf("%s is not a mapped column of %s", name, r.meta.Table))
			continue
		case !r.meta.Columns[i].Writable():
			errs = append(errs, fmt.Errorf("%s.%s is %s, so COPY cannot supply a value for it",
				r.meta.Table, name, whyNotWritable(r.meta.Columns[i])))
			continue
		case slices.Contains(names, name):
			errs = append(errs, fmt.Errorf("%s.%s is copied twice", r.meta.Table, name))
			continue
		}
		idx = append(idx, i)
		names = append(names, name)
	}
	if len(errs) > 0 {
		return nil, nil, fmt.Errorf("copying into %s: %w", r.meta.Table, errors.Join(errs...))
	}
	if len(idx) == 0 {
		return nil, nil, fmt.Errorf("copying into %s: no columns were named", r.meta.Table)
	}
	return idx, names, nil
}

// entityCopySource feeds pgx one row at a time.
//
// It holds one values slice for the whole COPY rather than one per row: pgx
// reads it before calling Next again, so reusing it is safe and is what keeps
// a million-row ingestion from allocating a million slices.
type entityCopySource[E any] struct {
	meta   *EntityMeta[E]
	idx    []int
	next   func() (E, bool, error)
	values []any
	entity E
	err    error
}

func (s *entityCopySource[E]) Next() bool {
	if s.err != nil {
		return false
	}
	e, ok, err := s.next()
	if err != nil {
		s.err = err
		return false
	}
	if !ok {
		return false
	}
	s.entity = e
	return true
}

func (s *entityCopySource[E]) Values() ([]any, error) {
	if s.err != nil {
		return nil, s.err
	}
	for i, col := range s.idx {
		s.values[i] = s.meta.Value(&s.entity, col)
	}
	return s.values, nil
}

func (s *entityCopySource[E]) Err() error { return s.err }

// sliceSource walks a slice already in memory.
func sliceSource[E any](entities []E) func() (E, bool, error) {
	i := 0
	return func() (E, bool, error) {
		var zero E
		if i >= len(entities) {
			return zero, false, nil
		}
		e := entities[i]
		i++
		return e, true, nil
	}
}

// seqSource turns a push sequence into the pull the copy protocol needs.
//
// The returned stop has to run whatever happens — a source that yielded an
// error, a cancelled context, a PostgreSQL failure — or the goroutine behind
// the pull stays parked on a channel nobody reads.
func seqSource[E any](rows iter.Seq2[E, error]) (func() (E, bool, error), func()) {
	next, stop := iter.Pull2(rows)
	return func() (E, bool, error) {
		e, err, ok := next()
		if !ok {
			var zero E
			return zero, false, nil
		}
		if err != nil {
			var zero E
			return zero, false, err
		}
		return e, true, nil
	}, stop
}
