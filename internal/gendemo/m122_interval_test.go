package gendemo_test

import (
	"errors"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
)

// M12.2: PostgreSQL intervals.
//
// The claim is that an interval is not a duration, and that the ORM keeps all
// three of its components rather than approximating any of them. The proof is a
// value with all three set — 14 months, -3 days, 05:04:03.123456 — going through
// PostgreSQL and coming back component by component.

var fullInterval = orm.Interval{Months: 14, Days: -3, Microseconds: 5*orm.Hours + 4*orm.Minutes + 3*orm.Seconds + 123456}

// Scenario J. The value in the seed was written by PostgreSQL's own parser from
// the text '14 mons -3 days 05:04:03.123456', so what this compares against is
// the server's reading of it rather than the ORM's.
func TestInterval_losslessRoundTrip(t *testing.T) {
	db, conn := rangeDB(t)

	t.Run("read", func(t *testing.T) {
		rows, err := db.Bookings.Query().Where(gendemo.Bookings.ID.Eq(int64(1))).All(t.Context())
		if err != nil {
			t.Fatalf("reading: %v", err)
		}
		got := rows[0].Lease
		if got != fullInterval {
			t.Errorf("lease = %+v (%s), want %+v (%s)", got, got, fullInterval, fullInterval)
		}
	})

	t.Run("write and read back", func(t *testing.T) {
		inserted, err := db.Bookings.Insert(t.Context(), gendemo.Booking{
			Room: "interval", Period: orm.ClosedOpen(rt0, rt1), Stay: orm.ClosedOpen(rt0, rt1),
			Shift: orm.ClosedOpen(rt0, rt1), Quota: orm.ClosedOpen[int32](1, 2),
			Span: orm.ClosedOpen[int64](1, 2), Lease: fullInterval,
			Holds: orm.Multirange[time.Time]{},
		})
		if err != nil {
			t.Fatalf("inserting: %v", err)
		}
		if inserted.Lease != fullInterval {
			t.Errorf("RETURNING gave %+v, want %+v", inserted.Lease, fullInterval)
		}
		// And PostgreSQL holds the components separately rather than a total.
		for _, tt := range []struct{ part, want string }{
			{"EXTRACT(YEAR FROM lease)", "1"},
			{"EXTRACT(MONTH FROM lease)", "2"},
			{"EXTRACT(DAY FROM lease)", "-3"},
			{"EXTRACT(HOUR FROM lease)", "5"},
			{"EXTRACT(MINUTE FROM lease)", "4"},
		} {
			got := pgTextFrom(t, conn, tt.part, "FROM bookings WHERE room = 'interval'")
			if got != tt.want {
				t.Errorf("%s = %s, want %s", tt.part, got, tt.want)
			}
		}
	})
}

// Stage 22: the differential matrix. Every component combination, including the
// signs, survives.
func TestInterval_differentialMatrix(t *testing.T) {
	db, conn := rangeDB(t)
	for _, tt := range []struct {
		name string
		in   orm.Interval
		want string
	}{
		{"zero", orm.Interval{}, "00:00:00"},
		{"months only", orm.Interval{Months: 3}, "3 mons"},
		{"a year in months", orm.Interval{Months: 12}, "1 year"},
		{"days only", orm.Interval{Days: 45}, "45 days"},
		{"microseconds only", orm.Interval{Microseconds: 90 * orm.Minutes}, "01:30:00"},
		{"sub-second", orm.Interval{Microseconds: 123456}, "00:00:00.123456"},
		{"negative months", orm.Interval{Months: -5}, "-5 mons"},
		{"negative days", orm.Interval{Days: -2}, "-2 days"},
		{"negative time", orm.Interval{Microseconds: -3 * orm.Hours}, "-03:00:00"},
		{"mixed signs", fullInterval, "1 year 2 mons -3 days +05:04:03.123456"},
		{"large but valid", orm.Interval{Months: 12000, Days: 30000, Microseconds: 1000 * orm.Hours}, "1000 years 30000 days 1000:00:00"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := pgText(t, conn, "$1::interval", tt.in); got != tt.want {
				t.Fatalf("PostgreSQL read it as %s, want %s", got, tt.want)
			}
			var back orm.Interval
			if err := conn.QueryRow(t.Context(), "SELECT $1::interval", tt.in).Scan(&back); err != nil {
				t.Fatalf("reading it back: %v", err)
			}
			if back != tt.in {
				t.Errorf("round trip gave %+v, want %+v", back, tt.in)
			}
		})
	}

	// SQL NULL is the pointer being nil, and nothing else.
	rows, err := db.Bookings.Query().OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if rows[0].Grace == nil {
		t.Error("booking 1 has a grace period and read as nil")
	}
	if rows[1].Grace != nil {
		t.Errorf("booking 2 has SQL NULL grace and read as %v", rows[1].Grace)
	}
	// The zero interval is a value, not NULL: booking 3's lease is '0 seconds'.
	if !rows[2].Lease.IsZero() {
		t.Errorf("booking 3's lease is %v, want the zero interval", rows[2].Lease)
	}
}

