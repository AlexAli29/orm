package gisdemo_test

import (
	"net/netip"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gen"
	"github.com/AlexAli29/orm/internal/gen/config"
	"github.com/AlexAli29/orm/internal/gen/lock"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/postgis"
)

// The maximal query, and the lock file.
//
// One query that uses everything M11, M12 and M13 have is worth more than
// twenty that use one thing each: the failures worth finding are the ones where
// two features interact — a placeholder that lands in the wrong position once a
// CTE is involved, a source that goes missing once a JSONB filter is added.

// A query combining a CTE, a derived table, a LEFT JOIN, JSONB, full-text
// search, a range predicate, a network predicate, interval arithmetic, a
// spatial radius and a spatial distance ranking — compared value for value with
// the same question written by hand.
func TestMaximal_everythingAtOnce(t *testing.T) {
	db, pool := openBoth(t)

	// The extra columns this query needs live in their own table, so the
	// spatial fixture stays a spatial fixture.
	if _, err := pool.Exec(t.Context(), `
		CREATE TABLE listings (
			id        bigint PRIMARY KEY,
			place_id  bigint NOT NULL,
			title     text NOT NULL,
			search    tsvector NOT NULL,
			settings  jsonb NOT NULL,
			window_   int4range NOT NULL,
			source_ip inet NOT NULL,
			lead_time interval NOT NULL
		);
		INSERT INTO listings VALUES
		 (1, 1, 'riverside cafe',  to_tsvector('english','riverside cafe with coffee'),
		     '{"tier":"gold","open":true}', '[9,17)',  '10.0.0.5',    '2 hours'),
		 (2, 2, 'east market',     to_tsvector('english','east market with coffee'),
		     '{"tier":"gold","open":true}', '[8,20)',  '10.0.0.9',    '30 minutes'),
		 (3, 3, 'far outpost',     to_tsvector('english','far outpost with coffee'),
		     '{"tier":"gold","open":true}', '[0,24)',  '10.0.0.11',   '1 day'),
		 (4, 4, 'north stall',     to_tsvector('english','north stall selling tea'),
		     '{"tier":"bronze","open":true}','[9,17)', '192.168.0.1', '15 minutes'),
		 (5, 1, 'closed kiosk',    to_tsvector('english','kiosk with coffee'),
		     '{"tier":"gold","open":false}','[9,17)',  '10.0.0.7',    '5 minutes')`); err != nil {
		t.Fatal(err)
	}

	src := orm.NewSource("public", "listings")
	var (
		lID     = orm.NewOrdCol[listing, int64](src, "id")
		lPlace  = orm.NewOrdCol[listing, int64](src, "place_id")
		lTitle  = orm.NewTextCol[listing](src, "title")
		lSearch = orm.NewCol[listing, orm.TSVector](src, "search")
		lSet    = orm.NewCol[listing, map[string]any](src, "settings")
		lWindow = orm.NewRangeCol[listing, int32](src, "window_", "int4range", "int4")
		lIP     = orm.NewCol[listing, netip.Prefix](src, "source_ip")
		lLead   = orm.NewOrdCol[listing, orm.Interval](src, "lead_time")
	)

	here := postgis.GeographyPoint(0, 0)

	// A CTE of the places within reach, carrying their distance out.
	placeID := orm.Named("place_id", orm.Of(gisdemo.Places.ID))
	metres := orm.Named("metres", orm.Of(postgis.OfGeog(gisdemo.Places.Spot).
		Distance(postgis.GeogValue[orm.Composed](here))))
	// The location comes out of the CTE too, so that the join below can use it
	// spatially. That is what FromExpr is for: the projection is an ordinary
	// expression and the caller says what geometry it holds.
	location := orm.Named("location", orm.Of(gisdemo.Places.Location))
	nearby := orm.CTE("nearby", orm.Rows(placeID, metres, location).
		From(gisdemo.Places.Source()).
		Where(postgis.OfGeog(gisdemo.Places.Spot).
			DWithin(postgis.GeogValue[orm.Composed](here), 120_000)))

	// A derived table of the listings that pass every non-spatial filter.
	kept := orm.Named("listing_id", orm.Of(lID))
	keptPlace := orm.Named("place_id", orm.Of(lPlace))
	keptTitle := orm.Named("title", orm.Of(lTitle))
	eligible := orm.Sub("eligible", orm.Rows(kept, keptPlace, keptTitle).
		From(src).
		Where(
			// JSONB, read as text and as a document.
			orm.Eq(orm.JSONPathText(orm.Of(lSet), "tier"), orm.Val("gold")),
			orm.JSONContains(orm.Of(lSet), orm.Val(map[string]any{"open": true})),
			// Full-text search.
			orm.Matches(lSearch, orm.PlainToTSQuery(orm.English, "coffee")),
			// A range predicate.
			orm.Cond(lWindow.Contains(12)),
			// A network predicate.
			orm.ContainedBy(orm.Of(lIP), orm.Val(netip.MustParsePrefix("10.0.0.0/24"))),
			// Interval arithmetic.
			orm.Lt(orm.Of(lLead), orm.Val(orm.IntervalOf(0, 0, 3*orm.Hours))),
		))

	// And the whole thing joined together, ordered by the spatial distance.
	type row struct {
		title  string
		metres float64
		zone   *string
	}
	shape := orm.Project3(
		orm.Ref(eligible, keptTitle),
		orm.Ref(nearby, metres),
		orm.Opt(gisdemo.Zones.Name),
		func(title string, m float64, zone *string) row { return row{title, m, zone} },
	)
	got, err := orm.Compose(db.Executor(), shape).
		With(nearby).
		From(eligible).
		Join(nearby, orm.Eq(orm.Ref(eligible, keptPlace), orm.Ref(nearby, placeID))).
		LeftJoin(gisdemo.Zones.Source(),
			postgis.Of(gisdemo.Zones.Area).Covers(
				postgis.FromExpr(orm.Ref(nearby, location), gisdemo.Places.Location.TypeMod()))).
		OrderBy(orm.Ref(nearby, metres).Asc(), orm.Ref(eligible, keptTitle).Asc()).
		Limit(10).
		All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	// The same question, by hand.
	handwritten := `
		WITH nearby AS (
			SELECT id AS place_id,
			       ST_Distance(spot, ST_GeogFromText('SRID=4326;POINT(0 0)')) AS metres
			FROM places
			WHERE ST_DWithin(spot, ST_GeogFromText('SRID=4326;POINT(0 0)'), 120000)
		)
		SELECT e.title, n.metres, z.name
		FROM (
			SELECT id AS listing_id, place_id, title
			FROM listings
			WHERE settings->>'tier' = 'gold'
			  AND (settings->'open')::jsonb = 'true'::jsonb
			  AND search @@ plainto_tsquery('english', 'coffee')
			  AND window_ @> 12
			  AND source_ip << '10.0.0.0/24'::inet
			  AND lead_time < interval '3 hours'
		) AS e
		JOIN nearby n ON n.place_id = e.place_id
		LEFT JOIN zones z ON ST_Covers(z.area, (SELECT location FROM places p WHERE p.id = e.place_id))
		ORDER BY n.metres ASC, e.title ASC
		LIMIT 10`

	rows, err := pool.Query(t.Context(), handwritten)
	if err != nil {
		t.Fatalf("the hand-written query: %v", err)
	}
	defer rows.Close()
	var want []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.title, &r.metres, &r.zone); err != nil {
			t.Fatal(err)
		}
		want = append(want, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) == 0 {
		t.Fatal("the maximal query selected nothing, so it proves nothing")
	}
	if len(got) != len(want) {
		t.Fatalf("the ORM read %d rows and the hand-written query read %d:\n%+v\n%+v",
			len(got), len(want), got, want)
	}
	for i := range got {
		if got[i].title != want[i].title || got[i].metres != want[i].metres {
			t.Errorf("row %d: ORM %+v, hand-written %+v", i, got[i], want[i])
		}
		if (got[i].zone == nil) != (want[i].zone == nil) {
			t.Errorf("row %d: the zone nullability differs: %v against %v", i, got[i].zone, want[i].zone)
		}
	}
}

