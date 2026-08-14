package orm

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/AlexAli29/orm/internal/expr"
)

// entityMarker gives a generic interface somewhere to put its entity type.
//
// An interface whose type parameter appears in no method signature constrains
// nothing: Loader[User] and Loader[Post] would be the same interface, and every
// relation would satisfy every query. Putting the parameter in a method fixes
// that, and it is also what lets Go infer the entity from the arguments.
type entityMarker[E any] struct{}

// Loader is a relation of entity E that a query may ask to load.
//
// Generated relation descriptors implement it. Nothing else can: the methods
// are unexported, so the set of loadable relations is exactly the set the
// generator proved from the catalog.
type Loader[E any] interface {
	loader(entityMarker[E])
	relation() relation[E]
}

// RelKey pairs a column of the parent's table with the column of the target's
// table it matches, in the order the foreign key declares them.
//
// The order is load bearing for a composite key and is never sorted: conkey and
// confkey ordinality is what pairs the two sides, and a set would pair the
// wrong columns while looking correct.
type RelKey struct {
	// Parent is the column on the parent's table.
	Parent string
	// Target is the column on the target's table.
	Target string
	// Type is the PostgreSQL type of the parent column, used to cast the key
	// array of a batched load.
	Type string
}

// AuxKeys is a query-local buffer of key values a relation reads from the
// statement that materialised its parents.
//
// It exists for the relation whose parent key columns the parent entity does
// not map. The keys are in the database and the statement can select them; what
// is missing is a Go field to put them in, and adding one would mean the ORM
// dictating the shape of an entity in order to load a relation the author asked
// for. Instead the parent's statement selects the columns as extra values, the
// buffer holds them for exactly as long as the child needs them, and they are
// discarded with the rest of the plan.
//
// Implementations are generated, one per relation that needs one, so the types
// are the column's own and nothing is decoded through an interface. Nothing
// outside generated code constructs one.
type AuxKeys interface {
	// Next returns the scan destinations for one more row.
	Next() []any
	// Reorder permutes the rows already scanned. order[i] is the index of the
	// row that belongs at position i, which is how the buffer comes to line up
	// with the parents the loader attached its rows to.
	Reorder(order []int)
	// Arrays returns one array per key column, ready to be a statement's
	// parameter.
	Arrays() []any
}

// relCfg is what a caller configured for one relation, with its target type
// erased. The typing that matters happened at the API boundary: by the time a
// predicate is here it has already been checked against the target entity.
type relCfg struct {
	// where restricts which related rows load. It never restricts the parents.
	where expr.Node
	// orderBy orders the rows within each parent.
	orderBy []expr.Order
	// limit caps the rows loaded for each parent.
	limit *int
	errs  []error
}

// configured reports whether anything was asked for beyond the relation itself.
func (c relCfg) configured() bool {
	return c.where != nil || len(c.orderBy) > 0 || c.limit != nil
}

func (c relCfg) clone() relCfg {
	c.orderBy = slices.Clone(c.orderBy)
	c.errs = slices.Clone(c.errs)
	return c
}

