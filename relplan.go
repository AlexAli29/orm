package orm

import (
	"context"
	"errors"
	"fmt"
	"github.com/AlexAli29/orm/observe"
	"strconv"

	"github.com/AlexAli29/orm/internal/expr"
)

// relAliasPrefix names the occurrences the relation planner joins in.
//
// The prefix is the one reserved for compiler-generated aliases, and the number
// is the relation's position in the query rather than anything read from a map,
// so the same query always renders the same SQL.
const relAliasPrefix = "_r"

// maxRelationDepth bounds how deep a requested relation tree may go.
//
// Nothing recurses on its own here — a tree is exactly as deep as it was
// written — so this is a guard against a program that builds one, not against
// the ORM building one. It is set far above any depth a person writes by hand;
// a query that reaches it has a bug above it, and saying so is better than
// exhausting the stack while planning.
const maxRelationDepth = 64

// relStrategy is how one requested relation will be loaded.
type relStrategy uint8

const (
	// stratFold joins the target into the root statement under an alias of the
	// planner's choosing. It costs no statement and streams, and it is what a
	// to-one relation with no options gets at the root of a query.
	stratFold relStrategy = iota
	// stratFoldTarget joins the target under its own name, so that the
	// predicates configured for the relation are in scope in the join
	// condition. It is the only way to filter a to-one relation whose parent
	// keys the entity does not map, since there is nowhere in the entity to
	// read those keys from.
	stratFoldTarget
	// stratBatch loads the relation in a statement of its own.
	stratBatch
	// stratEmpty loads the relation with no rows and runs no statement, which
	// is what a per-parent limit of zero asks for.
	stratEmpty
)

// batched reports whether the strategy costs a statement of its own, which is
// what decides whether a query can stream.
func (s relStrategy) batched() bool { return s == stratBatch }

// planNode is one relation of the requested tree, resolved.
//
// Planning happens before the root statement runs, so that everything the
// execution needs — which statements there will be, which columns each has to
// select on a child's behalf, and what to call anything that goes wrong — is
// known while it can still be reported cheaply.
type planNode struct {
	node     relNode
	strategy relStrategy
	children []planNode
}

// rootPlan is a root statement together with everything needed to read it.
type rootPlan[E any] struct {
	sel *expr.Select
	// folded holds the to-one relations joined into the statement, in the
	// order their columns appear after the entity's own.
	folded []folded[E]
	// nodes holds every relation requested at the root, in request order,
	// whether it folds into the statement or follows it.
	nodes []planNode
}

// plan resolves the requested relation tree and assembles the root statement.
func (q *Query[E]) plan() (*rootPlan[E], error) {
	sel, err := q.build()
	if err != nil {
		return nil, err
	}
	p := &rootPlan[E]{sel: sel}

	root := q.repo.meta.Table.Name
	for i, rel := range q.with {
		node, err := planRelation(newRelNode(rel), root, 1, true)
		if err != nil {
			return nil, err
		}
		p.nodes = append(p.nodes, node)

		switch node.strategy {
		case stratFold, stratFoldTarget:
			target := rel.targetSrc
			if node.strategy == stratFold {
				// Each unconfigured relation gets its own occurrence of the
				// target table. Two relations to one table — a self-reference,
				// or a creator and an editor both pointing at users — would
				// otherwise be one ambiguous name in the statement.
				target = NewSource(rel.target.Schema, rel.target.Name).Reserved(relAliasPrefix + strconv.Itoa(i))
			}
			f := rel.fold(q.source, target)
			// A filter on a folded relation belongs in the join condition, not
			// in the WHERE clause. In the WHERE it would drop the root rows
			// whose relation does not match; in the ON it makes those rows
			// arrive with the relation absent, which is what was asked for.
			if rel.cfg.where != nil {
				f.on = expr.Group{Op: expr.OpAnd, Items: []expr.Node{f.on, rel.cfg.where}}
			}
			p.sel.Joins = append(p.sel.Joins, expr.Join{Kind: expr.JoinLeft, Source: target, On: f.on})
			p.sel.Columns = append(p.sel.Columns, f.columns...)
			p.folded = append(p.folded, f)
		}
	}
	return p, nil
}

// planRelation resolves one node and everything under it.
//
// The recursion builds a plan; it does not load anything. Loading is iterative
// and breadth-first, which is what keeps a deep tree from holding one query's
// resources open while the next one runs.
func planRelation(n relNode, parentPath string, depth int, atRoot bool) (planNode, error) {
	n.path = parentPath + "." + n.name
	if depth > maxRelationDepth {
		return planNode{}, fmt.Errorf("relation %s is nested more than %d levels deep, which is past the point where a requested tree is a mistake rather than a request",
			n.path, maxRelationDepth)
	}
	if err := errors.Join(n.cfg.errs...); err != nil {
		return planNode{}, fmt.Errorf("relation %s: %w", n.path, err)
	}

	out := planNode{node: n}
	for _, c := range n.children {
		// A child is never at the root, whatever its parent was: only the root
		// statement has somewhere to fold a relation into.
		child, err := planRelation(c, n.path, depth+1, false)
		if err != nil {
			return planNode{}, err
		}
		out.children = append(out.children, child)
	}

	strat, err := relationStrategy(n, out.children, atRoot)
	if err != nil {
		return planNode{}, fmt.Errorf("relation %s: %w", n.path, err)
	}
	out.strategy = strat
	return out, nil
}

