package desired

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Choosing a PostgreSQL type for a Go field.
//
// Database-first mode asks whether a Go type can hold a column PostgreSQL
// already has. Managed mode asks the opposite question, and the two have to
// agree: a schema this package creates has to be one reconciliation then
// accepts, or a project would generate a migration and immediately fail its own
// check.
//
// So the mapping is narrow and explicit. Where a Go type has one obvious
// PostgreSQL counterpart it is used; where it has several — an int could be
// int4 or int8, a string could be text or varchar or citext — the widest safe
// one is chosen and a type: tag overrides it. Nothing here guesses at a type
// from a field's name.

// goTypes maps a Go kind to the PostgreSQL type a managed schema creates for it.
var goTypes = map[model.GoKind]string{
	model.KindBool:    "bool",
	model.KindInt16:   "int2",
	model.KindInt32:   "int4",
	model.KindInt:     "int8",
	model.KindInt64:   "int8",
	model.KindFloat32: "float4",
	model.KindFloat64: "float8",
	model.KindString:  "text",
	model.KindBytes:   "bytea",
	model.KindTime:    "timestamptz",
	model.KindMap:     "jsonb",
	model.KindAny:     "jsonb",
}

// goNamedTypes maps a named Go type to the PostgreSQL type a managed schema
// creates for it, for the types whose Go kind says nothing useful.
//
// orm.Interval is a struct, and a struct is jsonb by default; it is an interval
// because of what it is, not because of what it is made of.
var goNamedTypes = map[string]string{
	"github.com/AlexAli29/orm.Interval": "interval",

	// The PostGIS types map to the unconstrained forms, because a Go type
	// cannot say which shape or which coordinate system a column holds and this
	// will not guess. A field declared postgis.Geometry with no type: tag is a
	// column that accepts any geometry — which is a real thing to want, and the
	// honest reading of a declaration that says nothing more.
	//
	// A column that should be constrained says so:
	//
	//	Location postgis.Geography `orm:"type:geography(Point,4326)"`
	//
	// and the tag path above handles it, because a spatial modifier is a type
	// modifier like any other.
	"github.com/AlexAli29/orm/postgis.Geometry":  "geometry",
	"github.com/AlexAli29/orm/postgis.Geography": "geography",
}

// rangeFamilies chooses the PostgreSQL range family for a Go element type.
//
// The time.Time entry is the interesting one. daterange, tsrange and tstzrange
// all hold time.Time, so the Go type cannot decide between them and this does
// not pretend to: it applies the same rule a bare time.Time field gets — the
// zoned one, because a wall clock with no offset denotes a different instant to
// everyone who reads it — and a pgtype: tag names one of the other two when
// that is what the author means.
//
//	Stay orm.Range[time.Time] `orm:"pgtype:daterange"`
//
// numrange is reached through a configured numeric mapping rather than from a
// Go kind, because there is no built-in Go type for an arbitrary-precision
// decimal and guessing float64 would corrupt money one level deeper than usual.
var rangeFamilies = map[model.GoKind]struct{ rng, multi string }{
	model.KindInt32: {"int4range", "int4multirange"},
	model.KindInt64: {"int8range", "int8multirange"},
	model.KindInt:   {"int8range", "int8multirange"},
	model.KindTime:  {"tstzrange", "tstzmultirange"},
}

// rangeContainers names the two generic containers a managed schema knows.
var rangeContainers = map[string]bool{
	"github.com/AlexAli29/orm.Range":      true,
	"github.com/AlexAli29/orm.Multirange": false,
}

