package orm

import (
	"github.com/AlexAli29/orm/internal/expr"
)

// Composition.
//
// Up to M10 every typed expression carried the entity whose sources it may
// name, and that tag did work a scope check could not: a Predicate[User] cannot
// reach a query over Post, and the mistake is a compile error rather than a
// statement PostgreSQL refuses.
//
// A composed query breaks the premise, not the guarantee. A join, a derived
// table and a CTE all put rows from several places into one statement, so there
// is no single entity for the expressions to belong to. What replaces the tag
// is what was always underneath it: source identity. Every expression depends
// on the occurrences it was built from, the compiler knows which occurrences a
// statement introduces and in what order, and a reference to one that is not in
// scope is refused before PostgreSQL sees it.
//
// So composed expressions carry [Composed] where an entity query's carry the
// entity. That is not a hole in the type system; it is where checking moves
// from the Go type to the SQL scope, and it is why the scope is structural,
// sequential and never disabled.

// Composed is the tag composed expressions carry in place of an entity.
//
// It is a type rather than a convention so that the ordinary combinators keep
// working: And, Or, Not, the aggregates, GroupBy and every result shape are
// written over an entity parameter, and a composed query instantiates them at
// this type instead of at User or Post. None of them is duplicated.
type Composed struct{}

// Expression is a typed SQL expression in a composed query.
//
// It carries two Go types rather than one, and the second is the whole reason
// source-induced nullability can be modelled honestly:
//
//	T  what the expression reads back as
//	N  what it reads back as when the source it comes from may be absent
//
// For a column of a NOT NULL bigint those are int64 and *int64. For a nullable
// text column they are both *string, because a value that could already be NULL
// does not become more nullable by being read through an outer join. Go cannot
// compute the second from the first — "pointer to T unless T is already a
// pointer" is not something a constraint can say — so it is carried, and every
// constructor that produces an Expression fills it in from something that knows.
//
// That is what makes [Opt] idempotent, and it is what stops a LEFT JOIN from
// producing a type that says a column cannot be NULL when the join can make it
// so.
type Expression[T, N any] struct {
	node  expr.Node
	alias string
	// nullSafe records that T can hold a NULL, which is true exactly when T
	// and N are the same type. The compiler needs the fact and cannot work it
	// out — the tree has never known what a Go type is — so it is computed
	// here, once, where both types are in hand.
	nullSafe bool
}

func (e Expression[T, N]) selectItem() expr.SelectItem {
	return expr.SelectItem{Node: e.node, Alias: e.alias, Nullable: e.nullSafe}
}
func (e Expression[T, N]) selectTyped(Composed, T) {}

func (e Expression[T, N]) optItem() expr.SelectItem {
	return expr.SelectItem{Node: e.node, Alias: e.alias, Nullable: e.nullSafe}
}
func (e Expression[T, N]) optTyped(Composed, N) {}

// nullSafeAs reports whether T and N are one type, which is what makes an
// expression able to absorb a NULL its source introduces.
//
// It is a type assertion rather than reflection: boxing a *T and asking whether
// it is a *N is a question the compiler answers by identity, and it costs
// nothing at the point an expression is built.
func nullSafeAs[T, N any]() bool {
	var t T
	_, ok := any(&t).(*N)
	return ok
}

func (e Expression[T, N]) groupNode() expr.Node { return e.node }
func (e Expression[T, N]) groupTyped(Composed)  {}

// As names the expression in the result. The receiver is untouched.
func (e Expression[T, N]) As(alias string) Expression[T, N] {
	out := e
	out.alias = alias
	return out
}

// Asc orders by the expression ascending.
func (e Expression[T, N]) Asc() Order[Composed] {
	return Order[Composed]{order: expr.Order{Expr: e.node}}
}

// Desc orders by the expression descending.
func (e Expression[T, N]) Desc() Order[Composed] {
	return Order[Composed]{order: expr.Order{Expr: e.node, Desc: true}}
}

// Comparisons against a value.
//
// The value has the expression's own result type, which for a nullable
// expression is a pointer. That is not an accident of the signature: comparing
// a possibly-NULL expression to a possibly-NULL value is a three-valued
// comparison, and spelling the argument *V is the type system saying so.

