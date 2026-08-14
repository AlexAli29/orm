package orm

import (
	"time"

	"github.com/AlexAli29/orm/internal/expr"
)

// Advanced PostgreSQL expressions.
//
// Everything here builds an [Expression], so all of it composes with everything
// else: a CASE can be selected, grouped by, compared, aliased into a derived
// table's output, or wrapped in an aggregate, without any of those learning
// what a CASE is.
//
// Two rules decide every signature.
//
// The first is that a result type is PostgreSQL's rather than Go's. Where the
// two disagree — extract returns numeric, not a float — the signature says so
// or asks the caller to name a type from the registry rather than inventing
// one.
//
// The second is that nullability is never claimed away. A CASE with no ELSE is
// nullable however non-nullable its branches are, NULLIF is nullable however
// non-nullable its arguments are, and COALESCE is non-nullable only when one of
// its arguments provably is. Where a claim cannot be proved, the conservative
// answer is the one taken: a nullable result costs a pointer, and a wrong
// non-nullable one costs a scan error at some hour nobody chose.

// Case starts a searched CASE with its first branch.
//
//	orm.Case(orm.Cond(Users.Active.Eq(true)), orm.Val("active")).
//	    When(orm.Cond(Users.Age.Lt(int32(18))), orm.Val("minor")).
//	    Else(orm.Val("inactive"))
//
// The first branch decides the result type and its nullable form, and every
// later branch and the ELSE have to produce the same type — which is what makes
// a mismatched branch a compile error rather than a message from PostgreSQL
// about types that cannot be matched.
//
// The branch is required because a CASE with none has no value to produce.
func Case[E, T, N any](cond Predicate[Composed], then Typed[E, T, N]) *CaseBuilder[T, N] {
	return &CaseBuilder[T, N]{
		when: []expr.CaseBranch{{Cond: cond.node, Then: then.selectItem().Node}},
		err:  cond.err,
	}
}

// CaseBuilder accumulates the branches of a searched CASE.
//
// It is a builder rather than a value because a CASE is a list, and it hands
// back an [Expression] at the end — either from [CaseBuilder.Else], which makes
// the result as nullable as its branches, or from [CaseBuilder.End], which
// makes it nullable whatever they are.
type CaseBuilder[T, N any] struct {
	when []expr.CaseBranch
	err  error
}

// When adds a branch. Branches are evaluated in the order they were added, and
// the first whose condition is TRUE decides the value.
func (c *CaseBuilder[T, N]) When(cond Predicate[Composed], then Selectable[Composed, T]) *CaseBuilder[T, N] {
	if c.err == nil {
		c.err = cond.err
	}
	c.when = append(c.when, expr.CaseBranch{Cond: cond.node, Then: then.selectItem().Node})
	return c
}

// Else ends the CASE with a fallback, so that every row has a value.
//
// The result keeps the branch type: with an ELSE there is no row for which the
// CASE produces nothing, so a CASE over non-nullable branches is non-nullable.
func (c *CaseBuilder[T, N]) Else(v Selectable[Composed, T]) Expression[T, N] {
	node := expr.Node(expr.Case{When: c.when, Else: v.selectItem().Node})
	if c.err != nil {
		node = expr.Fail{Err: c.err}
	}
	return Expression[T, N]{node: node, nullSafe: nullSafeAs[T, N]()}
}

// End ends the CASE without a fallback.
//
// The result is nullable whatever the branches produce, because PostgreSQL's
// answer for a row that matches no branch is NULL. That is not a conservative
// choice — it is the only correct one, and it is why the two endings have
// different result types rather than a flag.
func (c *CaseBuilder[T, N]) End() Expression[N, N] {
	node := expr.Node(expr.Case{When: c.when})
	if c.err != nil {
		node = expr.Fail{Err: c.err}
	}
	return Expression[N, N]{node: node, nullSafe: true}
}