// relation is what a query works with: a relation with its target type erased,
// because a Query[E] knows its own entity and nothing about the other one.
type relation[E any] struct {
	name string
	keys []RelKey
	// parent is the occurrence of the declaring entity's table this descriptor
	// belongs to. An aliased table yields descriptors bound to the alias, so a
	// relation of Users.As("u") correlates against "u" rather than "users".
	parent *Source
	// targetSrc is the target's own occurrence, which is the one the target's
	// generated descriptors qualify against — and so the only one a caller's
	// relation predicates can name.
	targetSrc *Source
	// target names the table a batched relation reads.
	target TableID
	// columns are the target columns a batched relation reads.
	columns []string
	// fold builds the join and the per-row binding for a to-one relation, and
	// is nil for a to-many one, which can never be folded.
	fold func(parent, target *Source) folded[E]
	// extract collects the parent key values as one array per key column. It is
	// nil when the parent's key columns are not mapped to Go fields, which is
	// legitimate and decides the loading strategy rather than being an error.
	extract func(parents []*E) ([]any, error)
	// auxColumns are the key columns on the parent's table that the parent
	// entity does not map, which the statement materialising the parents has to
	// select instead. Empty when extract is available.
	auxColumns []string
	// newAux allocates the buffer those columns are scanned into.
	newAux func() AuxKeys
	// run executes a batched relation, attaches its rows, and fills the aux
	// buffers this relation's own children asked the statement for.
	run func(ctx context.Context, ex Executor, parents []*E, sql string, args []any, aux []AuxKeys) error
	// attachNone marks the relation loaded and empty for every parent, without
	// a statement. A relation asked for and known in advance to hold nothing is
	// still loaded: leaving it alone would be indistinguishable from never
	// having asked.
	attachNone func(parents []*E)
	// collect returns pointers to the targets this relation loaded, flattened
	// in parent order and erased. It is what the next level down loads against.
	collect func(parents []*E) targetSet
	// children are the relations of the target requested alongside it, already
	// erased because their entity is the target rather than E.
	children []relNode
	cfg      relCfg
}

func (r relation[E]) clone() relation[E] {
	r.cfg = r.cfg.clone()
	r.children = slices.Clone(r.children)
	for i := range r.children {
		r.children[i] = r.children[i].clone()
	}
	return r
}

// relNode is one node of the load plan with its target type erased.
//
// Erasure is what lets a plan be a tree at all: a relation of User to Post and
// one of Post to Comment have no type in common, and a Query[User] cannot name
// Comment. The typed halves are the closures generated code built, each paired
// with exactly the metadata that produced it, so nothing here casts a value it
// did not create.
type relNode struct {
	name string
	// path is the node's place in the requested tree, built by the planner and
	// used for diagnostics: a failure four levels down says which four.
	path string
	// auxColumns are the columns the statement materialising this node's
	// parents must select on its behalf.
	auxColumns []string
	newAux     func() AuxKeys
	children   []relNode
	cfg        relCfg
	// foldable reports whether this node could be joined into its parent's
	// statement, which only the root statement can do.
	foldable bool
	// exec loads the node and returns pointers to its targets, together with
	// the aux buffers its own children asked for.
	exec func(ctx context.Context, ex Executor, strat relStrategy, parents targetSet, aux AuxKeys) (targetSet, []AuxKeys, error)
}

func (n relNode) clone() relNode {
	n.cfg = n.cfg.clone()
	n.auxColumns = slices.Clone(n.auxColumns)
	n.children = slices.Clone(n.children)
	for i := range n.children {
		n.children[i] = n.children[i].clone()
	}
	return n
}

// folded is a to-one relation joined into the root statement.
type folded[E any] struct {
	on      expr.Node
	columns []expr.Column
	// bind allocates one row's worth of scan destinations and returns the
	// function that attaches whatever was scanned to the parent.
	bind func() (dest []any, attach func(*E))
}

// Rel is a generated relation descriptor.
//
// It is the value a caller names in [Query.With]:
//
//	db.Users.Query().With(Users.Profile, Users.Posts)
//
// carrying options and, in turn, relations of its own target:
//
//	db.Users.Query().With(
//	    Users.Posts.
//	        Where(Posts.Published.Eq(true)).
//	        OrderBy(Posts.CreatedAt.Desc()).
//	        Limit(5).
//	        With(Posts.Comments.With(Comments.Author)),
//	)
//
// E is the entity that declares the relation and T the one it points at. The
// entity parameter is what stops another entity's relation reaching this query;
// the target parameter is what stops another entity's predicate configuring it,
// and what makes a nested With accept only relations of the target.
//
// A Rel is a value and every option returns a modified copy, so the generated
// descriptor is never changed and two configured copies of one relation cannot
// see each other's options.
type Rel[E, T any] struct {
	rel relation[E]
	// The options stay typed until the query needs them, which is what makes
	// Users.Posts.Where(Users.Active.Eq(true)) a compile error rather than a
	// statement PostgreSQL rejects.
	wheres   []Predicate[T]
	orders   []Order[T]
	limit    *int
	children []Loader[T]
	errs     []error
}