// columnType chooses the PostgreSQL type and nullability for a field.
//
// Nullability comes from the Go type by the same rule reconciliation uses: a
// pointer or a sql.Null wrapper can hold NULL and a plain value cannot. There
// is no second set of rules and no tag that contradicts them — a field declared
// as string is a column that rejects NULL, because a column that accepted it
// would have no way to scan back into that field.
func (b *builder) columnType(f model.GoField) (schema.Type, bool, error) {
	nullable := f.Type.Ptr || f.Type.SQLNull

	// A named PostgreSQL type is the author saying what the column is, which
	// outranks anything derivable from the Go type.
	if pg := f.Tags.PGType; pg != "" {
		return b.parseTypeRef(pg), nullable, nil
	}

	// A named string type is an enum when something declared one for it. That
	// is what keeps UserState a public.user_state rather than text, without
	// inferring anything from the name.
	if name := b.enumFor(f.Type); name != "" {
		return b.parseTypeRef(name), nullable, nil
	}

	if pg, ok := goNamedTypes[f.Type.Named]; ok {
		return schema.Type{Name: pg}, nullable, nil
	}

	if isRange, ok := rangeContainers[f.Type.Named]; ok {
		pg, err := b.rangeType(f, isRange)
		if err != nil {
			return schema.Type{}, false, err
		}
		return schema.Type{Name: pg}, nullable, nil
	}

	// A slice of a scalar is an array of that scalar. []byte is bytea and is
	// handled by the kind table, since it is not an array of anything.
	if f.Type.Kind == model.KindSlice && f.Type.Elem != nil {
		elem, ok := goTypes[f.Type.Elem.Kind]
		if !ok {
			return schema.Type{}, false, fmt.Errorf("no PostgreSQL type for []%s; name one with a type: tag", f.Type.Elem.Src)
		}
		return schema.Type{Name: elem, Array: true}, nullable, nil
	}

	pg, ok := goTypes[f.Type.Kind]
	if !ok {
		return schema.Type{}, false, fmt.Errorf("no PostgreSQL type for %s; name one with a type: tag or a configured mapping", f.Type.Src)
	}
	return schema.Type{Name: pg}, nullable, nil
}

// rangeType chooses the PostgreSQL family for orm.Range[T] or
// orm.Multirange[T].
func (b *builder) rangeType(f model.GoField, isRange bool) (string, error) {
	args := f.Type.TypeArgs
	if len(args) != 1 {
		return "", fmt.Errorf("%s has no element type; declare it as orm.Range[int32] or another concrete instantiation", f.Type.Src)
	}
	elem := args[0]
	fam, ok := rangeFamilies[elem.Kind]
	if !ok {
		// A configured numeric mapping is the only other element a managed
		// schema can place, and it is placed by what it maps to rather than by
		// its Go kind: a decimal is a struct like any other.
		if m, ok := b.cfg.Types["numeric"]; ok && elem.Named == m.Qualified {
			fam = struct{ rng, multi string }{"numrange", "nummultirange"}
		} else {
			return "", fmt.Errorf("no PostgreSQL range family for %s; name one with a pgtype: tag, such as `orm:\"pgtype:daterange\"`", f.Type.Src)
		}
	}
	if isRange {
		return fam.rng, nil
	}
	return fam.multi, nil
}

// enumFor reports the enum a Go type was declared as, if any.
//
// The declaration is matched by the type's own name rather than by convention:
// an //orm:enum names the PostgreSQL type, and the Go type it sits on is the
// one that uses it.
func (b *builder) enumFor(t model.GoType) string {
	if t.Named == "" {
		return ""
	}
	short := t.Named
	if i := strings.LastIndexAny(short, "./"); i >= 0 {
		short = short[i+1:]
	}
	return b.enumsByGoType[short]
}

// parseTypeRef reads a PostgreSQL type reference written in configuration or a
// declaration, such as text, text[] or public.user_state.
func (b *builder) parseTypeRef(ref string) schema.Type {
	t := schema.Type{}
	name := strings.TrimSpace(ref)
	if strings.HasSuffix(name, "[]") {
		t.Array = true
		name = strings.TrimSuffix(name, "[]")
	}
	if schemaName, rest, ok := strings.Cut(name, "."); ok {
		t.Schema = schemaName
		name = rest
	}
	t.Name = name
	// A built-in carries no schema, which is how int8 stays distinguishable
	// from public.user_state without a catalog lookup.
	if t.Schema == "pg_catalog" {
		t.Schema = ""
	}
	return t.Canonical()
}