// Every argument of the maximal query has to land where the writer put it. A
// placeholder graph that renumbered one operand into another's position would
// give a plausible wrong answer rather than an error.
func TestMaximal_placeholderGraph(t *testing.T) {
	db := openDB(t)
	here := postgis.GeographyPoint(1.5, 2.5)

	metres := postgis.OfGeog(gisdemo.Places.Spot).
		Distance(postgis.GeogValue[orm.Composed](here))
	shape := orm.Project2(orm.Of(gisdemo.Places.Name), orm.Of(metres),
		func(n string, m float64) string { return n })

	sql, args, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Where(
			orm.Cond(gisdemo.Places.Name.Ne("skip me")),
			postgis.OfGeog(gisdemo.Places.Spot).
				DWithin(postgis.GeogValue[orm.Composed](here), 999_000),
			postgis.Of(gisdemo.Places.Location).
				DWithin(postgis.Value[orm.Composed](postgis.NewPoint(4326, 7, 8)), 42),
			orm.Cond(gisdemo.Places.ID.Gt(0)),
		).
		OrderBy(orm.Of(metres).Asc()).
		Limit(3).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}

	// Each value appears exactly where it was written, in order.
	wantOrder := []any{
		here,            // the select-list distance
		"skip me",       // the name predicate
		here, 999_000.0, // the geography radius
		postgis.NewPoint(4326, 7, 8), 42.0, // the geometry radius
		int64(0), // the id predicate
		here,     // the ORDER BY distance
		3,        // the limit, if it is bound
	}
	// The limit may be rendered rather than bound; compare the prefix that is
	// definitely bound.
	if len(args) < len(wantOrder)-1 {
		t.Fatalf("the statement bound %d arguments:\n%s\n%v", len(args), sql, args)
	}
	for i := 0; i < len(wantOrder)-1; i++ {
		if !sameArg(args[i], wantOrder[i]) {
			t.Errorf("argument %d is %#v, want %#v\n%s", i+1, args[i], wantOrder[i], sql)
		}
	}
	// And the placeholders are numbered from one with no gaps.
	for i := 1; i <= len(args); i++ {
		if !containsPlaceholder(sql, i) {
			t.Errorf("the statement has no $%d:\n%s", i, sql)
		}
	}
}