// Coalesce returns the first of the expressions that is not NULL, falling back
// to one that cannot be.
//
// The fallback comes last in the signature and last in the SQL. Because it
// cannot be NULL, neither can the result — which is the one case where a
// non-nullable claim about COALESCE is provable rather than hoped for.
//
//	orm.Coalesce(orm.Val(""), orm.Of(Users.Nickname))   // Expression[string, *string]
func Coalesce[E, V any](fallback Typed[E, V, *V], vs ...Selectable[Composed, *V]) Expression[V, *V] {
	args := make([]expr.Node, 0, len(vs)+1)
	for _, v := range vs {
		args = append(args, v.selectItem().Node)
	}
	args = append(args, fallback.selectItem().Node)
	return Expression[V, *V]{node: expr.Call{Func: "coalesce", Args: args}}
}

// CoalesceNull returns the first of the expressions that is not NULL.
//
// Every argument may be NULL, so the result may be too: there is no fallback to
// prove otherwise.
func CoalesceNull[V any](first Selectable[Composed, *V], rest ...Selectable[Composed, *V]) Expression[*V, *V] {
	args := make([]expr.Node, 0, len(rest)+1)
	args = append(args, first.selectItem().Node)
	for _, v := range rest {
		args = append(args, v.selectItem().Node)
	}
	return Expression[*V, *V]{node: expr.Call{Func: "coalesce", Args: args}, nullSafe: true}
}

