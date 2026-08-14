package orm

import (
	"fmt"

	"github.com/AlexAli29/orm/diagnostics"
	"github.com/AlexAli29/orm/internal/expr"
	"github.com/AlexAli29/orm/plan"
)

// Structural diagnostics.
//
// Everything here reads the statement the query would send and says what is in
// it. Nothing runs, nothing connects, and nothing needs a database — which is
// the point: a shape is available in a unit test, in a review, in a linter,
// before the query has ever been near PostgreSQL.
//
// What crosses out of the compiler is a description rather than the tree. The
// internal boundary hands over names, operators and counts, so a Shape holds
// identifiers this package wrote and numbers it counted. There is nowhere in it
// for a bind value to appear, which is what makes one safe to log.

// shapeFrom converts the compiler's description into the public one.
func shapeFrom(info expr.SelectInfo, kind diagnostics.Kind) diagnostics.Shape {
	s := diagnostics.Shape{
		Kind:          kind,
		Analyzable:    true,
		Root:          publicSource(info.Root),
		Correlated:    info.Correlated,
		FilterColumns: info.FilterColumns,
		FilterCount:   info.FilterCount,
		Projected:     info.Projected,
		GroupBy:       info.GroupBy,
		HasLimit:      info.HasLimit,
		Limit:         info.Limit,
		HasOffset:     info.HasOffset,
		Distinct:      info.Distinct,
		Aggregates:    info.Aggregates,
		Windows:       info.Windows,
	}
	for _, j := range info.Joins {
		s.Joins = append(s.Joins, diagnostics.Join{
			Type:         j.Kind,
			Source:       publicSource(j.Source),
			HasCondition: j.HasCondition,
		})
	}
	for _, c := range info.CTEs {
		s.CTEs = append(s.CTEs, publicSource(c))
	}
	for _, d := range info.Derived {
		s.Derived = append(s.Derived, publicSource(d))
	}
	for _, o := range info.OrderBy {
		s.OrderBy = append(s.OrderBy, diagnostics.Ordering{Expr: o.Expr, Desc: o.Desc})
	}
	if info.Lock != nil {
		s.Lock = &diagnostics.Lock{Strength: info.Lock.Strength, Wait: info.Lock.Wait, Of: info.Lock.Of}
	}
	return s
}

func publicSource(si expr.SourceInfo) diagnostics.Source {
	kind := diagnostics.SourceTable
	switch si.Kind {
	case expr.SourceDerived:
		kind = diagnostics.SourceDerived
	case expr.SourceCTE:
		kind = diagnostics.SourceCTE
	}
	return diagnostics.Source{
		Kind: kind, Schema: si.Schema, Table: si.Table,
		Alias: si.Alias, Name: si.Name,
		Recursive: si.Recursive, Lateral: si.Lateral,
	}
}

// Shape describes the statement this query would send.
//
// It is the ORM's own reading of the query rather than a reading of its SQL: the
// sources come from the tree, the joins come from the join list, and the
// relation steps come from the relation planner, which is what makes the
// statement count exact rather than a guess.
//
// It contacts nothing.
func (q *Query[E]) Shape() (diagnostics.Shape, error) {
	p, err := q.plan()
	if err != nil {
		return diagnostics.Shape{}, err
	}
	s := shapeFrom(expr.Describe(p.sel), diagnostics.KindSelect)
	// All and One read every row before returning; Rows hands them over as
	// they arrive. A query is described as the buffered form because that is
	// what Shape is asked about on a query rather than on an iteration, and
	// what makes it buffer is worth naming.
	s.BufferedBecause = "All and One read every row before returning; use Rows to stream"
	s.Relations, s.Statements = relationSteps(p.nodes)
	if len(s.Relations) > 0 {
		s.BufferedBecause = "relation loading needs every parent row before it can load their children in one statement"
	}
	return s, nil
}

