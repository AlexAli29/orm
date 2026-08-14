package reconcile

import (
	"fmt"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/model"
	"github.com/AlexAli29/orm/postgis"
)

// Reconciling a spatial column with a Go field.
//
// Two of PostGIS's four facts live in the Go type and two do not. The storage
// family is a Go type — postgis.Geometry and postgis.Geography are different
// types on purpose — and nullability is a pointer, as everywhere else. The
// shape, the coordinate system and the dimensionality are not: they are the
// column's type modifier, and no Go type carries a number.
//
// So the check is in two halves. The family and nullability are checked against
// the field's Go type, which is where they belong. The other three are checked
// against the type: tag when the field has one — because a tag that says
// geography(Point,4326) over a column that is geometry(Polygon,3857) is an
// author who believes something false, and finding out at generation time beats
// finding out when a query returns degrees.
//
// A field with no tag states nothing about shape or SRID and is checked on
// nothing but its family. That is not laxity: a project doing database-first
// against an existing PostGIS schema has the column as its source of truth, and
// requiring it to be restated in a tag would be requiring it to be restated
// wrongly.

// spatialShapes maps a PostGIS storage family to the Go type that holds it.
//
// There are two, and there is no third. The shape is not in the Go type because
// PostGIS does not keep it in the value's type either: ST_Intersection of two
// polygons can be a line, and a Go type that promised Polygon would be a
// promise the database breaks.
var spatialGoTypes = map[postgis.Family]struct{ named, desc string }{
	postgis.FamilyGeometry: {
		named: "github.com/AlexAli29/orm/postgis.Geometry",
		desc:  "postgis.Geometry",
	},
	postgis.FamilyGeography: {
		named: "github.com/AlexAli29/orm/postgis.Geography",
		desc:  "postgis.Geography",
	},
}

// checkSpatial reports every way a Go field fails to represent a PostGIS
// column, and reports whether the column was a spatial one at all.
func (r *reconciler) checkSpatial(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, gt model.GoType) bool {
	actual, ok := col.Spatial()
	if !ok {
		return false
	}
	want := spatialGoTypes[actual.Family]

	if gt.Named != want.named {
		other := spatialGoTypes[postgis.FamilyGeometry]
		if actual.Family == postgis.FamilyGeometry {
			other = spatialGoTypes[postgis.FamilyGeography]
		}
		reason := fmt.Sprintf("%s stores %s", col.Qualified(), actual.Family)
		if gt.Named == other.named {
			// The two spatial types are the confusable pair, so the diagnostic
			// says what the confusion costs rather than only that it happened.
			reason = fmt.Sprintf(
				"%s stores %s and the field is %s; the two are measured on different surfaces,"+
					" so a distance read through the wrong one is in the wrong units",
				col.Qualified(), actual.Family, other.desc)
		}
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s is %s but %s is %s", f.Name, describeGo(gt), col.Qualified(), actual),
			Reason:  reason,
			Fix:     fmt.Sprintf("declare the field as %s", withNullability(want.desc, col)),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: gt.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: actual.String(),
			Pos: f.Pos,
		})
		return true
	}

	r.checkSpatialModifier(em, f, col, actual)
	return true
}

// checkSpatialModifier compares what a type: tag declares against what the
// column actually is.
//
// Each of the three facts is reported separately, because each is a different
// mistake with a different fix: a shape mismatch means the wrong column, an
// SRID mismatch means coordinates that will land somewhere else, and a
// dimensionality mismatch means an ordinate that gets dropped on write.
func (r *reconciler) checkSpatialModifier(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, actual postgis.TypeMod) {
	declared := f.Tags.PGType
	if declared == "" {
		return
	}
	want, err := postgis.ParseTypeMod(declared)
	if err != nil {
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s declares type:%s, which is not a spatial type this tool can read", f.Name, declared),
			Reason:  err.Error(),
			Fix:     fmt.Sprintf("write the column's own type: %s", actual),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: actual.String(),
			Pos: f.Pos,
		})
		return
	}
	if !want.Spatial() {
		// The tag names something that is not spatial at all over a column that
		// is, which the ordinary type check does not see because the family
		// came from the Go type.
		r.add(diag.Finding{
			Code:    diag.E006,
			Message: fmt.Sprintf("%s declares type:%s but %s is %s", f.Name, declared, col.Qualified(), actual),
			Reason:  "the declared type is not a PostGIS type and the column is",
			Fix:     fmt.Sprintf("write the column's own type: %s", actual),
			Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
			Table: col.Table.Qualified(), Column: col.Name, PGType: actual.String(),
			Pos: f.Pos,
		})
		return
	}

	if want.Family != actual.Family {
		r.spatialMismatch(em, f, col, actual,
			fmt.Sprintf("the tag says %s and the column is %s", want.Family, actual.Family),
			"the two are different PostgreSQL types measured on different surfaces")
		return
	}
	if !want.Constrained() || !actual.Constrained() {
		// One side accepts anything. A tag that says plain geometry over a
		// constrained column is a declaration that would create the wrong
		// column in managed mode, so it is still worth reporting — but as the
		// modifier difference it is, not as three separate ones.
		if want.Constrained() != actual.Constrained() {
			r.spatialMismatch(em, f, col, actual,
				fmt.Sprintf("the tag says %s and the column is %s", want, actual),
				"one of them constrains the shape, dimensionality and coordinate system and the other accepts anything")
		}
		return
	}

	if want.Kind != actual.Kind {
		r.spatialMismatch(em, f, col, actual,
			fmt.Sprintf("the tag says %s and the column holds %s", want.Kind, actual.Kind),
			"a column constrained to one shape rejects every other one on write")
	}
	if want.SRID != actual.SRID {
		r.spatialMismatch(em, f, col, actual,
			fmt.Sprintf("the tag says SRID %d and the column is SRID %d", want.SRID, actual.SRID),
			"the same coordinates mean a different place in each system, and PostGIS refuses to relate geometries across them")
	}
	if want.Dim != actual.Dim {
		r.spatialMismatch(em, f, col, actual,
			fmt.Sprintf("the tag says %s and the column is %s", dimLabel(want.Dim), dimLabel(actual.Dim)),
			"the ordinates a column does not have are not stored, and the ones it requires cannot be left out")
	}
}

func (r *reconciler) spatialMismatch(em *model.EntityMapping, f *model.GoField, col *model.PGColumn, actual postgis.TypeMod, message, reason string) {
	r.add(diag.Finding{
		Code:    diag.E006,
		Message: fmt.Sprintf("%s does not match %s: %s", f.Name, col.Qualified(), message),
		Reason:  reason,
		Fix:     fmt.Sprintf("write the column's own type in the tag: `orm:\"type:%s\"`", actual),
		Entity:  em.Entity.Display(), Field: f.Name, GoType: f.Type.Src,
		Table: col.Table.Qualified(), Column: col.Name, PGType: actual.String(),
		Pos: f.Pos,
	})
}

func dimLabel(d postgis.Dim) string {
	if s := d.String(); s != "" {
		return "XY" + s
	}
	return "XY"
}
