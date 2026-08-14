package reconcile

import (
	"fmt"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/model"
)

// Reconciling ranges and multiranges.
//
// A range is one generic Go type over one element type, so checking it is two
// questions rather than a table of six answers: is the container orm.Range, and
// does its type argument map to the subtype the catalog reports? That is what
// makes int4range and int8range different types in Go without either being
// spelled out here — the catalog says int4 and int8, and the ordinary scalar
// mapping says int32 and int64.
//
// The families that share a Go element type are the reason this is worth doing
// carefully. daterange, tsrange and tstzrange all carry time.Time, and all three
// are therefore orm.Range[time.Time] on the Go side. They remain three distinct
// PostgreSQL types: which one a column has is read from the catalog in
// database-first mode and declared with a pgtype: tag in managed mode, and
// neither mode ever infers the family from the Go type. It is exactly the arrangement
// inet and cidr already have, one level further in.

// rangeContainers names the Go generic each range kind maps to.
var rangeContainers = map[model.PGTypeKind]struct {
	qualified string
	short     string
}{
	model.PGRange:      {"github.com/AlexAli29/orm.Range", "orm.Range"},
	model.PGMultirange: {"github.com/AlexAli29/orm.Multirange", "orm.Multirange"},
}

// checkRange reports every way a Go type fails to represent a range or
// multirange column.
func (r *reconciler) checkRange(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, gt model.GoType, pt *model.PGType) {
	container := rangeContainers[pt.Kind]

	// The element of a multirange is its range type, whose own element is the
	// subtype: int4multirange -> int4range -> int4. The Go side skips the
	// middle, because Multirange[int32] is a set of Range[int32].
	elem := pt.Elem
	if pt.Kind == model.PGMultirange && elem != nil {
		elem = elem.Resolve().Elem
	}
	if elem == nil {
		r.unsupportedType(em, f, col, pt,
			fmt.Sprintf("the catalog reports no subtype for %s, so there is no element type to map", pt.String()))
		return
	}

	elemDesc, matches, why := r.elementMapping(f, elem)
	if why != "" {
		r.add(diag.Finding{
			Code:    diag.E012,
			Message: fmt.Sprintf("%s has the unsupported type %s", col.Qualified(), pt.String()),
			Reason:  fmt.Sprintf("%s ranges over %s, and %s", pt.String(), elem.String(), why),
			Fix:     "exclude the field with orm:\"-\", change the column's type, or configure a mapping for the element type under types in orm.yaml",
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
		return
	}

	want := container.short + "[" + elemDesc + "]"
	if gt.Named != container.qualified || len(gt.TypeArgs) != 1 || !matches(gt.TypeArgs[0]) {
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s is %s but %s is %s", f.Name, describeGo(gt), col.Qualified(), pt.String()),
			Reason:  fmt.Sprintf("%s maps to %s", pt.String(), want),
			Fix:     fmt.Sprintf("declare the field as %s", withNullability(want, col)),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: col.Type.String(),
			Pos: f.Pos,
		})
	}
}

// elementMapping answers the two questions a range's element raises: what to
// write in Go, and whether a given Go type is it.
//
// It goes through the same three sources checkShape does — a configured
// mapping, a type that requires one, the built-in scalar shapes — so that a
// numrange over a configured decimal works for the same reason a numeric column
// does, and fails with the same explanation when nothing is configured.
//
// A non-empty why means the element has no mapping at all.
func (r *reconciler) elementMapping(f *model.GoField, pt *model.PGType) (desc string, matches func(model.GoType) bool, why string) {
	resolved := pt.Resolve()

	if _, mapping, ok := r.configuredType(f, pt); ok {
		return shortName(mapping.Qualified),
			func(gt model.GoType) bool { return gt.Named == mapping.Qualified },
			""
	}
	if need, ok := requiresConfiguration[resolved.Name]; ok {
		return "", nil, fmt.Sprintf("%s has no configured Go mapping: %s. Add a types.%s entry to orm.yaml naming a Go type such as %s",
			resolved.String(), need.reason, resolved.Name, need.example)
	}
	sh, ok := scalarShapes[resolved.Name]
	if !ok {
		reason := knownUnsupported[resolved.Name]
		if reason == "" {
			reason = fmt.Sprintf("%s is not one of the types v1 maps", resolved.String())
		}
		return "", nil, reason
	}
	return sh.goDesc, func(gt model.GoType) bool {
		if sh.named != "" && gt.Named != sh.named {
			return false
		}
		for _, k := range sh.kinds {
			if gt.Kind == k {
				return true
			}
		}
		return false
	}, ""
}
