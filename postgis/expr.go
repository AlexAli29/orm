package postgis

import (
	"fmt"

	"github.com/AlexAli29/orm"
)

// The typed spatial expression model.
//
// Everything here composes through the ORM's extension boundary, which means
// there is one query compiler and one scope check for spatial and non-spatial
// statements alike. A spatial predicate is an ordinary predicate; it goes into
// Where next to Users.Active.Eq(true), is rendered by the same writer, and its
// values are bind parameters like every other value.
//
// What this layer adds is the part PostgreSQL cannot check until it runs:
//
//	geometry is not geography, and mixing them is a compile error rather than
//	an answer in the wrong units
//
//	a geometry in SRID 3857 does not belong in a predicate against a column in
//	4326, and saying so at build time beats "Operation on mixed SRID geometries"
//	from the server
//
//	a measurement over a nullable column is nullable, because ST_Distance of a
//	NULL is NULL and not zero
//
// The SRID check is only as good as what the column declares. A column typed
// geometry(Point,4326) states its coordinate system and is checked; a column
// typed plain geometry states nothing, and nothing is checked — the server
// remains the authority either way.

// GeomExpr is a geometry-valued expression over entity E.
//
// It is what every geometry column, constructor and transformation produces, so
// operations chain: buffer a column, take the centroid of the result, ask
// whether that intersects something. The entity tag rides along, which is what
// keeps a predicate built from one table's columns out of another table's
// query.
//
// It carries what it knows about itself. The SRID and the shape come from the
// column's declared type or from the constructor that produced it, and they are
// what the build-time checks read; both are permitted to be unknown, and an
// unknown one is not checked rather than assumed.
type GeomExpr[E any] struct {
	arg  orm.Arg
	srid int32
	kind Kind
	// dim is the dimensionality the expression is known to produce, which a
	// column states in its type modifier and a constructor states by which
	// function it is.
	dim Dim
	// nullable records that reading this can produce SQL NULL — because the
	// column allows it, or because an argument could.
	nullable bool
}

// GeogExpr is a geography-valued expression over entity E.
//
// It is a separate type from [GeomExpr] and not a flag on it, so that handing a
// geography to a function that measures in the plane does not compile. The
// mistake it prevents is the expensive one: ST_Distance over geometry(4326)
// returns degrees, which is a number, which looks like an answer.
type GeogExpr[E any] struct {
	e GeomExpr[E]
}

// DeclaredSRID reports the coordinate system the expression's result is in, or
// [UnknownSRID] when the expression does not say.
//
// It is what the column's declared type or the constructor said, known while
// the query is being built — where [GeomExpr.SRID] asks the server what the
// stored geometry is actually labelled with, and is an expression rather than a
// number. A plain geometry column has no declared SRID and holds geometries
// that each have one.
func (g GeomExpr[E]) DeclaredSRID() int32 { return g.srid }

// DeclaredKind reports the shape the expression is known to produce, or zero
// when it may produce any.
func (g GeomExpr[E]) DeclaredKind() Kind { return g.kind }

// DeclaredDim reports the dimensionality the expression is known to produce.
func (g GeomExpr[E]) DeclaredDim() Dim { return g.dim }

// Nullable reports whether reading the expression can produce SQL NULL.
func (g GeomExpr[E]) Nullable() bool { return g.nullable }

// DeclaredSRID reports the coordinate system the expression's result is in,
// which for a geography is almost always 4326.
func (g GeogExpr[E]) DeclaredSRID() int32 { return g.e.srid }

// Nullable reports whether reading the expression can produce SQL NULL.
func (g GeogExpr[E]) Nullable() bool { return g.e.nullable }

// Spatial column descriptors.
//
// Each embeds the ORM's own column descriptor, so a spatial column is selected,
// grouped, ordered and compared for equality by the machinery every other
// column uses — and reads back through the codec in this package. What the
// spatial type adds is the operations PostGIS defines and the type information
// PostgreSQL keeps in the column's type modifier.