// NullIf returns the first expression, or NULL when the two are equal.
//
// The result is always nullable, whatever the arguments are: producing NULL for
// some rows is the entire purpose of the function, so a non-nullable NULLIF
// would be a contradiction rather than an approximation.
func NullIf[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Expression[*V, *V] {
	return Expression[*V, *V]{
		node:     expr.Call{Func: "nullif", Args: []expr.Node{a.optItem().Node, b.optItem().Node}},
		nullSafe: true,
	}
}

// PGType names a PostgreSQL type and the Go type that reads it back.
//
// It exists so that a cast cannot claim an arbitrary Go result type. The pairing
// is stated once, here, rather than at each call site where it could be stated
// differently — and [PGTypeOf] is how a project adds one for a type this
// package does not know, which is the same trust boundary [RawValue] has.
type PGType[V any] struct {
	name string
}

// Name returns the PostgreSQL type name.
func (t PGType[V]) Name() string { return t.name }

// PGTypeOf pairs a PostgreSQL type name with the Go type that reads it back.
//
//	var Numeric = orm.PGTypeOf[pgtype.Numeric]("numeric")
//
// Nothing checks the pairing — this package cannot know what a project's types
// decode into — so it is a trust boundary, and a wrong pairing fails as a scan
// error rather than as a wrong value. Prefer the descriptors below.
func PGTypeOf[V any](name string) PGType[V] { return PGType[V]{name: name} }

// The PostgreSQL types whose Go mapping this package can state itself. A type
// not here is spelled with [PGTypeOf].
var (
	Text            = PGType[string]{"text"}
	BigInt          = PGType[int64]{"int8"}
	Integer         = PGType[int32]{"int4"}
	SmallInt        = PGType[int16]{"int2"}
	Boolean         = PGType[bool]{"bool"}
	Real            = PGType[float32]{"float4"}
	DoublePrecision = PGType[float64]{"float8"}
	Timestamptz     = PGType[time.Time]{"timestamptz"}
	Timestamp       = PGType[time.Time]{"timestamp"}
	Date            = PGType[time.Time]{"date"}
	ByteA           = PGType[[]byte]{"bytea"}
)

// Cast converts an expression to another PostgreSQL type.
//
// The target decides the Go result type, so a cast cannot be used to relabel an
// expression as whatever the caller wanted it to be. A cast of a non-nullable
// expression is non-nullable: the conversion can fail, and then the statement
// fails, but a successful cast of a value is a value.
func Cast[E, T, V any](v Selectable[E, T], to PGType[V]) Expression[V, *V] {
	return Expression[V, *V]{node: expr.Cast{X: v.selectItem().Node, Type: to.name}}
}

// CastNull converts a nullable expression, which stays nullable: casting NULL
// produces NULL, whatever the target type is.
func CastNull[E, W, V any](v Selectable[E, *W], to PGType[V]) Expression[*V, *V] {
	return Expression[*V, *V]{node: expr.Cast{X: v.selectItem().Node, Type: to.name}, nullSafe: true}
}

// String functions.
//
// The set is deliberately small: the ones a query builder needs to express a
// case-insensitive comparison or a length restriction without reaching for raw
// SQL. PostgreSQL has hundreds more, and [RawValue] is how to reach them.

// Lower folds a string to lower case.
func Lower[E any](v Selectable[E, string]) Expression[string, *string] {
	return call1[string](v, "lower")
}

// LowerNull folds a nullable string, which stays nullable.
func LowerNull[E any](v Selectable[E, *string]) Expression[*string, *string] {
	return callNull1[string](v, "lower")
}

// Upper folds a string to upper case.
func Upper[E any](v Selectable[E, string]) Expression[string, *string] {
	return call1[string](v, "upper")
}

// UpperNull folds a nullable string, which stays nullable.
func UpperNull[E any](v Selectable[E, *string]) Expression[*string, *string] {
	return callNull1[string](v, "upper")
}

// Trim removes leading and trailing whitespace.
func Trim[E any](v Selectable[E, string]) Expression[string, *string] {
	return call1[string](v, "btrim")
}

// TrimNull removes leading and trailing whitespace from a nullable string.
func TrimNull[E any](v Selectable[E, *string]) Expression[*string, *string] {
	return callNull1[string](v, "btrim")
}

// Length counts the characters in a string. PostgreSQL's length returns
// integer, so this reads back as int32 rather than as Go's int.
func Length[E any](v Selectable[E, string]) Expression[int32, *int32] {
	return Expression[int32, *int32]{node: expr.Call{Func: "length", Args: []expr.Node{v.selectItem().Node}}}
}

// LengthNull counts the characters in a nullable string.
func LengthNull[E any](v Selectable[E, *string]) Expression[*int32, *int32] {
	return Expression[*int32, *int32]{
		node:     expr.Call{Func: "length", Args: []expr.Node{v.selectItem().Node}},
		nullSafe: true,
	}
}

// Concat joins strings.
//
// PostgreSQL's concat is not the || operator: it ignores NULL arguments rather
// than propagating them, and over no arguments at all it is the empty string.
// So the result cannot be NULL, and this takes non-nullable arguments because
// mixing the two in one variadic list is not something Go can express — pass a
// nullable value through [Coalesce] first.
func Concat(vs ...Selectable[Composed, string]) Expression[string, *string] {
	args := make([]expr.Node, 0, len(vs))
	for _, v := range vs {
		args = append(args, v.selectItem().Node)
	}
	return Expression[string, *string]{node: expr.Call{Func: "concat", Args: args}}
}

func call1[V any, E any](v Selectable[E, V], fn string) Expression[V, *V] {
	return Expression[V, *V]{node: expr.Call{Func: fn, Args: []expr.Node{v.selectItem().Node}}}
}

func callNull1[V any, E any](v Selectable[E, *V], fn string) Expression[*V, *V] {
	return Expression[*V, *V]{
		node:     expr.Call{Func: fn, Args: []expr.Node{v.selectItem().Node}},
		nullSafe: true,
	}
}

// Date and time.

// DateField names a field of a date or timestamp.
//
// It is a closed set because the name becomes syntax in an extract, where
// PostgreSQL's grammar takes a keyword rather than a value.
type DateField string

// The fields extract and DateTrunc accept.
const (
	Year        DateField = "year"
	Quarter     DateField = "quarter"
	Month       DateField = "month"
	Week        DateField = "week"
	Day         DateField = "day"
	Hour        DateField = "hour"
	Minute      DateField = "minute"
	Second      DateField = "second"
	DayOfWeek   DateField = "dow"
	DayOfYear   DateField = "doy"
	EpochSecond DateField = "epoch"
)

// Now is PostgreSQL's now(), the transaction's start time as a timestamptz.
//
// It is the transaction's rather than the statement's, which is the PostgreSQL
// behaviour worth knowing: two calls in one transaction agree, and neither is
// the wall clock at the moment the row was written.
func Now() Expression[time.Time, *time.Time] {
	return Expression[time.Time, *time.Time]{node: expr.Call{Func: "now"}}
}

// DateTrunc rounds a timestamp down to a field.
//
// The field travels as a parameter rather than as text, because date_trunc
// takes it as an argument — which is the difference between it and [Extract],
// where the field is part of the grammar.
func DateTrunc[E any](field DateField, v Selectable[E, time.Time]) Expression[time.Time, *time.Time] {
	return Expression[time.Time, *time.Time]{node: dateTruncNode(field, v.selectItem().Node)}
}

// DateTruncNull rounds a nullable timestamp down, which stays nullable.
func DateTruncNull[E any](field DateField, v Selectable[E, *time.Time]) Expression[*time.Time, *time.Time] {
	return Expression[*time.Time, *time.Time]{node: dateTruncNode(field, v.selectItem().Node), nullSafe: true}
}

func dateTruncNode(field DateField, x expr.Node) expr.Node {
	return expr.Call{Func: "date_trunc", Args: []expr.Node{expr.Arg{Value: string(field)}, x}}
}

// Extract pulls a field out of a timestamp, converted to a type you name.
//
// The conversion is written into the SQL rather than assumed. PostgreSQL's
// extract returns numeric, whose Go mapping is a project's own decision, so
// claiming one here would be claiming something this package cannot know — and
// a cast makes the claim true instead of merely stated:
//
//	orm.Extract(orm.Year, orm.Of(Users.CreatedAt), orm.Integer)   // int32
func Extract[E, V any](field DateField, v Selectable[E, time.Time], as PGType[V]) Expression[V, *V] {
	return Expression[V, *V]{node: expr.Cast{
		X:    expr.Extract{Field: string(field), X: v.selectItem().Node},
		Type: as.name,
	}}
}

// ExtractNull pulls a field out of a nullable timestamp, which stays nullable.
func ExtractNull[E, V any](field DateField, v Selectable[E, *time.Time], as PGType[V]) Expression[*V, *V] {
	return Expression[*V, *V]{
		node: expr.Cast{
			X:    expr.Extract{Field: string(field), X: v.selectItem().Node},
			Type: as.name,
		},
		nullSafe: true,
	}
}

// Row comparisons.
//
// PostgreSQL compares tuples element by element, which is what makes a
// composite key one comparison rather than a conjunction of several — and what
// an index on those columns is built to answer.

// Pair is two values compared as one tuple.
type Pair[V, W any] struct {
	// First and Second are the two halves of the tuple, in the order they are
	// written into SQL. Which column each corresponds to is decided by whatever
	// takes the pair.
	First  V
	Second W
}

// Both builds a [Pair], so that a list of them reads as a list of tuples.
func Both[V, W any](first V, second W) Pair[V, W] { return Pair[V, W]{First: first, Second: second} }

// Row2Eq compares two expressions to two values as one tuple.
//
//	orm.Row2Eq(Branches.Region, Branches.Code, "eu", "b1")
//
// It is not the same statement as two equalities joined by AND when a NULL is
// involved, and it is the shape a composite-key index answers directly.
func Row2Eq[A, B, V, W any](a Optional[A, *V], b Optional[B, *W], first V, second W) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Binary{
		Op:    expr.OpEq,
		Left:  expr.RowValue{Items: []expr.Node{a.optItem().Node, b.optItem().Node}},
		Right: expr.RowValue{Items: []expr.Node{expr.Arg{Value: first}, expr.Arg{Value: second}}},
	}}
}