// Eq builds expression = value.
func (e Expression[T, N]) Eq(v T) Predicate[Composed] { return e.cmp(expr.OpEq, v) }

// Ne builds expression <> value.
func (e Expression[T, N]) Ne(v T) Predicate[Composed] { return e.cmp(expr.OpNe, v) }

// Gt builds expression > value.
func (e Expression[T, N]) Gt(v T) Predicate[Composed] { return e.cmp(expr.OpGt, v) }

// Gte builds expression >= value.
func (e Expression[T, N]) Gte(v T) Predicate[Composed] { return e.cmp(expr.OpGte, v) }

// Lt builds expression < value.
func (e Expression[T, N]) Lt(v T) Predicate[Composed] { return e.cmp(expr.OpLt, v) }

// Lte builds expression <= value.
func (e Expression[T, N]) Lte(v T) Predicate[Composed] { return e.cmp(expr.OpLte, v) }

// In builds expression IN (values...). Over no values it is FALSE, which is
// what a caller who passed an empty slice meant and what PostgreSQL has no
// syntax for.
func (e Expression[T, N]) In(vs ...T) Predicate[Composed] {
	nodes := make([]expr.Node, 0, len(vs))
	for _, v := range vs {
		nodes = append(nodes, expr.Arg{Value: v})
	}
	return Predicate[Composed]{node: expr.In{X: e.node, Values: nodes}}
}

// Between builds expression BETWEEN lo AND hi, inclusive at both ends.
func (e Expression[T, N]) Between(lo, hi T) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Between{
		X: e.node, Lo: expr.Arg{Value: lo}, Hi: expr.Arg{Value: hi},
	}}
}

// IsNull builds expression IS NULL.
func (e Expression[T, N]) IsNull() Predicate[Composed] {
	return Predicate[Composed]{node: expr.Unary{Op: expr.OpIsNull, X: e.node}}
}

// IsNotNull builds expression IS NOT NULL.
func (e Expression[T, N]) IsNotNull() Predicate[Composed] {
	return Predicate[Composed]{node: expr.Unary{Op: expr.OpIsNotNull, X: e.node}}
}

func (e Expression[T, N]) cmp(op expr.Op, v T) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Binary{Op: op, Left: e.node, Right: expr.Arg{Value: v}}}
}

// Arithmetic.
//
// The result keeps both of the expression's types, which is how source-induced
// nullability survives being computed with. A column read through a LEFT JOIN
// is nullable; adding one to it does not make it less so, because SQL
// arithmetic propagates NULL, and the type says the same thing.

// Add builds expression + value.
func (e Expression[T, N]) Add(v T) Expression[T, N] { return e.arith(expr.OpAdd, expr.Arg{Value: v}) }

// Sub builds expression - value.
func (e Expression[T, N]) Sub(v T) Expression[T, N] { return e.arith(expr.OpSub, expr.Arg{Value: v}) }

// Mul builds expression * value.
func (e Expression[T, N]) Mul(v T) Expression[T, N] { return e.arith(expr.OpMul, expr.Arg{Value: v}) }

// Div builds expression / value.
func (e Expression[T, N]) Div(v T) Expression[T, N] { return e.arith(expr.OpDiv, expr.Arg{Value: v}) }

// AddOf builds expression + other.
func (e Expression[T, N]) AddOf(other Selectable[Composed, T]) Expression[T, N] {
	return e.arith(expr.OpAdd, other.selectItem().Node)
}

// SubOf builds expression - other.
func (e Expression[T, N]) SubOf(other Selectable[Composed, T]) Expression[T, N] {
	return e.arith(expr.OpSub, other.selectItem().Node)
}

// The result keeps nullSafe, which is what carries source-induced nullability
// through a computation: NULL + 1 is NULL, and the type still says so.
func (e Expression[T, N]) arith(op expr.ArithOp, right expr.Node) Expression[T, N] {
	out := e
	out.node = expr.Arith{Op: op, Left: e.node, Right: right}
	out.alias = ""
	return out
}

