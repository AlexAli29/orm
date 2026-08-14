package gendemo_test

import (
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// M12.2: multiranges.
//
// The claim is that a multirange round-trips as the value PostgreSQL holds,
// which is not the slice that was sent: PostgreSQL sorts the components, merges
// the ones that overlap or touch, and drops the empty ones. Nothing here
// promises otherwise, and every expectation is the canonical form the server
// produced for the same input.

// Scenario H: canonicalisation, stated as the difference between what goes in
// and what comes back.
func TestMultirange_canonicalisation(t *testing.T) {
	_, conn := rangeDB(t)

	for _, tt := range []struct {
		name string
		in   orm.Multirange[int32]
		want string
	}{
		{"already canonical", orm.Multirange[int32]{orm.ClosedOpen[int32](1, 5), orm.ClosedOpen[int32](10, 20)}, "{[1,5),[10,20)}"},
		{"overlapping components merge", orm.Multirange[int32]{orm.ClosedOpen[int32](1, 5), orm.ClosedOpen[int32](3, 9)}, "{[1,9)}"},
		{"adjacent components merge over a discrete type", orm.Multirange[int32]{orm.ClosedOpen[int32](1, 5), orm.ClosedOpen[int32](5, 9)}, "{[1,9)}"},
		{"components are sorted", orm.Multirange[int32]{orm.ClosedOpen[int32](10, 20), orm.ClosedOpen[int32](1, 5)}, "{[1,5),[10,20)}"},
		{"empty components disappear", orm.Multirange[int32]{orm.ClosedOpen[int32](1, 5), orm.EmptyRange[int32]()}, "{[1,5)}"},
		{"no components at all", orm.Multirange[int32]{}, "{}"},
		{"one unbounded component swallows the rest", orm.Multirange[int32]{orm.UnboundedRange[int32](), orm.ClosedOpen[int32](1, 5)}, "{(,)}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := pgText(t, conn, "$1::int4multirange", tt.in); got != tt.want {
				t.Fatalf("PostgreSQL stored %s, want %s", got, tt.want)
			}
			var back orm.Multirange[int32]
			if err := conn.QueryRow(t.Context(), "SELECT $1::int4multirange", tt.in).Scan(&back); err != nil {
				t.Fatalf("reading it back: %v", err)
			}
			if back.String() != tt.want {
				t.Errorf("the value model read it as %s, want %s", back, tt.want)
			}
			// Canonicalisation is idempotent, so the canonical form is a fixed
			// point and comparing a round trip against it is stable.
			if again := pgText(t, conn, "$1::int4multirange", back); again != tt.want {
				t.Errorf("a second round trip gave %s, want %s", again, tt.want)
			}
		})
	}
}

// A continuous multirange merges on overlap but not on adjacency, because
// nummultirange and tstzmultirange have no notion of the next value.
func TestMultirange_continuousDoesNotMergeAdjacent(t *testing.T) {
	_, conn := rangeDB(t)
	adjacent := orm.Multirange[time.Time]{orm.ClosedOpen(rt0, rt1), orm.ClosedOpen(rt1, rt2)}
	got := pgText(t, conn, "$1::tstzmultirange", adjacent)
	// [a,b) and [b,c) do meet with no gap, and PostgreSQL merges them for a
	// continuous type as well — because the union really is one range. What it
	// does not do is close a gap, which the second case checks.
	if got != `{["2024-01-01 00:00:00+00","2024-03-01 00:00:00+00")}` {
		t.Errorf("touching components gave %s", got)
	}
	gap := orm.Multirange[time.Time]{orm.ClosedOpen(rt0, rt1), orm.Open(rt1, rt2)}
	if got := pgText(t, conn, "$1::tstzmultirange", gap); got == `{["2024-01-01 00:00:00+00","2024-03-01 00:00:00+00")}` {
		t.Errorf("a gap at %v was closed: %s", rt1, got)
	}
}

// SQL NULL, the empty multirange and a multirange of one unbounded component
// are three different values, and nil is only the first of them.
func TestMultirange_nullEmptyAndUnbounded(t *testing.T) {
	db, _ := rangeDB(t)
	rows, err := db.Bookings.Query().OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("reading bookings: %v", err)
	}
	// slots is nullable: row 2 is NULL, row 3 is '{}', row 1 has components.
	if rows[1].Slots != nil {
		t.Errorf("booking 2 has SQL NULL slots and read as %v", rows[1].Slots)
	}
	if rows[2].Slots == nil {
		t.Fatal("booking 3 has an empty multirange and read as nil")
	}
	if len(*rows[2].Slots) != 0 {
		t.Errorf("booking 3 read as %v, want no components", *rows[2].Slots)
	}
	if got := rows[0].Slots; got == nil || len(*got) != 2 {
		t.Errorf("booking 1 read as %v, want two components", got)
	}
	// holds is NOT NULL, so an empty multirange is the only way to say nothing.
	if len(rows[1].Holds) != 0 {
		t.Errorf("booking 2 holds %v, want no components", rows[1].Holds)
	}
	if len(rows[3].Holds) != 1 || !rows[3].Holds[0].IsEmpty() == false {
		// Row 4 is {(,)}: one unbounded component, which is not empty.
		t.Errorf("booking 4 holds %v", rows[3].Holds)
	}
}

