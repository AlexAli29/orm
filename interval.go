package orm

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// PostgreSQL intervals.
//
// An interval is not a duration. PostgreSQL stores three independent
// components, and it stores them separately on purpose:
//
//	months          a calendar step
//	days            a civil day, which is 24 hours except when it is not
//	microseconds    an exact elapsed time
//
// The three are kept apart because they answer to different calendars. Adding
// one month to 31 January is 28 February, and adding one month to 31 March is
// 30 April, so a month is not a fixed number of days. Adding one day across a
// daylight-saving boundary in a zoned timestamp is 23 or 25 hours, so a day is
// not a fixed number of hours either. Only microseconds are exact.
//
// This is why the ORM does not map interval to time.Duration. A Duration is a
// count of nanoseconds and has nowhere to put "one month"; writing one as
// 30 days, or as 720 hours, produces a value that gives the wrong answer for
// most of the dates it is added to. Choosing that silently would be the kind of
// convenience that corrupts data, so [Interval] keeps all three components and
// [Interval.Duration] refuses the conversion rather than guessing.

// Interval is a PostgreSQL interval, with its three components kept apart.
//
// The zero value is the zero interval, which is what PostgreSQL writes as
// '00:00:00'. SQL NULL is not one of its states: a NOT NULL interval column is
// Interval and a nullable one is *Interval, the same convention every other
// type in this package follows.
//
// The fields are exported because there is nothing to maintain between them.
// Any combination of signs is a legal PostgreSQL interval — '14 mons -3 days
// +05:04:03' is a value the server will hand back unchanged — so there is no
// invariant a constructor would be protecting.
type Interval struct {
	// Months is the calendar-month component. PostgreSQL displays 14 of them as
	// "1 year 2 mons" but stores and returns the count of months.
	Months int32
	// Days is the civil-day component, distinct from 24 hours in a zone that
	// observes daylight saving.
	Days int32
	// Microseconds is the exact sub-day component. PostgreSQL's interval
	// resolution is one microsecond, so a Go Duration's nanoseconds do not
	// survive a round-trip.
	Microseconds int64
}

// Microsecond counts, for writing an interval's time component readably. They
// compose by addition:
//
//	orm.Interval{Months: 3, Microseconds: 90 * orm.Minutes}
//
// They are plural because Hour, Minute and Second already name the fields
// EXTRACT reads, and because a count reads better that way in an arithmetic
// expression. There is deliberately no Days or Months constant: those are
// separate fields precisely because they are not a number of microseconds.
const (
	Micros  int64 = 1
	Millis        = 1000 * Micros
	Seconds       = 1000 * Millis
	Minutes       = 60 * Seconds
	Hours         = 60 * Minutes
)

// IntervalOf builds an interval from its three components, for callers who
// prefer a call to a struct literal. It is the only constructor: every
// combination of the three is legal, so there is nothing else to offer.
func IntervalOf(months, days int32, microseconds int64) Interval {
	return Interval{Months: months, Days: days, Microseconds: microseconds}
}

// IntervalFromDuration converts a Go duration into an interval, exactly and in
// one direction.
//
// It is exact because a Duration has no calendar components to lose: the result
// has zero months and zero days, and the whole duration lands in Microseconds.
// Sub-microsecond precision is the one thing that does not survive, because
// PostgreSQL intervals have none, so the conversion truncates towards zero and
// says so here rather than surprising anyone later.
func IntervalFromDuration(d time.Duration) Interval {
	return Interval{Microseconds: int64(d / time.Microsecond)}
}

// ErrCalendarInterval is returned by [Interval.Duration] for an interval whose
// months or days are non-zero.
var ErrCalendarInterval = errors.New("interval has calendar components")

// Duration converts the interval to a Go duration, and refuses when it cannot
// do so exactly.
//
// It succeeds only for an interval whose months and days are both zero, because
// those two components have no fixed length: a month is 28 to 31 days depending
// on which month, and a day is 23 to 25 hours depending on the zone and the
// date. There is no correct number to return for them without knowing the
// instant the interval is being added to, and this function does not know it.
//
// An interval with calendar components is not a failure to be worked around. It
// is a value that means something a Duration cannot mean, and the way to use it
// is to send it to PostgreSQL — [Add] and [Sub] apply it to a timestamp with
// the calendar rules that make it correct.
func (i Interval) Duration() (time.Duration, error) {
	if i.Months != 0 || i.Days != 0 {
		return 0, fmt.Errorf("%w (%d months, %d days): a month and a day have no fixed length, so there is no duration that equals %s for every instant; add it to a timestamp in PostgreSQL instead",
			ErrCalendarInterval, i.Months, i.Days, i)
	}
	return time.Duration(i.Microseconds) * time.Microsecond, nil
}

// IsZero reports whether every component is zero.
func (i Interval) IsZero() bool { return i.Months == 0 && i.Days == 0 && i.Microseconds == 0 }

// String renders the interval with its components named, which is what makes a
// test failure legible: "14 mons -3 days 05:04:03.123456" says what
// "1 year 2 mons -3 days" and a duration never could.
func (i Interval) String() string {
	if i.IsZero() {
		return "00:00:00"
	}
	var parts []string
	if i.Months != 0 {
		parts = append(parts, fmt.Sprintf("%d mons", i.Months))
	}
	if i.Days != 0 {
		parts = append(parts, fmt.Sprintf("%d days", i.Days))
	}
	if i.Microseconds != 0 {
		us := i.Microseconds
		sign := ""
		if us < 0 {
			sign, us = "-", -us
		}
		h, rem := us/Hours, us%Hours
		m, rem := rem/Minutes, rem%Minutes
		s, frac := rem/Seconds, rem%Seconds
		t := fmt.Sprintf("%s%02d:%02d:%02d", sign, h, m, s)
		if frac != 0 {
			t += strings.TrimRight(fmt.Sprintf(".%06d", frac), "0")
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// IntervalValue implements pgx's interval protocol, which is how an Interval
// reaches PostgreSQL in the binary format. It is not the API to call.
func (i Interval) IntervalValue() (pgtype.Interval, error) {
	return pgtype.Interval{Months: i.Months, Days: i.Days, Microseconds: i.Microseconds, Valid: true}, nil
}

// ScanInterval implements pgx's interval protocol.
//
// A NULL is refused rather than read as the zero interval: the column is
// nullable and the Go type says it is not, and "no interval" and "an interval
// of no length" are different facts. A nullable interval column is *Interval.
func (i *Interval) ScanInterval(v pgtype.Interval) error {
	if !v.Valid {
		return errors.New("cannot scan NULL into orm.Interval: declare the field as *orm.Interval for a nullable interval column")
	}
	i.Months, i.Days, i.Microseconds = v.Months, v.Days, v.Microseconds
	return nil
}
