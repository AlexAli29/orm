package diagnostics

import (
	"fmt"
	"strings"
)

// The static half: what the query says about itself.
//
// A Shape is the ORM's own reading of a statement — which sources it names, how
// they are joined, whether it streams, what it locks — with the bind values
// left out. It is built from the typed tree the query was assembled from, never
// by parsing SQL text, which is why a Raw statement has almost nothing in it
// and says so rather than inventing structure from a string.
//
// Nothing in a Shape depends on a database. That is the point: these findings
// are available before a statement has ever run, in a unit test, in a code
// review, in a linter.

// Kind is what a statement does.
type Kind string

// The statement kinds.
const (
	KindSelect Kind = "SELECT"
	KindInsert Kind = "INSERT"
	KindUpdate Kind = "UPDATE"
	KindDelete Kind = "DELETE"
	KindCopy   Kind = "COPY"
	KindRaw    Kind = "raw"
)

// SourceKind is how a source produces its rows.
type SourceKind string

// The source kinds.
const (
	SourceTable   SourceKind = "table"
	SourceDerived SourceKind = "derived table"
	SourceCTE     SourceKind = "CTE"
)

// Source is one row source the statement reads.
type Source struct {
	// Kind is what this source is: a table, a subquery, a CTE, a join.
	Kind SourceKind
	// Schema and Table name a table. They are empty for a derived table.
	Schema, Table string
	// Alias is the occurrence's name, empty when the source is not aliased.
	Alias string
	// Name is what a CTE is called.
	Name string
	// Recursive marks a recursive CTE.
	Recursive bool
	// Lateral marks a derived table that may name the sources before it.
	Lateral bool
}

// String names the source the way the SQL refers to it.
func (s Source) String() string {
	name := s.Name
	if s.Table != "" {
		name = s.Table
		if s.Schema != "" && s.Schema != "public" {
			name = s.Schema + "." + s.Table
		}
	}
	if name == "" {
		name = string(s.Kind)
	}
	if s.Alias != "" && s.Alias != name {
		name += " " + s.Alias
	}
	return name
}

// Join is one join in the statement.
type Join struct {
	// Type is the join as SQL names it: INNER, LEFT, RIGHT, FULL, CROSS.
	Type string
	// Source is what is joined in.
	Source Source
	// HasCondition is false for a join with no ON clause, which is a cross
	// join however it was spelled.
	HasCondition bool
}

// Ordering is one ORDER BY term, without the expression's values.
type Ordering struct {
	// Expr names what is ordered by: a column as "table.column", or the kind
	// of expression when it is not a plain column.
	Expr string
	Desc bool
}

// String renders the term as it would read in SQL.
func (o Ordering) String() string {
	if o.Desc {
		return o.Expr + " DESC"
	}
	return o.Expr + " ASC"
}

// Lock describes a locking clause.
type Lock struct {
	// Strength is the clause PostgreSQL names: FOR UPDATE, FOR SHARE and the
	// two between them.
	Strength string
	// Wait is how the statement treats a row somebody else holds: empty for
	// waiting, otherwise NOWAIT or SKIP LOCKED.
	Wait string
	// Of names the sources the clause locks, and is empty when it locks all of
	// them.
	Of []string
}

// Relation is one step of a relation load.
type Relation struct {
	// Path is how the relation was reached, rooted at the entity:
	// "users.Posts", then "users.Posts.Comments".
	Path string
	// Depth is how many relation steps deep this one is, counting from one.
	Depth int
	// Target names the table the step reads.
	Target string
	// Cardinality is "one" or "many".
	Cardinality string
	// Batched reports that the step loads every parent's children in one
	// statement rather than one statement per parent.
	Batched bool
	// PerParentLimit is the limit applied to each parent's children, and is
	// zero when there is none.
	PerParentLimit int
}