func (r Rel[E, T]) loader(entityMarker[E]) {}

// relation erases the options and the child relations onto the descriptor the
// query works with.
func (r Rel[E, T]) relation() relation[E] {
	out := r.rel
	out.cfg = relCfg{limit: r.limit, errs: slices.Clone(r.errs)}

	if w := And(r.wheres...); w.err != nil {
		out.cfg.errs = append(out.cfg.errs, w.err)
	} else if !expr.IsTrue(w.node) {
		out.cfg.where = w.node
	}
	for _, o := range r.orders {
		if o.IsZero() {
			out.cfg.errs = append(out.cfg.errs, errors.New("order term has no column"))
			continue
		}
		out.cfg.orderBy = append(out.cfg.orderBy, o.order)
	}

	// Children are erased here, where T is still known. Below this point the
	// tree is uniform and the query orchestrates it without naming any entity.
	out.children = nil
	seen := make(map[string]bool, len(r.children))
	for _, l := range r.children {
		child := l.relation()
		// Duplicate detection is per level. The same relation on two different
		// branches is two different requests and stays legal; the same relation
		// twice under one parent is one request written twice, and running both
		// would make the result depend on which one attached last.
		if seen[child.name] {
			out.cfg.errs = append(out.cfg.errs, fmt.Errorf("relation %s was requested more than once", child.name))
			continue
		}
		seen[child.name] = true
		out.children = append(out.children, newRelNode(child))
	}
	return out
}

// With loads relations of this relation's target alongside it:
//
//	db.Users.Query().With(Users.Posts.With(Posts.Comments))
//
// The loaders are relations of T, so a relation of any other entity does not
// compile. Nesting is explicit at every level and goes as deep as it is
// written: nothing loads a relation that was not asked for, and nothing
// recurses on its own.
//
// Loading is breadth-first and batched. Every Post across every User is loaded
// by one statement, then every Comment across every Post by one more, so the
// number of statements follows the shape of the requested tree and not the
// number of rows in it.
//
// Asking for the same relation twice under one parent is an error. The same
// relation under two different parents is not: those are different paths.
func (r Rel[E, T]) With(loaders ...Loader[T]) Rel[E, T] {
	r.children = append(slices.Clone(r.children), loaders...)
	return r
}

// Where restricts which related rows are loaded.
//
//	db.Users.Query().With(Users.Posts.Where(Posts.Published.Eq(true))).All(ctx)
//
// It filters the relation, never the roots, and never an ancestor. Every user
// the query would have returned is still returned; a user with no published
// post gets a loaded and empty Posts, which is a different fact from an
// unloaded one. Filtering the roots by their relation is [Rel.Any].
//
// Several predicates in one call combine with AND, and so do several calls, so
// these are the same relation — the same rule the root [Query.Where] follows:
//
//	Users.Posts.Where(a, b)
//	Users.Posts.Where(a).Where(b)
func (r Rel[E, T]) Where(ps ...Predicate[T]) Rel[E, T] {
	r.wheres = append(slices.Clone(r.wheres), ps...)
	return r
}

// OrderBy orders the rows loaded for each parent.
//
//	db.Users.Query().With(Users.Posts.OrderBy(Posts.CreatedAt.Desc())).All(ctx)
//
// The ordering applies inside each parent's relation and says nothing about the
// order of the roots, or of any other level. Repeated calls append, so the
// second call refines the first rather than replacing it:
//
//	Users.Posts.OrderBy(Posts.CreatedAt.Desc()).OrderBy(Posts.ID.Asc())
//
// orders by created_at descending and breaks ties by id.
func (r Rel[E, T]) OrderBy(os ...Order[T]) Rel[E, T] {
	r.orders = append(slices.Clone(r.orders), os...)
	return r
}