// Optional is an expression that knows the Go type it takes when the query
// context can make it NULL.
//
// It exists because "the nullable form of T" is not something Go can compute. A
// column of int64 reads back as *int64 when its source may be absent, and a
// column that was already *string reads back as *string still — the second is
// idempotent and the first is not, and no constraint over T can tell them
// apart. So each expression states its own answer, and the descriptor lattice
// makes every one of them right: a nullable column's nullable form is itself,
// because NullCol inherits the answer Col gives for its value type.
//
// The interface is closed by unexported methods, and N appears in a method
// signature so that Optional[User, *int64] is a different interface from
// Optional[User, *string] rather than the same one twice — which is also what
// lets Go infer N from the argument.
type Optional[E, N any] interface {
	optItem() expr.SelectItem
	optTyped(E, N)
}

// Typed is an expression that reports both its result type and its nullable
// form, which is every expression this package builds.
//
// It is an argument type rather than something to implement: naming both
// interfaces in one place is what lets [Of], [Opt] and [Named] infer T and N
// from a single argument instead of making the caller write them out.
type Typed[E, T, N any] interface {
	Selectable[E, T]
	Optional[E, N]
}

// Of lifts an expression into a composed query.
//
// The result type is unchanged, which is the whole contract: a nullable column
// stays nullable, a non-nullable one stays non-nullable, and the expression
// still depends on exactly the source it was built from. What is dropped is the
// entity tag, and only because a composed query has no single entity to check
// against — the source it names is checked instead, and more strictly.
//
//	orm.Of(Users.Email)   // Expression[string, *string]
//	orm.Of(Users.Bio)     // Expression[*string, *string]
//
// Use [Opt] instead for something read through an outer join, where the source
// itself may be absent.
func Of[E, T, N any](v Typed[E, T, N]) Expression[T, N] {
	it := v.selectItem()
	return Expression[T, N]{node: it.Node, alias: it.Alias, nullSafe: nullSafeAs[T, N]()}
}

// OfNull lifts an expression that is already nullable and is not a column.
//
// An aggregate over an empty group is NULL and a scalar subquery over no rows
// is NULL, so both are already spelled *V — but neither is a column, so neither
// can say that its nullable form is itself. This says it for them. [Of] would
// type the outer-joined form as **V, which is a compile error at the
// destination rather than a wrong value, but this is what was meant.
func OfNull[E, V any](v Selectable[E, *V]) Expression[*V, *V] {
	it := v.selectItem()
	return Expression[*V, *V]{node: it.Node, alias: it.Alias, nullSafe: true}
}

// Opt lifts an expression into a composed query as its nullable form.
//
// This is what an outer join needs. A LEFT JOIN can produce a row in which the
// whole right-hand source is absent, and every value read through it is then
// NULL — whatever the column's own constraint says. So the type widens:
//
//	orm.Opt(Profiles.ID)   // OrdCol[Profile, int64]  -> Expression[*int64, *int64]
//	orm.Opt(Profiles.Bio)  // NullTextCol[Profile]    -> Expression[*string, *string]
//
// The second is idempotent, because a nullable column's nullable form is
// itself. Nothing about the original descriptor changes: Profiles.ID still
// means a non-nullable column everywhere else, and this builds a new expression
// rather than editing that one.
func Opt[E, T, N any](v Typed[E, T, N]) Expression[N, N] {
	it := v.optItem()
	return Expression[N, N]{node: it.Node, alias: it.Alias, nullSafe: true}
}

// Cond lifts a predicate into a composed query.
//
// Every typed comparison an entity's descriptors produce — Eq, ILike, Between,
// In, IsNull, an aggregate's Gt for a HAVING clause — builds a Predicate over
// that entity, and all of them are wanted in composed queries too. Rather than
// a second lattice of composed comparisons, there is this: the predicate keeps
// its tree, and the sources it names are checked against the composed
// statement's scope like everything else.
//
//	q.Where(orm.Cond(Users.Active.Eq(true)))
func Cond[E any](p Predicate[E]) Predicate[Composed] {
	return Predicate[Composed]{node: p.node, err: p.err}
}

// Val is a literal value as a composed expression.
//
// It is a parameter, not text: the value joins the statement's argument list
// like every other value this package sends. That is the safe default and it
// has one consequence worth knowing — a bare parameter carries no PostgreSQL
// type, so where the server cannot infer one from context it refuses the
// statement rather than guessing. The usual context is enough: a comparison
// against a column, an argument of a function with one signature, a branch of a
// CASE beside a typed one. Where it is not — an argument of an overloaded
// function such as lower, or a select-list item standing alone — say which type
// it is:
//
//	orm.Cast(orm.Val("ABC"), orm.Text)
func Val[V any](v V) Expression[V, *V] {
	return Expression[V, *V]{node: expr.Arg{Value: v}}
}

