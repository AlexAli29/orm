package gendemo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// M12.2 through the composition layer M11 built.
//
// A range is not a special case. It reads through a derived table, a CTE and an
// outer join like every other value, its expressions declare the sources they
// name so scope validation can refuse the wrong ones, and an outer join makes it
// nullable whatever the column's own constraint says.

// Scenario L, and the release-critical half of Stage 18.
//
// bookings.period, bookings.quota and bookings.lease are all physically NOT
// NULL. Read through a LEFT JOIN that produces no matching row, every one of
// them is NULL — and no amount of NOT NULL in the catalog changes that.
func TestRangeCompose_outerJoinNullability(t *testing.T) {
	db, conn := rangeDB(t)

	type row struct {
		room        string
		period      *orm.Range[time.Time]
		quota       *orm.Range[int32]
		lease       *orm.Interval
		holds       *orm.Multirange[time.Time]
		lower       *int32
		quotaEmpty  *bool
		periodEmpty *bool
	}

	// users has no relation to bookings, which is the point: joining on a
	// condition that never holds is the cleanest way to produce a row whose
	// right-hand side is entirely absent.
	got, err := orm.Compose(db.Executor(), orm.Project8(
		orm.Of(gendemo.Users.Email),
		orm.Opt(gendemo.Bookings.Period),
		orm.Opt(gendemo.Bookings.Quota),
		orm.Opt(gendemo.Bookings.Lease),
		orm.Opt(gendemo.Bookings.Holds),
		// lower() is already nullable, so its nullable form is itself: OfNull
		// says that, where Opt would type it **int32 and be a compile error at
		// the destination.
		orm.OfNull(gendemo.Bookings.Quota.Lower()),
		orm.Opt(gendemo.Bookings.Quota.IsEmpty()),
		orm.Opt(gendemo.Bookings.Period.IsEmpty()),
		func(email string, period *orm.Range[time.Time], quota *orm.Range[int32],
			lease *orm.Interval, holds *orm.Multirange[time.Time],
			lower *int32, quotaEmpty, periodEmpty *bool) row {
			return row{email, period, quota, lease, holds, lower, quotaEmpty, periodEmpty}
		})).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Bookings.Source(), orm.Cond(gendemo.Bookings.Room.Eq("no such room"))).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("outer join over ranges: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the join produced no rows, so it proves nothing")
	}
	for _, r := range got {
		if r.period != nil || r.quota != nil || r.lease != nil || r.holds != nil {
			t.Errorf("%s: the absent side read as %v/%v/%v/%v, want all nil", r.room, r.period, r.quota, r.lease, r.holds)
		}
		if r.lower != nil || r.quotaEmpty != nil || r.periodEmpty != nil {
			t.Errorf("%s: functions over the absent side read as %v/%v/%v, want all nil",
				r.room, r.lower, r.quotaEmpty, r.periodEmpty)
		}
	}

	// The same statement in handwritten SQL says the same thing, which is what
	// makes the check about PostgreSQL rather than about the ORM.
	var nulls int64
	if err := conn.QueryRow(t.Context(), `
		SELECT count(*) FROM users
		LEFT JOIN bookings ON bookings.room = 'no such room'
		WHERE bookings.period IS NULL AND bookings.lease IS NULL
		  AND isempty(bookings.quota) IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("the handwritten comparison: %v", err)
	}
	if nulls != int64(len(got)) {
		t.Errorf("PostgreSQL found %d all-NULL rows, the ORM read %d", nulls, len(got))
	}
}

// A matched outer join produces values, so the nullable types are not merely
// always nil.
func TestRangeCompose_outerJoinWhenMatched(t *testing.T) {
	db, _ := rangeDB(t)
	type row struct {
		quota *orm.Range[int32]
		lease *orm.Interval
	}
	got, err := orm.Compose(db.Executor(), orm.Project2(
		orm.Opt(gendemo.Bookings.Quota),
		orm.Opt(gendemo.Bookings.Lease),
		func(q *orm.Range[int32], l *orm.Interval) row { return row{q, l} })).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Bookings.Source(), orm.Cond(gendemo.Bookings.Room.Eq("blue"))).
		Where(orm.Cond(gendemo.Users.ID.Eq(int64(1)))).
		All(t.Context())
	if err != nil {
		t.Fatalf("matched outer join: %v", err)
	}
	if len(got) != 1 || got[0].quota == nil || got[0].lease == nil {
		t.Fatalf("matched row = %+v, want values on both", got)
	}
	if s := got[0].quota.String(); s != "[1,10)" {
		t.Errorf("quota = %s, want [1,10)", s)
	}
	if *got[0].lease != fullInterval {
		t.Errorf("lease = %+v, want %+v", *got[0].lease, fullInterval)
	}
}

// Scenario M: a range read out of a derived table, and one computed there.
func TestRangeCompose_derivedSource(t *testing.T) {
	db, _ := rangeDB(t)

	periods := orm.Named("period", orm.Of(gendemo.Bookings.Period))
	lowers := orm.NamedNull("lo", orm.Of(gendemo.Bookings.Quota.Lower()))
	rooms := orm.Named("room", orm.Of(gendemo.Bookings.Room))
	sub := orm.Sub("b", orm.Rows(rooms, periods, lowers).
		From(gendemo.Bookings.Source()).
		Where(orm.Cond(gendemo.Bookings.Room.Eq("blue"))))

	type row struct {
		room   string
		period orm.Range[time.Time]
		lo     *int32
	}
	got, err := orm.Compose(db.Executor(), orm.Project3(
		orm.Ref(sub, rooms), orm.Ref(sub, periods), orm.Ref(sub, lowers),
		func(room string, p orm.Range[time.Time], lo *int32) row { return row{room, p, lo} })).
		From(sub).
		All(t.Context())
	if err != nil {
		t.Fatalf("derived range source: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows, want 1", len(got))
	}
	if got[0].room != "blue" {
		t.Errorf("room = %q", got[0].room)
	}
	if lo, kind := got[0].period.LowerBound(); kind != orm.BoundInclusive || !lo.Equal(rt0) {
		t.Errorf("period = %s, want it to start inclusively at %v", got[0].period, rt0)
	}
	if got[0].lo == nil || *got[0].lo != 1 {
		t.Errorf("lo = %v, want 1", got[0].lo)
	}
}

// A range through a CTE, which is the other way a typed source is built.
func TestRangeCompose_cte(t *testing.T) {
	db, _ := rangeDB(t)

	quotas := orm.Named("quota", orm.Of(gendemo.Bookings.Quota))
	ids := orm.Named("id", orm.Of(gendemo.Bookings.ID))
	cte := orm.CTE("bounded", orm.Rows(ids, quotas).
		From(gendemo.Bookings.Source()).
		Where(orm.Cond(gendemo.Bookings.Quota.IsEmptyIs(false))))

	type row struct {
		id    int64
		quota orm.Range[int32]
	}
	got, err := orm.Compose(db.Executor(), orm.Project2(
		orm.Ref(cte, ids), orm.Ref(cte, quotas),
		func(id int64, q orm.Range[int32]) row { return row{id, q} })).
		With(cte).From(cte).
		OrderBy(orm.Ref(cte, ids).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("range through a CTE: %v", err)
	}
	// Booking 3 is the empty one and is excluded; the rest come through.
	for _, r := range got {
		if r.id == 3 {
			t.Error("the empty range came through a filter that excluded it")
		}
		if r.quota.IsEmpty() {
			t.Errorf("booking %d read as empty", r.id)
		}
	}
	if len(got) < 2 {
		t.Fatalf("read %d rows, want the non-empty quotas", len(got))
	}
}

// Stage 17: every new expression declares the sources it names, so a statement
// that does not read those sources is refused before it reaches PostgreSQL.
func TestRangeCompose_sourceDependencies(t *testing.T) {
	db, _ := rangeDB(t)
	other := gendemo.Bookings.As("other")

	for _, tt := range []struct {
		name  string
		build func() (string, []any, error)
	}{
		{"a range operator naming an unread source", func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })).
				From(gendemo.Users.Source()).
				Where(orm.Cond(gendemo.Bookings.Quota.Contains(3))).
				SQL()
		}},
		{"a range function naming an unread source", func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(orm.Of(gendemo.Bookings.Quota.Lower()), func(v *int32) *int32 { return v })).
				From(gendemo.Users.Source()).
				SQL()
		}},
		{"interval arithmetic naming an unread source", func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(
				orm.AddInterval(gendemo.Bookings.StartsAt, gendemo.Bookings.Lease),
				func(v time.Time) time.Time { return v })).
				From(gendemo.Users.Source()).
				SQL()
		}},
		{"a multirange operator naming an unread source", func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })).
				From(gendemo.Users.Source()).
				Where(orm.Cond(gendemo.Bookings.Slots.Contains(3))).
				SQL()
		}},
		{"an alias is a different source from the table", func() (string, []any, error) {
			return orm.Compose(nil, orm.Project1(orm.Of(other.Quota.Lower()), func(v *int32) *int32 { return v })).
				From(gendemo.Bookings.Source()).
				SQL()
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sql, _, err := tt.build()
			if err == nil {
				t.Fatalf("the statement compiled: %s", sql)
			}
			if !strings.Contains(err.Error(), "scope error") {
				t.Errorf("error = %v, want a scope error", err)
			}
		})
	}

	// And the correct spelling of the last one works, which is what makes the
	// refusal above about scope rather than about ranges.
	sql, _, err := orm.Compose(nil, orm.Project1(orm.Of(other.Quota.Lower()), func(v *int32) *int32 { return v })).
		From(other.Source()).
		SQL()
	if err != nil {
		t.Fatalf("reading the alias it was built from: %v", err)
	}
	if !strings.Contains(sql, `"other"."quota"`) {
		t.Errorf("SQL = %s, want it qualified by the alias", sql)
	}

	// A self-join comparing two occurrences of one range column, which is the
	// operation an alias exists for.
	rows, err := orm.Compose(db.Executor(), orm.Project2(
		orm.Of(gendemo.Bookings.ID), orm.Of(other.ID),
		func(a, b int64) [2]int64 { return [2]int64{a, b} })).
		From(gendemo.Bookings.Source()).
		Join(other.Source(), orm.Cond(gendemo.Bookings.Period.OverlapsCol(other.Period))).
		Where(orm.Cond(gendemo.Bookings.ID.Lt(int64(3)))).
		OrderBy(orm.Of(gendemo.Bookings.ID).Asc(), orm.Of(other.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("self-join on overlapping periods: %v", err)
	}
	if len(rows) == 0 {
		t.Error("no pair of bookings overlapped, so the self-join proves little")
	}
}

// Stage 11: COPY carries ranges, multiranges and intervals, in every state.
func TestRangeCopy(t *testing.T) {
	db, conn := rangeDB(t)

	empty := orm.EmptyRange[time.Time]()
	unbounded := orm.UnboundedRange[time.Time]()
	slots := orm.Multirange[int32]{orm.ClosedOpen[int32](1, 4)}
	rows := []gendemo.Booking{
		{
			Room: "copy normal", StartsAt: rt0,
			Period: orm.ClosedOpen(rt0, rt1), Stay: orm.ClosedOpen(rt0, rt1), Shift: orm.ClosedOpen(rt0, rt1),
			Quota: orm.ClosedOpen[int32](1, 5), Span: orm.ClosedOpen[int64](10, 50),
			Revised: &unbounded, Lease: fullInterval, Grace: &orm.Interval{Days: 2},
			Holds: orm.Multirange[time.Time]{orm.ClosedOpen(rt0, rt1)}, Slots: &slots,
		},
		{
			Room: "copy empty and null", StartsAt: rt1,
			Period: empty, Stay: empty, Shift: empty,
			Quota: orm.EmptyRange[int32](), Span: orm.EmptyRange[int64](),
			Revised: nil, Lease: orm.Interval{}, Grace: nil,
			Holds: orm.Multirange[time.Time]{}, Slots: nil,
		},
		{
			Room: "copy unbounded", StartsAt: rt2,
			Period: unbounded, Stay: unbounded, Shift: unbounded,
			Quota: orm.RangeFrom[int32](7), Span: orm.RangeUntil[int64](70),
			Revised: &empty, Lease: orm.Interval{Months: -1}, Grace: nil,
			Holds: orm.Multirange[time.Time]{unbounded}, Slots: &orm.Multirange[int32]{},
		},
	}

	n, err := db.Bookings.CopyFrom(t.Context(), rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != int64(len(rows)) {
		t.Fatalf("CopyFrom copied %d rows, want %d", n, len(rows))
	}

	got, err := db.Bookings.Query().
		Where(gendemo.Bookings.Room.Like("copy%")).
		OrderBy(gendemo.Bookings.Room.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("reading the copied rows: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d copied rows, want 3", len(got))
	}

	byRoom := map[string]gendemo.Booking{}
	for _, b := range got {
		byRoom[b.Room] = b
	}

	normal := byRoom["copy normal"]
	if s := normal.Quota.String(); s != "[1,5)" {
		t.Errorf("a normal range copied as %s", s)
	}
	if normal.Lease != fullInterval {
		t.Errorf("an interval copied as %+v", normal.Lease)
	}
	if normal.Revised == nil || !normal.Revised.IsEmpty() == false {
		// (,) is not empty; this only checks it arrived at all.
		t.Errorf("a nullable range copied as %v", normal.Revised)
	}

	blank := byRoom["copy empty and null"]
	if !blank.Period.IsEmpty() || !blank.Quota.IsEmpty() {
		t.Errorf("an empty range copied as %s / %s", blank.Period, blank.Quota)
	}
	if blank.Revised != nil || blank.Grace != nil || blank.Slots != nil {
		t.Errorf("a NULL copied as %v / %v / %v", blank.Revised, blank.Grace, blank.Slots)
	}
	if len(blank.Holds) != 0 {
		t.Errorf("an empty multirange copied as %v", blank.Holds)
	}

	wide := byRoom["copy unbounded"]
	if _, kind := wide.Period.LowerBound(); kind != orm.BoundUnbounded {
		t.Errorf("an unbounded range copied as %s", wide.Period)
	}
	if wide.Revised == nil || !wide.Revised.IsEmpty() {
		t.Errorf("an empty range in a nullable column copied as %v", wide.Revised)
	}
	if wide.Slots == nil || len(*wide.Slots) != 0 {
		t.Errorf("an empty multirange in a nullable column copied as %v", wide.Slots)
	}

	// PostgreSQL's own reading of what arrived.
	if s := pgTextFrom(t, conn, "quota", "FROM bookings WHERE room = 'copy unbounded'"); s != "[7,)" {
		t.Errorf("PostgreSQL holds %s, want [7,)", s)
	}
	if s := pgTextFrom(t, conn, "revised IS NULL", "FROM bookings WHERE room = 'copy unbounded'"); s != "false" {
		t.Errorf("an empty range was stored as NULL")
	}
}

// COPY runs inside a transaction and rolls back with it, so a failure leaves no
// half-written ranges behind.
func TestRangeCopy_rollback(t *testing.T) {
	db, _ := rangeDB(t)

	before, err := db.Bookings.Query().Count(t.Context())
	if err != nil {
		t.Fatalf("counting: %v", err)
	}

	sentinel := errRollback{}
	err = db.Tx(t.Context(), func(tx *gendemo.DB) error {
		if _, err := tx.Bookings.CopyFrom(t.Context(), []gendemo.Booking{{
			Room: "rolled back", StartsAt: rt0,
			Period: orm.ClosedOpen(rt0, rt1), Stay: orm.ClosedOpen(rt0, rt1), Shift: orm.ClosedOpen(rt0, rt1),
			Quota: orm.ClosedOpen[int32](1, 2), Span: orm.ClosedOpen[int64](1, 2),
			Lease: orm.Interval{Days: 1}, Holds: orm.Multirange[time.Time]{},
		}}); err != nil {
			return err
		}
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("Tx returned %v, want the sentinel", err)
	}

	after, err := db.Bookings.Query().Count(t.Context())
	if err != nil {
		t.Fatalf("counting again: %v", err)
	}
	if after != before {
		t.Errorf("the rolled-back COPY left %d extra rows", after-before)
	}
}

type errRollback struct{}

func (errRollback) Error() string { return "rolling back on purpose" }