// Shape is the ORM's structural reading of one statement.
//
// Every field is derived from the typed tree. There are no bind values in it
// and there is nowhere to put one: the fields hold identifiers, operators and
// counts, which is what makes a Shape safe to log.
type Shape struct {
	// Kind is the statement this shape describes.
	Kind Kind
	// Analyzable is false when the statement's structure is not available —
	// which means Raw, where the SQL was written by the caller and this package
	// does not parse SQL.
	Analyzable bool
	// Root is the source the statement is anchored on.
	Root Source
	// Joins holds the joins in the order they were attached.
	Joins []Join
	// CTEs holds the WITH items in declaration order.
	CTEs []Source
	// Derived holds the derived tables the statement reads from.
	Derived []Source
	// Correlated counts subqueries that name a source from outside themselves.
	Correlated int

	// FilterColumns names the columns the statement filters on, deduplicated
	// and in the order first seen. It holds identifiers, never values.
	FilterColumns []string
	// FilterCount is how many top-level conditions the statement carries.
	FilterCount int
	// Projected is how many expressions the statement selects, and is zero for
	// a whole-entity read.
	Projected int
	// GroupBy names the grouping expressions.
	GroupBy []string
	// OrderBy holds the ordering terms.
	OrderBy []Ordering
	// HasLimit and Limit report a LIMIT.
	HasLimit bool
	Limit    int
	// HasOffset reports an OFFSET.
	HasOffset bool
	// Distinct reports DISTINCT or DISTINCT ON.
	Distinct bool
	// Aggregates and Windows report whether the statement uses them.
	Aggregates bool
	Windows    bool
	// Returning reports a write with a RETURNING clause.
	Returning bool

	// Lock describes the locking clause, or nil.
	Lock *Lock

	// Streams reports that the API returns rows as they arrive rather than
	// reading them all first.
	Streams bool
	// BufferedBecause says why an operation buffers, and is empty when it
	// streams. It is a reason a person can act on: "relation loading needs
	// every parent before it can batch the children".
	BufferedBecause string

	// Relations holds the relation-loading steps, in the order they run.
	Relations []Relation
	// Statements is how many statements the whole operation runs: the root plus
	// one per relation step. It is exact — the relation loader batches, so the
	// count does not depend on how many rows come back.
	Statements int

	// Table and Columns describe a COPY.
	Table   string
	Columns []string
}

// Static reads a Shape and reports what its structure says.
//
// It contacts nothing and executes nothing. Every finding is available before
// the statement has ever run.
func Static(s Shape) []Diagnostic {
	if !s.Analyzable {
		return []Diagnostic{{
			Code: CodeRawBoundary, Severity: Info, Confidence: High,
			Message: "this is a raw statement, so its structure is not available",
			Evidence: []Evidence{
				ev("statement kind", string(s.Kind)),
			},
			Note: "the SQL was written by the caller and this package does not parse SQL, so static findings are limited to what the ORM was told. " +
				"EXPLAIN still works, and plan findings are unaffected",
		}}
	}

	out := []Diagnostic{shapeFinding(s)}
	out = append(out, crossJoinFindings(s)...)
	out = append(out, unboundedFinding(s)...)
	out = append(out, lockFinding(s)...)
	out = append(out, bufferingFinding(s)...)
	out = append(out, relationFinding(s)...)
	out = append(out, correlationFinding(s)...)
	return out
}