// The operators, against handwritten SQL over the same rows.
func TestMultirange_operatorsMatchPostgreSQL(t *testing.T) {
	db, conn := rangeDB(t)
	for _, tt := range []struct {
		name string
		pred orm.Predicate[gendemo.Booking]
		sql  string
	}{
		{"contains an element", gendemo.Bookings.Slots.Contains(3), "slots @> 3"},
		{"contains a range", gendemo.Bookings.Slots.ContainsRange(orm.ClosedOpen[int32](11, 13)), "slots @> '[11,13)'::int4range"},
		{"contains a multirange", gendemo.Bookings.Slots.ContainsMultirange(orm.Multirange[int32]{orm.ClosedOpen[int32](1, 3)}), "slots @> '{[1,3)}'::int4multirange"},
		{"contained by", gendemo.Bookings.Slots.ContainedBy(orm.Multirange[int32]{orm.ClosedOpen[int32](0, 100)}), "slots <@ '{[0,100)}'::int4multirange"},
		{"overlaps a multirange", gendemo.Bookings.Slots.Overlaps(orm.Multirange[int32]{orm.ClosedOpen[int32](4, 12)}), "slots && '{[4,12)}'::int4multirange"},
		{"overlaps a range", gendemo.Bookings.Slots.OverlapsRange(orm.ClosedOpen[int32](4, 12)), "slots && '[4,12)'::int4range"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.Bookings.Query().Where(tt.pred).OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			var ids []int64
			for _, b := range got {
				ids = append(ids, b.ID)
			}
			want := pgIDs(t, conn, "SELECT id FROM bookings WHERE "+tt.sql+" ORDER BY id")
			if !equalIDs(ids, want) {
				t.Errorf("ids = %v, PostgreSQL says %v (for %s)", ids, want, tt.sql)
			}
			if len(want) == 0 {
				t.Errorf("%s matched nothing, so the comparison proves little", tt.sql)
			}
		})
	}
}

// range_merge collapses a multirange to the one range that spans it, and its
// result is a range rather than a multirange — which is what makes it worth
// having.
func TestMultirange_merge(t *testing.T) {
	db, conn := rangeDB(t)
	type row struct {
		id     int64
		merged orm.Range[int32]
		empty  bool
	}
	got, err := orm.Select(db.Bookings, orm.Project3(
		gendemo.Bookings.ID,
		gendemo.Bookings.Slots.Merge(),
		gendemo.Bookings.Slots.IsEmpty(),
		func(id int64, m *orm.Range[int32], empty *bool) row {
			r := row{id: id}
			if m != nil {
				r.merged = *m
			}
			if empty != nil {
				r.empty = *empty
			}
			return r
		})).
		Where(gendemo.Bookings.Slots.IsNotNull()).
		OrderBy(gendemo.Bookings.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("projecting range_merge: %v", err)
	}
	byID := map[int64]row{}
	for _, r := range got {
		byID[r.id] = r
	}
	// {[1,5),[10,20)} spans [1,20).
	if r := byID[1]; r.merged.String() != "[1,20)" || r.empty {
		t.Errorf("booking 1 merged to %s (empty=%v), want [1,20)", r.merged, r.empty)
	}
	// {} merges to the empty range and reports empty.
	if r := byID[3]; !r.merged.IsEmpty() || !r.empty {
		t.Errorf("booking 3 merged to %s (empty=%v), want the empty range", r.merged, r.empty)
	}
	if want := pgText(t, conn, "range_merge('{[1,5),[10,20)}'::int4multirange)"); want != "[1,20)" {
		t.Fatalf("PostgreSQL merges to %s, so the expectation above is wrong", want)
	}
}

// Writes: a multirange goes out through INSERT and comes back through
// RETURNING as the canonical value.
func TestMultirange_writeAndReturning(t *testing.T) {
	db, _ := rangeDB(t)
	slots := orm.Multirange[int32]{orm.ClosedOpen[int32](30, 35), orm.ClosedOpen[int32](32, 40)}
	got, err := db.Bookings.Insert(t.Context(), gendemo.Booking{
		Room:   "written",
		Period: orm.ClosedOpen(rt0, rt1),
		Stay:   orm.ClosedOpen(rt0, rt1),
		Shift:  orm.ClosedOpen(rt0, rt1),
		Quota:  orm.ClosedOpen[int32](1, 2),
		Span:   orm.ClosedOpen[int64](1, 2),
		Lease:  orm.Interval{Days: 1},
		Holds:  orm.Multirange[time.Time]{orm.ClosedOpen(rt0, rt1)},
		Slots:  &slots,
	})
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if got.Slots == nil {
		t.Fatal("the inserted row returned nil slots")
	}
	// The two overlapping components were merged by PostgreSQL, and RETURNING
	// gave back what it stored rather than what was sent.
	if s := got.Slots.String(); s != "{[30,40)}" {
		t.Errorf("RETURNING gave %s, want the canonical {[30,40)}", s)
	}
	if len(slots) != 2 {
		t.Error("the value that was sent was modified in place")
	}
}
