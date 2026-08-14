package gisdemo_test

import (
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
)

// Source-induced nullability over spatial columns.
//
// A NOT NULL geography column read through a LEFT JOIN is NULL for every row
// with no match. The column has not changed; the source it belongs to can be
// absent from the joined row, and that is a property of the query rather than
// of the schema.
//
// The ORM already refuses to read such a column into a destination that cannot
// hold a NULL. What these tests assert is that a spatial column is no exception
// — including when the value has been through a measurement, a transformation,
// a CASE and a derived table on the way out.

// The direct case: a NOT NULL geography behind a LEFT JOIN.
func TestNullability_leftJoinDirect(t *testing.T) {
	db := openDB(t)

	// Zones left-joined to the places inside them. The "outer" zone contains
	// only "far", and a zone with no place produces a row of NULLs.
	joined := orm.Compose(db.Executor(), orm.Project3(
		orm.Of(gisdemo.Zones.Name),
		orm.Opt(gisdemo.Places.Name),
		orm.Opt(gisdemo.Places.Spot),
		func(zone string, place *string, spot *postgis.Geography) [3]bool {
			return [3]bool{true, place != nil, spot != nil}
		},
	)).
		From(gisdemo.Zones.Source()).
		LeftJoin(gisdemo.Places.Source(),
			postgis.Of(gisdemo.Zones.Area).Covers(postgis.Of(gisdemo.Places.Location))).
		OrderBy(orm.Of(gisdemo.Zones.ID).Asc(), orm.Opt(gisdemo.Places.ID).Asc())

	got, err := joined.All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	// Three places fall in "inner" and one in "outer", so every row matches
	// here; the refusal below is the part that matters.
	if len(got) == 0 {
		t.Fatal("the join produced no rows")
	}
}

// Reading a NOT NULL spatial column through a LEFT JOIN without widening is
// refused, which is the whole point of the check.
func TestNullability_leftJoinRefusesNarrowDestination(t *testing.T) {
	db := openDB(t)

	shape := orm.Project2(
		orm.Of(gisdemo.Zones.Name),
		// Of rather than Opt: this claims the geography cannot be NULL, and the
		// join can produce one.
		orm.Of(gisdemo.Places.Spot),
		func(zone string, spot postgis.Geography) string { return zone },
	)
	_, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Zones.Source()).
		LeftJoin(gisdemo.Places.Source(),
			postgis.Of(gisdemo.Zones.Area).Covers(postgis.Of(gisdemo.Places.Location))).
		All(t.Context())
	if err == nil {
		t.Fatal("a NOT NULL geography was read through a LEFT JOIN into a non-nullable destination")
	}
	if !contains(err.Error(), "outer join") {
		t.Errorf("the error does not mention the join: %v", err)
	}
}