// shapeFinding states what the statement is, which is the thing a reader wants
// first and the thing a fingerprint alone does not tell them.
func shapeFinding(s Shape) Diagnostic {
	evidence := []Evidence{ev("statement", string(s.Kind))}
	if s.Kind == KindCopy {
		evidence = append(evidence,
			ev("table", s.Table),
			ev("columns", fmt.Sprintf("%d (%s)", len(s.Columns), strings.Join(s.Columns, ", "))))
		return Diagnostic{
			Code: CodeShape, Severity: Info, Confidence: High,
			Message: fmt.Sprintf("COPY into %s", s.Table), Evidence: evidence,
		}
	}

	evidence = append(evidence, ev("root source", s.Root.String()))
	if len(s.Joins) > 0 {
		kinds := make([]string, 0, len(s.Joins))
		for _, j := range s.Joins {
			kinds = append(kinds, j.Type+" "+j.Source.String())
		}
		evidence = append(evidence, ev("joins", strings.Join(kinds, "; ")))
	}
	if len(s.CTEs) > 0 {
		names := make([]string, 0, len(s.CTEs))
		for _, c := range s.CTEs {
			n := c.Name
			if c.Recursive {
				n += " (recursive)"
			}
			names = append(names, n)
		}
		evidence = append(evidence, ev("CTEs", strings.Join(names, ", ")))
	}
	if len(s.Derived) > 0 {
		names := make([]string, 0, len(s.Derived))
		for _, d := range s.Derived {
			n := d.Alias
			if d.Lateral {
				n += " (lateral)"
			}
			names = append(names, n)
		}
		evidence = append(evidence, ev("derived tables", strings.Join(names, ", ")))
	}
	if len(s.FilterColumns) > 0 {
		evidence = append(evidence, ev("filtered on", strings.Join(s.FilterColumns, ", ")))
	}
	if len(s.GroupBy) > 0 {
		evidence = append(evidence, ev("grouped by", strings.Join(s.GroupBy, ", ")))
	}
	if len(s.OrderBy) > 0 {
		terms := make([]string, 0, len(s.OrderBy))
		for _, o := range s.OrderBy {
			terms = append(terms, o.String())
		}
		evidence = append(evidence, ev("ordered by", strings.Join(terms, ", ")))
	}
	if s.HasLimit {
		evidence = append(evidence, ev("limit", s.Limit))
	}
	if s.HasOffset {
		evidence = append(evidence, ev("offset", "present"))
	}
	if s.Distinct {
		evidence = append(evidence, ev("distinct", "yes"))
	}
	if s.Aggregates {
		evidence = append(evidence, ev("aggregates", "yes"))
	}
	if s.Windows {
		evidence = append(evidence, ev("window functions", "yes"))
	}
	if s.Returning {
		evidence = append(evidence, ev("returning", "yes"))
	}

	summary := fmt.Sprintf("%s over %s", s.Kind, s.Root.String())
	if len(s.Joins) > 0 {
		summary += fmt.Sprintf(" with %d join(s)", len(s.Joins))
	}
	return Diagnostic{
		Code: CodeShape, Severity: Info, Confidence: High,
		Message: summary, Evidence: evidence,
	}
}

// crossJoinFindings reports a join with no condition.
//
// It is information rather than a warning. A cross join against a one-row
// source is a normal way to bring a computed value into a statement, and
// calling that a bug would be wrong. What is worth saying is that the join has
// no condition, because when that is not deliberate it is expensive.
func crossJoinFindings(s Shape) []Diagnostic {
	var out []Diagnostic
	for _, j := range s.Joins {
		if j.HasCondition {
			continue
		}
		out = append(out, Diagnostic{
			Code: CodeCrossJoin, Severity: Info, Confidence: High,
			Message: fmt.Sprintf("%s is joined with no condition", j.Source.String()),
			Evidence: []Evidence{
				ev("join", j.Type),
				ev("source", j.Source.String()),
			},
			Note: "every row on one side is paired with every row on the other. That is often deliberate — a one-row source brought in for a computed value — and is worth checking when it is not",
		})
	}
	return out
}

// unboundedFinding reports a row-returning query with no LIMIT.
//
// It is suppressed wherever reading everything is the point. Counting,
// existence, aggregation and grouping all consume the whole input by
// definition, and streaming hands rows over as they arrive rather than holding
// them, so none of those wants to be told about a missing LIMIT. What is left
// is a query that will buffer an unknown number of rows into memory, which is
// worth a quiet word and nothing more.
func unboundedFinding(s Shape) []Diagnostic {
	switch {
	case s.Kind != KindSelect,
		s.HasLimit,
		s.Streams,
		s.Aggregates,
		len(s.GroupBy) > 0:
		return nil
	}
	return []Diagnostic{{
		Code: CodeUnbounded, Severity: Info, Confidence: Low,
		Message: "the query has no LIMIT, so it returns as many rows as match",
		Evidence: []Evidence{
			ev("root source", s.Root.String()),
			ev("buffered", "yes — the rows are read into a slice before returning"),
		},
		Note: "the number of rows is whatever matches; All reads them into a slice, and Rows hands them over as they arrive. " +
			"Many queries legitimately want every row, which is why this is reported at low confidence and as information",
	}}
}

