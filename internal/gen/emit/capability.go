package emit

import (
	"slices"
	"strconv"

	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/postgis"
)

// capability is the set of operations a column descriptor offers.
//
// The lattice is cumulative: text implies ordered implies base. Which one a
// column gets is a question about PostgreSQL, never about Go — a jsonb column
// does not acquire Gt because the Go type it maps to happens to be comparable,
// and a text column has ILike because PostgreSQL has ILIKE.
type capability uint8

const (
	// capBase offers equality, membership and ordering direction.
	capBase capability = iota
	// capOrd adds magnitude comparison and ranges.
	capOrd
	// capText adds pattern matching.
	capText
)

// pgCapability maps a PostgreSQL base type onto what PostgreSQL can do with it.
//
// The omissions are the interesting part. bytea orders in PostgreSQL, but a
// byte-wise comparison of two blobs answers no question anybody asks, so it
// stays at equality. json and jsonb likewise: jsonb has a total order defined
// for indexing, and exposing it as Gt would invite the reading that it compares
// values the way a person would.
var pgCapability = map[string]capability{
	"text":    capText,
	"varchar": capText,
	"bpchar":  capText,
	"citext":  capText,
	"name":    capText,

	"int2":        capOrd,
	"int4":        capOrd,
	"int8":        capOrd,
	"float4":      capOrd,
	"float8":      capOrd,
	"numeric":     capOrd,
	"uuid":        capOrd,
	"date":        capOrd,
	"timestamp":   capOrd,
	"timestamptz": capOrd,

	// PostgreSQL orders inet and cidr, and the ordering is meaningful: it
	// sorts by address family, then by network, then by host. macaddr orders
	// too, byte by byte, which is the order a person reading MAC addresses
	// expects.
	"inet":    capOrd,
	"cidr":    capOrd,
	"macaddr": capOrd,

	// PostgreSQL orders intervals, and the order is the one a person means:
	// it compares them by the time they represent, taking a month as 30 days
	// and a day as 24 hours for the purpose of sorting only.
	"interval": capOrd,

	// A tsvector has a total order for indexing and comparing two of them
	// answers no question anybody asks, so it stays at equality — the same
	// reasoning bytea and jsonb get.
	"tsvector": capBase,
	"tsquery":  capBase,

	"bool":  capBase,
	"json":  capBase,
	"jsonb": capBase,
	"bytea": capBase,
}

// capabilityOf decides which descriptor a mapped column receives.
func capabilityOf(col *model.PGColumn, value model.GoType) capability {
	resolved := col.Type.Resolve()

	c := capBase
	switch resolved.Kind {
	case model.PGEnum:
		// A PostgreSQL enum has a declared order and compares by it. It is not
		// text, and LIKE does not apply to one.
		c = capOrd
	case model.PGArray:
		// Arrays compare element-wise for equality. Ordering them is defined
		// but almost never meant.
		c = capBase
	default:
		if known, ok := pgCapability[resolved.Name]; ok {
			c = known
		}
	}

	// Text capability is spelled in terms of Go strings — Like takes a
	// pattern, and a pattern is a string. A column whose Go side is a named
	// type keeps everything PostgreSQL offers except the operations that would
	// require pretending the named type is a string.
	if c == capText && value.Value != "string" {
		c = capOrd
	}
	return c
}

// descriptor is the generated column descriptor for one mapped column.
type descriptor struct {
	// Type is the descriptor type as written, for example
	// "orm.OrdCol[User, int64]".
	Type string
	// Ctor is the constructor as written, for example
	// "orm.NewOrdCol[User, int64]".
	Ctor string
	// Extra holds constructor arguments after the column name, quoted as Go
	// string literals when the descriptor is written out.
	Extra []string
	// Raw holds constructor arguments written verbatim, for the ones that are
	// not strings: a spatial descriptor takes an SRID, a shape constant and a
	// dimensionality constant, and quoting any of them would not compile.
	Raw []string
	// Imports lists the packages the value type needs.
	Imports []string
}

// rangeDescriptors names the descriptor a range kind receives. A range column
// is neither ordered nor text: what PostgreSQL offers it is containment,
// overlap and the bound functions, and those live on their own descriptors
// rather than being bolted onto the lattice everything else shares.
var rangeDescriptors = map[model.PGTypeKind]struct{ plain, null string }{
	model.PGRange:      {"RangeCol", "NullRangeCol"},
	model.PGMultirange: {"MultirangeCol", "NullMultirangeCol"},
}