// Limit caps the rows loaded for each parent — not the rows loaded altogether.
//
//	db.Users.Query().With(Users.Posts.OrderBy(Posts.CreatedAt.Desc()).Limit(5))
//
// gives every user up to five posts, not five posts shared between them. It
// still costs one statement however many parents there are, and a limit at one
// level says nothing about any other: five posts per user and ten comments per
// post are two independent caps.
//
// Without an ordering, which rows a parent gets is unspecified — PostgreSQL
// returns whichever five it reaches first, and no ordering is invented here to
// hide that. Pair Limit with OrderBy whenever it matters which rows arrive.
//
// A limit of zero loads the relation with no rows for every parent, and skips
// the statement entirely rather than asking the server for nothing. Its own
// relations then have no parents to load for, so they cost nothing either. A
// negative limit is a mistake, and so is a second call: silently replacing a
// cardinality bound somebody already stated is how a wrong number survives
// review.
func (r Rel[E, T]) Limit(n int) Rel[E, T] {
	switch {
	case n < 0:
		r.errs = append(slices.Clone(r.errs), fmt.Errorf("negative limit %d", n))
	case r.limit != nil:
		r.errs = append(slices.Clone(r.errs),
			fmt.Errorf("limit set twice, to %d and then %d; state one per-parent limit", *r.limit, n))
	default:
		r.limit = &n
	}
	return r
}

// Any matches root rows having at least one related row that satisfies the
// predicates:
//
//	db.Users.Query().Where(Users.Posts.Any(Posts.Published.Eq(true))).All(ctx)
//
// returns the users with a published post. With no predicates it asks only
// whether a related row exists at all.
//
// This is root filtering, which [Rel.Where] is not: the result is a
// Predicate[User] for the root query, and the relation is not loaded by it.
// Reading the posts as well means asking for them as well, with [Query.With].
//
// Because the result is a predicate over the parent, one relation's Any nests
// inside another's:
//
//	Users.Posts.Any(Posts.Comments.Any(Comments.Visible.Eq(true)))
//
// It compiles to a correlated EXISTS, so it costs no extra statement, returns
// each root row at most once, and composes with everything else a root query
// does — Limit, Offset, Count, Exists, ForUpdate.
func (r Rel[E, T]) Any(ps ...Predicate[T]) Predicate[E] { return r.semiJoin(false, ps) }

// None matches root rows having no related row that satisfies the predicates:
//
//	db.Users.Query().Where(Users.Posts.None()).All(ctx)
//
// returns the users with no posts at all, and
//
//	db.Users.Query().Where(Users.Posts.None(Posts.Published.Eq(true))).All(ctx)
//
// the users with no published one — including the users with no posts.
//
// It compiles to NOT EXISTS rather than an outer join tested for NULL, which
// says the same thing without changing the cardinality on the way.
func (r Rel[E, T]) None(ps ...Predicate[T]) Predicate[E] { return r.semiJoin(true, ps) }

// semiJoinAliasPrefix names the occurrence a self-relation's subquery reads.
const semiJoinAliasPrefix = "_s"

