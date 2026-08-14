package gendemo_test

import (
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M12.2 compile-time cost.
//
// These measure building and rendering statements, not the database. A range
// predicate carries one extra cast node compared to an ordinary comparison, and
// the point of measuring is to know that the cost is that and not more.

func BenchmarkRange_onePredicate(b *testing.B) {
	for b.Loop() {
		_, _, err := gendemo.New(nil).Bookings.Query().
			Where(gendemo.Bookings.Quota.Contains(5)).
			SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRange_tenPredicates(b *testing.B) {
	probe := orm.ClosedOpen[int32](5, 15)
	for b.Loop() {
		_, _, err := gendemo.New(nil).Bookings.Query().
			Where(
				gendemo.Bookings.Quota.Contains(5),
				gendemo.Bookings.Quota.ContainsRange(probe),
				gendemo.Bookings.Quota.ContainedBy(probe),
				gendemo.Bookings.Quota.Overlaps(probe),
				gendemo.Bookings.Quota.StrictlyLeftOf(probe),
				gendemo.Bookings.Quota.StrictlyRightOf(probe),
				gendemo.Bookings.Quota.NotRightOf(probe),
				gendemo.Bookings.Quota.NotLeftOf(probe),
				gendemo.Bookings.Quota.Adjacent(probe),
				gendemo.Bookings.Period.Overlaps(orm.ClosedOpen(rt0, rt1)),
			).SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The comparison that makes the number above mean something: an ordinary
// predicate over the same table.
func BenchmarkRange_plainPredicateForComparison(b *testing.B) {
	for b.Loop() {
		_, _, err := gendemo.New(nil).Bookings.Query().
			Where(gendemo.Bookings.Room.Eq("blue")).
			SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMultirange_predicate(b *testing.B) {
	m := orm.Multirange[int32]{orm.ClosedOpen[int32](1, 5), orm.ClosedOpen[int32](10, 20)}
	for b.Loop() {
		_, _, err := gendemo.New(nil).Bookings.Query().
			Where(gendemo.Bookings.Slots.Overlaps(m)).
			SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkInterval_arithmetic(b *testing.B) {
	shape := orm.Project2(
		orm.Of(gendemo.Bookings.ID),
		orm.AddInterval(gendemo.Bookings.StartsAt, gendemo.Bookings.Lease),
		func(id int64, t time.Time) int64 { return id })
	for b.Loop() {
		_, _, err := orm.Compose(nil, shape).From(gendemo.Bookings.Source()).SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// The range functions, which is the other shape a range column produces.
func BenchmarkRange_functionProjection(b *testing.B) {
	shape := orm.Project3(
		gendemo.Bookings.Quota.Lower(),
		gendemo.Bookings.Quota.Upper(),
		gendemo.Bookings.Quota.IsEmpty(),
		func(lo, hi *int32, empty bool) int32 { return 0 })
	for b.Loop() {
		_, _, err := orm.Select(gendemo.New(nil).Bookings, shape).SQL()
		if err != nil {
			b.Fatal(err)
		}
	}
}

// COPY of rows carrying ranges, multiranges and an interval, measured against
// real PostgreSQL because the encoding is the part worth knowing about.
func BenchmarkRange_copyFrom(b *testing.B) {
	testdb.AdminDSN(b)
	db := freshDB(b)
	rows := make([]gendemo.Booking, 1000)
	for i := range rows {
		rows[i] = gendemo.Booking{
			Room: "bench", StartsAt: rt0,
			Period: orm.ClosedOpen(rt0, rt1), Stay: orm.ClosedOpen(rt0, rt1),
			Shift: orm.ClosedOpen(rt0, rt1),
			Quota: orm.ClosedOpen(int32(i), int32(i+10)),
			Span:  orm.ClosedOpen(int64(i), int64(i+100)),
			Lease: fullInterval,
			Holds: orm.Multirange[time.Time]{orm.ClosedOpen(rt0, rt1)},
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Bookings.CopyFrom(b.Context(), rows); err != nil {
			b.Fatal(err)
		}
	}
}