// Null is the SQL NULL literal, typed as a nullable V.
//
// It is a node rather than a nil parameter because assigning NULL and binding a
// nil value are different statements to write, and because NULL is the one
// value that never needs to be sent.
func Null[V any]() Expression[*V, *V] {
	return Expression[*V, *V]{node: expr.Null{}, nullSafe: true}
}

// Comparisons between two expressions.
//
// These compare by value type rather than by result type, which is what lets a
// nullable column be compared to a non-nullable one — the ordinary shape of a
// join condition, since a foreign key is usually nullable and the key it points
// at is not. Both sides are taken as their nullable form, so int64 and *int64
// meet at *int64 and the comparison is the one SQL would perform anyway.
//
//	orm.Eq(Posts.AuthorID, Users.ID)     // *int64 against int64
//	orm.Eq(orm.Ref(stats, userID), Users.ID)

// Eq builds a = b.
func Eq[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return compare(expr.OpEq, a, b)
}

// Ne builds a <> b.
func Ne[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return compare(expr.OpNe, a, b)
}

// Gt builds a > b.
func Gt[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return compare(expr.OpGt, a, b)
}

// Gte builds a >= b.
func Gte[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return compare(expr.OpGte, a, b)
}

// Lt builds a < b.
func Lt[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return compare(expr.OpLt, a, b)
}

// Lte builds a <= b.
func Lte[A, B, V any](a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return compare(expr.OpLte, a, b)
}

func compare[A, B, V any](op expr.Op, a Optional[A, *V], b Optional[B, *V]) Predicate[Composed] {
	return Predicate[Composed]{node: expr.Binary{
		Op: op, Left: a.optItem().Node, Right: b.optItem().Node,
	}}
}

// The Optional implementations on the descriptor lattice.
//
// A column's nullable form is a pointer to its value type, and the nullable
// descriptors inherit that answer from Col rather than restating it: NullCol's
// value type is already *V, and *V is its own nullable form. That inheritance
// is the reason Opt is idempotent without anything checking whether it needs
// to be.

func (c Col[E, V]) optItem() expr.SelectItem {
	return expr.SelectItem{Node: c.col, Nullable: true}
}
func (c Col[E, V]) optTyped(E, *V) {}

func (v Value[E, T]) optItem() expr.SelectItem {
	return expr.SelectItem{Node: v.node, Alias: v.alias, Nullable: true}
}
func (v Value[E, T]) optTyped(E, *T) {}

func (a Agg[E, T]) optItem() expr.SelectItem {
	return expr.SelectItem{Node: a.agg, Alias: a.alias, Nullable: true}
}
func (a Agg[E, T]) optTyped(E, *T) {}

// Out is a declared output column of a row source.
//
// It is the handle a derived table or a CTE hands back, and it carries the same
// two types an [Expression] does: what the column reads back as, and what it
// reads back as when the source it belongs to can be absent. Both come from the
// expression the output was declared from, so neither is a claim anybody had to
// make — which is what keeps a derived column from being addressed by a string
// and typed by hope.
type Out[T, N any] struct {
	name string
	item expr.SelectItem
}

// Name returns the SQL name of the output column.
func (o Out[T, N]) Name() string { return o.name }

// Asc orders a set operation's result by this output column, ascending.
//
//	thingID := orm.Named("thing_id", orm.Of(Users.ID))
//	orm.UnionAll(fromUsers, fromArchive).OrderBy(thingID.Asc())
//
// It orders a set operation and nothing else. A plain query orders by an
// expression — [Expression.Asc] and the columns' own Asc — because PostgreSQL
// lets it; a compound may only name one of its own output columns, so a term for
// one is a name and a direction and cannot be anything else.
//
// The declaration is the same value [Ref] takes, so a union that is both ordered
// and used as a source names its columns once.
func (o Out[T, N]) Asc() OutputOrder { return OutputOrder{name: o.name} }