// semiJoin builds the correlated EXISTS behind Any and None.
func (r Rel[E, T]) semiJoin(not bool, ps []Predicate[T]) Predicate[E] {
	fail := func(format string, a ...any) Predicate[E] {
		return Predicate[E]{err: fmt.Errorf("relation %s: "+format, append([]any{r.rel.name}, a...)...)}
	}
	switch {
	case r.rel.parent == nil:
		return fail("the descriptor has no parent occurrence, so a correlated subquery cannot be built")
	case r.rel.targetSrc == nil:
		return fail("the descriptor has no target occurrence, so a correlated subquery cannot be built")
	case len(r.rel.keys) == 0:
		return fail("the descriptor has no key columns")
	// An ordering or a limit says which related rows to load. Existence is not
	// a question about which rows, and quietly ignoring the option would leave
	// a caller believing it had narrowed something.
	case len(r.orders) > 0:
		return fail("OrderBy configures which related rows load and cannot change whether one exists; drop it from the Any or None")
	case r.limit != nil:
		return fail("Limit configures how many related rows load and cannot change whether one exists; drop it from the Any or None")
	case len(r.children) > 0:
		return fail("With loads related rows and cannot change whether one exists; drop it from the Any or None")
	}

	// The subquery reads the target's own occurrence, so that a predicate built
	// from the target's descriptors — or a raw fragment naming its table — is
	// in scope inside it.
	child := r.rel.targetSrc
	if child.Ref() == r.rel.parent.Ref() {
		// A relation to its own table would put two occurrences under one name
		// in one statement, where the inner shadows the outer and the
		// correlation silently compares a row with itself.
		if len(ps) > 0 || len(r.wheres) > 0 {
			return fail("relates %s to itself, so a predicate naming that table inside the subquery"+
				" cannot be told from one naming the row being tested; Any and None over a self relation take no predicates",
				r.rel.parent.Ref())
		}
		child = child.Reserved(semiJoinAliasPrefix + r.rel.name)
	}

	conds := []expr.Node{joinCondition(r.rel.parent, child, r.rel.keys)}
	// The options configured for loading are part of the same question when
	// they say which rows count, so a Where travels into the subquery.
	all := And(append(slices.Clone(r.wheres), ps...)...)
	if all.err != nil {
		return Predicate[E]{err: all.err}
	}
	if !expr.IsTrue(all.node) {
		conds = append(conds, all.node)
	}
	if err := errors.Join(r.errs...); err != nil {
		return Predicate[E]{err: err}
	}

	sub := &expr.Select{From: child, SelectOne: true, Where: expr.Group{Op: expr.OpAnd, Items: conds}}
	if len(conds) == 1 {
		sub.Where = conds[0]
	}
	return Predicate[E]{node: expr.Exists{Not: not, Sub: sub}}
}

// OneRelSpec is everything the runtime needs to load a to-one relation.
//
// It is a struct rather than a parameter list because generated code fills it
// once and a reader has to be able to tell which callback is which. Nothing
// outside generated code builds one.
type OneRelSpec[E, T any] struct {
	// Name is the relation's Go field name.
	Name string
	// Parent is the occurrence of E's table these descriptors belong to, and
	// Target the target's own package-level occurrence.
	Parent, Target *Source
	// Keys pairs the columns the relation matches on, in constraint order.
	Keys []RelKey
	// Columns are the target columns a statement of its own reads.
	Columns []string
	// Bind allocates one row's worth of storage for the folded path.
	Bind func() ([]any, func(*E))
	// ExtractKeys reads the parent keys out of the entities, and is nil when
	// the entity does not map them.
	ExtractKeys func(parents []*E) ([]any, error)
	// AuxColumns are those unmapped key columns, which the statement that
	// materialises the parents selects instead. NewAux allocates the buffer.
	AuxColumns []string
	NewAux     func() AuxKeys
	// Dest returns scan destinations for a target loaded on its own.
	Dest func(*T) []any
	// Attach sets the relation, with a nil target meaning loaded-absent.
	Attach func(*E, *T)
	// Refs appends pointers to the targets loaded on one parent, which is what
	// this relation's own relations load against.
	Refs func(*E, []*T) []*T
}

