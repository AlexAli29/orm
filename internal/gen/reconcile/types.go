package reconcile

import (
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// shape is the Go representation a PostgreSQL base type accepts.
type shape struct {
	// kinds lists the acceptable Go kinds, after pointers and sql null
	// wrappers have been removed.
	kinds []model.GoKind
	// named, when set, additionally requires that exact named Go type.
	named string
	// goDesc is what to write in Go, quoted in diagnostics.
	goDesc string
	// warn is a finding raised even when the mapping is accepted.
	warn diag.Code
}

// scalarShapes is the built-in type mapping, keyed by the pg_catalog type name.
//
// Two families are deliberately absent. numeric and uuid appear in
// requiresConfiguration instead, because there is no honest default: mapping
// numeric to float64 silently loses precision on money, and uuid has three
// popular Go representations with no winner. The rest of PostgreSQL's types are
// absent because v1 does not map them, and an unmapped type is reported rather
// than guessed at.
var scalarShapes = map[string]shape{
	"int2":        {kinds: []model.GoKind{model.KindInt16}, goDesc: "int16"},
	"int4":        {kinds: []model.GoKind{model.KindInt32}, goDesc: "int32"},
	"int8":        {kinds: []model.GoKind{model.KindInt64}, goDesc: "int64"},
	"float4":      {kinds: []model.GoKind{model.KindFloat32}, goDesc: "float32"},
	"float8":      {kinds: []model.GoKind{model.KindFloat64}, goDesc: "float64"},
	"bool":        {kinds: []model.GoKind{model.KindBool}, goDesc: "bool"},
	"text":        {kinds: []model.GoKind{model.KindString}, goDesc: "string"},
	"varchar":     {kinds: []model.GoKind{model.KindString}, goDesc: "string"},
	"bpchar":      {kinds: []model.GoKind{model.KindString}, goDesc: "string"},
	"citext":      {kinds: []model.GoKind{model.KindString}, goDesc: "string"},
	"name":        {kinds: []model.GoKind{model.KindString}, goDesc: "string"},
	"bytea":       {kinds: []model.GoKind{model.KindBytes}, goDesc: "[]byte"},
	"date":        {kinds: []model.GoKind{model.KindTime}, named: "time.Time", goDesc: "time.Time"},
	"timestamptz": {kinds: []model.GoKind{model.KindTime}, named: "time.Time", goDesc: "time.Time"},
	"timestamp":   {kinds: []model.GoKind{model.KindTime}, named: "time.Time", goDesc: "time.Time", warn: diag.W015},
	// The network types map to the standard library, and pgx encodes those
	// shapes natively — so they need no configuration and no registration.
	// netip.Prefix is what inet and cidr both are: an address with a prefix
	// length, which is the whole of what PostgreSQL stores. Reducing either to
	// a string would keep the text and lose the semantics every network
	// operator is defined in terms of.
	"inet":    {kinds: []model.GoKind{model.KindStruct}, named: "net/netip.Prefix", goDesc: "netip.Prefix"},
	"cidr":    {kinds: []model.GoKind{model.KindStruct}, named: "net/netip.Prefix", goDesc: "netip.Prefix"},
	"macaddr": {kinds: []model.GoKind{model.KindBytes}, named: "net.HardwareAddr", goDesc: "net.HardwareAddr"},

	// Full text search. Both map to named Go types rather than to string: a
	// tsvector is a parsed document and a tsquery is a parsed query, and
	// letting either be an ordinary string would make "compare this column to
	// that word" compile when PostgreSQL means something else by it.
	// An interval is three independent components — months, days, microseconds
	// — and time.Duration has room for one of them. orm.Interval keeps all
	// three, which is why interval maps to it and to nothing else.
	"interval": {kinds: []model.GoKind{model.KindStruct}, named: "github.com/AlexAli29/orm.Interval", goDesc: "orm.Interval"},

	"tsvector": {kinds: []model.GoKind{model.KindString}, named: "github.com/AlexAli29/orm.TSVector", goDesc: "orm.TSVector"},
	"tsquery":  {kinds: []model.GoKind{model.KindString}, named: "github.com/AlexAli29/orm.TSQuery", goDesc: "orm.TSQuery"},
	"json":     {kinds: []model.GoKind{model.KindStruct, model.KindMap, model.KindSlice, model.KindBytes, model.KindAny}, goDesc: "a struct, map, slice or any"},
	"jsonb":    {kinds: []model.GoKind{model.KindStruct, model.KindMap, model.KindSlice, model.KindBytes, model.KindAny}, goDesc: "a struct, map, slice or any"},
}

// configurable is a type that maps only through orm.yaml.
type configurable struct {
	// reason says why the tool refuses to choose for the author.
	reason string
	// example is a plausible Go type to put in the suggestion.
	example string
	// codec is the codec name to suggest alongside it.
	codec string
}

// requiresConfiguration lists the types with no default mapping.
var requiresConfiguration = map[string]configurable{
	"numeric": {
		reason:  "there is no lossless built-in Go type for an arbitrary-precision decimal, and mapping it to float64 would silently corrupt money",
		example: "github.com/shopspring/decimal.Decimal",
		codec:   "decimal",
	},
	"uuid": {
		reason:  "Go has no uuid type, and the popular third-party ones are not interchangeable",
		example: "github.com/google/uuid.UUID",
		codec:   "uuid",
	},
}

// knownUnsupported explains the types v1 leaves alone, so that E012 can say
// more than "unsupported".
var knownUnsupported = map[string]string{
	"money":    "money is locale-dependent and lossy; use numeric with a configured decimal type",
	"time":     "time of day without a date is not mapped in v1",
	"timetz":   "time of day with a time zone is not mapped in v1",
	"xml":      "xml is not mapped in v1",
	"bit":      "bit strings are not mapped in v1",
	"varbit":   "bit strings are not mapped in v1",
	"pg_lsn":   "internal types are not mapped",
	"oid":      "internal types are not mapped",
	"tid":      "internal types are not mapped",
	"jsonpath": "jsonpath is not mapped in v1",
}

// checkColumnType reports every way a field's Go type fails to represent its
// column, and returns nothing: findings are the output.
func (r *reconciler) checkColumnType(em *model.EntityMapping, f *model.GoField, col *model.PGColumn) {
	r.checkNullability(em, f, col)

	if col.Dims > 1 {
		r.add(diag.Finding{
			Code:    diag.E012,
			Message: fmt.Sprintf("%s is a %d-dimensional array", col.Qualified(), col.Dims),
			Reason:  "v1 maps one-dimensional arrays only; a Go slice of slices does not round-trip through PostgreSQL's nested array representation",
			Fix:     "store the value as a one-dimensional array, as jsonb, or exclude the field with orm:\"-\"",
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
		return
	}
	r.checkShape(em, f, col, f.Type, col.Type)
}

// checkNullability reports the two ways a column's nullability and a Go type's
// null capability can disagree.
func (r *reconciler) checkNullability(em *model.EntityMapping, f *model.GoField, col *model.PGColumn) {
	// A view's columns carry no nullability PostgreSQL can vouch for.
	//
	// pg_attribute records no NOT NULL on any of them — the catalog cannot say
	// whether an expression yields NULL, and neither can anything else without
	// evaluating it. So every view column reads as nullable, and comparing that
	// against a Go field would report every non-pointer field on every view as
	// wrong while proving nothing: the answer is "unknown", not "nullable".
	//
	// The conservative direction is preserved where it is decidable, in
	// generation: a database-first view generates pointer fields, because the
	// column may be NULL and a non-pointer would fail on the first row that is.
	// What is dropped here is only the reconciliation of a declaration against
	// an answer the catalog does not have. A declaration that gets it wrong is
	// caught by the scan failing at runtime, which is the same place PostgreSQL
	// would catch it.
	if em.Entity.Kind != model.RelTable {
		return
	}
	switch {
	case col.Nullable() && !f.Type.Nullable():
		r.add(diag.Finding{
			Code:    diag.E004,
			Message: fmt.Sprintf("nullable column %s cannot be represented by %s", col.Qualified(), f.Type.Src),
			Reason:  nullReason(f.Type),
			Fix:     nullFix(f.Type),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
	case !col.Nullable() && f.Type.Ptr:
		r.add(diag.Finding{
			Code:    diag.W005,
			Message: fmt.Sprintf("%s is a pointer but %s is NOT NULL", f.Name, col.Qualified()),
			Reason:  "the column can never be NULL, so the pointer adds a nil case that the database cannot produce",
			Fix:     fmt.Sprintf("drop the pointer: %s", strings.TrimPrefix(f.Type.Src, "*")),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
	}
}

func nullReason(t model.GoType) string {
	if t.Kind == model.KindSlice || t.Kind == model.KindBytes {
		return "a slice cannot distinguish SQL NULL from an empty array: both arrive as a nil or empty slice"
	}
	return fmt.Sprintf("%s has no value that means SQL NULL", t.Src)
}

func nullFix(t model.GoType) string {
	// sql.Null[T] is only worth suggesting for the scalar shapes it reads
	// naturally on; *[]string is the idiomatic nullable array, not
	// sql.Null[[]string].
	switch t.Kind {
	case model.KindSlice, model.KindBytes, model.KindStruct:
		return fmt.Sprintf("use *%s", t.Src)
	default:
		return fmt.Sprintf("use *%s, or sql.Null[%s]", t.Src, t.Src)
	}
}

// checkShape compares a Go type against a PostgreSQL type, following domains to
// their base type and arrays to their element type.
func (r *reconciler) checkShape(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, gt model.GoType, pt *model.PGType) {
	// A configured mapping wins over everything: the author has said what this
	// type is in Go, and the tool's job is to check that the field agrees.
	if key, mapping, ok := r.configuredType(f, pt); ok {
		if gt.Named != mapping.Qualified {
			r.add(diag.Finding{
				Code:    diag.E006,
				Message: fmt.Sprintf("%s is %s but %s maps to %s", f.Name, describeGo(gt), pt.String(), mapping.Qualified),
				Reason:  fmt.Sprintf("orm.yaml configures types.%s as %s", key, mapping.Qualified),
				Fix:     fmt.Sprintf("declare the field as %s, or change types.%s", mapping.Qualified, key),
				Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
				Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
				Pos: f.Pos,
			})
		}
		return
	}

	// PostGIS's types are base types with a load-bearing modifier, so they are
	// checked before the kind switch: by the time a type is resolved to its
	// name the modifier is gone, and the modifier is the whole difference
	// between geometry(Point,4326) and geometry(MultiPolygon,3857).
	if r.checkSpatial(em, f, col, gt) {
		return
	}

	resolved := pt.Resolve()
	switch resolved.Kind {
	case model.PGArray:
		r.checkArray(em, f, col, gt, resolved)
		return
	case model.PGEnum:
		r.checkEnum(em, f, col, gt, resolved)
		return
	case model.PGRange, model.PGMultirange:
		r.checkRange(em, f, col, gt, resolved)
		return
	case model.PGComposite, model.PGPseudo:
		r.unsupportedType(em, f, col, resolved, fmt.Sprintf("%s types are not mapped in v1", resolved.Kind))
		return
	}

	if need, ok := requiresConfiguration[resolved.Name]; ok {
		r.add(diag.Finding{
			Code:    diag.E013,
			Message: fmt.Sprintf("%s has no configured Go mapping for %s", col.Qualified(), resolved.String()),
			Reason:  need.reason,
			Fix: fmt.Sprintf("add a types.%s entry to orm.yaml:\ntypes:\n  %s:\n    go: %s\n    codec: %s",
				resolved.Name, resolved.Name, need.example, need.codec),
			Entity: em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
		return
	}

	sh, ok := scalarShapes[resolved.Name]
	if !ok {
		reason := knownUnsupported[resolved.Name]
		if reason == "" {
			reason = fmt.Sprintf("%s is not one of the types v1 maps", resolved.String())
		}
		r.unsupportedType(em, f, col, resolved, reason)
		return
	}

	if !slices.Contains(sh.kinds, gt.Kind) || (sh.named != "" && gt.Named != sh.named) {
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s is %s but %s is %s", f.Name, describeGo(gt), col.Qualified(), pt.String()),
			Reason:  fmt.Sprintf("%s maps to %s", resolved.String(), sh.goDesc),
			Fix:     fmt.Sprintf("declare the field as %s", withNullability(sh.goDesc, col)),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
		return
	}

	if sh.warn == diag.W015 && r.cfg.Strict.TimestampWithoutTZ != config.Off {
		r.add(diag.Finding{
			Code:     diag.W015,
			Severity: severity(r.cfg.Strict.TimestampWithoutTZ),
			Message:  fmt.Sprintf("%s is timestamp without time zone", col.Qualified()),
			Reason:   "the column stores a wall clock reading with no offset, so the instant it denotes depends on whoever reads it",
			Fix:      "change the column to timestamptz, or set strict.timestamp_without_tz: off if the ambiguity is intended",
			Entity:   em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
	}
}

// checkArray requires a Go slice whose element maps to the array's element.
func (r *reconciler) checkArray(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, gt model.GoType, arr *model.PGType) {
	if gt.Kind != model.KindSlice || gt.Elem == nil {
		want := "[]" + goDescFor(arr.Elem)
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s is %s but %s is the array %s", f.Name, describeGo(gt), col.Qualified(), arr.String()),
			Reason:  fmt.Sprintf("%s maps to %s", arr.String(), want),
			Fix:     fmt.Sprintf("declare the field as %s", withNullability(want, col)),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
		return
	}
	r.checkShape(em, f, col, *gt.Elem, arr.Elem)
}

// checkEnum requires a named Go string type whose constants are exactly the
// enum's labels.
func (r *reconciler) checkEnum(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, gt model.GoType, enum *model.PGType) {
	if gt.Kind != model.KindString || gt.Named == "" {
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s is %s but %s is the enum %s", f.Name, describeGo(gt), col.Qualified(), enum.String()),
			Reason:  "an enum column maps to a named Go string type whose constants are its labels, so that the two can be reconciled",
			Fix:     fmt.Sprintf("declare a named string type with one constant per label (%s)", strings.Join(enum.Labels, ", ")),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
		return
	}

	inGo := make(map[string]bool, len(gt.Enum))
	for _, c := range gt.Enum {
		inGo[c.Value()] = true
	}
	inPG := make(map[string]bool, len(enum.Labels))
	for _, l := range enum.Labels {
		inPG[l] = true
	}

	var missing, extra []string
	for _, l := range enum.Labels {
		if !inGo[l] {
			missing = append(missing, l)
		}
	}
	for _, c := range gt.Enum {
		if !inPG[c.Value()] {
			extra = append(extra, fmt.Sprintf("%s (%q)", c.Name, c.Value()))
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return
	}

	var reason []string
	if len(missing) > 0 {
		reason = append(reason, fmt.Sprintf("%s has no constant for %s", shortName(gt.Named), strings.Join(quoteAll(missing), ", ")))
	}
	if len(extra) > 0 {
		reason = append(reason, fmt.Sprintf("%s has no label for %s", enum.String(), strings.Join(extra, ", ")))
	}
	r.add(diag.Finding{
		Code:    diag.E007,
		Message: fmt.Sprintf("the labels of %s and the constants of %s differ", enum.String(), shortName(gt.Named)),
		Reason:  strings.Join(reason, "; "),
		Fix:     "add the missing constants, or change the PostgreSQL enum so both sides agree",
		Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
		Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
		Pos: f.Pos,
	})
}

func (r *reconciler) unsupportedType(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, pt *model.PGType, reason string) {
	r.add(diag.Finding{
		Code:    diag.E012,
		Message: fmt.Sprintf("%s has the unsupported type %s", col.Qualified(), pt.String()),
		Reason:  reason,
		Fix:     "exclude the field with orm:\"-\", change the column's type, or configure a mapping under types in orm.yaml",
		Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
		Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
		Pos: f.Pos,
	})
}

// configuredType finds the orm.yaml mapping that applies to a column, trying an
// explicit type: tag first, then the column's own type name, then the type a
// domain resolves to.
func (r *reconciler) configuredType(f *model.GoField, pt *model.PGType) (string, config.TypeMapping, bool) {
	keys := make([]string, 0, 3)
	if f.Tags.Type != "" {
		keys = append(keys, f.Tags.Type)
	}
	keys = append(keys, pt.Key())
	if resolved := pt.Resolve(); resolved != pt {
		keys = append(keys, resolved.Key())
	}
	for _, k := range keys {
		if m, ok := r.cfg.Types[k]; ok {
			return k, m, true
		}
	}
	return "", config.TypeMapping{}, false
}

// goDescFor is the Go rendering a PostgreSQL type maps to, used when building a
// suggestion rather than checking one.
func goDescFor(pt *model.PGType) string {
	if pt == nil {
		return "?"
	}
	resolved := pt.Resolve()
	if resolved.Kind == model.PGArray {
		return "[]" + goDescFor(resolved.Elem)
	}
	if sh, ok := scalarShapes[resolved.Name]; ok {
		return sh.goDesc
	}
	return resolved.String()
}

// withNullability decorates a suggested Go type with a pointer when the column
// is nullable, because that is the type the author actually needs.
func withNullability(goDesc string, col *model.PGColumn) string {
	if !col.Nullable() || strings.ContainsAny(goDesc, " ,") {
		return goDesc
	}
	return "*" + goDesc
}

// describeGo renders a Go type for a message, naming the kind when the type is
// unnamed so that "is a slice" reads better than "is []struct{}".
func describeGo(t model.GoType) string {
	if t.Src != "" {
		return t.Src
	}
	return t.Kind.String()
}

func shortName(qualified string) string {
	if i := strings.LastIndexByte(qualified, '.'); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

func severity(s config.Strictness) diag.Severity {
	if s == config.Error {
		return diag.SeverityError
	}
	return diag.SeverityWarning
}

// columnName derives the column a field maps to when no column: tag says
// otherwise: the lower_snake_case of the field name.
//
// This is the one place the tool infers a name, and it is deliberate. A table
// name is a design decision that belongs in the schema and is therefore always
// written down; a field's column is a mechanical transliteration of a name the
// author already chose, and a tag overrides it whenever the transliteration is
// wrong.
func columnName(field string) string {
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