// GeomCol is a NOT NULL geometry column of entity E.
type GeomCol[E any] struct {
	orm.Col[E, Geometry]
	srid int32
	kind Kind
	dim  Dim
}

// NullGeomCol is a nullable geometry column of entity E.
//
// It selects as *Geometry, because NULL and an empty geometry are different
// answers and only a pointer can tell them apart.
type NullGeomCol[E any] struct {
	orm.NullCol[E, Geometry]
	srid int32
	kind Kind
	dim  Dim
}

// GeogCol is a NOT NULL geography column of entity E.
type GeogCol[E any] struct {
	orm.Col[E, Geography]
	srid int32
	kind Kind
	dim  Dim
}

// NullGeogCol is a nullable geography column of entity E.
type NullGeogCol[E any] struct {
	orm.NullCol[E, Geography]
	srid int32
	kind Kind
	dim  Dim
}

// NewGeomCol returns a geometry column descriptor. Generated code calls it.
//
// srid, kind and dim come from the column's declared type — geometry(Point,4326)
// gives all three, plain geometry gives none — and are what the build-time
// checks read. Passing [UnknownSRID] and a zero Kind means "the column does not
// say", which switches the corresponding check off rather than assuming a
// default.
func NewGeomCol[E any](src *orm.Source, name string, srid int32, kind Kind, dim Dim) GeomCol[E] {
	return GeomCol[E]{Col: orm.NewCol[E, Geometry](src, name), srid: srid, kind: kind, dim: dim}
}

// NewNullGeomCol returns a nullable geometry column descriptor.
func NewNullGeomCol[E any](src *orm.Source, name string, srid int32, kind Kind, dim Dim) NullGeomCol[E] {
	return NullGeomCol[E]{
		NullCol: orm.NewNullCol[E, Geometry](src, name), srid: srid, kind: kind, dim: dim,
	}
}

// NewGeogCol returns a geography column descriptor.
func NewGeogCol[E any](src *orm.Source, name string, srid int32, kind Kind, dim Dim) GeogCol[E] {
	return GeogCol[E]{Col: orm.NewCol[E, Geography](src, name), srid: srid, kind: kind, dim: dim}
}

// NewNullGeogCol returns a nullable geography column descriptor.
func NewNullGeogCol[E any](src *orm.Source, name string, srid int32, kind Kind, dim Dim) NullGeogCol[E] {
	return NullGeogCol[E]{
		NullCol: orm.NewNullCol[E, Geography](src, name), srid: srid, kind: kind, dim: dim,
	}
}

// SRID reports the coordinate system the column is declared in, or
// [UnknownSRID] when its type does not constrain one.
func (c GeomCol[E]) SRID() int32 { return c.srid }

// SRID reports the coordinate system the column is declared in, or [UnknownSRID] when its type does not constrain one.
func (c NullGeomCol[E]) SRID() int32 { return c.srid }

// SRID reports the coordinate system the column is declared in, or [UnknownSRID] when its type does not constrain one.
func (c GeogCol[E]) SRID() int32 { return c.srid }

// SRID reports the coordinate system the column is declared in, or [UnknownSRID] when its type does not constrain one.
func (c NullGeogCol[E]) SRID() int32 { return c.srid }

// Kind reports the shape the column's type constrains it to, or zero when its
// type accepts any.
func (c GeomCol[E]) Kind() Kind { return c.kind }

// Kind reports the shape the column's type constrains it to, or zero when its type accepts any.
func (c NullGeomCol[E]) Kind() Kind { return c.kind }

// Kind reports the shape the column's type constrains it to, or zero when its type accepts any.
func (c GeogCol[E]) Kind() Kind { return c.kind }

// Kind reports the shape the column's type constrains it to, or zero when its type accepts any.
func (c NullGeogCol[E]) Kind() Kind { return c.kind }