// NewOneRel builds a descriptor for a to-one relation.
//
// Unconfigured and at the root of a query, it folds into the root statement
// with a LEFT JOIN, which is safe there and only there: a to-one relation adds
// at most one row per root row, so it cannot change how many rows the root
// query returns and pagination keeps meaning what it did. Anywhere else it
// loads in a statement of its own — a filtered join would have to be written
// against an occurrence the caller's predicates cannot name, and a relation
// statement has no root to fold into.
func NewOneRel[E, T any](s OneRelSpec[E, T]) Rel[E, T] {
	return Rel[E, T]{rel: relation[E]{
		name:      s.Name,
		parent:    s.Parent,
		targetSrc: s.Target,
		target:    tableOf(s.Target),
		keys:      s.Keys,
		columns:   s.Columns,
		fold: func(parent, target *Source) folded[E] {
			f := folded[E]{bind: s.Bind}
			for _, c := range s.Columns {
				f.columns = append(f.columns, expr.Column{Source: target, Name: c})
			}
			f.on = joinCondition(parent, target, s.Keys)
			return f
		},
		extract:    s.ExtractKeys,
		auxColumns: s.AuxColumns,
		newAux:     s.NewAux,
		run: func(ctx context.Context, ex Executor, parents []*E, sql string, args []any, aux []AuxKeys) error {
			groups, order, err := loadGroups(ctx, ex, len(parents), sql, args, s.Dest, aux)
			if err != nil {
				return err
			}
			for i, p := range parents {
				if len(groups[i]) == 0 {
					s.Attach(p, nil)
					continue
				}
				// A to-one relation is backed by a unique constraint, so a
				// second row would mean that uniqueness is gone. Taking the
				// first is also what an explicit Limit on the relation asks
				// for, and the ordering the caller gave decides which it is.
				s.Attach(p, &groups[i][0])
			}
			reorder(aux, order)
			return nil
		},
		attachNone: func(parents []*E) {
			for _, p := range parents {
				s.Attach(p, nil)
			}
		},
		collect: collector(s.Refs),
	}}
}

// ManyRelSpec is everything the runtime needs to load a to-many relation. Its
// fields mean what [OneRelSpec]'s do.
type ManyRelSpec[E, T any] struct {
	// Name is the Go field the relation loads into, which is what a trace
	// event and a diagnostic call it.
	Name string
	// Parent is the source the parent rows came from; Target is the source the
	// related rows are read from. Both are needed because a relation may join a
	// table to itself, and the two occurrences are then different sources with
	// the same table behind them.
	Parent, Target *Source
	// Keys pairs the parent column with the target column, in the order the
	// foreign key declares them. A composite key has more than one.
	Keys []RelKey
	// Columns are the target columns to select, in the order Dest fills them.
	Columns []string
	// ExtractKeys collects the key values of a batch of parents, which become
	// the IN list of the one statement this relation runs.
	ExtractKeys func(parents []*E) ([]any, error)
	// AuxColumns are extra columns selected only to match rows back to their
	// parents — the target's side of the key when it is not otherwise
	// projected. NewAux allocates the scan destinations for them.
	AuxColumns []string
	NewAux     func() AuxKeys
	// Dest returns the scan destinations for one target row, in Columns order.
	Dest func(*T) []any
	// Attach stores the loaded rows on the parent. It is called once per
	// parent, with that parent's rows.
	Attach func(*E, []T)
	// Refs returns the pointers to the rows just attached, so that a further
	// level of relations can be loaded into them without copying.
	Refs func(*E, []*T) []*T
}