// relationStrategy decides how one node loads, or reports why it cannot.
//
// The decision is made once, before anything runs, and used by everything: the
// statement count, whether the query may stream, and which occurrence the
// target is read from. Deciding it twice is how a query comes to stream a
// relation it also batches.
func relationStrategy(n relNode, children []planNode, atRoot bool) (relStrategy, error) {
	// Zero rows per parent is a question with a known answer, and asking
	// PostgreSQL for nothing is still a round trip.
	if n.cfg.limit != nil && *n.cfg.limit == 0 {
		return stratEmpty, nil
	}
	if !n.foldable {
		return stratBatch, nil
	}
	// A child that reads its keys from the statement rather than from the
	// entity needs that statement to exist. A folded relation has none of its
	// own, so the parent gives one up to provide it.
	needsOwnStatement := false
	for _, c := range children {
		if len(c.node.auxColumns) > 0 {
			needsOwnStatement = true
			break
		}
	}
	if atRoot && !n.cfg.configured() && !needsOwnStatement {
		return stratFold, nil
	}
	// A configured to-one relation is filtered against the target's own
	// occurrence, and the root statement already reads a differently-named one.
	// Loading it separately is what lets the caller's predicates name the table
	// they were written against.
	if len(n.auxColumns) == 0 {
		return stratBatch, nil
	}
	// The parent's key columns are not mapped, so there is nothing in the
	// entity to batch on. At the root a filter can still travel into the join
	// condition, where it makes the relation absent rather than the root row
	// missing; an ordering or a limit cannot, because a join has no per-parent
	// ordering to apply.
	if !atRoot {
		// Away from the root there is no statement to fold into, so the keys
		// have to come from the statement that produced the parents.
		return stratBatch, nil
	}
	switch {
	case len(n.cfg.orderBy) > 0:
		return 0, errors.New("cannot be ordered: the entity does not map the key columns it matches on, so the relation is read through a join rather than a statement of its own")
	case n.cfg.limit != nil:
		return 0, errors.New("cannot be limited: the entity does not map the key columns it matches on, so the relation is read through a join rather than a statement of its own")
	case needsOwnStatement:
		return 0, errors.New("cannot load its own relations: the entity does not map the key columns it matches on, so it is read through a join, and a relation of it would have nowhere to read its keys from")
	}
	return stratFoldTarget, nil
}

// scanner returns a function reading one row into a new entity, including the
// relations folded into the statement.
//
// The destinations are rebuilt per row because each folded relation needs its
// own storage: reusing one holder would leave every parent pointing at the last
// row's relation.
func (p *rootPlan[E]) scanner(q *Query[E]) func(pgxRows) (E, error) {
	base := q.scanner()
	if len(p.folded) == 0 {
		return base
	}
	meta := q.repo.meta
	return func(rows pgxRows) (E, error) {
		var e E
		dest := make([]any, 0, len(meta.Columns))
		for i := range meta.Columns {
			d := meta.Dest(&e, i)
			if d == nil {
				return e, fmt.Errorf("scanning %s: metadata has no destination for column %d (%s)", meta.Table, i, meta.Columns[i].Name)
			}
			dest = append(dest, d)
		}
		attach := make([]func(*E), 0, len(p.folded))
		for _, f := range p.folded {
			d, set := f.bind()
			dest = append(dest, d...)
			attach = append(attach, set)
		}
		if err := rows.Scan(dest...); err != nil {
			return e, fmt.Errorf("scanning %s: %w", meta.Table, err)
		}
		for _, set := range attach {
			set(&e)
		}
		return e, nil
	}
}

// pending is one relation node waiting to be loaded, with the parents it will
// load against.
type pending struct {
	node    planNode
	parents targetSet
	// aux carries the key values this node's parent statement selected on its
	// behalf, and is nil when the node reads its keys from the entities.
	aux AuxKeys
}

