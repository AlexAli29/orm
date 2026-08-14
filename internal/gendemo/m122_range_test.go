package gendemo_test

import (
	"context"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// M12.2: PostgreSQL ranges.
//
// The claim under test is that a range survives the round trip as the value
// PostgreSQL holds — all six bound shapes, the canonical form of a discrete
// range, and the difference between SQL NULL, the empty range and an unbounded
// one — and that the operators and functions built over it are PostgreSQL's own
// with PostgreSQL's nullability.
//
// Nothing here compares the ORM against itself. Every expected value is either
// a handwritten SQL statement run through pgx on the same connection, or a
// literal taken from PostgreSQL's documented behaviour and then checked against
// the server.

var (
	rt0 = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	rt1 = time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)
	rt2 = time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
)

// bookingSeed writes rows with plain SQL, so what the ORM reads was not written
// by the thing under test.
const bookingSeed = `
INSERT INTO bookings (id, room, starts_at, period, stay, shift, quota, span, revised, lease, grace, holds, slots) VALUES
  (1, 'blue',  '2024-01-31T00:00:00Z', '[2024-01-01,2024-02-01)', '[2024-01-01,2024-02-01)', '[2024-01-01,2024-02-01)',
      '[1,10)', '[100,1000)', '[2024-01-15,2024-01-20)', '14 mons -3 days 05:04:03.123456', '1 day',
      '{[2024-01-01,2024-02-01)}', '{[1,5),[10,20)}'),
  (2, 'green', '2024-02-01T00:00:00Z', '[2024-02-01,2024-03-01)', '[2024-02-01,2024-03-01)', '[2024-02-01,2024-03-01)',
      '[5,15)', '[500,1500)', NULL, '00:30:00', NULL,
      '{}', NULL),
  (3, 'grey',  '2024-03-01T00:00:00Z', 'empty', 'empty', 'empty', 'empty', 'empty', NULL, '0 seconds', NULL, '{}', '{}'),
  (4, 'wide',  '2024-04-01T00:00:00Z', '(,)', '(,)', '(,)', '(,)', '(,)', '(,)', '1 mon', '2 mons',
      '{(,)}', '{(,)}');
`

func rangeDB(t *testing.T) (*gendemo.DB, *pgx.Conn) {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed+bookingSeed)
	pool := poolFor(t, dsn)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	if err := gendemo.RegisterTypes(t.Context(), conn); err != nil {
		t.Fatalf("registering types: %v", err)
	}
	// The seed inserts explicit ids, which leaves the identity sequence at one.
	// Any database whose rows arrived through the identity is already past that
	// point, and a write test should not be measuring the fixture.
	if _, err := conn.Exec(t.Context(),
		`SELECT setval(pg_get_serial_sequence('bookings', 'id'),
			GREATEST(coalesce((SELECT max(id) FROM bookings), 0), 1))`); err != nil {
		t.Fatalf("advancing the bookings sequence: %v", err)
	}
	return gendemo.New(pool), conn
}

// pgText evaluates an expression on the raw connection and returns its text, so
// that a test can compare what the ORM produced against what PostgreSQL says.
func pgText(t *testing.T, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	return pgTextFrom(t, conn, sql, "", args...)
}

// pgTextFrom is pgText over a row of a table, with the FROM outside the cast.
func pgTextFrom(t *testing.T, conn *pgx.Conn, sql, tail string, args ...any) string {
	t.Helper()
	stmt := "SELECT (" + sql + ")::text"
	if tail != "" {
		stmt += " " + tail
	}
	var s string
	if err := conn.QueryRow(t.Context(), stmt, args...).Scan(&s); err != nil {
		t.Fatalf("evaluating %s: %v", stmt, err)
	}
	return s
}