func sameArg(got, want any) bool {
	switch w := want.(type) {
	case postgis.Geography:
		g, ok := got.(postgis.Geography)
		return ok && g.Equal(w)
	case postgis.Geometry:
		g, ok := got.(postgis.Geometry)
		return ok && g.Equal(w)
	default:
		return got == want
	}
}

func containsPlaceholder(sql string, n int) bool {
	needle := "$" + itoa(n)
	for i := 0; i+len(needle) <= len(sql); i++ {
		if sql[i:i+len(needle)] != needle {
			continue
		}
		// A longer number starting with the same digits is a different
		// placeholder.
		if i+len(needle) < len(sql) && sql[i+len(needle)] >= '0' && sql[i+len(needle)] <= '9' {
			continue
		}
		return true
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// Removing a source the query names has to be refused before the database sees
// it, whatever else the query does.
func TestMaximal_sourceGraph(t *testing.T) {
	db := openDB(t)

	shape := orm.Project2(
		orm.Of(gisdemo.Places.Name),
		orm.Of(postgis.Of(gisdemo.Zones.Area).Distance(postgis.Of(gisdemo.Places.Location))),
		func(n string, d float64) string { return n })

	// With both sources, it builds.
	if _, _, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		Join(gisdemo.Zones.Source(), orm.Cond(gisdemo.Zones.ID.Gt(0))).
		SQL(); err != nil {
		t.Fatalf("the well-formed query was refused: %v", err)
	}

	// Without the zones source, it is not a query.
	if _, _, err := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		SQL(); err == nil {
		t.Fatal("a spatial expression over an unnamed source built a statement")
	}

	// And an alias is a different source: naming the table does not admit the
	// alias.
	alias := gisdemo.Zones.As("z2")
	aliased := orm.Project1(
		orm.Of(postgis.Of(alias.Area).Distance(postgis.Of(gisdemo.Places.Location))),
		func(d float64) float64 { return d })
	if _, _, err := orm.Compose(db.Executor(), aliased).
		From(gisdemo.Places.Source()).
		Join(gisdemo.Zones.Source(), orm.Cond(gisdemo.Zones.ID.Gt(0))).
		SQL(); err == nil {
		t.Fatal("an aliased source's column was accepted by a query naming only the table")
	}
}

// The lock fingerprint over a spatial mapping has to be stable, or a project
// would be told its generated code is stale on every run.
func TestMaximal_lockFingerprintIsDeterministic(t *testing.T) {
	requirePostGIS(t)
	dsn := testdb.Create(t, schemaSQL(t))
	t.Setenv("ORM_GISDEMO_DSN", dsn)

	cfg, err := config.Load("orm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var first string
	for i := range 4 {
		result, err := gen.Check(t.Context(), cfg)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		got := lock.Fingerprint(result.Mapping)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("run %d fingerprinted %s and run 0 fingerprinted %s", i, got, first)
		}
	}
	if first == "" {
		t.Fatal("the fingerprint is empty")
	}
	t.Logf("the spatial mapping fingerprints as %s", first)
}

type listing struct{}
