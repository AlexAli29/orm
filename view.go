package orm

import (
	"context"
	"errors"
	"fmt"

	"github.com/AlexAli29/orm/internal/expr"
	"github.com/AlexAli29/orm/observe"
)

// Views and materialized views as query sources.
//
// The capability is the type. A [ViewRepo] has no Insert to call, not because
// calling it returns an error but because there is no such method — the mistake
// is a compile failure at the line that made it, rather than a runtime error in
// production on the one path nobody tested. PostgreSQL will accept writes
// through some views, and generating a write API whose success depends on the
// shape of a definition would be a promise the generator cannot keep by reading
// a catalog. So view sources are read-only, and the absence is the API.
//
// Reading is not reimplemented. Both types hold an ordinary [Repo] and hand its
// query builder straight back, so a view goes through the same AST, the same
// writer, the same placeholder namespace and the same scope checking a table
// does. There is no view query compiler, and a view is not a special case
// anywhere below this file.

// ViewRepo reads entities of type E from an ordinary PostgreSQL view.
//
// It offers query composition and nothing else. Everything a [Query] can do
// over a table it can do over a view — joins, CTEs, window functions, EXPLAIN,
// tracing — because it is the same builder over the same compiler.
type ViewRepo[E any] struct {
	// The read capability is delegated rather than reimplemented. Making this a
	// field rather than an embedded *Repo is what keeps the write methods out:
	// embedding would promote Insert, Update, Delete and CopyFrom onto every
	// view in the project.
	repo *Repo[E]
}

// NewViewRepo binds an executor to a generated view's metadata. Generated code
// calls it.
func NewViewRepo[E any](ex Executor, meta *EntityMeta[E]) *ViewRepo[E] {
	return &ViewRepo[E]{repo: NewRepo(ex, meta)}
}

// Query starts a new query over the view.
//
// The returned builder is mutable and is not safe for concurrent use. Build one
// per query, or [Query.Clone] before branching.
func (v *ViewRepo[E]) Query() *Query[E] { return v.repo.Query() }

// QueryFrom starts a query reading from a particular occurrence of the view,
// which is how a query selects through an alias or self-joins one.
func (v *ViewRepo[E]) QueryFrom(src *Source) *Query[E] { return v.repo.QueryFrom(src) }

// MaterializedViewRepo reads entities of type E from a materialized view.
//
// It offers what a view offers, plus [MaterializedViewRepo.Refresh]. It offers
// no writes: PostgreSQL has no INSERT, UPDATE or DELETE for a materialized
// view, so there is nothing here that could be generated even in principle.
type MaterializedViewRepo[E any] struct {
	ViewRepo[E]
	// concurrentIndex is the unique index that lets REFRESH ... CONCURRENTLY
	// run, empty when the view has none that qualifies. Generated code fills it
	// in from the schema, which is where PostgreSQL's rule can be checked
	// before a statement is sent rather than after it is rejected.
	concurrentIndex string
}

// NewMaterializedViewRepo binds an executor to a generated materialized view's
// metadata.
//
// concurrentIndex is the name of a unique index over plain columns covering
// every row, or empty when the view has none. It is the schema's half of
// PostgreSQL's CONCURRENTLY rule; the runtime half — whether the view holds
// data — belongs to the server and is not cached here.
func NewMaterializedViewRepo[E any](ex Executor, meta *EntityMeta[E], concurrentIndex string) *MaterializedViewRepo[E] {
	return &MaterializedViewRepo[E]{
		ViewRepo:        ViewRepo[E]{repo: NewRepo(ex, meta)},
		concurrentIndex: concurrentIndex,
	}
}

// RefreshOption changes how a materialized view is refreshed.
type RefreshOption func(*refreshConfig)

type refreshConfig struct {
	concurrently bool
	noData       bool
}