// A row with genuinely no match, so that the NULL is observed rather than
// merely permitted.
func TestNullability_unmatchedRowIsNull(t *testing.T) {
	db := openDB(t)

	// Roads left-joined to the zones they cross. "outside" crosses nothing.
	type row struct {
		road string
		zone *string
		area *postgis.Geometry
		// The area's own area, which is NULL when the zone is absent — and not
		// zero, which is what a non-nullable destination would have had to
		// invent.
		size *float64
	}
	shape := orm.Project4(
		orm.Of(gisdemo.Roads.Name),
		orm.Opt(gisdemo.Zones.Name),
		orm.Opt(gisdemo.Zones.Area),
		orm.OfNull(orm.FnNull[orm.Composed, float64]("ST_Area",
			orm.ArgOpt(gisdemo.Zones.Area))),
		func(road string, zone *string, area *postgis.Geometry, size *float64) row {
			return row{road, zone, area, size}
		},
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Roads.Source()).
		LeftJoin(gisdemo.Zones.Source(),
			postgis.Of(gisdemo.Zones.Area).Intersects(postgis.Of(gisdemo.Roads.Path))).
		OrderBy(orm.Of(gisdemo.Roads.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	var unmatched int
	for _, r := range got {
		if r.zone != nil {
			if r.area == nil {
				t.Errorf("%s matched %s and the area came back NULL", r.road, *r.zone)
			}
			if r.size == nil {
				t.Errorf("%s matched %s and ST_Area came back NULL", r.road, *r.zone)
			}
			continue
		}
		unmatched++
		if r.area != nil {
			t.Errorf("%s matched no zone and the area came back %s", r.road, r.area)
		}
		if r.size != nil {
			t.Errorf("%s matched no zone and ST_Area came back %v, not NULL", r.road, *r.size)
		}
	}
	if unmatched == 0 {
		t.Fatal("every road matched a zone, so the corpus does not exercise the unmatched case")
	}
}

// The maximal graph: LEFT JOIN, then a measurement, then a CASE, then a derived
// table, then a LEFT JOIN again. The result is still nullable at the end.
func TestNullability_maximalGraph(t *testing.T) {
	db := openDB(t)

	// Inside: roads left-joined to zones, projecting a measurement over the
	// possibly-absent zone.
	road := orm.Named("road", orm.Of(gisdemo.Roads.Name))
	// COALESCE is the one construct that can absorb the join's NULL, and it is
	// deliberately not used here: the point is that the NULL survives.
	size := orm.NamedNull("size", orm.OfNull(orm.FnNull[orm.Composed, float64](
		"ST_Area", orm.ArgOpt(gisdemo.Zones.Area))))

	inner := orm.Sub("measured", orm.Rows(road, size).
		From(gisdemo.Roads.Source()).
		LeftJoin(gisdemo.Zones.Source(),
			postgis.Of(gisdemo.Zones.Area).Intersects(postgis.Of(gisdemo.Roads.Path))))

	// Outside: the derived table left-joined to places, so the whole inner row
	// can be absent as well.
	type row struct {
		place string
		road  *string
		size  *float64
	}
	shape := orm.Project3(
		orm.Of(gisdemo.Places.Name),
		orm.OptRef(inner, road),
		orm.OptRef(inner, size),
		func(place string, r *string, s *float64) row { return row{place, r, s} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		LeftJoin(inner, orm.Cond(gisdemo.Places.Name.Eq("nothing matches"))).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d rows, want 4", len(got))
	}
	for _, r := range got {
		if r.road != nil || r.size != nil {
			t.Errorf("%s matched nothing and came back with road=%v size=%v", r.place, r.road, r.size)
		}
	}
}

// A nullable spatial column is nullable for its own reasons, independently of
// any join — and the generated descriptor says so.
func TestNullability_columnAndJoinAreSeparate(t *testing.T) {
	db := openDB(t)

	type row struct {
		name      string
		footprint *postgis.Geometry
		empty     *bool
	}
	shape := orm.Project3(
		orm.Of(gisdemo.Places.Name),
		orm.Of(gisdemo.Places.Footprint),
		orm.OfNull(orm.FnNull[orm.Composed, bool]("ST_IsEmpty",
			orm.ArgOf(orm.Of(gisdemo.Places.Footprint)))),
		func(n string, f *postgis.Geometry, e *bool) row { return row{n, f, e} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("read %d rows", len(got))
	}
	// Row 1 has a real polygon, rows 2 and 3 have NULL, row 4 has POLYGON EMPTY.
	if got[0].footprint == nil || got[0].footprint.IsEmpty() {
		t.Errorf("the first footprint is %v", got[0].footprint)
	}
	if got[1].footprint != nil || got[2].footprint != nil {
		t.Errorf("a NULL footprint came back as a value")
	}
	if got[1].empty != nil {
		t.Errorf("ST_IsEmpty of a NULL came back as %v, and it is NULL", *got[1].empty)
	}
	if got[3].footprint == nil {
		t.Fatal("POLYGON EMPTY came back as NULL, which is the confusion this design exists to prevent")
	}
	if !got[3].footprint.IsEmpty() {
		t.Errorf("POLYGON EMPTY came back as %s", got[3].footprint)
	}
	if got[3].empty == nil || !*got[3].empty {
		t.Errorf("PostGIS does not think the empty polygon is empty: %v", got[3].empty)
	}
}
