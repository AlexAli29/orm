package gendemo_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/jackc/pgx/v5"
)

// The M12 final audit: attacks on M12.2 rather than demonstrations of it.
//
// Each of these was written to fail. What they go after is the class of defect
// that a green suite hides: a value that is silently wrong, a type that claims
// more than PostgreSQL will give, and state a caller can still reach after the
// ORM has taken a copy of it.

// Item 101, release-critical if it holds: a Multirange is a slice, and a slice
// passed to a builder is a window onto the caller's memory. If the expression
// tree keeps the caller's backing array, mutating it after the predicate is
// built changes what the statement means — a class of bug that produces a wrong
// answer with no error anywhere.
func TestAudit_multirangeValueIsNotAliased(t *testing.T) {
	db, _ := rangeDB(t)

	m := orm.Multirange[int32]{orm.ClosedOpen[int32](1, 4)}
	pred := gendemo.Bookings.Slots.Overlaps(m)

	// Build the SQL once, then vandalise the caller's slice.
	before, argsBefore, err := db.Bookings.Query().Where(pred).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	m[0] = orm.ClosedOpen[int32](900, 999)

	after, argsAfter, err := db.Bookings.Query().Where(pred).SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	if before != after {
		t.Errorf("the SQL changed: %s -> %s", before, after)
	}
	if len(argsBefore) != 1 || len(argsAfter) != 1 {
		t.Fatalf("args = %v / %v", argsBefore, argsAfter)
	}
	got, ok := argsAfter[0].(orm.Multirange[int32])
	if !ok {
		t.Fatalf("the argument is %T, want a Multirange", argsAfter[0])
	}
	if s := got.String(); s != "{[1,4)}" {
		t.Errorf("mutating the caller's slice changed the predicate's argument to %s;"+
			" the expression must hold a copy", s)
	}
	if len(argsBefore) == 1 {
		if b, ok := argsBefore[0].(orm.Multirange[int32]); ok && b.String() != "{[1,4)}" {
			t.Errorf("the first rendering's argument also changed, to %s", b)
		}
	}

	// Every other way a multirange becomes an argument, for the same reason.
	for _, tt := range []struct {
		name string
		pred orm.Predicate[gendemo.Booking]
	}{
		{"ContainsMultirange", vandalised(gendemo.Bookings.Slots.ContainsMultirange)},
		{"ContainedBy", vandalised(gendemo.Bookings.Slots.ContainedBy)},
		{"Overlaps", vandalised(gendemo.Bookings.Slots.Overlaps)},
		{"Eq", vandalised(gendemo.Bookings.Slots.Eq)},
		{"Ne", vandalised(gendemo.Bookings.Slots.Ne)},
		{"In", vandalised(func(m orm.Multirange[int32]) orm.Predicate[gendemo.Booking] {
			return gendemo.Bookings.Slots.In(m)
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, args, err := db.Bookings.Query().Where(tt.pred).SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			for _, a := range args {
				if m, ok := a.(orm.Multirange[int32]); ok && m.String() != "{[1,4)}" {
					t.Errorf("the argument is %s; the caller's later write reached it", m)
				}
			}
		})
	}

	t.Run("Set", func(t *testing.T) {
		m := orm.Multirange[int32]{orm.ClosedOpen[int32](1, 4)}
		assign := gendemo.Bookings.Slots.Set(m)
		m[0] = orm.ClosedOpen[int32](900, 999)
		_, args, err := db.Bookings.Update().
			Set(assign).
			Where(gendemo.Bookings.ID.Eq(int64(1))).SQL()
		if err != nil {
			t.Fatalf("SQL: %v", err)
		}
		for _, a := range args {
			if m, ok := a.(orm.Multirange[int32]); ok && m.String() != "{[1,4)}" {
				t.Errorf("the assignment holds %s; the caller's later write reached it", m)
			}
		}
	})
}

// vandalised builds a predicate from a fresh multirange and then writes to that
// slice. Whatever the expression kept is what the assertion sees: a copy still
// says {[1,4)}, and a shared backing array says {[900,999)}.
func vandalised(build func(orm.Multirange[int32]) orm.Predicate[gendemo.Booking]) orm.Predicate[gendemo.Booking] {
	m := orm.Multirange[int32]{orm.ClosedOpen[int32](1, 4)}
	p := build(m)
	m[0] = orm.ClosedOpen[int32](900, 999)
	return p
}

// The same question for a value on its way to the database: a multirange sent
// as an argument must not be readable back through the caller's slice header
// in a way that lets a later write change what was stored.
func TestAudit_multirangeSentValueIsStable(t *testing.T) {
	db, conn := rangeDB(t)

	m := orm.Multirange[int32]{orm.ClosedOpen[int32](50, 60)}
	if _, err := db.Bookings.Insert(t.Context(), gendemo.Booking{
		Room: "aliasing", StartsAt: rt0,
		Period: orm.ClosedOpen(rt0, rt1), Stay: orm.ClosedOpen(rt0, rt1), Shift: orm.ClosedOpen(rt0, rt1),
		Quota: orm.ClosedOpen[int32](1, 2), Span: orm.ClosedOpen[int64](1, 2),
		Lease: orm.Interval{Days: 1},
		Holds: orm.Multirange[time.Time]{}, Slots: &m,
	}); err != nil {
		t.Fatalf("inserting: %v", err)
	}
	m[0] = orm.ClosedOpen[int32](900, 999)

	if s := pgTextFrom(t, conn, "slots", "FROM bookings WHERE room = 'aliasing'"); s != "{[50,60)}" {
		t.Errorf("PostgreSQL holds %s after the caller mutated its slice", s)
	}
}

// Item 25: &< and &> are not <<' and >>'. A corpus where each of the four gives
// a different answer is the only way to catch an implementation that wired two
// of them to the same operator.
func TestAudit_extendOperatorsAreDistinct(t *testing.T) {
	db, conn := rangeDB(t)

	// quota values in the fixture: [1,10), [5,15), empty, (,).
	probe := orm.ClosedOpen[int32](5, 15)
	cases := []struct {
		name string
		pred orm.Predicate[gendemo.Booking]
		sql  string
	}{
		{"strictly left of", gendemo.Bookings.Quota.StrictlyLeftOf(probe), "quota << '[5,15)'::int4range"},
		{"strictly right of", gendemo.Bookings.Quota.StrictlyRightOf(probe), "quota >> '[5,15)'::int4range"},
		{"not right of", gendemo.Bookings.Quota.NotRightOf(probe), "quota &< '[5,15)'::int4range"},
		{"not left of", gendemo.Bookings.Quota.NotLeftOf(probe), "quota &> '[5,15)'::int4range"},
	}

	results := map[string][]int64{}
	for _, c := range cases {
		got, err := db.Bookings.Query().Where(c.pred).OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var ids []int64
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		want := pgIDs(t, conn, "SELECT id FROM bookings WHERE "+c.sql+" ORDER BY id")
		if !equalIDs(ids, want) {
			t.Errorf("%s: ids = %v, PostgreSQL says %v", c.name, ids, want)
		}
		results[c.name] = ids
	}

	// &< and << must differ on this corpus, and so must &> and >>. If they did
	// not, the test would prove nothing about which operator was emitted.
	if equalIDs(results["strictly left of"], results["not right of"]) {
		t.Errorf("<< and &< gave the same rows (%v); the corpus does not separate them",
			results["not right of"])
	}
	if equalIDs(results["strictly right of"], results["not left of"]) {
		t.Errorf(">> and &> gave the same rows (%v); the corpus does not separate them",
			results["not left of"])
	}
}

// Item 43: the package-level operators take expressions on both sides, so no
// cast is inserted for them. A bare bind parameter under an overloaded operator
// is exactly what broke JSONIndex, so the question is whether these are usable
// at all — and if they resolve, whether they resolve to the operator meant.
func TestAudit_expressionLevelRangeOperators(t *testing.T) {
	db, conn := rangeDB(t)

	t.Run("column against column, both typed", func(t *testing.T) {
		other := gendemo.Bookings.As("other")
		got, err := orm.Compose(db.Executor(), orm.Project2(
			orm.Of(gendemo.Bookings.ID), orm.Of(other.ID),
			func(a, b int64) [2]int64 { return [2]int64{a, b} })).
			From(gendemo.Bookings.Source()).
			Join(other.Source(), orm.RangeOverlaps(orm.Of(gendemo.Bookings.Quota), orm.Of(other.Quota))).
			OrderBy(orm.Of(gendemo.Bookings.ID).Asc(), orm.Of(other.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("RangeOverlaps over two columns: %v", err)
		}
		var pairs []string
		for _, p := range got {
			pairs = append(pairs, fmt.Sprintf("%d-%d", p[0], p[1]))
		}
		want := pgPairs(t, conn, `SELECT a.id, b.id FROM bookings a, bookings b
			WHERE a.quota && b.quota ORDER BY a.id, b.id`)
		if strings.Join(pairs, ",") != strings.Join(want, ",") {
			t.Errorf("pairs = %v, PostgreSQL says %v", pairs, want)
		}
		if len(want) == 0 {
			t.Error("no pair overlapped, so the comparison proves little")
		}
	})

	t.Run("a literal element needs its type stated, and says so", func(t *testing.T) {
		// orm.Val is a bare parameter and carries no PostgreSQL type. Under an
		// overloaded operator that is not a silent wrong answer: PostgreSQL
		// refuses the statement. Casting is the documented spelling.
		_, err := orm.Compose(db.Executor(), orm.Project1(
			orm.Of(gendemo.Bookings.ID), func(id int64) int64 { return id })).
			From(gendemo.Bookings.Source()).
			Where(orm.RangeContains(orm.Of(gendemo.Bookings.Quota), orm.Val(int32(5)))).
			All(t.Context())
		if err == nil {
			t.Log("a bare parameter resolved; PostgreSQL chose an overload for it")
		}

		got, err := orm.Compose(db.Executor(), orm.Project1(
			orm.Of(gendemo.Bookings.ID), func(id int64) int64 { return id })).
			From(gendemo.Bookings.Source()).
			Where(orm.RangeContains(orm.Of(gendemo.Bookings.Quota), orm.Cast(orm.Val(int32(5)), orm.Integer))).
			OrderBy(orm.Of(gendemo.Bookings.ID).Asc()).
			All(t.Context())
		if err != nil {
			t.Fatalf("a cast element: %v", err)
		}
		want := pgIDs(t, conn, "SELECT id FROM bookings WHERE quota @> 5 ORDER BY id")
		if !equalIDs(got, want) {
			t.Errorf("ids = %v, PostgreSQL says %v", got, want)
		}
	})
}

// Item 18: a daterange column takes a time.Time, and a time.Time carries a
// time of day and a location that a date cannot hold. What happens to them is a
// contract, and it has to be PostgreSQL's answer rather than a client-side
// reinterpretation nobody wrote down.
func TestAudit_daterangeDropsTimeOfDay(t *testing.T) {
	db, conn := rangeDB(t)

	// A time of day, in a zone that is not UTC, on both bounds.
	loc := time.FixedZone("plus5", 5*60*60)
	lo := time.Date(2024, 6, 1, 23, 30, 0, 0, loc)
	hi := time.Date(2024, 6, 10, 1, 15, 0, 0, loc)

	inserted, err := db.Bookings.Insert(t.Context(), gendemo.Booking{
		Room: "dates", StartsAt: rt0,
		Period: orm.ClosedOpen(rt0, rt1),
		Stay:   orm.ClosedOpen(lo, hi),
		Shift:  orm.ClosedOpen(rt0, rt1),
		Quota:  orm.ClosedOpen[int32](1, 2), Span: orm.ClosedOpen[int64](1, 2),
		Lease: orm.Interval{Days: 1}, Holds: orm.Multirange[time.Time]{},
	})
	if err != nil {
		t.Fatalf("inserting a daterange from a zoned timestamp: %v", err)
	}

	// PostgreSQL decides. Whatever it stored is what RETURNING must report, and
	// the two have to be the same fact.
	stored := pgTextFrom(t, conn, "stay", "FROM bookings WHERE room = 'dates'")
	t.Logf("a daterange built from %v..%v is stored as %s", lo, hi, stored)

	// The bounds that come back have no time of day, because a date has none.
	sLo, _ := inserted.Stay.LowerBound()
	if sLo.Hour() != 0 || sLo.Minute() != 0 || sLo.Second() != 0 || sLo.Nanosecond() != 0 {
		t.Errorf("the lower bound came back as %v, which carries a time of day a date cannot hold", sLo)
	}
	// And the date is the one PostgreSQL chose, not one Go picked: reading the
	// stored text is the only authority worth comparing against.
	if !strings.HasPrefix(stored, "["+sLo.Format("2006-01-02")+",") {
		t.Errorf("the returned lower bound %v does not agree with the stored %s", sLo, stored)
	}
}

// Item 17: two timestamps written with different offsets that denote the same
// instant are one value in a tstzrange, and the ORM must not make them two.
func TestAudit_tstzrangeIsAboutInstants(t *testing.T) {
	_, conn := rangeDB(t)

	utc := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)
	plus5 := utc.In(time.FixedZone("plus5", 5*60*60))
	minus8 := utc.In(time.FixedZone("minus8", -8*60*60))

	a := orm.ClosedOpen(utc, utc.Add(24*time.Hour))
	b := orm.ClosedOpen(plus5, plus5.Add(24*time.Hour))
	c := orm.ClosedOpen(minus8, minus8.Add(24*time.Hour))

	var equalAB, equalAC bool
	if err := conn.QueryRow(t.Context(),
		"SELECT $1::tstzrange = $2::tstzrange, $1::tstzrange = $3::tstzrange", a, b, c).
		Scan(&equalAB, &equalAC); err != nil {
		t.Fatalf("comparing: %v", err)
	}
	if !equalAB || !equalAC {
		t.Errorf("three spellings of one instant compared as %v / %v; a tstzrange is about instants", equalAB, equalAC)
	}

	// A tsrange is not: it holds a wall clock reading with no offset, so the
	// three spellings are three different values there.
	var tsEqualAB bool
	if err := conn.QueryRow(t.Context(),
		"SELECT $1::tsrange = $2::tsrange", a, b).Scan(&tsEqualAB); err != nil {
		t.Fatalf("comparing as tsrange: %v", err)
	}
	if tsEqualAB {
		t.Error("two different wall clock readings compared equal as a tsrange")
	}
}

// Item 47, mandatory: one month and thirty days are different intervals, and
// nothing in the ORM may make them equal.
func TestAudit_monthIsNotThirtyDays(t *testing.T) {
	_, conn := rangeDB(t)

	month := orm.Interval{Months: 1}
	thirty := orm.Interval{Days: 30}

	if month == thirty {
		t.Fatal("one month and thirty days are the same Go value")
	}

	var pgEqual bool
	if err := conn.QueryRow(t.Context(), "SELECT $1::interval = $2::interval", month, thirty).Scan(&pgEqual); err != nil {
		t.Fatalf("comparing: %v", err)
	}
	// PostgreSQL compares intervals by converting to a canonical estimate, and
	// it happens to call these equal. That is PostgreSQL's comparison operator,
	// and it is not what "the same value" means: applied to a date they differ.
	t.Logf("PostgreSQL's interval = operator says %v for 1 month vs 30 days", pgEqual)

	var withMonth, withDays string
	if err := conn.QueryRow(t.Context(),
		"SELECT ('2024-01-31'::date + $1::interval)::text, ('2024-01-31'::date + $2::interval)::text",
		month, thirty).Scan(&withMonth, &withDays); err != nil {
		t.Fatalf("applying: %v", err)
	}
	if withMonth == withDays {
		t.Errorf("31 January plus one month and plus thirty days both gave %s", withMonth)
	}
	if withMonth != "2024-02-29 00:00:00" {
		t.Errorf("31 January plus one month is %s", withMonth)
	}
	if withDays != "2024-03-01 00:00:00" {
		t.Errorf("31 January plus thirty days is %s", withDays)
	}

	// And the round trip keeps them apart: a month does not become days.
	for _, iv := range []orm.Interval{month, thirty} {
		var back orm.Interval
		if err := conn.QueryRow(t.Context(), "SELECT $1::interval", iv).Scan(&back); err != nil {
			t.Fatalf("round trip: %v", err)
		}
		if back != iv {
			t.Errorf("%+v came back as %+v", iv, back)
		}
	}
}

// Item 49: microseconds are not milliseconds, and the boundary is where a
// truncation would hide.
func TestAudit_intervalMicrosecondPrecision(t *testing.T) {
	_, conn := rangeDB(t)
	for _, us := range []int64{1, 999, 1000, 1001, 999999, 1000001, -1, -999} {
		iv := orm.Interval{Microseconds: us}
		var back orm.Interval
		if err := conn.QueryRow(t.Context(), "SELECT $1::interval", iv).Scan(&back); err != nil {
			t.Fatalf("%d microseconds: %v", us, err)
		}
		if back.Microseconds != us {
			t.Errorf("%d microseconds came back as %d", us, back.Microseconds)
		}
	}
	// PostgreSQL has no nanoseconds, so a Duration carrying them truncates —
	// and the conversion says so by producing the truncated value rather than
	// rounding or erroring.
	if iv := orm.IntervalFromDuration(1999 * time.Nanosecond); iv.Microseconds != 1 {
		t.Errorf("1999ns became %d microseconds", iv.Microseconds)
	}
	if iv := orm.IntervalFromDuration(-1999 * time.Nanosecond); iv.Microseconds != -1 {
		t.Errorf("-1999ns became %d microseconds, want truncation towards zero", iv.Microseconds)
	}
}

// Item 67, release-critical: COPY is positional. Three columns holding the same
// Go type, named in an order that is not the table's, must still land where the
// caller said.
func TestAudit_copyColumnOrderWithIdenticalGoTypes(t *testing.T) {
	db, conn := rangeDB(t)

	// period, stay and shift are all orm.Range[time.Time] and are three
	// different PostgreSQL types, which is exactly the shape a positional bug
	// would survive in.
	distinct := orm.ClosedOpen(
		time.Date(2024, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 7, 5, 0, 0, 0, 0, time.UTC))
	other := orm.ClosedOpen(
		time.Date(2024, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 8, 9, 0, 0, 0, 0, time.UTC))
	third := orm.ClosedOpen(
		time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 9, 3, 0, 0, 0, 0, time.UTC))

	rows := []gendemo.Booking{{
		Room: "ordered", StartsAt: rt0,
		Period: distinct, Stay: other, Shift: third,
		Quota: orm.ClosedOpen[int32](1, 2), Span: orm.ClosedOpen[int64](1, 2),
		Lease: orm.Interval{Days: 1}, Holds: orm.Multirange[time.Time]{},
	}}

	// Name the columns in an order the table does not use.
	n, err := orm.CopyColumns(t.Context(), db.Bookings, rows,
		gendemo.Bookings.Shift, gendemo.Bookings.Holds, gendemo.Bookings.Lease,
		gendemo.Bookings.Stay, gendemo.Bookings.Span, gendemo.Bookings.Quota,
		gendemo.Bookings.Period, gendemo.Bookings.StartsAt, gendemo.Bookings.Room)
	if err != nil {
		t.Fatalf("CopyColumns: %v", err)
	}
	if n != 1 {
		t.Fatalf("copied %d rows", n)
	}

	got, err := db.Bookings.Query().Where(gendemo.Bookings.Room.Eq("ordered")).All(t.Context())
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d rows", len(got))
	}
	b := got[0]
	if lo, _ := b.Period.LowerBound(); lo.Month() != time.July {
		t.Errorf("period is %s, want the July range", b.Period)
	}
	if lo, _ := b.Stay.LowerBound(); lo.Month() != time.August {
		t.Errorf("stay is %s, want the August range", b.Stay)
	}
	if lo, _ := b.Shift.LowerBound(); lo.Month() != time.September {
		t.Errorf("shift is %s, want the September range", b.Shift)
	}
	// And the three columns are still three different PostgreSQL types.
	for _, tt := range []struct{ col, want string }{
		{"period", "tstzrange"}, {"stay", "daterange"}, {"shift", "tsrange"},
	} {
		if ty := pgTextFrom(t, conn, "pg_typeof("+tt.col+")", "FROM bookings WHERE room = 'ordered'"); ty != tt.want {
			t.Errorf("bookings.%s is %s, want %s", tt.col, ty, tt.want)
		}
	}
}

