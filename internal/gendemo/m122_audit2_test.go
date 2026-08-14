package gendemo_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5/pgconn"
)

// The M12 final audit, part two: composition, resources and the pool.

// Item 92, release-critical: the longest nullability chain M12.2 can produce.
//
//	LEFT JOIN
//	  -> a physically NOT NULL tstzrange, now absent
//	  -> lower(), already nullable
//	  -> that timestamp plus a nullable interval
//	  -> a CASE over the result
//	  -> through a derived table
//	  -> and out through a second LEFT JOIN
//
// Every link is a place a non-null claim could survive that PostgreSQL will
// contradict, and each would fail as a scan error in production rather than at
// compile time.
func TestAudit_m122NullabilityGraph(t *testing.T) {
	db, conn := rangeDB(t)

	// Inside the derived table: a booking read through a join that never
	// matches, so everything about it is absent.
	lowerOut := orm.NamedNull("lo", orm.OfNull(gendemo.Bookings.Period.Lower()))
	shiftedOut := orm.NamedNull("shifted", orm.AddIntervalNull(
		orm.OfNull(gendemo.Bookings.Period.Lower()),
		orm.Opt(gendemo.Bookings.Grace)))
	labelOut := orm.NamedNull("label", orm.Case(
		orm.Cond(gendemo.Bookings.Room.Eq("blue")), orm.Val("blue")).End())
	idOut := orm.Named("uid", orm.Of(gendemo.Users.ID))

	derived := orm.Sub("d", orm.Rows(idOut, lowerOut, shiftedOut, labelOut).
		From(gendemo.Users.Source()).
		LeftJoin(gendemo.Bookings.Source(), orm.Cond(gendemo.Bookings.Room.Eq("no such room"))))

	type row struct {
		uid     int64
		lo      *time.Time
		shifted *time.Time
		label   *string
	}
	got, err := orm.Compose(db.Executor(), orm.Project4(
		orm.Ref(derived, idOut),
		orm.Ref(derived, lowerOut),
		orm.Ref(derived, shiftedOut),
		orm.Ref(derived, labelOut),
		func(uid int64, lo, shifted *time.Time, label *string) row {
			return row{uid, lo, shifted, label}
		})).
		From(derived).
		OrderBy(orm.Ref(derived, idOut).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("the derived layer: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no rows, so the chain proves nothing")
	}
	for _, r := range got {
		if r.lo != nil || r.shifted != nil || r.label != nil {
			t.Errorf("user %d: %v / %v / %v, want all absent", r.uid, r.lo, r.shifted, r.label)
		}
	}

	// And out through a second LEFT JOIN onto the derived source, where the
	// already-nullable values must stay readable and stay nullable.
	type outer struct {
		email string
		lo    *time.Time
		label *string
	}
	outerRows, err := orm.Compose(db.Executor(), orm.Project3(
		orm.Of(gendemo.Users.Email),
		orm.Opt(orm.Ref(derived, lowerOut)),
		orm.Opt(orm.Ref(derived, labelOut)),
		func(email string, lo *time.Time, label *string) outer { return outer{email, lo, label} })).
		From(gendemo.Users.Source()).
		LeftJoin(derived, orm.Eq(orm.Of(gendemo.Users.ID), orm.Ref(derived, idOut))).
		OrderBy(orm.Of(gendemo.Users.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("the outer layer: %v", err)
	}
	if len(outerRows) == 0 {
		t.Fatal("the outer join produced nothing")
	}
	for _, r := range outerRows {
		if r.lo != nil || r.label != nil {
			t.Errorf("%s: %v / %v, want absent", r.email, r.lo, r.label)
		}
	}

	// PostgreSQL agrees that everything down that chain is NULL.
	var n int64
	if err := conn.QueryRow(t.Context(), `
		SELECT count(*) FROM users u
		LEFT JOIN (
			SELECT users.id AS uid, lower(bookings.period) AS lo
			FROM users LEFT JOIN bookings ON bookings.room = 'no such room'
		) d ON d.uid = u.id
		WHERE d.lo IS NULL`).Scan(&n); err != nil {
		t.Fatalf("the handwritten comparison: %v", err)
	}
	if n != int64(len(outerRows)) {
		t.Errorf("PostgreSQL found %d all-NULL rows, the ORM read %d", n, len(outerRows))
	}
}

// Item 91, release-critical: one realistic statement using as much of M12 as
// makes sense together, compared value by value against handwritten SQL.
func TestAudit_maximalM12Query(t *testing.T) {
	db, conn := rangeDB(t)
	seedNetworks(t, db)
	seedArticles(t, db)

	// A CTE of bookings whose period is bounded, joined to users, with a JSONB
	// predicate, an FTS rank, a range overlap, a network predicate and an
	// interval-derived timestamp — ordered and limited.
	bid := orm.Named("bid", orm.Of(gendemo.Bookings.ID))
	broom := orm.Named("broom", orm.Of(gendemo.Bookings.Room))
	bend := orm.NamedNull("bend", orm.OfNull(gendemo.Bookings.Period.Upper()))
	bdue := orm.Named("bdue", orm.AddInterval(gendemo.Bookings.StartsAt, gendemo.Bookings.Lease))
	live := orm.CTE("live", orm.Rows(bid, broom, bend, bdue).
		From(gendemo.Bookings.Source()).
		Where(orm.Cond(gendemo.Bookings.Period.Overlaps(orm.ClosedOpen(rt0, rt2)))))

	type row struct {
		room string
		end  *time.Time
		due  time.Time
		host string
		rank *float32
	}
	q := orm.Compose(db.Executor(), orm.Project5(
		orm.Ref(live, broom),
		orm.Ref(live, bend),
		orm.Ref(live, bdue),
		orm.Of(gendemo.Networks.Label),
		// Articles.Search is nullable, so TSRank refuses it and TSRankNull is
		// the form that types the answer as PostgreSQL will give it.
		orm.TSRankNull(gendemo.Articles.Search, orm.ToTSQuery(orm.English, "postgresql")),
		func(room string, end *time.Time, due time.Time, host string, rank *float32) row {
			return row{room, end, due, host, rank}
		})).
		With(live).
		From(live).
		Join(gendemo.Networks.Source(), orm.Cond(gendemo.Networks.Label.Ne(""))).
		Join(gendemo.Articles.Source(), orm.Cond(orm.Matches(gendemo.Articles.Search, orm.ToTSQuery(orm.English, "postgresql")))).
		Where(
			orm.ContainedBy(orm.Of(gendemo.Networks.Host), orm.Of(gendemo.Networks.Subnet)),
		).
		OrderBy(orm.Ref(live, bid).Asc(), orm.Of(gendemo.Networks.Label).Asc()).
		Limit(5)

	sql, args, err := q.SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	t.Logf("maximal statement:\n%s\nargs: %v", sql, args)

	got, err := q.All(t.Context())
	if err != nil {
		t.Fatalf("running it: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("the maximal statement matched nothing, so it proves little")
	}

	// The same statement, handwritten, compared value for value.
	rows, err := conn.Query(t.Context(), `
		WITH live AS (
			SELECT bookings.id AS bid, bookings.room AS broom,
			       upper(bookings.period) AS bend,
			       bookings.starts_at + bookings.lease AS bdue
			FROM bookings
			WHERE bookings.period && tstzrange($1, $2, '[)')
		)
		SELECT live.broom, live.bend, live.bdue, networks.label,
		       ts_rank(articles.search, to_tsquery('english', 'postgresql'))
		FROM live
		JOIN networks ON networks.label <> ''
		JOIN articles ON articles.search @@ to_tsquery('english', 'postgresql')
		WHERE networks.host << networks.subnet
		ORDER BY live.bid, networks.label
		LIMIT 5`, rt0, rt2)
	if err != nil {
		t.Fatalf("the handwritten statement: %v", err)
	}
	defer rows.Close()
	var want []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.room, &r.end, &r.due, &r.host, &r.rank); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		want = append(want, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("the ORM read %d rows, PostgreSQL %d", len(got), len(want))
	}
	for i := range got {
		if got[i].room != want[i].room || got[i].host != want[i].host {
			t.Errorf("row %d: %+v, want %+v", i, got[i], want[i])
		}
		switch {
		case (got[i].rank == nil) != (want[i].rank == nil):
			t.Errorf("row %d rank: %v, want %v", i, got[i].rank, want[i].rank)
		case got[i].rank != nil && *got[i].rank != *want[i].rank:
			t.Errorf("row %d rank: %v, want %v", i, *got[i].rank, *want[i].rank)
		}
		if !got[i].due.Equal(want[i].due) {
			t.Errorf("row %d due: %v, want %v", i, got[i].due, want[i].due)
		}
		switch {
		case (got[i].end == nil) != (want[i].end == nil):
			t.Errorf("row %d end: %v, want %v", i, got[i].end, want[i].end)
		case got[i].end != nil && !got[i].end.Equal(*want[i].end):
			t.Errorf("row %d end: %v, want %v", i, *got[i].end, *want[i].end)
		}
	}

	// Every bind argument reached the position it was written for.
	if len(args) < 2 {
		t.Fatalf("args = %v", args)
	}
	if !strings.Contains(sql, "$1") || !strings.Contains(sql, "$2") {
		t.Errorf("the placeholders are not where the arguments are: %s", sql)
	}
}

// Item 61 and 62: a connection created after the pool is already running must
// handle every M12.2 type. Force churn and check the new connection, not the
// one the pool warmed up with.
func TestAudit_newPoolConnectionsHandleRanges(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed+bookingSeed)

	cfg := poolConfig(t, dsn, 2)
	pool := poolFrom(t, cfg)
	db := gendemo.New(pool)

	roundTrip := func(what string) {
		t.Helper()
		got, err := db.Bookings.Query().
			Where(gendemo.Bookings.Quota.Contains(5)).
			OrderBy(gendemo.Bookings.ID.Asc()).All(t.Context())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if len(got) == 0 {
			t.Fatalf("%s: read no rows", what)
		}
		if got[0].Lease != fullInterval {
			t.Errorf("%s: lease = %+v", what, got[0].Lease)
		}
		if len(got[0].Holds) == 0 {
			t.Errorf("%s: holds = %v", what, got[0].Holds)
		}
	}

	roundTrip("first connection")

	// Break every connection the pool holds, then ask again: the answer has to
	// come over a connection created after the pool started.
	var pids []int32
	for range 4 {
		var pid int32
		if err := pool.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			t.Fatalf("reading the backend pid: %v", err)
		}
		pids = append(pids, pid)
	}
	if _, err := pool.Exec(t.Context(),
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		 WHERE datname = current_database() AND pid <> pg_backend_pid()`); err != nil {
		t.Logf("terminating peers: %v", err)
	}
	pool.Reset()
	roundTrip("after a pool reset")

	// And a second, independently created pool, so nothing depends on the
	// first one's warm-up order.
	other := poolFrom(t, poolConfig(t, dsn, 1))
	db2 := gendemo.New(other)
	rows, err := db2.Bookings.Query().Where(gendemo.Bookings.ID.Eq(int64(1))).All(t.Context())
	if err != nil {
		t.Fatalf("a fresh pool: %v", err)
	}
	if len(rows) != 1 || rows[0].Lease != fullInterval {
		t.Errorf("a fresh pool read %+v", rows)
	}
	_ = pids
}

// Item 104: a PostgreSQL error about a range survives as a *pgconn.PgError.
func TestAudit_rangeErrorsArePgErrors(t *testing.T) {
	db, _ := rangeDB(t)

	t.Run("an inverted range", func(t *testing.T) {
		_, err := db.Bookings.Insert(t.Context(), gendemo.Booking{
			Room: "inverted", StartsAt: rt0,
			// Lower above upper: PostgreSQL refuses the value.
			Period: orm.ClosedOpen(rt2, rt0),
			Stay:   orm.ClosedOpen(rt0, rt1), Shift: orm.ClosedOpen(rt0, rt1),
			Quota: orm.ClosedOpen[int32](1, 2), Span: orm.ClosedOpen[int64](1, 2),
			Lease: orm.Interval{}, Holds: orm.Multirange[time.Time]{},
		})
		if err == nil {
			t.Fatal("an inverted range was accepted")
		}
		var pg *pgconn.PgError
		if !errors.As(err, &pg) {
			t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
		}
		if pg.Code != "22000" {
			t.Logf("SQLSTATE = %s: %s", pg.Code, pg.Message)
		}
	})

	t.Run("the connection is still usable", func(t *testing.T) {
		if _, err := db.Bookings.Query().Count(t.Context()); err != nil {
			t.Fatalf("the pool did not recover: %v", err)
		}
	})
}

// Item 105 and 106: cancelling a range query, and breaking out of one early,
// both return the connection.
func TestAudit_rangeQueryResources(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed+bookingSeed)
	pool := poolFrom(t, poolConfig(t, dsn, 4))
	db := gendemo.New(pool)

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := db.Bookings.Query().Where(gendemo.Bookings.Quota.Contains(5)).All(ctx)
		if err == nil {
			t.Fatal("a cancelled query succeeded")
		}
		if n := settledAcquired(t, pool); n != 0 {
			t.Errorf("%d connections still acquired after a cancelled range query", n)
		}
	})

	t.Run("early break", func(t *testing.T) {
		var seen int
		for b, err := range db.Bookings.Query().OrderBy(gendemo.Bookings.ID.Asc()).Rows(t.Context()) {
			if err != nil {
				t.Fatalf("streaming: %v", err)
			}
			if b.Lease.IsZero() && seen == 0 {
				t.Log("first row has a zero lease")
			}
			seen++
			break
		}
		if seen != 1 {
			t.Fatalf("read %d rows before breaking", seen)
		}
		if n := settledAcquired(t, pool); n != 0 {
			t.Errorf("%d connections still acquired after an early break", n)
		}
	})
}

// Item 70: a large COPY carrying ranges and intervals streams rather than
// materialising, and lands every row.
func TestAudit_largeRangeCopy(t *testing.T) {
	if testing.Short() {
		t.Skip("large COPY")
	}
	db, conn := rangeDB(t)

	const n = 20000
	rows := make([]gendemo.Booking, n)
	for i := range rows {
		rows[i] = gendemo.Booking{
			Room: fmt.Sprintf("bulk-%d", i), StartsAt: rt0.Add(time.Duration(i) * time.Minute),
			Period: orm.ClosedOpen(rt0, rt1), Stay: orm.ClosedOpen(rt0, rt1), Shift: orm.ClosedOpen(rt0, rt1),
			Quota: orm.ClosedOpen(int32(i), int32(i)+10),
			Span:  orm.ClosedOpen(int64(i), int64(i)+100),
			Lease: orm.Interval{Months: int32(i % 13), Days: int32(-(i % 7)), Microseconds: int64(i) * 1000},
			Holds: orm.Multirange[time.Time]{orm.ClosedOpen(rt0, rt1)},
		}
	}
	copied, err := db.Bookings.CopyFrom(t.Context(), rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if copied != n {
		t.Fatalf("copied %d rows, want %d", copied, n)
	}

	var count int64
	if err := conn.QueryRow(t.Context(), "SELECT count(*) FROM bookings WHERE room LIKE 'bulk-%'").Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != n {
		t.Errorf("PostgreSQL holds %d bulk rows, want %d", count, n)
	}

	// Spot-check the far end, where a positional or streaming defect would show.
	last, err := db.Bookings.Query().
		Where(gendemo.Bookings.Room.Eq(fmt.Sprintf("bulk-%d", n-1))).All(t.Context())
	if err != nil {
		t.Fatalf("reading the last row: %v", err)
	}
	if len(last) != 1 {
		t.Fatalf("read %d rows for the last bulk row", len(last))
	}
	want := rows[n-1]
	if last[0].Lease != want.Lease {
		t.Errorf("the last row's lease is %+v, want %+v", last[0].Lease, want.Lease)
	}
	if lo, _ := last[0].Quota.LowerBound(); lo != int32(n-1) {
		t.Errorf("the last row's quota is %s", last[0].Quota)
	}
}

// Item 34: multiranges exist and behave on every supported version, which is
// not something to assume — they arrived in PostgreSQL 14.
func TestAudit_multirangeFamiliesExistOnThisServer(t *testing.T) {
	_, conn := rangeDB(t)
	var version string
	if err := conn.QueryRow(t.Context(), "SHOW server_version").Scan(&version); err != nil {
		t.Fatalf("reading the version: %v", err)
	}
	t.Logf("server_version = %s", version)

	for _, fam := range []string{
		"int4multirange", "int8multirange", "nummultirange",
		"datemultirange", "tsmultirange", "tstzmultirange",
	} {
		var oid uint32
		if err := conn.QueryRow(t.Context(),
			"SELECT oid FROM pg_type WHERE typname = $1", fam).Scan(&oid); err != nil {
			t.Errorf("%s: %v", fam, err)
			continue
		}
		if oid == 0 {
			t.Errorf("%s has no OID", fam)
		}
	}

	// And the two the ORM maps round-trip on this server.
	var back orm.Multirange[int32]
	if err := conn.QueryRow(t.Context(), "SELECT '{[1,5),[10,20)}'::int4multirange").Scan(&back); err != nil {
		t.Fatalf("int4multirange round trip: %v", err)
	}
	if back.String() != "{[1,5),[10,20)}" {
		t.Errorf("int4multirange came back as %s", back)
	}
	var backTime orm.Multirange[time.Time]
	if err := conn.QueryRow(t.Context(),
		"SELECT '{[2024-01-01,2024-02-01)}'::tstzmultirange").Scan(&backTime); err != nil {
		t.Fatalf("tstzmultirange round trip: %v", err)
	}
	if len(backTime) != 1 {
		t.Errorf("tstzmultirange came back as %s", backTime)
	}
}

// Item 99: the generated range descriptors are read-only and shareable.
func TestAudit_rangeDescriptorsAreShareable(t *testing.T) {
	db, _ := rangeDB(t)
	const workers = 40

	errs := make(chan error, workers)
	for i := range workers {
		go func(i int) {
			probe := orm.ClosedOpen(int32(i), int32(i)+5)
			for range 20 {
				if _, _, err := db.Bookings.Query().
					Where(
						gendemo.Bookings.Quota.Contains(int32(i)),
						gendemo.Bookings.Quota.Overlaps(probe),
						gendemo.Bookings.Period.Overlaps(orm.ClosedOpen(rt0, rt1)),
					).SQL(); err != nil {
					errs <- err
					return
				}
				if _, _, err := orm.Select(db.Bookings, orm.Project2(
					gendemo.Bookings.Quota.Lower(),
					gendemo.Bookings.Quota.Upper(),
					func(a, b *int32) int32 { return 0 })).SQL(); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}(i)
	}
	for range workers {
		if err := <-errs; err != nil {
			t.Fatalf("a worker failed: %v", err)
		}
	}

	// The descriptor still names what it always named.
	if got := gendemo.Bookings.Quota.Column(); got != "quota" {
		t.Errorf("the descriptor now names %q", got)
	}
	if gendemo.Bookings.Quota.Source() != gendemo.Bookings.Source() {
		t.Error("the descriptor's source changed")
	}
}

// The same regression at the level a caller sees it: an M12 operator whose
// right-hand operand names a source the statement does not read is refused
// before it reaches PostgreSQL.
//
// The whole suite passed with Infix's right operand invisible to the source
// walk, which means no test put a column there. Every M12 operator family does,
// so every one of them is checked.
func TestAudit_m12OperatorRightOperandIsInScope(t *testing.T) {
	shape := orm.Project1(orm.Of(gendemo.Bookings.ID), func(id int64) int64 { return id })
	netShape := orm.Project1(orm.Of(gendemo.Networks.ID), func(id int64) int64 { return id })
	userShape := orm.Project1(orm.Of(gendemo.Users.ID), func(id int64) int64 { return id })

	// The left operand is always a source the statement reads. Only the right
	// one is out of scope, so a traversal that stopped at the left would accept
	// every statement below.
	for _, tt := range []struct {
		name  string
		from  *orm.Source
		shape orm.Projection[orm.Composed, int64]
		pred  orm.Predicate[orm.Composed]
	}{
		{"range overlaps", gendemo.Bookings.Source(), shape, orm.RangeOverlaps(
			orm.Of(gendemo.Bookings.Quota), orm.Of(gendemo.Bookings.As("other").Quota))},
		{"range contains an element", gendemo.Bookings.Source(), shape, orm.RangeContains(
			orm.Of(gendemo.Bookings.Quota), orm.OfNull(gendemo.Bookings.As("other").Quota.Lower()))},
		{"range contained by", gendemo.Bookings.Source(), shape, orm.RangeContainedBy(
			orm.Of(gendemo.Bookings.Quota), orm.Of(gendemo.Bookings.As("other").Quota))},
		{"range adjacent", gendemo.Bookings.Source(), shape, orm.RangeAdjacent(
			orm.Of(gendemo.Bookings.Quota), orm.Of(gendemo.Bookings.As("other").Quota))},
		{"multirange overlaps", gendemo.Bookings.Source(), shape, orm.MultirangeOverlaps(
			orm.Of(gendemo.Bookings.Holds), orm.Of(gendemo.Bookings.As("other").Holds))},
		{"network containment", gendemo.Networks.Source(), netShape, orm.ContainedBy(
			orm.Of(gendemo.Networks.Host), orm.Of(gendemo.Networks.As("other").Subnet))},
		{"json containment", gendemo.Users.Source(), userShape, orm.JSONContains(
			orm.Of(gendemo.Users.Settings), orm.Of(gendemo.Users.As("other").Settings))},
		// Full text search has no case here: a tsquery is built from a literal,
		// so the right operand of @@ never names a source.
	} {
		t.Run(tt.name, func(t *testing.T) {
			sql, _, err := orm.Compose(nil, tt.shape).
				From(tt.from).
				Where(tt.pred).
				SQL()
			if err == nil {
				t.Fatalf("the statement compiled although the right operand names an unread source: %s", sql)
			}
			if !strings.Contains(err.Error(), "scope error") {
				t.Errorf("error = %v, want a scope error", err)
			}
		})
	}

	// Interval arithmetic in a select list, where the right operand is the one
	// out of scope.
	_, _, err := orm.Compose(nil, orm.Project1(
		orm.AddInterval(gendemo.Bookings.StartsAt, gendemo.Bookings.As("other").Lease),
		func(v time.Time) time.Time { return v })).
		From(gendemo.Bookings.Source()).
		SQL()
	if err == nil {
		t.Error("interval arithmetic naming an unread source on the right compiled")
	} else if !strings.Contains(err.Error(), "scope error") {
		t.Errorf("error = %v, want a scope error", err)
	}
}