// loadRelations loads the requested tree, one level at a time.
//
// Breadth-first is what makes the cost of a tree the shape of the tree. Every
// parent for a given node is available at once, so the node runs once for all
// of them however many there are; loading depth-first would mean asking for one
// parent's children before the next parent existed, which is the query per row
// this design exists to make impossible.
//
// Each level finishes — statement run, rows scanned, result set closed — before
// the next begins, so a deep tree never holds several result sets open.
func (p *rootPlan[E]) loadRelations(ctx context.Context, q *Query[E], roots []E) error {
	if len(p.nodes) == 0 || len(roots) == 0 {
		return nil
	}
	// The pointers are taken only now the slice has stopped growing. Taking
	// them while appending would leave the loaders writing into an array the
	// slice no longer uses.
	parents := make([]*E, len(roots))
	for i := range roots {
		parents[i] = &roots[i]
	}
	rootSet := targetSet{value: parents, n: len(parents)}

	level := make([]pending, 0, len(p.nodes))
	for _, n := range p.nodes {
		level = append(level, pending{node: n, parents: rootSet})
	}

	for len(level) > 0 {
		var next []pending
		for _, item := range level {
			children, err := runNode(ctx, q.repo.ex, item)
			if err != nil {
				return err
			}
			next = append(next, children...)
		}
		level = next
	}
	return nil
}

// runNode loads one node and returns the work its children become.
func runNode(ctx context.Context, ex Executor, item pending) ([]pending, error) {
	n := item.node
	// Each relation statement gets its own event, tagged with the path it
	// loads. That is what makes relation loading legible in a trace: one
	// operation event for the call, one of these for each statement it took,
	// and a count a reader can check against the number of relations asked for.
	//
	// There is no N+1 hiding in it. The loader issues one statement per
	// relation per level, whatever the number of parents, and the events say so
	// by being that many.
	ctx, sp := startSpan(ctx, ex, observe.StartEvent{
		Op:       observe.OpRelation,
		Relation: n.node.path,
	})
	targets, childAux, err := n.node.exec(ctx, ex, n.strategy, item.parents, item.aux)
	sp.end(err, int64(targets.n), err == nil)
	if err != nil {
		return nil, fmt.Errorf("loading relation %s: %w", n.node.path, err)
	}
	if len(n.children) == 0 {
		return nil, nil
	}
	// A relation that loaded nothing has no rows for its own relations to
	// attach to, so the level below it is not work that was skipped — it is
	// work that does not exist. Running it anyway would mean a statement
	// against an empty key array, asked and answered for nobody.
	if targets.n == 0 {
		return nil, nil
	}
	out := make([]pending, 0, len(n.children))
	for i, c := range n.children {
		item := pending{node: c, parents: targets}
		if i < len(childAux) {
			item.aux = childAux[i]
		}
		out = append(out, item)
	}
	return out, nil
}

// relationSelect assembles the statement one batched relation runs.
//
// It is separate from executing it so that the SQL a relation produces can be
// rendered and asserted without a database — the same reason [Query.SQL]
// exists for the root.
func relationSelect[E any](rel relation[E], args []any, aux []string) *expr.RelationSelect {
	// The child is the target's own occurrence, which is what the caller's
	// relation predicates and orderings were built against.
	child := rel.targetSrc
	stmt := &expr.RelationSelect{
		Child:   child,
		Args:    args,
		Where:   rel.cfg.where,
		OrderBy: rel.cfg.orderBy,
		Limit:   rel.cfg.limit,
	}
	for _, c := range rel.columns {
		stmt.Columns = append(stmt.Columns, expr.Column{Source: child, Name: c})
	}
	// The columns a child asked for follow the entity's own, so the scanner and
	// the statement cannot disagree about which is which.
	for _, c := range aux {
		stmt.Columns = append(stmt.Columns, expr.Column{Source: child, Name: c})
	}
	for _, k := range rel.keys {
		stmt.KeyTypes = append(stmt.KeyTypes, k.Type)
		stmt.ChildKeys = append(stmt.ChildKeys, k.Target)
	}
	return stmt
}

// streamable reports the relation that stops a query from streaming, or nil.
//
// A folded relation streams because it is part of the root statement and
// arrives with the row. Anything else cannot: it needs every root row before it
// can be attached, and answering it would mean reading the whole result into
// memory — which is exactly what Rows exists not to do. The whole tree is
// checked, not just its top: a relation that folds but whose own relations do
// not is still a query that cannot stream.
func (q *Query[E]) streamable() error {
	root := q.repo.meta.Table.Name
	for _, rel := range q.with {
		node, err := planRelation(newRelNode(rel), root, 1, true)
		if err != nil {
			return err
		}
		if path, ok := firstBatched(node); ok {
			return fmt.Errorf("%w: relation %s is loaded after the root rows, which needs every row first; use All",
				ErrStreamingRelation, path)
		}
	}
	return nil
}

// firstBatched returns the path of the first node in the tree that costs a
// statement of its own.
func firstBatched(n planNode) (string, bool) {
	if n.strategy.batched() || n.strategy == stratEmpty {
		return n.node.path, true
	}
	for _, c := range n.children {
		if path, ok := firstBatched(c); ok {
			return path, true
		}
	}
	return "", false
}