// relationSteps flattens the resolved relation tree into the steps it will run,
// and counts the statements.
//
// The count is the root plus one per batched step. It does not depend on how
// many rows come back, because a batched step loads every parent's children in
// one statement — which is the fact that makes the N+1 question answerable
// rather than a warning.
func relationSteps(nodes []planNode) ([]diagnostics.Relation, int) {
	statements := 1
	var out []diagnostics.Relation
	var walk func([]planNode, int)
	walk = func(ns []planNode, depth int) {
		for _, n := range ns {
			step := diagnostics.Relation{
				Path:        n.node.path,
				Depth:       depth,
				Target:      n.node.name,
				Cardinality: "many",
				Batched:     n.strategy.batched(),
			}
			if n.strategy == stratFold || n.strategy == stratFoldTarget {
				step.Cardinality = "one"
			}
			if n.node.cfg.limit != nil {
				step.PerParentLimit = *n.node.cfg.limit
			}
			if n.strategy.batched() {
				statements++
			}
			out = append(out, step)
			walk(n.children, depth+1)
		}
	}
	walk(nodes, 1)
	return out, statements
}

// Diagnostics reports what the query's structure says, without running it.
//
// Everything it returns is derived from the statement the query would send. It
// is the half of a performance report that needs no database; the other half
// needs a plan, and [Query.Explain] is where that comes from.
func (q *Query[E]) Diagnostics() (diagnostics.Report, error) {
	s, err := q.Shape()
	if err != nil {
		return diagnostics.Report{}, err
	}
	return diagnostics.Report{Static: diagnostics.Static(s)}, nil
}

// Shape describes the statement this projection would send.
func (q *SelectQuery[E, R]) Shape() (diagnostics.Shape, error) {
	sel, err := q.build()
	if err != nil {
		return diagnostics.Shape{}, err
	}
	s := shapeFrom(expr.Describe(sel), diagnostics.KindSelect)
	s.BufferedBecause = "All reads every row before returning; use Rows to stream"
	return s, nil
}

// Diagnostics reports what the projection's structure says.
func (q *SelectQuery[E, R]) Diagnostics() (diagnostics.Report, error) {
	s, err := q.Shape()
	if err != nil {
		return diagnostics.Report{}, err
	}
	return diagnostics.Report{Static: diagnostics.Static(s)}, nil
}

// Shape describes the statement this composed query would send.
func (q *ComposedQuery[R]) Shape() (diagnostics.Shape, error) {
	sel, err := q.build()
	if err != nil {
		return diagnostics.Shape{}, err
	}
	s := shapeFrom(expr.Describe(sel), diagnostics.KindSelect)
	s.BufferedBecause = "All reads every row before returning; use Rows to stream"
	return s, nil
}

// Diagnostics reports what the composed query's structure says.
func (q *ComposedQuery[R]) Diagnostics() (diagnostics.Report, error) {
	s, err := q.Shape()
	if err != nil {
		return diagnostics.Report{}, err
	}
	return diagnostics.Report{Static: diagnostics.Static(s)}, nil
}

// Shape describes a raw statement, which is to say it describes almost nothing.
//
// The SQL was written by the caller and this package does not parse SQL, so
// there is no structure to report. Saying that plainly is the honest answer:
// inventing sources and predicates out of a string would produce a report that
// looks like the others and means less.
func (q *RawQuery[E]) Shape() (diagnostics.Shape, error) {
	return diagnostics.Shape{Kind: diagnostics.KindRaw, Analyzable: false}, nil
}

// Diagnostics reports the little a raw statement's structure allows.
func (q *RawQuery[E]) Diagnostics() (diagnostics.Report, error) {
	s, _ := q.Shape()
	return diagnostics.Report{Static: diagnostics.Static(s)}, nil
}

// Shape describes the UPDATE this builder would send.
func (u *Update[E]) Shape() (diagnostics.Shape, error) {
	stmt, err := u.build()
	if err != nil {
		return diagnostics.Shape{}, err
	}
	s := shapeFrom(expr.DescribeUpdate(stmt), diagnostics.KindUpdate)
	s.Returning = expr.ReturningCount(stmt.Returning, stmt.ReturningItems) > 0
	if s.Returning {
		s.BufferedBecause = "the returned rows are read before the call comes back"
	}
	return s, nil
}