// Row2In tests two expressions against a list of tuples.
//
//	orm.Row2In(Branches.Region, Branches.Code,
//	    orm.Both("eu", "b1"), orm.Both("us", "b2"))
//
// Over no tuples it is FALSE, for the same reason [Col.In] over no values is:
// PostgreSQL has no syntax for an empty list, and nothing is what the caller
// asked to match.
func Row2In[A, B, V, W any](a Optional[A, *V], b Optional[B, *W], pairs ...Pair[V, W]) Predicate[Composed] {
	values := make([]expr.Node, 0, len(pairs))
	for _, p := range pairs {
		values = append(values, expr.RowValue{Items: []expr.Node{
			expr.Arg{Value: p.First}, expr.Arg{Value: p.Second},
		}})
	}
	return Predicate[Composed]{node: expr.In{
		X:      expr.RowValue{Items: []expr.Node{a.optItem().Node, b.optItem().Node}},
		Values: values,
	}}
}

// Array expressions.
//
// Only the three operators a mapped array column can answer without a type DSL
// behind them. The element type ties the two sides together, so comparing a
// text[] to an int8[] does not compile.

// AnyOf builds value = ANY(array), which is how a scalar is tested for
// membership of an array column.
func AnyOf[E, A, V any](v Optional[E, *V], array Optional[A, *[]V]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Quantified{
		Op: expr.OpEq, Left: v.optItem().Node, Right: array.optItem().Node,
	}}
}