// Scenario A, and the whole reason Stage 1 existed: three instantiations of one
// generic origin are three types, and the generated descriptors prove it.
//
// The compiler enforces the rest — Quota.Contains(int64) does not build — and
// the permanent proof of that is in the compile-fail suite. What is checkable
// here is that the descriptors carry the element types the schema implies.
func TestRange_instantiationsStayDistinct(t *testing.T) {
	var (
		_ orm.RangeCol[gendemo.Booking, int32]          = gendemo.Bookings.Quota
		_ orm.RangeCol[gendemo.Booking, int64]          = gendemo.Bookings.Span
		_ orm.RangeCol[gendemo.Booking, time.Time]      = gendemo.Bookings.Period
		_ orm.RangeCol[gendemo.Booking, time.Time]      = gendemo.Bookings.Stay
		_ orm.RangeCol[gendemo.Booking, time.Time]      = gendemo.Bookings.Shift
		_ orm.NullRangeCol[gendemo.Booking, time.Time]  = gendemo.Bookings.Revised
		_ orm.MultirangeCol[gendemo.Booking, time.Time] = gendemo.Bookings.Holds
		_ orm.NullMultirangeCol[gendemo.Booking, int32] = gendemo.Bookings.Slots
	)

	// Scenario F: three columns, one Go element type, three PostgreSQL types.
	// The Go side cannot tell them apart and never has to — the catalog does.
	_, conn := rangeDB(t)
	for _, tt := range []struct{ column, want string }{
		{"period", "tstzrange"},
		{"stay", "daterange"},
		{"shift", "tsrange"},
		{"quota", "int4range"},
		{"span", "int8range"},
		{"holds", "tstzmultirange"},
		{"slots", "int4multirange"},
		{"lease", "interval"},
	} {
		got := pgTextFrom(t, conn, "pg_typeof("+tt.column+")", "FROM bookings WHERE id = 1")
		if got != tt.want {
			t.Errorf("bookings.%s is %s, want %s", tt.column, got, tt.want)
		}
	}
}

// The value model represents every state PostgreSQL has, and a round trip
// returns the value the server holds.
//
// Scenario B and C live here: [1,10) contains 1 and 9 but not 10, and a
// discrete range comes back canonicalised.
func TestRange_boundShapesRoundTrip(t *testing.T) {
	_, conn := rangeDB(t)

	for _, tt := range []struct {
		name string
		in   orm.Range[int32]
		// want is what PostgreSQL stores, which for a discrete range is the
		// canonical form rather than what was sent.
		want string
	}{
		{"closed", orm.Closed[int32](1, 10), "[1,11)"},
		{"open", orm.Open[int32](1, 10), "[2,10)"},
		{"closed-open", orm.ClosedOpen[int32](1, 10), "[1,10)"},
		{"open-closed", orm.OpenClosed[int32](1, 10), "[2,11)"},
		{"lower-unbounded", orm.RangeUntil[int32](10), "(,10)"},
		{"upper-unbounded", orm.RangeFrom[int32](1), "[1,)"},
		{"fully unbounded", orm.UnboundedRange[int32](), "(,)"},
		{"empty", orm.EmptyRange[int32](), "empty"},
		{"empty by collapse", orm.ClosedOpen[int32](1, 1), "empty"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := pgText(t, conn, "$1::int4range", tt.in)
			if got != tt.want {
				t.Fatalf("PostgreSQL stored %s, want %s", got, tt.want)
			}
			// And back again, into the value model.
			var back orm.Range[int32]
			if err := conn.QueryRow(t.Context(), "SELECT $1::int4range", tt.in).Scan(&back); err != nil {
				t.Fatalf("reading it back: %v", err)
			}
			if back.String() != tt.want {
				t.Errorf("the value model read it as %s, want %s", back, tt.want)
			}
			// A second round trip is a fixed point: canonicalisation happens
			// once, so what came back goes out unchanged.
			if again := pgText(t, conn, "$1::int4range", back); again != tt.want {
				t.Errorf("a second round trip gave %s, want %s", again, tt.want)
			}
		})
	}
}