// Diagnostics reports what the UPDATE's structure says.
func (u *Update[E]) Diagnostics() (diagnostics.Report, error) {
	s, err := u.Shape()
	if err != nil {
		return diagnostics.Report{}, err
	}
	return diagnostics.Report{Static: diagnostics.Static(s)}, nil
}

// Shape describes the DELETE this builder would send.
func (d *Delete[E]) Shape() (diagnostics.Shape, error) {
	stmt, err := d.build()
	if err != nil {
		return diagnostics.Shape{}, err
	}
	s := shapeFrom(expr.DescribeDelete(stmt), diagnostics.KindDelete)
	s.Returning = expr.ReturningCount(stmt.Returning, stmt.ReturningItems) > 0
	if s.Returning {
		s.BufferedBecause = "the returned rows are read before the call comes back"
	}
	return s, nil
}

// Diagnostics reports what the DELETE's structure says.
func (d *Delete[E]) Diagnostics() (diagnostics.Report, error) {
	s, err := d.Shape()
	if err != nil {
		return diagnostics.Report{}, err
	}
	return diagnostics.Report{Static: diagnostics.Static(s)}, nil
}

// CopyShape describes a COPY into the repository's table.
//
// COPY has no statement to describe — it is a protocol operation rather than
// SQL — so what there is to say is the target and the column shape, which is
// exactly what decides how the rows are encoded and in what order.
func CopyShape[E any](r *Repo[E], cols ...InsertColumn[E]) diagnostics.Shape {
	s := diagnostics.Shape{Kind: diagnostics.KindCopy, Analyzable: true}
	if r == nil {
		return s
	}
	s.Root = diagnostics.Source{
		Kind:   diagnostics.SourceTable,
		Schema: r.meta.Table.Schema, Table: r.meta.Table.Name,
	}
	s.Table = r.meta.Table.String()
	if _, names, err := r.copyColumns(cols); err == nil {
		s.Columns = names
	}
	s.BufferedBecause = "COPY streams rows to the server as they are produced"
	s.Streams = true
	return s
}

// DebugSQL renders a statement for a person to read: the SQL as it would be
// sent, with the placeholders still in it, followed by the fingerprint and how
// many arguments there are.
//
// The arguments themselves are deliberately absent. A debug helper is the most
// likely thing in this package to end up in a log line, a bug report or a chat
// message, and one that inlined values would carry whatever the query was
// filtering on into all three. What it prints instead is the shape:
//
//	SELECT "users"."id" FROM "users" WHERE "users"."email" = $1
//	-- fingerprint: v1:3f2a…
//	-- 1 argument
//
// There is deliberately no counterpart that substitutes the values in. Writing
// one would mean a second SQL serialiser — a literal writer with its own
// quoting rules, sitting beside the one that is actually used — and the two
// would drift. The place to see values is a debugger or the argument slice
// [Statement.SQL] already returns.
func DebugSQL(s Statement) string {
	if s == nil {
		return "-- no statement"
	}
	sql, args, err := s.SQL()
	if err != nil {
		return "-- the statement did not compile: " + err.Error()
	}
	out := sql
	if fp, ferr := FingerprintOf(s); ferr == nil {
		out += "\n-- fingerprint: " + fp.String()
	}
	switch len(args) {
	case 0:
		out += "\n-- no arguments"
	case 1:
		out += "\n-- 1 argument (values are not shown)"
	default:
		out += fmt.Sprintf("\n-- %d arguments (values are not shown)", len(args))
	}
	return out
}

// DiagnosePlan reports what a plan says, and is the other half of a report.
//
// It is a function rather than a method because a plan outlives the query that
// produced it: one obtained an hour ago, or read from a log, or captured in a
// test fixture, is as good an input as one just fetched. Nothing here executes
// or connects — the plan is the whole input.
func DiagnosePlan(p *plan.Plan) []diagnostics.Diagnostic {
	return diagnostics.FromPlan(p)
}