// NewManyRel builds a descriptor for a to-many relation, which is loaded in one
// further statement after its parents are known.
//
// A to-many relation is never folded. Joining it would multiply the parent rows
// by their children, so a limit would stop counting parents and start counting
// pairs — asking for a relation would change which rows you got back.
func NewManyRel[E, T any](s ManyRelSpec[E, T]) Rel[E, T] {
	return Rel[E, T]{rel: relation[E]{
		name:       s.Name,
		parent:     s.Parent,
		targetSrc:  s.Target,
		target:     tableOf(s.Target),
		keys:       s.Keys,
		columns:    s.Columns,
		extract:    s.ExtractKeys,
		auxColumns: s.AuxColumns,
		newAux:     s.NewAux,
		run: func(ctx context.Context, ex Executor, parents []*E, sql string, args []any, aux []AuxKeys) error {
			groups, order, err := loadGroups(ctx, ex, len(parents), sql, args, s.Dest, aux)
			if err != nil {
				return err
			}
			for i, p := range parents {
				s.Attach(p, groups[i])
			}
			reorder(aux, order)
			return nil
		},
		attachNone: func(parents []*E) {
			for _, p := range parents {
				s.Attach(p, nil)
			}
		},
		collect: collector(s.Refs),
	}}
}

// targetSet is a relation's loaded targets on their way to the level below.
//
// The slice inside is []*T for the target of the relation that produced it, and
// only the closures generated for that same T ever open it. The count travels
// alongside so that the orchestration can tell an empty level from a full one
// without knowing, or reflecting on, what is in it.
type targetSet struct {
	value any
	n     int
}

// collector flattens the targets a relation loaded, in parent order.
//
// The pointers are taken only after every parent's relation has been attached,
// so the slices they point into have stopped growing. Taken while the loader
// was still appending, they would refer to an array the relation no longer
// uses, and the next level would load against rows nobody can see.
func collector[E, T any](refs func(*E, []*T) []*T) func([]*E) targetSet {
	return func(parents []*E) targetSet {
		// One slice grown once, rather than one allocation per parent: a
		// relation over ten thousand parents should not cost ten thousand
		// intermediate slices on the way to the next level.
		out := make([]*T, 0, len(parents))
		for _, p := range parents {
			out = refs(p, out)
		}
		return targetSet{value: out, n: len(out)}
	}
}

// reorder puts every aux buffer into the order its rows were attached in.
func reorder(aux []AuxKeys, order []int) {
	for _, a := range aux {
		if a != nil {
			a.Reorder(order)
		}
	}
}

// newRelNode erases one relation into a plan node.
//
// The closure is the only place the target type survives, and it is built by
// the same call that knows it. Nothing else casts these values: a node's parents
// are always the targets the node above it produced, which is the same type by
// construction.
func newRelNode[E any](r relation[E]) relNode {
	n := relNode{
		name:       r.name,
		auxColumns: r.auxColumns,
		newAux:     r.newAux,
		children:   r.children,
		cfg:        r.cfg,
		foldable:   r.fold != nil,
	}
	n.exec = func(ctx context.Context, ex Executor, strat relStrategy, in targetSet, aux AuxKeys) (targetSet, []AuxKeys, error) {
		// The parents are always the targets the node above produced, which is
		// this entity by construction. The check is here because a nil plan or
		// a hand-built node would otherwise fail somewhere less obvious.
		parents, ok := in.value.([]*E)
		if !ok {
			return targetSet{}, nil, fmt.Errorf("relation %s was given parents of the wrong type", r.name)
		}
		switch strat {
		case stratFold, stratFoldTarget:
			// The rows arrived with the parent statement and are already
			// attached; all that is left is to say what they were.
			return r.collect(parents), nil, nil
		case stratEmpty:
			r.attachNone(parents)
			return r.collect(parents), nil, nil
		}

		args, err := relationArgs(r, parents, aux)
		if err != nil {
			return targetSet{}, nil, err
		}
		// Each child that cannot read its keys from the target entity gets a
		// buffer, and the columns behind it join this statement's select list.
		childAux := make([]AuxKeys, len(n.children))
		var auxColumns []string
		for i, c := range n.children {
			if len(c.auxColumns) == 0 {
				continue
			}
			childAux[i] = c.newAux()
			auxColumns = append(auxColumns, c.auxColumns...)
		}

		sql, sqlArgs, err := relationSelect(r, args, auxColumns).Compile()
		if err != nil {
			return targetSet{}, nil, fmt.Errorf("compiling the loader: %w", err)
		}
		if err := r.run(ctx, ex, parents, sql, sqlArgs, childAux); err != nil {
			return targetSet{}, nil, err
		}
		return r.collect(parents), childAux, nil
	}
	return n
}