// Dim reports the dimensionality the column's type constrains it to.
func (c GeomCol[E]) Dim() Dim { return c.dim }

// Dim reports the dimensionality the column's type constrains it to.
func (c NullGeomCol[E]) Dim() Dim { return c.dim }

// Dim reports the dimensionality the column's type constrains it to.
func (c GeogCol[E]) Dim() Dim { return c.dim }

// Dim reports the dimensionality the column's type constrains it to.
func (c NullGeogCol[E]) Dim() Dim { return c.dim }

// Expr lifts the column into a spatial expression, which is what the operations
// in this package compose over.
//
// Every spatial method is on [GeomExpr] rather than repeated on each of the four
// descriptors, and this is the one step between them. It is a method rather than
// something implicit because the descriptors differ in exactly one way that
// matters — whether the column can be NULL — and this is where that fact is
// recorded.
func (c GeomCol[E]) Expr() GeomExpr[E] {
	return GeomExpr[E]{arg: orm.ArgOf[E, Geometry](c.Col), srid: c.srid, kind: c.kind, dim: c.dim}
}

// Expr lifts the column into a spatial expression, which is nullable because
// the column is.
func (c NullGeomCol[E]) Expr() GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOf[E, *Geometry](c.NullCol), srid: c.srid, kind: c.kind, dim: c.dim, nullable: true,
	}
}

// Expr lifts the column into a spatial expression on the spheroid.
func (c GeogCol[E]) Expr() GeogExpr[E] {
	return GeogExpr[E]{e: GeomExpr[E]{
		arg: orm.ArgOf[E, Geography](c.Col), srid: c.srid, kind: c.kind, dim: c.dim,
	}}
}

// Expr lifts the column into a spatial expression on the spheroid, which is
// nullable because the column is.
func (c NullGeogCol[E]) Expr() GeogExpr[E] {
	return GeogExpr[E]{e: GeomExpr[E]{
		arg:      orm.ArgOf[E, *Geography](c.NullCol),
		srid:     c.srid,
		kind:     c.kind,
		dim:      c.dim,
		nullable: true,
	}}
}

// Value lifts a geometry into an expression, as a bind parameter.
//
// The coordinates are never text. They cross as EWKB in the statement's
// parameter list, which is the same path every other value in this package
// takes and the reason there is no way to interpolate a coordinate into SQL.
//
// The cast is not decoration: PostGIS overloads almost every function on
// geometry and geography, and an uncast parameter leaves PostgreSQL resolving
// the overload with nothing to go on — it picks the deprecated text form and
// fails on bytes that were never text.
func Value[E any](g Geometry) GeomExpr[E] {
	return GeomExpr[E]{arg: orm.ArgCast(g, "geometry"), srid: g.SRID(), kind: g.Kind(), dim: g.Dim()}
}

// GeogValue lifts a geography into an expression, as a bind parameter.
func GeogValue[E any](g Geography) GeogExpr[E] {
	return GeogExpr[E]{e: GeomExpr[E]{
		arg: orm.ArgCast(g, "geography"), srid: g.SRID(), kind: g.Kind(), dim: g.Dim(),
	}}
}

// FromExpr lifts a geometry-valued expression this package did not build.
//
// A derived table or a CTE that projects a geometry hands back an ordinary
// [orm.Expression], because the ORM's own machinery has no reason to know about
// PostGIS. This is how such a column re-enters the spatial layer: the caller
// states what the projected geometry is, because the projection did not carry
// it — a CTE column's type is whatever the CTE selected, and no part of the
// query records that it happened to be a point in 4326.
//
// That makes it a trust boundary of the same kind [orm.RawValue] is: nothing
// checks the claim, and a wrong one makes the build-time SRID check wrong
// rather than the query wrong. Pass the zero [TypeMod] to claim nothing, which
// switches those checks off and leaves the server as the only authority.
//
//	loc := orm.Named("location", orm.Of(Places.Location))
//	near := orm.CTE("near", orm.Rows(loc).From(Places.Source()))
//	postgis.FromExpr(orm.Ref(near, loc), Places.Location.TypeMod())
func FromExpr[E any](v orm.Selectable[E, Geometry], mod TypeMod) GeomExpr[E] {
	return GeomExpr[E]{arg: orm.ArgOf(v), srid: mod.SRID, kind: mod.Kind, dim: mod.Dim}
}