// Item 79: an index changes how PostgreSQL finds rows and never which rows
// there are. The same corpus, the same predicates, before and after.
func TestAudit_gistIndexDoesNotChangeAnswers(t *testing.T) {
	db, conn := rangeDB(t)

	probe := orm.ClosedOpen(rt0, rt2)
	run := func() []int64 {
		got, err := db.Bookings.Query().
			Where(gendemo.Bookings.Period.Overlaps(probe)).
			OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		var ids []int64
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		return ids
	}

	// The fixture already declares the index; drop it, ask, put it back, ask.
	withIndex := run()
	if _, err := conn.Exec(t.Context(), "DROP INDEX bookings_period_gist"); err != nil {
		t.Fatalf("dropping the index: %v", err)
	}
	without := run()
	if _, err := conn.Exec(t.Context(), "CREATE INDEX bookings_period_gist ON bookings USING gist (period)"); err != nil {
		t.Fatalf("recreating the index: %v", err)
	}
	again := run()

	if !equalIDs(withIndex, without) || !equalIDs(without, again) {
		t.Errorf("the index changed the answer: %v / %v / %v", withIndex, without, again)
	}
	if len(withIndex) == 0 {
		t.Error("the predicate matched nothing, so the comparison proves little")
	}
}

// Item 27: a predicate over a NULL range is NULL, not false, and the difference
// shows in NOT.
func TestAudit_nullRangePredicateIsUnknown(t *testing.T) {
	db, conn := rangeDB(t)

	// revised is NULL for bookings 2 and 3.
	matching, err := db.Bookings.Query().
		Where(gendemo.Bookings.Revised.Overlaps(orm.UnboundedRange[time.Time]())).
		OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	negated, err := db.Bookings.Query().
		Where(orm.Not(gendemo.Bookings.Revised.Overlaps(orm.UnboundedRange[time.Time]()))).
		OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("negated query: %v", err)
	}
	total, err := db.Bookings.Query().Count(t.Context())
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	if int64(len(matching)+len(negated)) == total {
		t.Errorf("the predicate and its negation partition the table (%d + %d = %d);"+
			" a NULL range must be neither", len(matching), len(negated), total)
	}
	want := pgIDs(t, conn, "SELECT id FROM bookings WHERE revised && '(,)'::tstzrange ORDER BY id")
	var ids []int64
	for _, b := range matching {
		ids = append(ids, b.ID)
	}
	if !equalIDs(ids, want) {
		t.Errorf("ids = %v, PostgreSQL says %v", ids, want)
	}
}