// AllOf builds value = ALL(array), which holds when every element equals it.
func AllOf[E, A, V any](v Optional[E, *V], array Optional[A, *[]V]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Quantified{
		Op: expr.OpEq, All: true, Left: v.optItem().Node, Right: array.optItem().Node,
	}}
}

// ArrayContains builds a @> b: every element of b is in a.
func ArrayContains[A, B, V any](a Optional[A, *[]V], b Optional[B, *[]V]) Predicate[Composed] {
	return arrayOp("@>", a, b)
}

// ArrayContainedBy builds a <@ b: every element of a is in b.
func ArrayContainedBy[A, B, V any](a Optional[A, *[]V], b Optional[B, *[]V]) Predicate[Composed] {
	return arrayOp("<@", a, b)
}

// ArrayOverlaps builds a && b: the two share at least one element.
func ArrayOverlaps[A, B, V any](a Optional[A, *[]V], b Optional[B, *[]V]) Predicate[Composed] {
	return arrayOp("&&", a, b)
}

func arrayOp[A, B, V any](op string, a Optional[A, *[]V], b Optional[B, *[]V]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{Op: op, Left: a.optItem().Node, Right: b.optItem().Node}}
}

// JSON expressions.
//
// Only the operators whose result type and nullability can be stated exactly.
// The -> operator returns jsonb, whose Go mapping is a project's own decision
// and whose nullability depends on the document, so it is left out rather than
// guessed at; ->> returns text and is NULL when the key is absent, which is a
// type this package can promise.
//
// The document argument is any expression, because which Go type a jsonb column
// maps to is the project's. Handing one of these something that is not jsonb is
// an error PostgreSQL reports precisely, naming the operator and the types.

// JSONText reads a top-level key as text, which is NULL when the key is absent.
func JSONText[E, J any](doc Optional[E, *J], key string) Expression[*string, *string] {
	return Expression[*string, *string]{
		node:     expr.Infix{Op: "->>", Left: doc.optItem().Node, Right: expr.Arg{Value: key}},
		nullSafe: true,
	}
}

// JSONHasKey builds doc ? key, which is TRUE when the document has that key at
// the top level.
func JSONHasKey[E, J any](doc Optional[E, *J], key string) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "?", Left: doc.optItem().Node, Right: expr.Arg{Value: key},
	}}
}

// JSONContains builds a @> b, which is TRUE when a contains b as a subdocument.
func JSONContains[A, B, J any](a Optional[A, *J], b Optional[B, *J]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Infix{
		Op: "@>", Left: a.optItem().Node, Right: b.optItem().Node,
	}}
}