// FromExprNull is [FromExpr] for a projected geometry that can be NULL.
func FromExprNull[E any](v orm.Optional[E, *Geometry], mod TypeMod) GeomExpr[E] {
	return GeomExpr[E]{
		arg: orm.ArgOpt(v), srid: mod.SRID, kind: mod.Kind, dim: mod.Dim, nullable: true,
	}
}

// GeogFromExpr is [FromExpr] for a geography.
func GeogFromExpr[E any](v orm.Selectable[E, Geography], mod TypeMod) GeogExpr[E] {
	return GeogExpr[E]{e: GeomExpr[E]{
		arg: orm.ArgOf(v), srid: mod.SRID, kind: mod.Kind, dim: mod.Dim,
	}}
}

// GeogFromExprNull is [GeogFromExpr] for a projected geography that can be NULL.
func GeogFromExprNull[E any](v orm.Optional[E, *Geography], mod TypeMod) GeogExpr[E] {
	return GeogExpr[E]{e: GeomExpr[E]{
		arg: orm.ArgOpt(v), srid: mod.SRID, kind: mod.Kind, dim: mod.Dim, nullable: true,
	}}
}

// TypeMod returns what the column's declared type says it holds, which is what
// [FromExpr] needs when that column is projected through a derived table.
func (c GeomCol[E]) TypeMod() TypeMod { return colTypeMod(FamilyGeometry, c.srid, c.kind, c.dim) }

// TypeMod returns what the column's declared type says it holds, which is what [FromExpr] needs when that column is projected through a derived table.
func (c NullGeomCol[E]) TypeMod() TypeMod { return colTypeMod(FamilyGeometry, c.srid, c.kind, c.dim) }

// TypeMod returns what the column's declared type says it holds, which is what [FromExpr] needs when that column is projected through a derived table.
func (c GeogCol[E]) TypeMod() TypeMod { return colTypeMod(FamilyGeography, c.srid, c.kind, c.dim) }

// TypeMod returns what the column's declared type says it holds, which is what [FromExpr] needs when that column is projected through a derived table.
func (c NullGeogCol[E]) TypeMod() TypeMod { return colTypeMod(FamilyGeography, c.srid, c.kind, c.dim) }

func colTypeMod(fam Family, srid int32, kind Kind, dim Dim) TypeMod {
	return TypeMod{Family: fam, SRID: srid, Kind: kind, Dim: dim,
		hasMod: srid != UnknownSRID || kind != AnyKind || dim != XY}
}

// AsGeography casts a geometry expression to geography, which is how a query
// asks for metres from a column stored in the plane.
//
// It is written out rather than inferred, exactly as [Geometry.AsGeography] is:
// the cast changes what the numbers mean, and PostGIS's own implicit cast to
// 4326 is a guess this package does not make. The expression must already state
// its coordinate system.
func (g GeomExpr[E]) AsGeography() GeogExpr[E] {
	out := g
	if g.srid == UnknownSRID {
		out.arg = orm.ArgFail(fmt.Errorf(
			"postgis: casting to geography needs a coordinate system, and this expression does not state one" +
				" (a column typed geometry(Point,4326) states it; a column typed plain geometry does not)"))
		return GeogExpr[E]{e: out}
	}
	out.arg = orm.ArgAs(g.arg, "geography")
	return GeogExpr[E]{e: out}
}