// The one conversion offered, and the one it refuses.
func TestInterval_durationConversion(t *testing.T) {
	t.Run("exact when there are no calendar components", func(t *testing.T) {
		iv := orm.Interval{Microseconds: 90*orm.Minutes + 500*orm.Millis}
		d, err := iv.Duration()
		if err != nil {
			t.Fatalf("Duration: %v", err)
		}
		if want := 90*time.Minute + 500*time.Millisecond; d != want {
			t.Errorf("Duration = %v, want %v", d, want)
		}
	})

	t.Run("refused when months or days are set", func(t *testing.T) {
		for _, iv := range []orm.Interval{{Months: 1}, {Days: 1}, fullInterval} {
			if _, err := iv.Duration(); !errors.Is(err, orm.ErrCalendarInterval) {
				t.Errorf("Duration of %v gave %v, want ErrCalendarInterval", iv, err)
			}
		}
	})

	t.Run("a duration converts exactly, the other way", func(t *testing.T) {
		iv := orm.IntervalFromDuration(36 * time.Hour)
		if iv.Months != 0 || iv.Days != 0 || iv.Microseconds != 36*orm.Hours {
			t.Errorf("IntervalFromDuration(36h) = %+v", iv)
		}
		// Thirty-six hours is not "1 day 12 hours": a day is a calendar unit
		// and a duration has none, so it all lands in the time component.
		if iv.Days != 0 {
			t.Error("a duration acquired calendar days")
		}
	})

	t.Run("sub-microsecond precision is what a round trip costs", func(t *testing.T) {
		iv := orm.IntervalFromDuration(1500 * time.Nanosecond)
		if iv.Microseconds != 1 {
			t.Errorf("1500ns became %d microseconds, want 1 (truncated)", iv.Microseconds)
		}
	})
}