// relationArgs produces the key arrays a batched relation is loaded by, from
// the entities when they carry the keys and from the parent statement's own
// buffer when they do not.
func relationArgs[E any](r relation[E], parents []*E, aux AuxKeys) ([]any, error) {
	if aux != nil {
		return aux.Arrays(), nil
	}
	if r.extract == nil {
		return nil, fmt.Errorf("relation %s has no way to read its parent keys", r.name)
	}
	args, err := r.extract(parents)
	if err != nil {
		return nil, fmt.Errorf("collecting the keys: %w", err)
	}
	return args, nil
}

// tableOf names the table an occurrence reads, for diagnostics.
func tableOf(src *Source) TableID {
	if src == nil {
		return TableID{}
	}
	schema, table := src.Relation()
	return TableID{Schema: schema, Name: table}
}

// joinCondition pairs the key columns of the two sides, in the order the
// foreign key declares them.
func joinCondition(parent, target *Source, keys []RelKey) expr.Node {
	items := make([]expr.Node, 0, len(keys))
	for _, k := range keys {
		items = append(items, expr.Binary{
			Op:    expr.OpEq,
			Left:  expr.Column{Source: target, Name: k.Target},
			Right: expr.Column{Source: parent, Name: k.Parent},
		})
	}
	if len(items) == 1 {
		return items[0]
	}
	return expr.Group{Op: expr.OpAnd, Items: items}
}

// loadGroups runs a relation statement and buckets its rows by parent ordinal.
//
// It returns the rows grouped by parent, and the scan order that flattening
// those groups in parent order corresponds to — which is what puts any aux
// values the statement also selected into the same order as the entities they
// belong to.
//
// The statement returns each row alongside the ordinal of the parent it
// matched, so bucketing is indexing rather than comparing. Nothing here decides
// which rows relate: PostgreSQL already did, under whatever equality the key's
// own type defines.
func loadGroups[T any](
	ctx context.Context,
	ex Executor,
	parents int,
	sql string,
	args []any,
	dest func(*T) []any,
	aux []AuxKeys,
) ([][]T, []int, error) {
	if ex == nil {
		return nil, nil, fmt.Errorf("the repository has no executor")
	}
	rows, err := ex.Query(ctx, sql, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("querying: %w", err)
	}
	// The rows are read and closed here, before anything below this node runs,
	// so a deep tree never holds several result sets open at once.
	defer rows.Close()

	groups := make([][]T, parents)
	order := make([][]int, parents)
	scanned := 0
	for rows.Next() {
		var (
			ord   int64
			child T
		)
		scan := append([]any{&ord}, dest(&child)...)
		for _, a := range aux {
			if a != nil {
				scan = append(scan, a.Next()...)
			}
		}
		if err := rows.Scan(scan...); err != nil {
			return nil, nil, fmt.Errorf("scanning a related row: %w", err)
		}
		// The ordinal comes from the statement this package wrote, so a value
		// outside the range means the statement and the parents have got out
		// of step. That is a defect rather than a data condition, and it is
		// still an error rather than a panic.
		if ord < 1 || ord > int64(parents) {
			return nil, nil, fmt.Errorf("a related row carries ordinal %d, which is outside the %d parents it was loaded for", ord, parents)
		}
		groups[ord-1] = append(groups[ord-1], child)
		order[ord-1] = append(order[ord-1], scanned)
		scanned++
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("reading related rows: %w", err)
	}

	flat := make([]int, 0, scanned)
	for _, g := range order {
		flat = append(flat, g...)
	}
	return groups, flat, nil
}