// pgPairs reads two-column integer rows as "a-b" strings.
func pgPairs(t *testing.T, conn *pgx.Conn, sql string) []string {
	t.Helper()
	rows, err := conn.Query(t.Context(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a, b int64
		if err := rows.Scan(&a, &b); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out = append(out, fmt.Sprintf("%d-%d", a, b))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return out
}

// Audit item 123, mutation 11: lower() must be marked nullable in its own
// metadata, not merely typed as a pointer.
//
// The two are different facts. The Go type decides what a destination can hold;
// the metadata is what tells the outer-join check that this expression already
// handles NULL, and it is read when a value expression is named into a source
// directly rather than lifted with Of or OfNull. Without it a legitimate query
// is refused — the safe direction, but still wrong — and nothing else in the
// suite reaches that path.
func TestAudit_rangeFunctionMetadataSaysNullable(t *testing.T) {
	db, _ := rangeDB(t)

	lo := orm.Named("lo", gendemo.Bookings.Quota.Lower())
	hi := orm.Named("hi", gendemo.Bookings.Quota.Upper())
	email := orm.Named("email", orm.Of(gendemo.Users.Email))

	// bookings is on the nullable side, so an item that is not marked nullable
	// is refused here. lower() and upper() are marked, so this compiles.
	sub := orm.Sub("d", orm.Rows(email, lo, hi).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Bookings.Source(), orm.Cond(gendemo.Bookings.Room.Eq("no such room"))))

	type row struct {
		email  string
		lo, hi *int32
	}
	got, err := orm.Compose(db.Executor(), orm.Project3(
		orm.Ref(sub, email), orm.Ref(sub, lo), orm.Ref(sub, hi),
		func(e string, a, b *int32) row { return row{e, a, b} })).
		From(sub).
		All(t.Context())
	if err != nil {
		t.Fatalf("naming lower()/upper() through an outer join was refused: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no rows, so the check proves nothing")
	}
	for _, r := range got {
		if r.lo != nil || r.hi != nil {
			t.Errorf("%s: %v / %v, want absent", r.email, r.lo, r.hi)
		}
	}

	// The counterpart: a range column itself is not nullable metadata, so
	// naming it directly through the same join is refused. That is what makes
	// the acceptance above a statement about lower() rather than about the
	// check being switched off.
	period := orm.Named("period", gendemo.Bookings.Period)
	_, _, err = orm.Compose(nil, orm.Project1(
		orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })).
		From(orm.Sub("e", orm.Rows(email, period).
			From(gendemo.Users.Source()).
			LeftJoin(gendemo.Bookings.Source(), orm.Cond(gendemo.Bookings.Room.Eq("x"))))).
		SQL()
	if err == nil {
		t.Error("a non-nullable range read through an outer join was accepted")
	}
}