// resolveForeignKeys turns relation declarations into canonical foreign keys.
//
// A relation and a foreign key are the same fact seen from two sides, and this
// is where they become one object. The relation says which entity points at
// which; the key columns come from the fields, and the target's primary key is
// what they point at.
//
// Only a to-one relation whose own table carries the key produces a constraint.
// The other side of that relation is the same constraint seen from the target,
// and creating it twice would be two constraints where the author declared one.
func (b *builder) resolveForeignKeys(s *schema.Schema) {
	byEntity := make(map[string]*model.GoEntity, len(b.entities))
	for _, e := range b.entities {
		byEntity[e.PkgPath+"."+e.Name] = e
	}
	tableOf := func(e *model.GoEntity) (int, bool) {
		for i := range s.Tables {
			if s.Tables[i].Schema == b.qualify(e.Table.Schema) && s.Tables[i].Name == e.Table.Name {
				return i, true
			}
		}
		return 0, false
	}

	for _, e := range b.entities {
		if e.Kind != model.RelTable {
			continue
		}
		ti, ok := tableOf(e)
		if !ok {
			continue
		}
		for _, f := range e.Fields {
			if f.Rel == nil || f.Tags.Ignore {
				continue
			}
			// Only the side that carries the key declares the constraint. The
			// other side of a relation is the same constraint seen from the
			// target, and creating it twice would be two constraints where the
			// author declared one.
			//
			// A side: tag settles it. Without one, the side is decided by
			// whether this entity actually declares the key columns — a
			// belongs-to has them and a has-one does not. That is a fact about
			// the declarations rather than a guess about intent, and the case
			// where neither side has them is an error rather than a silent
			// omission.
			if f.Tags.HasSide && f.Tags.Side != model.FKLocal {
				continue
			}

			target, ok := byEntity[f.Rel.Target]
			if !ok {
				b.fail("%s: %s.%s relates to %s, which is not a mapped entity", e.Pos, e.Name, f.Name, f.Rel.Target)
				continue
			}
			tj, ok := tableOf(target)
			if !ok {
				continue
			}
			b.foreignKey(e, f, &s.Tables[ti], s.Tables[tj])
		}
	}
}

// foreignKey adds the constraint one relation implies.
func (b *builder) foreignKey(e *model.GoEntity, f model.GoField, from *schema.Table, to schema.Table) {
	if to.PrimaryKey == nil || len(to.PrimaryKey.Columns) == 0 {
		b.fail("%s: %s.%s points at %s, which declares no primary key for it to reference",
			e.Pos, e.Name, f.Name, to.Qualified())
		return
	}

	// The key columns are derived from the relation's own name and the target's
	// key: a Post.Author referencing users(id) is posts.author_id. A field
	// already declaring those columns is used as it stands.
	cols := make([]string, 0, len(to.PrimaryKey.Columns))
	for _, ref := range to.PrimaryKey.Columns {
		cols = append(cols, snake(f.Name)+"_"+ref)
	}
	for _, c := range cols {
		if _, ok := from.Column(c); ok {
			continue
		}
		// The columns are not here, so this is not the side that carries the
		// key. A side: tag that says otherwise is a mistake worth reporting;
		// without one, the other side owns the constraint.
		if f.Tags.HasSide {
			b.fail("%s: %s.%s is declared as carrying its foreign key, which would be on %s.%s — a column the entity does not declare",
				e.Pos, e.Name, f.Name, from.Name, c)
		}
		return
	}

	name := f.Tags.FK
	if name == "" {
		name = constraintName(from.Name, cols, "fkey")
	}
	fk := schema.ForeignKey{
		Name: name, Columns: cols,
		RefSchema: to.Schema, RefTable: to.Name, RefColumns: slices.Clone(to.PrimaryKey.Columns),
	}

	// A constraint the author already declared with the same name has to agree
	// with the relation, or the two sources contradict each other and neither
	// can be preferred from here.
	if i := slices.IndexFunc(from.ForeignKeys, func(x schema.ForeignKey) bool { return x.Name == name }); i >= 0 {
		existing := from.ForeignKeys[i]
		if !slices.Equal(existing.Columns, fk.Columns) ||
			existing.RefTable != fk.RefTable || !slices.Equal(existing.RefColumns, fk.RefColumns) {
			b.fail("%s: %s.%s implies foreign key %s on (%s) -> %s(%s), and a declaration of the same name says (%s) -> %s(%s);"+
				" they cannot both be right",
				e.Pos, e.Name, f.Name, name,
				strings.Join(fk.Columns, ", "), fk.RefTable, strings.Join(fk.RefColumns, ", "),
				strings.Join(existing.Columns, ", "), existing.RefTable, strings.Join(existing.RefColumns, ", "))
		}
		return
	}
	from.ForeignKeys = append(from.ForeignKeys, fk)
}

// snake is the same transliteration reconciliation uses for a column name, so
// that a relation's implied key matches the field an author would have written.
func snake(field string) string {
	runes := []rune(field)
	var b strings.Builder
	b.Grow(len(runes) + 4)
	for i, r := range runes {
		if unicode.IsUpper(r) && i > 0 {
			prev := runes[i-1]
			nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
			if !unicode.IsUpper(prev) || nextIsLower {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}