// Desc orders a set operation's result by this output column, descending.
func (o Out[T, N]) Desc() OutputOrder { return OutputOrder{name: o.name, desc: true} }

// OutputOrder is one ORDER BY term of a set operation, produced by an output
// declaration's Asc or Desc.
//
// It is a separate type from [Order] because the two order different things.
// An Order carries an expression and is checked against the sources a statement
// introduces; this carries the name of a column of the result, which belongs to
// no source. Giving [UnionQuery.OrderBy] an Order would let a caller write a
// term PostgreSQL refuses outright, which is the thing this type exists to make
// unwritable.
type OutputOrder struct {
	name string
	desc bool
}

// Name returns the output column the term orders by.
func (o OutputOrder) Name() string { return o.name }

func (o Out[T, N]) outName() string          { return o.name }
func (o Out[T, N]) outItem() expr.SelectItem { return o.item }

// Output is a declared output column, whatever it reads back as.
//
// It is what a row-producing query's select list is built from, and it is
// deliberately type-erased: the list has one type and the columns do not, which
// is the same reason [Projection] is written out per arity rather than being
// variadic. The types come back where they are used, through [Ref] and
// [OptRef], which know the declaration they were given.
type Output interface {
	outName() string
	outItem() expr.SelectItem
}

// Named declares an output column of a row source.
//
// The name is the SQL name, and it is required rather than derived. A physical
// column could lend its own, but count(*) has none, and inventing one from the
// rendered expression would make the column of a derived table depend on how
// the compiler happened to spell it — a name that changes when the compiler
// improves is not a name.
//
//	userID := orm.Named("user_id", orm.Of(Posts.AuthorID))
//	posts  := orm.Named("post_count", orm.Of(orm.Count[orm.Composed]()))
//
// Both the result type and its nullable form are inferred from the expression,
// so a derived column can never claim a type the expression it selects does not
// produce. Whether the expression can be NULL is recorded either way, which is
// what a recursive CTE's convergence check reads.
//
// For an expression that is already nullable and is not a column — an aggregate
// like Min, or a scalar subquery — prefer [NamedNull]. Named gives such an
// output the right result type but the wrong nullable form, so reading it back
// through an outer join with [OptRef] produces a type nothing can bind.
func Named[E, T, N any](name string, v Typed[E, T, N]) Out[T, N] {
	it := v.selectItem()
	it.Alias = name
	return Out[T, N]{name: name, item: it}
}

// NamedNull declares an output column whose expression is already nullable.
//
// It is [Named] for the case where the nullable form cannot be computed. An
// aggregate over an empty group is NULL and a scalar subquery over no rows is
// NULL; both are already spelled *V, and declaring one with Named would type
// its outer-joined form as **V.
//
//	latest := orm.NamedNull("latest_post", orm.Max(Posts.CreatedAt))
func NamedNull[E, V any](name string, v Selectable[E, *V]) Out[*V, *V] {
	it := v.selectItem()
	it.Alias = name
	return Out[*V, *V]{name: name, item: it}
}

// Ref reads a column of a row source.
//
// The declaration decides the type and the source decides which occurrence it
// is read from, so one declaration read through two aliases of a CTE produces
// two expressions that qualify differently and scan the same.
//
//	orm.Ref(stats, postCount)   // Expression[int64, *int64]
//
// A declaration the source does not provide is refused when the statement
// compiles, naming the columns it does have.
func Ref[T, N any](src *Source, o Out[T, N]) Expression[T, N] {
	return Expression[T, N]{node: expr.Column{Source: src, Name: o.name}, nullSafe: nullSafeAs[T, N]()}
}

// OptRef reads a column of a row source that may be absent.
//
// It is [Ref] for a source brought in by an outer join. The column's own
// nullability is not the question: a LEFT JOIN that matches nothing produces a
// row in which every column of the right-hand source is NULL, including the
// ones the inner query proved could not be.
//
//	stats := orm.Sub("post_stats", ...)   // post_count is count(*), never NULL
//	q.LeftJoin(stats, ...)
//	orm.OptRef(stats, postCount)          // Expression[*int64, *int64]
func OptRef[T, N any](src *Source, o Out[T, N]) Expression[N, N] {
	return Expression[N, N]{node: expr.Column{Source: src, Name: o.name}, nullSafe: true}
}