// lockFinding reports a locking clause, which changes what the statement does
// to other transactions and is worth stating plainly.
func lockFinding(s Shape) []Diagnostic {
	if s.Lock == nil {
		return nil
	}
	evidence := []Evidence{ev("strength", s.Lock.Strength)}
	if s.Lock.Wait != "" {
		evidence = append(evidence, ev("waiting policy", s.Lock.Wait))
	} else {
		evidence = append(evidence, ev("waiting policy", "waits for a row somebody else holds"))
	}
	if len(s.Lock.Of) > 0 {
		evidence = append(evidence, ev("locks", strings.Join(s.Lock.Of, ", ")))
	} else {
		evidence = append(evidence, ev("locks", "every source the statement reads"))
	}
	return []Diagnostic{{
		Code: CodeLocking, Severity: Notice, Confidence: High,
		Message:  fmt.Sprintf("the statement takes %s on the rows it returns", s.Lock.Strength),
		Evidence: evidence,
		Note: "the lock is held until the transaction ends, so a statement that locks and then commits immediately has locked nothing for any length of time. " +
			"Without a target list the clause locks every source, including ones the caller only meant to read",
	}}
}

// bufferingFinding says whether the operation streams, and when it does not,
// why. "Why" is the useful half: it is what a caller would change.
func bufferingFinding(s Shape) []Diagnostic {
	if s.Streams {
		return []Diagnostic{{
			Code: CodeBuffered, Severity: Info, Confidence: High,
			Message: "the operation streams: rows are handed over as they arrive",
			Evidence: []Evidence{
				ev("memory", "one row at a time"),
				ev("connection", "held for as long as the caller iterates"),
			},
		}}
	}
	if s.BufferedBecause == "" {
		return nil
	}
	return []Diagnostic{{
		Code: CodeBuffered, Severity: Info, Confidence: High,
		Message: "the operation reads every row before returning",
		Evidence: []Evidence{
			ev("reason", s.BufferedBecause),
		},
	}}
}

// relationFinding reports what a relation load will do, and in particular how
// many statements it is.
//
// The N+1 question is answered here rather than guessed at. The relation loader
// batches: one statement per relation step, whatever the number of parents. So
// the count is exact and can be stated as a fact rather than a hope, which is
// the opposite of warning about N+1 merely because relations are in use.
func relationFinding(s Shape) []Diagnostic {
	if len(s.Relations) == 0 {
		return nil
	}
	evidence := []Evidence{
		ev("statements", s.Statements),
		ev("root", "1"),
		ev("relation steps", len(s.Relations)),
	}
	depth := 0
	for _, r := range s.Relations {
		if r.Depth > depth {
			depth = r.Depth
		}
		desc := r.Path + " -> " + r.Target + " (" + r.Cardinality
		if r.Batched {
			desc += ", batched"
		}
		if r.PerParentLimit > 0 {
			desc += fmt.Sprintf(", %d per parent", r.PerParentLimit)
		}
		desc += ")"
		evidence = append(evidence, ev("step", desc))
	}
	evidence = append(evidence, ev("depth", depth))

	batched := true
	for _, r := range s.Relations {
		if !r.Batched {
			batched = false
		}
	}
	msg := fmt.Sprintf("loading %d relation(s) takes %d statements in total", len(s.Relations), s.Statements)
	note := ""
	if batched {
		note = "the count does not grow with the number of rows: every step loads all of its parents' children in one statement, so this is not an N+1"
	} else {
		note = "at least one step is not batched, so its statement count depends on how many parents came back"
	}
	return []Diagnostic{{
		Code: CodeRelationPlan, Severity: Info, Confidence: High,
		Message: msg, Evidence: evidence, Note: note,
	}}
}

// correlationFinding reports subqueries that name a source from outside
// themselves, which is the shape that runs once per outer row.
func correlationFinding(s Shape) []Diagnostic {
	if s.Correlated == 0 {
		return nil
	}
	return []Diagnostic{{
		Code: CodeCorrelated, Severity: Info, Confidence: High,
		Message: fmt.Sprintf("the statement contains %d correlated subquery(s)", s.Correlated),
		Evidence: []Evidence{
			ev("correlated subqueries", s.Correlated),
		},
		Note: "a correlated subquery may be evaluated per outer row. PostgreSQL often rewrites one into a join, and the plan is where to check whether it did",
	}}
}