// A continuous range keeps its bounds exactly as written, which is the half of
// canonicalisation the discrete cases hide.
func TestRange_continuousKeepsItsBounds(t *testing.T) {
	_, conn := rangeDB(t)
	for _, tt := range []struct {
		name string
		in   orm.Range[time.Time]
		want string
	}{
		{"closed", orm.Closed(rt0, rt1), `["2024-01-01 00:00:00+00","2024-02-01 00:00:00+00"]`},
		{"open", orm.Open(rt0, rt1), `("2024-01-01 00:00:00+00","2024-02-01 00:00:00+00")`},
		{"open-closed", orm.OpenClosed(rt0, rt1), `("2024-01-01 00:00:00+00","2024-02-01 00:00:00+00"]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := pgText(t, conn, "$1::tstzrange", tt.in); got != tt.want {
				t.Errorf("PostgreSQL stored %s, want %s", got, tt.want)
			}
		})
	}
	// daterange is discrete, so the same bounds canonicalise where tstzrange's
	// did not. This is the difference the two families keep despite sharing a
	// Go element type.
	if got := pgText(t, conn, "$1::daterange", orm.Closed(rt0, rt1)); got != "[2024-01-01,2024-02-02)" {
		t.Errorf("daterange stored %s, want [2024-01-01,2024-02-02)", got)
	}
}

// Scenario G: SQL NULL, the empty range and the unbounded range are three
// different things, and the Go types keep them apart.
func TestRange_nullEmptyAndUnbounded(t *testing.T) {
	db, conn := rangeDB(t)

	rows, err := db.Bookings.Query().OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("reading bookings: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("read %d bookings, want 4", len(rows))
	}

	// Row 1 has a value; row 2 has SQL NULL; row 3 is empty; row 4 is unbounded.
	if rows[0].Revised == nil {
		t.Error("booking 1 has a revised period and read as nil")
	}
	if rows[1].Revised != nil {
		t.Errorf("booking 2 has SQL NULL and read as %v", rows[1].Revised)
	}
	if !rows[2].Period.IsEmpty() {
		t.Errorf("booking 3 is empty and read as %v", rows[2].Period)
	}
	if rows[2].Revised != nil {
		t.Error("booking 3 has a NULL revised period and read as non-nil")
	}
	// An empty range is a value: it is not nil, and it is not unbounded.
	if rows[3].Period.IsEmpty() {
		t.Errorf("booking 4 is unbounded and read as empty")
	}
	lo, loKind := rows[3].Period.LowerBound()
	hi, hiKind := rows[3].Period.UpperBound()
	if loKind != orm.BoundUnbounded || hiKind != orm.BoundUnbounded {
		t.Errorf("booking 4 read as %v..%v (%s..%s), want unbounded on both sides", lo, hi, loKind, hiKind)
	}

	// And PostgreSQL agrees about which is which: only rows 2 and 3 are NULL,
	// while row 4 holds an unbounded range, which is a value.
	for _, tt := range []struct{ id, want string }{
		{"1", "false"}, {"2", "true"}, {"3", "true"}, {"4", "false"},
	} {
		if got := pgTextFrom(t, conn, "revised IS NULL", "FROM bookings WHERE id = "+tt.id); got != tt.want {
			t.Errorf("booking %s: revised IS NULL is %s, want %s", tt.id, got, tt.want)
		}
	}
}

// Scenario B: containment at the boundaries, against PostgreSQL.
func TestRange_containmentBoundaries(t *testing.T) {
	db, conn := rangeDB(t)
	// bookings.quota for room 'blue' is [1,10).
	for _, tt := range []struct {
		v    int32
		want bool
	}{
		{0, false}, {1, true}, {5, true}, {9, true}, {10, false}, {11, false},
	} {
		got, err := db.Bookings.Query().
			Where(gendemo.Bookings.Room.Eq("blue"), gendemo.Bookings.Quota.Contains(tt.v)).
			Exists(t.Context())
		if err != nil {
			t.Fatalf("quota @> %d: %v", tt.v, err)
		}
		want := pgText(t, conn, "'[1,10)'::int4range @> $1::int4", tt.v) == "true"
		if want != tt.want {
			t.Fatalf("the test's own expectation for %d disagrees with PostgreSQL", tt.v)
		}
		if got != tt.want {
			t.Errorf("quota @> %d is %v, want %v", tt.v, got, tt.want)
		}
	}
}

// Every operator, against a handwritten statement over the same rows.
func TestRange_operatorsMatchPostgreSQL(t *testing.T) {
	db, conn := rangeDB(t)

	probe := orm.ClosedOpen[int32](5, 15)
	for _, tt := range []struct {
		name string
		pred orm.Predicate[gendemo.Booking]
		sql  string
	}{
		{"contains range", gendemo.Bookings.Quota.ContainsRange(orm.ClosedOpen[int32](6, 8)), "quota @> '[6,8)'::int4range"},
		{"contained by", gendemo.Bookings.Quota.ContainedBy(orm.ClosedOpen[int32](0, 100)), "quota <@ '[0,100)'::int4range"},
		{"overlaps", gendemo.Bookings.Quota.Overlaps(probe), "quota && '[5,15)'::int4range"},
		{"strictly left of", gendemo.Bookings.Quota.StrictlyLeftOf(orm.ClosedOpen[int32](20, 30)), "quota << '[20,30)'::int4range"},
		{"strictly right of", gendemo.Bookings.Quota.StrictlyRightOf(orm.ClosedOpen[int32](-5, 0)), "quota >> '[-5,0)'::int4range"},
		{"not right of", gendemo.Bookings.Quota.NotRightOf(orm.ClosedOpen[int32](0, 20)), "quota &< '[0,20)'::int4range"},
		{"not left of", gendemo.Bookings.Quota.NotLeftOf(orm.ClosedOpen[int32](0, 20)), "quota &> '[0,20)'::int4range"},
		{"adjacent", gendemo.Bookings.Quota.Adjacent(orm.ClosedOpen[int32](10, 20)), "quota -|- '[10,20)'::int4range"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := db.Bookings.Query().Where(tt.pred).
				OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
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

// Scenario D: the booking overlap query, which is what tstzrange is for.
func TestRange_bookingOverlap(t *testing.T) {
	db, conn := rangeDB(t)
	requested := orm.ClosedOpen(
		time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC),
		time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC),
	)
	got, err := db.Bookings.Query().
		Where(gendemo.Bookings.Period.Overlaps(requested)).
		OrderBy(gendemo.Bookings.ID.Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("overlap query: %v", err)
	}
	var ids []int64
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	want := pgIDs(t, conn,
		`SELECT id FROM bookings WHERE period && '[2024-01-15,2024-02-10)'::tstzrange ORDER BY id`)
	if !equalIDs(ids, want) {
		t.Errorf("overlapping bookings = %v, PostgreSQL says %v", ids, want)
	}
	// Rooms 'blue' and 'green' both collide with it; 'grey' is empty and
	// collides with nothing.
	if len(ids) != 3 {
		t.Errorf("ids = %v, want the two bounded bookings and the unbounded one", ids)
	}
}

// Stage 5 and Stage 19: the range functions, their result types and their
// nullability, all read from PostgreSQL rather than assumed.
//
// lower() and upper() are the release-critical pair. A NOT NULL range column can
// still hold 'empty' or '(,)', for which there is no bound to return, so the
// answer is nullable even though the column is not.
func TestRange_functionsAndTheirNullability(t *testing.T) {
	db, conn := rangeDB(t)

	t.Run("lower and upper are nullable over a NOT NULL column", func(t *testing.T) {
		type row struct {
			id       int64
			lo, hi   *int32
			empty    bool
			loInc    bool
			loInf    bool
			hiInf    bool
			upperInc bool
		}
		got, err := orm.Select(db.Bookings,
			orm.Project7(
				gendemo.Bookings.ID,
				gendemo.Bookings.Quota.Lower(),
				gendemo.Bookings.Quota.Upper(),
				gendemo.Bookings.Quota.IsEmpty(),
				gendemo.Bookings.Quota.LowerInc(),
				gendemo.Bookings.Quota.LowerInf(),
				gendemo.Bookings.Quota.UpperInf(),
				func(id int64, lo, hi *int32, empty, loInc, loInf, hiInf bool) row {
					return row{id: id, lo: lo, hi: hi, empty: empty, loInc: loInc, loInf: loInf, hiInf: hiInf}
				})).OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("projecting the range functions: %v", err)
		}
		byID := map[int64]row{}
		for _, r := range got {
			byID[r.id] = r
		}

		// Row 1 is [1,10): both bounds exist, the lower is inclusive, neither
		// side is infinite.
		if r := byID[1]; r.lo == nil || *r.lo != 1 || r.hi == nil || *r.hi != 10 ||
			r.empty || !r.loInc || r.loInf || r.hiInf {
			t.Errorf("booking 1 = %+v", r)
		}
		// Row 3 is empty: no bounds at all, and isempty says so.
		if r := byID[3]; r.lo != nil || r.hi != nil || !r.empty || r.loInc || r.loInf || r.hiInf {
			t.Errorf("booking 3 (empty) = %+v (lower=%v upper=%v)", r, r.lo, r.hi)
		}
		// Row 4 is (,): no bounds either, but it is not empty and both sides
		// report infinite. This is the case that would scan a NULL into an
		// int32 if lower() had been typed from the column's NOT NULL.
		if r := byID[4]; r.lo != nil || r.hi != nil || r.empty || !r.loInf || !r.hiInf {
			t.Errorf("booking 4 (unbounded) = %+v", r)
		}
	})

	t.Run("PostgreSQL agrees about every one of them", func(t *testing.T) {
		// The two that can be NULL over a NOT NULL column, stated as a fact
		// about the data rather than about the ORM.
		for _, q := range []struct{ sql, want string }{
			{"SELECT count(*) FROM bookings WHERE lower(quota) IS NULL", "2"},
			{"SELECT count(*) FROM bookings WHERE upper(quota) IS NULL", "2"},
			{"SELECT count(*) FROM bookings WHERE isempty(quota) IS NULL", "0"},
			{"SELECT count(*) FROM bookings WHERE lower_inc(quota) IS NULL", "0"},
		} {
			var n int64
			if err := conn.QueryRow(t.Context(), q.sql).Scan(&n); err != nil {
				t.Fatalf("%s: %v", q.sql, err)
			}
			if got := int64Str(n); got != q.want {
				t.Errorf("%s = %s, want %s", q.sql, got, q.want)
			}
		}
	})
}

// Stage 19: a permanent pg_typeof matrix. Every expression the ORM builds is
// asked what PostgreSQL made of it, and the answer is compared to the Go type
// the expression carries.
func TestRange_pgTypeofMatrix(t *testing.T) {
	_, conn := rangeDB(t)
	for _, tt := range []struct {
		sql      string
		wantType string
		wantNull bool
	}{
		{"lower('[1,10)'::int4range)", "integer", false},
		{"upper('[1,10)'::int4range)", "integer", false},
		{"lower('empty'::int4range)", "integer", true},
		{"lower('(,10)'::int4range)", "integer", true},
		{"upper('[1,)'::int4range)", "integer", true},
		{"lower('[1,10)'::int8range)", "bigint", false},
		{"lower('[2024-01-01,2024-02-01)'::daterange)", "date", false},
		{"lower('[2024-01-01,2024-02-01)'::tsrange)", "timestamp without time zone", false},
		{"lower('[2024-01-01,2024-02-01)'::tstzrange)", "timestamp with time zone", false},
		{"isempty('[1,10)'::int4range)", "boolean", false},
		{"isempty(NULL::int4range)", "boolean", true},
		{"lower_inc('empty'::int4range)", "boolean", false},
		{"lower_inf('(,10)'::int4range)", "boolean", false},
		{"'[1,10)'::int4range @> 5", "boolean", false},
		{"'[1,10)'::int4range @> NULL::int4", "boolean", true},
		{"'[1,10)'::int4range && '[5,7)'::int4range", "boolean", false},
		{"'[1,10)'::int4range -|- '[10,20)'::int4range", "boolean", false},
	} {
		t.Run(tt.sql, func(t *testing.T) {
			var ty string
			var isNull bool
			if err := conn.QueryRow(t.Context(),
				"SELECT pg_typeof("+tt.sql+")::text, ("+tt.sql+") IS NULL").Scan(&ty, &isNull); err != nil {
				t.Fatalf("asking PostgreSQL: %v", err)
			}
			if ty != tt.wantType {
				t.Errorf("pg_typeof = %s, want %s", ty, tt.wantType)
			}
			if isNull != tt.wantNull {
				t.Errorf("IS NULL = %v, want %v", isNull, tt.wantNull)
			}
		})
	}
}

func pgIDs(t *testing.T, conn *pgx.Conn, sql string) []int64 {
	t.Helper()
	rows, err := conn.Query(t.Context(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading: %v", err)
	}
	return out
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func int64Str(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