// describeColumn builds the descriptor for one mapped column.
func describeColumn(entity string, cm model.ColMapping) descriptor {
	if d, ok := describeSpatial(entity, cm); ok {
		return d
	}
	if d, ok := describeRange(entity, cm); ok {
		return d
	}
	c := capabilityOf(cm.Column, cm.Field.Type)
	value := cm.Field.Type.Value
	nullable := cm.Column.Nullable()

	var name string
	switch {
	case c == capText && nullable:
		name = "NullTextCol"
	case c == capText:
		name = "TextCol"
	case c == capOrd && nullable:
		name = "NullOrdCol"
	case c == capOrd:
		name = "OrdCol"
	case nullable:
		name = "NullCol"
	default:
		name = "Col"
	}

	// The text descriptors fix their value type to string, so they take the
	// entity alone.
	args := entity
	if c != capText {
		args = entity + ", " + value
	}
	return descriptor{
		Type:    "orm." + name + "[" + args + "]",
		Ctor:    "orm.New" + name + "[" + args + "]",
		Imports: cm.Field.Type.Refs,
	}
}

// spatialDescriptors names the descriptor for each PostGIS storage family.
var spatialDescriptors = map[postgis.Family]struct{ plain, null string }{
	postgis.FamilyGeometry:  {"GeomCol", "NullGeomCol"},
	postgis.FamilyGeography: {"GeogCol", "NullGeogCol"},
}

// describeSpatial builds the descriptor for a PostGIS column.
//
// It carries three numbers the ordinary descriptors have no room for — the
// coordinate system, the shape and the dimensionality — because they are the
// column's type modifier rather than anything derivable from the Go type, and
// because the query layer checks against them. A column typed plain geometry
// declares none of the three, and the descriptor says so with the same zero
// values the type modifier had: the check that reads them then checks nothing,
// which is the right answer for a column that constrains nothing.
func describeSpatial(entity string, cm model.ColMapping) (descriptor, bool) {
	mod, ok := cm.Column.Spatial()
	if !ok {
		return descriptor{}, false
	}
	names, ok := spatialDescriptors[mod.Family]
	if !ok {
		return descriptor{}, false
	}
	name := names.plain
	if cm.Column.Nullable() {
		name = names.null
	}
	params := entity
	return descriptor{
		Type: "postgis." + name + "[" + params + "]",
		Ctor: "postgis.New" + name + "[" + params + "]",
		Raw: []string{
			strconv.FormatInt(int64(mod.SRID), 10),
			"postgis." + postgis.KindConst(mod.Kind),
			"postgis." + postgis.DimConst(mod.Dim),
		},
		Imports: append(slices.Clone(cm.Field.Type.Refs), postgisImport),
	}, true
}

// postgisImport is the package a generated spatial descriptor needs. It is
// named here rather than inferred from the field's type so that a column whose
// Go side came from a configured mapping still gets it.
const postgisImport = "github.com/AlexAli29/orm/postgis"

// describeRange builds the descriptor for a range or multirange column, whose
// type parameters are the entity and the range's *element* rather than the
// entity and the field's value type.
//
// That is what makes Bookings.Period.Contains take an int32 rather than a
// Range[int32], and it is read from the Go type's own type argument rather than
// from the PostgreSQL subtype: the two were already proved to agree during
// reconciliation, and the Go spelling is the one that has to compile here.
func describeRange(entity string, cm model.ColMapping) (descriptor, bool) {
	names, ok := rangeDescriptors[cm.Column.Type.Resolve().Kind]
	if !ok {
		return descriptor{}, false
	}
	args := cm.Field.Type.TypeArgs
	if len(args) != 1 {
		return descriptor{}, false
	}
	name := names.plain
	if cm.Column.Nullable() {
		name = names.null
	}
	params := entity + ", " + args[0].Src
	return descriptor{
		Type:    "orm." + name + "[" + params + "]",
		Ctor:    "orm.New" + name + "[" + params + "]",
		Extra:   rangeTypeNames(cm.Column.Type.Resolve()),
		Imports: cm.Field.Type.Refs,
	}, true
}

// rangeTypeNames lists the catalog names a range descriptor is constructed
// with: the column's type, then each type down the chain to the subtype.
//
// A range gives two — int4range, int4 — and a multirange three —
// int4multirange, int4range, int4. They are the operand casts that keep an
// overloaded operator resolving to the form the descriptor means, and they are
// read from the catalog because the catalog is the only thing that knows them.
func rangeTypeNames(pt *model.PGType) []string {
	var out []string
	for t := pt; t != nil; t = t.Resolve().Elem {
		out = append(out, t.Resolve().String())
	}
	return out
}
