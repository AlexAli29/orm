package gisdemo_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gisdemo"
	"github.com/AlexAli29/orm/postgis"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The runtime properties: concurrency, connection churn, streaming and
// cancellation.
//
// None of these is about geometry. They are about whether adding a codec and a
// registration hook broke the things the rest of the ORM already promised — and
// a codec is exactly the kind of thing that breaks them, because it is per
// connection and connections come and go.

// The generated descriptors are shared, immutable values. Building and running
// queries from many goroutines has to be safe, or every application using them
// is one.
func TestRuntime_race(t *testing.T) {
	db := openDB(t)

	const goroutines = 32
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			here := postgis.Value[orm.Composed](postgis.NewPoint(4326, float64(i)/10, 0))
			shape := orm.Project3(
				orm.Of(gisdemo.Places.Name),
				orm.Of(gisdemo.Places.Location),
				orm.Of(postgis.Of(gisdemo.Places.Location).Distance(here)),
				func(n string, g postgis.Geometry, d float64) string { return n },
			)
			for range 8 {
				_, err := orm.Compose(db.Executor(), shape).
					From(gisdemo.Places.Source()).
					Where(postgis.Of(gisdemo.Places.Location).DWithin(here, 100)).
					OrderBy(orm.Of(gisdemo.Places.ID).Asc()).
					All(context.Background())
				if err != nil {
					errs <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent spatial query failed: %v", err)
	}
}

// Codec registration is per connection, so every way a connection can come into
// existence has to end up with the types registered.
//
// This is the failure that hides in a test suite: register once on the first
// connection, everything passes, and the first time the pool grows in
// production a geometry comes back as an unreadable blob.
func TestRuntime_poolChurn(t *testing.T) {
	pool := openPool(t)
	db := gisdemo.New(pool)

	read := func(what string) {
		t.Helper()
		got, err := db.Places.Query().
			Where(gisdemo.Places.Name.Eq("origin")).
			One(t.Context())
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if !got.Location.Equal(postgis.NewPoint(4326, 0, 0)) {
			t.Errorf("%s read the location as %s", what, got.Location)
		}
	}

	// The first connection.
	read("the first connection")

	// A second one, opened because the first is busy.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); read("a concurrent connection") }()
	}
	wg.Wait()

	// A connection the server killed, which the pool replaces.
	if _, err := pool.Exec(t.Context(), `
		SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`); err != nil {
		t.Logf("terminating backends: %v", err)
	}
	pool.Reset()
	read("a replacement connection")

	// A transaction-bound connection.
	if err := db.Tx(t.Context(), func(tx *gisdemo.DB) error {
		got, err := tx.Places.Query().Where(gisdemo.Places.Name.Eq("origin")).One(t.Context())
		if err != nil {
			return err
		}
		if !got.Location.Equal(postgis.NewPoint(4326, 0, 0)) {
			t.Errorf("inside a transaction the location is %s", got.Location)
		}
		return nil
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	// And a second pool over the same database.
	cfg := pool.Config().Copy()
	other, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatalf("opening a second pool: %v", err)
	}
	defer other.Close()
	second := gisdemo.New(other)
	if _, err := second.Places.Query().Where(gisdemo.Places.Name.Eq("origin")).One(t.Context()); err != nil {
		t.Fatalf("the second pool: %v", err)
	}
}

// Streaming a large spatial result, and stopping early.
//
// The connection has to come back to the pool when the caller breaks out of the
// loop, and the whole result must not have been buffered first — a spatial
// result set is large, and buffering one is how a query that should stream
// exhausts memory.
func TestRuntime_streamingAndEarlyBreak(t *testing.T) {
	db := openDB(t)

	rows := make([]gisdemo.Place, 0, 2000)
	for i := range 2000 {
		p := newPlace(int64(10_000+i), float64(i)/1000, float64(i)/2000)
		poly := postgis.NewPolygon(4326, postgis.XY, bigRing(200))
		p.Footprint = &poly
		rows = append(rows, p)
	}
	if _, err := db.Places.CopyFrom(t.Context(), rows); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}

	shape := orm.Project2(
		orm.Of(gisdemo.Places.ID),
		orm.Of(gisdemo.Places.Location),
		func(id int64, g postgis.Geometry) int64 { return id },
	)
	q := orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		OrderBy(orm.Of(gisdemo.Places.ID).Asc())

	seen := 0
	for _, err := range q.Rows(t.Context()) {
		if err != nil {
			t.Fatalf("streaming: %v", err)
		}
		seen++
		if seen == 5 {
			break
		}
	}
	if seen != 5 {
		t.Fatalf("read %d rows before breaking, want 5", seen)
	}

	// The connection came back: another query works.
	if _, err := db.Places.Query().Where(gisdemo.Places.ID.Eq(1)).One(t.Context()); err != nil {
		t.Fatalf("after breaking out of the stream: %v", err)
	}
}

func bigRing(n int) []postgis.Coord {
	cs := make([]postgis.Coord, 0, n+1)
	for i := range n {
		f := float64(i) / float64(n)
		cs = append(cs, postgis.Coord{X: f, Y: f * f})
	}
	cs = append(cs, cs[0])
	return cs
}

// Cancelling a spatial statement leaves the pool usable, which is the property
// that matters: a cancelled query that poisons a connection turns one slow
// request into an outage.
func TestRuntime_cancellation(t *testing.T) {
	pool := openPool(t)
	db := gisdemo.New(pool)

	// A cancelled SELECT.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := db.Places.Query().All(ctx)
	if err == nil {
		t.Error("a query with a cancelled context succeeded")
	} else if !errors.Is(err, context.Canceled) {
		t.Logf("the cancelled query failed with %v", err)
	}

	// A cancelled COPY.
	ctx2, cancel2 := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel2()
	big := make([]gisdemo.Place, 0, 5000)
	for i := range 5000 {
		big = append(big, newPlace(int64(50_000+i), float64(i)/1000, 0))
	}
	if _, err := db.Places.CopyFrom(ctx2, big); err == nil {
		t.Log("the COPY finished before the deadline; that is fine")
	}

	// A cancelled expensive spatial operation.
	ctx3, cancel3 := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel3()
	shape := orm.Project1(
		orm.Of(gisdemo.Places.Location.Expr().Buffer(0.001).Value()),
		func(g postgis.Geometry) postgis.Geometry { return g },
	)
	_, _ = orm.Compose(db.Executor(), shape).
		From(gisdemo.Places.Source()).
		All(ctx3)

	// The pool is still usable.
	if _, err := db.Places.Query().Where(gisdemo.Places.Name.Eq("origin")).One(t.Context()); err != nil {
		t.Fatalf("after cancellation the pool is unusable: %v", err)
	}
}
