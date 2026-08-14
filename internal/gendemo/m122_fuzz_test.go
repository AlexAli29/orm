package gendemo_test

import (
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// M12.2 property tests.
//
// The property is not "what goes in comes out". PostgreSQL canonicalises a
// discrete range and a multirange of any kind, so the value that comes back may
// legitimately differ from the one that was sent. What has to hold is that the
// round trip reaches PostgreSQL's canonical form and stays there: sending a
// value back unchanged produces the same value again.
//
// That is the honest property, and it is stronger than it sounds. It catches a
// bound kind lost on the way out, an element decoded into the wrong side, an
// empty range read as an unbounded one, and a multirange whose components were
// reordered by the client rather than by the server. A naive equality assertion
// would catch none of those, because it would already be failing for the right
// reasons.

// fuzzConn opens one connection for a fuzz target. The targets need a server
// and no schema: every value goes out as a parameter and comes back as a
// result, so there is nothing to create.
func fuzzConn(f *testing.F) *pgx.Conn {
	f.Helper()
	testdb.AdminDSN(f)
	return testdb.Connect(f, testdb.Create(f, ""))
}

// boundOf maps an arbitrary byte onto a bound kind, so a fuzzer explores all
// four rather than the two a constructor would give it.
func boundOf(b byte) orm.BoundKind {
	switch b % 4 {
	case 0:
		return orm.BoundEmpty
	case 1:
		return orm.BoundUnbounded
	case 2:
		return orm.BoundInclusive
	default:
		return orm.BoundExclusive
	}
}

// FuzzRange_roundTrip sends an arbitrary int4range through PostgreSQL twice.
func FuzzRange_roundTrip(f *testing.F) {
	f.Add(int32(1), int32(10), byte(2), byte(3))
	f.Add(int32(0), int32(0), byte(0), byte(0))
	f.Add(int32(-5), int32(5), byte(1), byte(1))
	f.Add(int32(7), int32(7), byte(2), byte(3))

	conn := fuzzConn(f)
	f.Fuzz(func(t *testing.T, lo, hi int32, loK, hiK byte) {
		in := orm.NewRange(lo, boundOf(loK), hi, boundOf(hiK))

		var first orm.Range[int32]
		err := conn.QueryRow(t.Context(), "SELECT $1::int4range", in).Scan(&first)
		if err != nil {
			// PostgreSQL refuses a range whose lower bound exceeds its upper
			// one. That is a value error about the input, not a failure of the
			// encoding, and it is the only error this target accepts.
			if !isRangeBoundsError(err) {
				t.Fatalf("sending %s: %v", in, err)
			}
			return
		}

		var second orm.Range[int32]
		if err := conn.QueryRow(t.Context(), "SELECT $1::int4range", first).Scan(&second); err != nil {
			t.Fatalf("resending the canonical %s: %v", first, err)
		}
		if first != second {
			t.Fatalf("canonicalisation is not a fixed point: %s became %s", first, second)
		}
		// And PostgreSQL's own rendering agrees with the value model's, which
		// is what makes String worth reading in a failure.
		var text string
		if err := conn.QueryRow(t.Context(), "SELECT ($1::int4range)::text", first).Scan(&text); err != nil {
			t.Fatalf("rendering %s: %v", first, err)
		}
		if text != first.String() {
			t.Fatalf("PostgreSQL writes %s where the value model writes %s", text, first)
		}
		// An empty range is empty at both ends or at neither.
		lo, loKind := first.LowerBound()
		hi, hiKind := first.UpperBound()
		if (loKind == orm.BoundEmpty) != (hiKind == orm.BoundEmpty) {
			t.Fatalf("%s is empty at one end only", first)
		}
		// A bound with no value carries the zero, so two equal ranges compare
		// equal as Go values.
		if !loKind.Bounded() && lo != 0 {
			t.Fatalf("%s carries a lower value of %d behind an unbounded kind", first, lo)
		}
		if !hiKind.Bounded() && hi != 0 {
			t.Fatalf("%s carries an upper value of %d behind an unbounded kind", first, hi)
		}
	})
}

// FuzzRange_timestamps is the same property over a continuous family, where
// PostgreSQL keeps the bounds as written rather than canonicalising them.
func FuzzRange_timestamps(f *testing.F) {
	f.Add(int64(0), int64(86400), byte(2), byte(3))
	f.Add(int64(-1), int64(1), byte(1), byte(2))

	conn := fuzzConn(f)
	f.Fuzz(func(t *testing.T, loSec, hiSec int64, loK, hiK byte) {
		// PostgreSQL's timestamp range is far narrower than int64 seconds, and
		// a value outside it is a question about time.Time rather than about
		// ranges.
		const limit = 60 * 60 * 24 * 365 * 200
		if loSec > limit || loSec < -limit || hiSec > limit || hiSec < -limit {
			t.Skip()
		}
		lo := time.Unix(loSec, 0).UTC()
		hi := time.Unix(hiSec, 0).UTC()
		in := orm.NewRange(lo, boundOf(loK), hi, boundOf(hiK))

		var first orm.Range[time.Time]
		if err := conn.QueryRow(t.Context(), "SELECT $1::tstzrange", in).Scan(&first); err != nil {
			if !isRangeBoundsError(err) {
				t.Fatalf("sending %s: %v", in, err)
			}
			return
		}
		var second orm.Range[time.Time]
		if err := conn.QueryRow(t.Context(), "SELECT $1::tstzrange", first).Scan(&second); err != nil {
			t.Fatalf("resending: %v", err)
		}
		fLo, fLoK := first.LowerBound()
		sLo, sLoK := second.LowerBound()
		fHi, fHiK := first.UpperBound()
		sHi, sHiK := second.UpperBound()
		if fLoK != sLoK || fHiK != sHiK || !fLo.Equal(sLo) || !fHi.Equal(sHi) {
			t.Fatalf("the round trip is not a fixed point: %s became %s", first, second)
		}
	})
}

// FuzzMultirange_roundTrip sends a small arbitrary multirange and checks the
// canonical form is reached and kept.
func FuzzMultirange_roundTrip(f *testing.F) {
	f.Add(int32(1), int32(5), int32(3), int32(9), byte(2), byte(3))
	f.Add(int32(10), int32(20), int32(1), int32(5), byte(2), byte(3))
	f.Add(int32(0), int32(0), int32(0), int32(0), byte(0), byte(0))

	conn := fuzzConn(f)
	f.Fuzz(func(t *testing.T, aLo, aHi, bLo, bHi int32, loK, hiK byte) {
		in := orm.Multirange[int32]{
			orm.NewRange(aLo, boundOf(loK), aHi, boundOf(hiK)),
			orm.NewRange(bLo, boundOf(hiK), bHi, boundOf(loK)),
		}
		var first orm.Multirange[int32]
		if err := conn.QueryRow(t.Context(), "SELECT $1::int4multirange", in).Scan(&first); err != nil {
			if !isRangeBoundsError(err) {
				t.Fatalf("sending %s: %v", in, err)
			}
			return
		}
		var second orm.Multirange[int32]
		if err := conn.QueryRow(t.Context(), "SELECT $1::int4multirange", first).Scan(&second); err != nil {
			t.Fatalf("resending the canonical %s: %v", first, err)
		}
		if first.String() != second.String() {
			t.Fatalf("canonicalisation is not a fixed point: %s became %s", first, second)
		}
		// PostgreSQL's canonical form has no empty components and is sorted
		// ascending with no two components touching.
		for i, r := range first {
			if r.IsEmpty() {
				t.Fatalf("%s kept an empty component at %d", first, i)
			}
		}
		for i := 1; i < len(first); i++ {
			prevHi, prevKind := first[i-1].UpperBound()
			curLo, curKind := first[i].LowerBound()
			if prevKind == orm.BoundUnbounded || curKind == orm.BoundUnbounded {
				t.Fatalf("%s has an unbounded component beside another", first)
			}
			if prevHi > curLo {
				t.Fatalf("%s is not sorted: component %d ends at %d, %d starts at %d",
					first, i-1, prevHi, i, curLo)
			}
		}
	})
}

// FuzzInterval_roundTrip is the exact one: an interval has no canonical form to
// converge to, so every component comes back as it went out.
func FuzzInterval_roundTrip(f *testing.F) {
	f.Add(int32(14), int32(-3), int64(18243123456))
	f.Add(int32(0), int32(0), int64(0))
	f.Add(int32(-1200), int32(9999), int64(-1))

	conn := fuzzConn(f)
	f.Fuzz(func(t *testing.T, months, days int32, micros int64) {
		in := orm.Interval{Months: months, Days: days, Microseconds: micros}
		var got orm.Interval
		if err := conn.QueryRow(t.Context(), "SELECT $1::interval", in).Scan(&got); err != nil {
			// An interval whose components overflow PostgreSQL's own range is a
			// value error, not an encoding one.
			if !isIntervalOverflow(err) {
				t.Fatalf("sending %+v: %v", in, err)
			}
			return
		}
		if got != in {
			t.Fatalf("interval round trip changed %+v into %+v", in, got)
		}
		// The refusal to approximate holds for every value, not just the
		// documented one.
		if _, err := got.Duration(); (err != nil) != (got.Months != 0 || got.Days != 0) {
			t.Fatalf("Duration of %+v gave err=%v", got, err)
		}
	})
}

func isRangeBoundsError(err error) bool {
	return containsAny(err.Error(),
		"range lower bound must be less than or equal to range upper bound",
		"timestamp out of range",
		"date out of range")
}

func isIntervalOverflow(err error) bool {
	return containsAny(err.Error(), "interval out of range", "out of range")
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) <= len(s) && indexOf(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