// Scenario K, and Stage 19's interval half: arithmetic with the result types
// PostgreSQL actually produces.
func TestInterval_arithmetic(t *testing.T) {
	db, conn := rangeDB(t)

	t.Run("timestamptz plus an interval, and PostgreSQL's calendar", func(t *testing.T) {
		type row struct {
			id     int64
			later  time.Time
			sooner time.Time
		}
		got, err := orm.Compose(db.Executor(), orm.Project3(
			orm.Of(gendemo.Bookings.ID),
			orm.AddInterval(gendemo.Bookings.StartsAt, gendemo.Bookings.Lease),
			orm.SubInterval(gendemo.Bookings.StartsAt, gendemo.Bookings.Lease),
			func(id int64, later, sooner time.Time) row { return row{id, later, sooner} })).
			From(gendemo.Bookings.Source()).
			Where(orm.Cond(gendemo.Bookings.ID.Eq(int64(1)))).
			All(t.Context())
		if err != nil {
			t.Fatalf("interval arithmetic: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("read %d rows, want 1", len(got))
		}
		// Booking 1 starts on 31 January 2024 and its lease is
		// 14 months -3 days 05:04:03.123456. PostgreSQL applies the months as
		// calendar steps, which is the whole reason the components are kept
		// apart: 31 January plus 14 months is 31 March, not "31 January plus
		// 426 days".
		// PostgreSQL renders a timestamptz with a UTC offset, so the comparison
		// carries one too rather than dropping information from both sides.
		const layout = "2006-01-02 15:04:05.999999-07"
		want := pgTextFrom(t, conn, "starts_at + lease", "FROM bookings WHERE id = 1")
		if s := got[0].later.UTC().Format(layout); s != want {
			t.Errorf("starts_at + lease = %s, PostgreSQL says %s", s, want)
		}
		wantSooner := pgTextFrom(t, conn, "starts_at - lease", "FROM bookings WHERE id = 1")
		if s := got[0].sooner.UTC().Format(layout); s != wantSooner {
			t.Errorf("starts_at - lease = %s, PostgreSQL says %s", s, wantSooner)
		}
		// 31 January 2024 plus 14 months is 31 March 2025 — a calendar step —
		// and then three days back. A duration of "426 days" would land
		// somewhere else entirely.
		if s := got[0].later.UTC().Format("2006-01-02"); s != "2025-03-28" {
			t.Errorf("the calendar step landed on %s", s)
		}
	})

	t.Run("interval plus interval, and scaling", func(t *testing.T) {
		type row struct {
			sum    orm.Interval
			diff   orm.Interval
			scaled orm.Interval
		}
		got, err := orm.Compose(db.Executor(), orm.Project3(
			orm.IntervalPlus(gendemo.Bookings.Lease, gendemo.Bookings.Lease),
			orm.IntervalMinus(gendemo.Bookings.Lease, gendemo.Bookings.Lease),
			orm.IntervalTimes(gendemo.Bookings.Lease, orm.Cast(orm.Val(2.0), orm.DoublePrecision)),
			func(sum, diff, scaled orm.Interval) row { return row{sum, diff, scaled} })).
			From(gendemo.Bookings.Source()).
			Where(orm.Cond(gendemo.Bookings.ID.Eq(int64(1)))).
			All(t.Context())
		if err != nil {
			t.Fatalf("interval arithmetic: %v", err)
		}
		// Addition is component-wise and does not normalise between them.
		want := orm.Interval{Months: 28, Days: -6, Microseconds: 2 * fullInterval.Microseconds}
		if got[0].sum != want {
			t.Errorf("lease + lease = %+v, want %+v", got[0].sum, want)
		}
		if !got[0].diff.IsZero() {
			t.Errorf("lease - lease = %+v, want zero", got[0].diff)
		}
		if got[0].scaled != want {
			t.Errorf("lease * 2 = %+v, want %+v", got[0].scaled, want)
		}
	})

	t.Run("nullability travels through the arithmetic", func(t *testing.T) {
		type row struct{ later *time.Time }
		got, err := orm.Compose(db.Executor(), orm.Project1(
			orm.AddIntervalNull(orm.Of(gendemo.Bookings.StartsAt), orm.Of(gendemo.Bookings.Grace)),
			func(later *time.Time) row { return row{later} })).
			From(gendemo.Bookings.Source()).
			OrderBy(orm.Of(gendemo.Bookings.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("nullable interval arithmetic: %v", err)
		}
		// Booking 2's grace is SQL NULL, so the sum is NULL — which the *time.Time
		// destination can hold and a time.Time could not.
		if got[0].later == nil {
			t.Error("booking 1 has a grace period, so the sum is not NULL")
		}
		if got[1].later != nil {
			t.Errorf("booking 2 has no grace period, so the sum is NULL, got %v", got[1].later)
		}
	})

	t.Run("PostgreSQL's own result types", func(t *testing.T) {
		for _, tt := range []struct {
			sql      string
			wantType string
		}{
			{"now() + '1 month'::interval", "timestamp with time zone"},
			{"now() - '1 month'::interval", "timestamp with time zone"},
			{"'2024-01-31'::timestamp + '1 month'::interval", "timestamp without time zone"},
			{"current_date + '1 month'::interval", "timestamp without time zone"},
			{"'1 month'::interval + '2 days'::interval", "interval"},
			{"'1 month'::interval - '2 days'::interval", "interval"},
			{"'1 month'::interval * 2.5", "interval"},
			{"NULL::timestamptz + '1 month'::interval", "timestamp with time zone"},
		} {
			var ty string
			if err := conn.QueryRow(t.Context(), "SELECT pg_typeof("+tt.sql+")::text").Scan(&ty); err != nil {
				t.Fatalf("%s: %v", tt.sql, err)
			}
			if ty != tt.wantType {
				t.Errorf("pg_typeof(%s) = %s, want %s", tt.sql, ty, tt.wantType)
			}
		}
	})

	t.Run("adding a month is a calendar step, not thirty days", func(t *testing.T) {
		// The proof that mapping interval to a duration would be wrong: adding
		// one month to 31 January is 29 February in a leap year, and adding
		// thirty days is 1 March.
		if got := pgText(t, conn, "('2024-01-31'::date + '1 month'::interval)"); got != "2024-02-29 00:00:00" {
			t.Errorf("31 January plus one month is %s", got)
		}
		if got := pgText(t, conn, "('2024-01-31'::date + '30 days'::interval)"); got != "2024-03-01 00:00:00" {
			t.Errorf("31 January plus thirty days is %s", got)
		}
	})
}