// Concurrently refreshes without taking an exclusive lock, so readers keep
// seeing the previous contents while the new ones are built.
//
// PostgreSQL's requirements for this are exact, and most of them are visible in
// the schema: the view needs a unique index that covers every row and is built
// from plain column names, so not partial and not over an expression. When the
// generated descriptor shows no such index, [MaterializedViewRepo.Refresh]
// fails before sending anything, because the server was always going to refuse.
//
// The remaining requirement is that the view already holds data, and that is
// state the server owns. It can change between one statement and the next, so
// nothing here remembers it: an unpopulated view produces PostgreSQL's own
// error, wrapped and reachable with errors.As.
//
// It cannot be combined with [WithNoData]: PostgreSQL has no way to
// concurrently replace contents with nothing.
func Concurrently() RefreshOption { return func(c *refreshConfig) { c.concurrently = true } }

// WithData refreshes the view and populates it, which is PostgreSQL's default
// and is what [MaterializedViewRepo.Refresh] does when given no options. It
// exists so that a caller can say so.
func WithData() RefreshOption { return func(c *refreshConfig) { c.noData = false } }

// WithNoData empties the materialized view and leaves it unpopulated.
//
// The view is then unscannable: PostgreSQL refuses to read a materialized view
// that holds no data, and says so, rather than returning no rows. That is the
// point of it — it discards contents cheaply — and it is why this is a named
// option rather than a boolean argument nobody can read at a call site.
func WithNoData() RefreshOption { return func(c *refreshConfig) { c.noData = true } }

// Refresh replaces the materialized view's contents.
//
// It is one statement and one traced operation. It goes through the same
// executor, the same context and the same error wrapping every other statement
// uses, so a PostgreSQL error is reachable with errors.As and a cancelled
// context cancels it.
//
// There are no bind parameters: REFRESH names a relation and takes no values,
// so there is nothing here that could carry one into a log or a span.
func (m *MaterializedViewRepo[E]) Refresh(ctx context.Context, opts ...RefreshOption) error {
	var cfg refreshConfig
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if m == nil || m.repo == nil || m.repo.meta == nil {
		return errors.New("orm: Refresh: the materialized view has no metadata")
	}
	name, err := m.qualified()
	if err != nil {
		return err
	}

	// The two ways PostgreSQL will refuse, checked here because both are
	// decidable from the schema and neither needs a round trip to discover.
	if cfg.concurrently && cfg.noData {
		return fmt.Errorf("orm: Refresh %s: CONCURRENTLY cannot be combined with WITH NO DATA: "+
			"a concurrent refresh replaces the contents row by row, and there are no rows to replace with", name)
	}
	if cfg.concurrently && m.concurrentIndex == "" {
		return fmt.Errorf("orm: Refresh %s: CONCURRENTLY needs a unique index over plain columns "+
			"covering every row, and this materialized view has none. A partial or expression "+
			"unique index does not qualify. Add one, or refresh without Concurrently", name)
	}

	stmt := "REFRESH MATERIALIZED VIEW "
	if cfg.concurrently {
		stmt += "CONCURRENTLY "
	}
	stmt += name
	if cfg.noData {
		stmt += " WITH NO DATA"
	}

	if m.repo.ex == nil {
		return fmt.Errorf("orm: Refresh %s: the repository has no executor", name)
	}
	// The same execution path every write uses: one span, the executor's own
	// Query, the command tag drained before the error is read. A refresh with a
	// connection of its own would be a second path for cancellation, tracing
	// and error wrapping to diverge on.
	ctx, sp := m.repo.startSpan(ctx, observe.OpRefresh, stmt, nil)
	_, err = m.repo.execWriteRows(ctx, stmt, nil, "refreshing "+name)
	sp.end(err, 0, err == nil)
	return err
}

// qualified renders the relation name through the writer that owns identifier
// quoting, so a name needing quotes gets them and no name is ever concatenated
// into SQL by hand.
func (m *MaterializedViewRepo[E]) qualified() (string, error) {
	t := m.repo.meta.Table
	name, err := expr.NewSource(t.Schema, t.Name).QuotedName()
	if err != nil {
		return "", fmt.Errorf("orm: Refresh: %w", err)
	}
	return name, nil
}