// AsGeometry casts a geography expression to geometry, so that the plane
// operations apply. Distances over the result are in the SRID's units.
func (g GeogExpr[E]) AsGeometry() GeomExpr[E] {
	out := g.e
	out.arg = orm.ArgAs(g.e.arg, "geometry")
	return out
}

// Composed spatial expressions.
//
// A predicate relating two tables cannot carry one entity tag, because it does
// not belong to one entity. The composed query's scope check validates it
// instead, and more strictly — the same trade the ORM's own cross-source
// comparisons make.

// Of lifts a column of any entity into a composed spatial expression.
//
//	postgis.Intersects(postgis.Of(Parks.Area), postgis.Of(Roads.Path))
//
// The entity tag is dropped and nothing else is: the SRID, the shape and the
// nullability travel through, so a composed query gets the same checks an
// entity query does.
func Of[E any](c interface{ Expr() GeomExpr[E] }) GeomExpr[orm.Composed] {
	e := c.Expr()
	return GeomExpr[orm.Composed]{arg: e.arg, srid: e.srid, kind: e.kind, dim: e.dim, nullable: e.nullable}
}

// OfGeog lifts a geography column of any entity into a composed spatial
// expression.
func OfGeog[E any](c interface{ Expr() GeogExpr[E] }) GeogExpr[orm.Composed] {
	e := c.Expr().e
	return GeogExpr[orm.Composed]{e: GeomExpr[orm.Composed]{
		arg: e.arg, srid: e.srid, kind: e.kind, dim: e.dim, nullable: e.nullable,
	}}
}

// Compose drops the entity tag from a spatial expression that already is one,
// which is what a chain of transformations produces.
func Compose[E any](g GeomExpr[E]) GeomExpr[orm.Composed] {
	return GeomExpr[orm.Composed]{arg: g.arg, srid: g.srid, kind: g.kind, dim: g.dim, nullable: g.nullable}
}

// ComposeGeog drops the entity tag from a geography expression.
func ComposeGeog[E any](g GeogExpr[E]) GeogExpr[orm.Composed] {
	return GeogExpr[orm.Composed]{e: GeomExpr[orm.Composed]{
		arg: g.e.arg, srid: g.e.srid, kind: g.e.kind, dim: g.e.dim, nullable: g.e.nullable,
	}}
}

// sameSRID reports the mistake of relating two expressions in different
// coordinate systems, or nil when there is none to report.
//
// PostGIS raises on this, and it raises at run time with a message naming two
// numbers. Catching it while the query is being built names the two operands
// instead, which is the difference between a bug found in a test and one found
// in a log.
//
// An expression that does not state its coordinate system is not checked. That
// is not laxity: a plain geometry column genuinely holds whatever was put in
// it, and refusing every query over one would make the check a reason to avoid
// declaring types.
func sameSRID(a, b GeomExpr[any], what string) error {
	if a.srid == UnknownSRID || b.srid == UnknownSRID || a.srid == b.srid {
		return nil
	}
	return fmt.Errorf("postgis: %s relates a geometry in SRID %d to one in SRID %d;"+
		" PostGIS refuses to mix coordinate systems, so transform one of them first (ST_Transform)",
		what, a.srid, b.srid)
}

// checkSRID is sameSRID for the two expressions this package actually holds,
// which are generic and so cannot be passed to it directly.
func checkSRID[E any](a, b GeomExpr[E], what string) error {
	return sameSRID(
		GeomExpr[any]{srid: a.srid},
		GeomExpr[any]{srid: b.srid},
		what,
	)
}

// binary builds an operand pair, replacing both with the mistake when there is
// one so that the statement cannot compile.
func binaryArgs[E any](a, b GeomExpr[E], what string) (orm.Arg, orm.Arg, bool) {
	if err := checkSRID(a, b, what); err != nil {
		return orm.ArgFail(err), orm.ArgFail(err), false
	}
	return a.arg, b.arg, true
}
