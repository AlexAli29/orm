package gendemo_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/AlexAli29/orm/observe"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tracing.
//
// Two things are being held to. The counts have to be exact, because a trace
// that double-reports one statement makes every number computed from it wrong.
// And no bound value may reach a tracer — not by default, not by an option, at
// all — because an ORM's tracing sees every query a program runs.

// recorder is a tracer that keeps everything it is given, so a test can assert
// on the whole of it rather than on a summary.
type recorder struct {
	mu     sync.Mutex
	starts []observe.StartEvent
	ends   []observe.EndEvent
}

func (r *recorder) Start(ctx context.Context, e observe.StartEvent) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts = append(r.starts, e)
	return ctx
}

func (r *recorder) End(ctx context.Context, e observe.EndEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, e)
}

// startEvents and endEvents copy what was recorded, so a caller reads a
// snapshot rather than a slice another goroutine may still be appending to.
func (r *recorder) startEvents() []observe.StartEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.starts)
}

func (r *recorder) endEvents() []observe.EndEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.ends)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts, r.ends = nil, nil
}

func (r *recorder) ops() []observe.Op {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]observe.Op, 0, len(r.starts))
	for _, e := range r.starts {
		out = append(out, e.Op)
	}
	return out
}

// text returns everything a tracer received, rendered, so that a search for a
// secret covers every field of every event.
func (r *recorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.starts {
		fmt.Fprintf(&b, "%#v\n", e)
	}
	for _, e := range r.ends {
		fmt.Fprintf(&b, "%#v\n", e)
		if e.Err != nil {
			b.WriteString(e.Err.Error())
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func traceDB(t *testing.T) (*gendemo.DB, *recorder, *pgx.Conn) {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	rec := &recorder{}
	return gendemo.New(orm.Traced(conn, rec)), rec, conn
}

// The release-critical property: no bound value reaches a tracer.
//
// The corpus is the kinds of value a program really handles, and the assertion
// is over every field of every event rather than over the one field somebody
// remembered to check.
func TestTrace_noBoundValuesReachTheTracer(t *testing.T) {
	db, rec, _ := traceDB(t)

	secrets := []string{
		"hunter2",
		"Bearer eyJhbGciOiJIUzI1NiJ9.secret.signature",
		"person@example.com",
		"4111111111111111",
		"-----BEGIN PRIVATE KEY-----",
	}

	for _, secret := range secrets {
		rec.reset()

		// A read with the secret in a predicate.
		if _, err := db.Users.Query().Where(gendemo.Users.Email.Eq(secret)).All(t.Context()); err != nil {
			t.Fatalf("All: %v", err)
		}
		// A write with the secret in an assignment.
		if _, err := db.Users.Update().
			Set(gendemo.Users.Bio.Set(secret)).
			Where(gendemo.Users.ID.Eq(1)).
			Exec(t.Context()); err != nil {
			t.Fatalf("Update: %v", err)
		}
		// A JSON document containing it.
		if _, err := db.Users.Update().
			Set(gendemo.Users.Settings.Set(map[string]any{"token": secret})).
			Where(gendemo.Users.ID.Eq(1)).
			Exec(t.Context()); err != nil {
			t.Fatalf("Update settings: %v", err)
		}
		// And a bytea.
		blob := []byte(secret)
		if _, err := db.Users.Update().
			Set(gendemo.Users.Avatar.Set(blob)).
			Where(gendemo.Users.ID.Eq(1)).
			Exec(t.Context()); err != nil {
			t.Fatalf("Update avatar: %v", err)
		}

		got := rec.text()
		if got == "" {
			t.Fatal("the tracer received nothing")
		}
		if strings.Contains(got, secret) {
			t.Errorf("the tracer received %q:\n%s", secret, got)
		}
	}
}

// The SQL a tracer receives is the statement with its placeholders, which is
// useful and carries no value.
func TestTrace_sqlHasPlaceholders(t *testing.T) {
	db, rec, _ := traceDB(t)

	if _, err := db.Users.Query().Where(gendemo.Users.Email.Eq("someone@example.com")).
		All(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rec.starts) != 1 {
		t.Fatalf("%d start events", len(rec.starts))
	}
	e := rec.starts[0]
	if !strings.Contains(e.SQL, `"email" = $1`) {
		t.Errorf("the SQL is not parameterised:\n%s", e.SQL)
	}
	if e.Args != 1 {
		t.Errorf("the event reports %d arguments", e.Args)
	}
	if e.Table != "public.users" {
		t.Errorf("the event's table is %q", e.Table)
	}
	if !strings.HasPrefix(e.Fingerprint, "v1:") {
		t.Errorf("the event's fingerprint is %q", e.Fingerprint)
	}
	if e.Raw {
		t.Error("a built statement is marked raw")
	}
}

// Exactly one event per operation, and one per relation statement — no more.
func TestTrace_eventCounts(t *testing.T) {
	db, rec, _ := traceDB(t)

	cases := []struct {
		name string
		run  func() error
		want []observe.Op
	}{
		{"All", func() error {
			_, err := db.Users.Query().All(t.Context())
			return err
		}, []observe.Op{observe.OpQuery}},
		{"One", func() error {
			_, err := db.Users.Query().Where(gendemo.Users.ID.Eq(1)).One(t.Context())
			return err
		}, []observe.Op{observe.OpQuery}},
		{"Rows", func() error {
			for _, err := range db.Users.Query().Rows(t.Context()) {
				if err != nil {
					return err
				}
			}
			return nil
		}, []observe.Op{observe.OpStream}},
		{"Update", func() error {
			_, err := db.Users.Update().Set(gendemo.Users.Age.Set(30)).
				Where(gendemo.Users.ID.Eq(1)).Exec(t.Context())
			return err
		}, []observe.Op{observe.OpUpdate}},
		{"Delete", func() error {
			_, err := db.Users.Delete().Where(gendemo.Users.ID.Eq(-1)).Exec(t.Context())
			return err
		}, []observe.Op{observe.OpDelete}},
		{"one relation", func() error {
			_, err := db.Users.Query().With(gendemo.Users.Posts).All(t.Context())
			return err
		}, []observe.Op{observe.OpQuery, observe.OpRelation}},
		{"two levels of relation", func() error {
			_, err := db.Users.Query().
				With(gendemo.Users.Posts.With(gendemo.Posts.Comments)).
				All(t.Context())
			return err
		}, []observe.Op{observe.OpQuery, observe.OpRelation, observe.OpRelation}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec.reset()
			if err := c.run(); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			got := rec.ops()
			if len(got) != len(c.want) {
				t.Fatalf("%d events %v, want %d %v", len(got), got, len(c.want), c.want)
			}
			// Counts of each op, so the order of the relation events does not
			// make the test brittle.
			countOf := func(ops []observe.Op) map[observe.Op]int {
				m := map[observe.Op]int{}
				for _, o := range ops {
					m[o]++
				}
				return m
			}
			gotCounts, wantCounts := countOf(got), countOf(c.want)
			for op, n := range wantCounts {
				if gotCounts[op] != n {
					t.Errorf("%d %s events, want %d", gotCounts[op], op, n)
				}
			}
			// Every start has exactly one end.
			if len(rec.ends) != len(rec.starts) {
				t.Errorf("%d starts and %d ends", len(rec.starts), len(rec.ends))
			}
		})
	}
}

// Relation loading is one statement per relation per level, whatever the number
// of parents. The events are how a reader confirms there is no N+1.
func TestTrace_relationsAreNotNPlusOne(t *testing.T) {
	db, rec, _ := traceDB(t)

	roots, err := db.Users.Query().With(gendemo.Users.Posts).All(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) < 2 {
		t.Fatalf("only %d roots, so the test proves nothing", len(roots))
	}

	var relations int
	var path string
	for _, e := range rec.starts {
		if e.Op == observe.OpRelation {
			relations++
			path = e.Relation
		}
	}
	if relations != 1 {
		t.Errorf("%d relation statements for %d parents", relations, len(roots))
	}
	// The path is the relation's own, qualified by the root it hangs off — which
	// is what makes two relations named Posts on different entities tell apart.
	if !strings.HasSuffix(path, "Posts") {
		t.Errorf("the relation event's path is %q", path)
	}
}

// The end event says how long, how many, and what went wrong.
func TestTrace_endEvents(t *testing.T) {
	db, rec, _ := traceDB(t)

	if _, err := db.Users.Query().All(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rec.ends) != 1 {
		t.Fatalf("%d end events", len(rec.ends))
	}
	e := rec.ends[0]
	if e.Duration <= 0 {
		t.Errorf("the duration is %v", e.Duration)
	}
	if !e.RowsKnown || e.Rows != 3 {
		t.Errorf("the event reports %d rows (known=%v), want the three seeded users", e.Rows, e.RowsKnown)
	}
	if e.Err != nil {
		t.Errorf("a successful query reported %v", e.Err)
	}
	if e.Cancelled {
		t.Error("a successful query was reported cancelled")
	}
	if e.Fingerprint != rec.starts[0].Fingerprint {
		t.Error("the start and end fingerprints differ")
	}
}

// A stream reports how many rows the caller took, not how many the query would
// have produced. That is what OpStream means.
func TestTrace_streamCountsWhatWasTaken(t *testing.T) {
	db, rec, _ := traceDB(t)

	taken := 0
	for _, err := range db.Users.Query().Rows(t.Context()) {
		if err != nil {
			t.Fatal(err)
		}
		taken++
		if taken == 2 {
			break
		}
	}
	if len(rec.ends) != 1 {
		t.Fatalf("%d end events", len(rec.ends))
	}
	if rec.ends[0].Rows != 2 {
		t.Errorf("the stream reports %d rows, want the two the caller took", rec.ends[0].Rows)
	}
	if rec.starts[0].Op != observe.OpStream {
		t.Errorf("a stream was reported as %s", rec.starts[0].Op)
	}
}

// An error reaches the tracer, and the caller still gets the original.
func TestTrace_errors(t *testing.T) {
	db, rec, _ := traceDB(t)

	// A duplicate primary key.
	dup := gendemo.User{ID: 1, Email: "dup@example.com", Age: 1, Active: true,
		State: gendemo.UserStateActive, Tags: []string{}, Settings: map[string]any{}}
	_, err := db.Users.Insert(t.Context(), dup)
	if err == nil {
		t.Fatal("inserting a duplicate succeeded")
	}

	if len(rec.ends) != 1 {
		t.Fatalf("%d end events", len(rec.ends))
	}
	e := rec.ends[0]
	if e.Err == nil {
		t.Fatal("the tracer received no error")
	}
	info := observe.Classify(e.Err)
	if !info.Failed {
		t.Error("the classification says nothing failed")
	}
	if info.SQLState != "23505" {
		t.Errorf("the SQLSTATE is %q, want 23505", info.SQLState)
	}
	if info.Kind != "unique_violation" {
		t.Errorf("the kind is %q", info.Kind)
	}
	if info.Constraint == "" {
		t.Error("the classification names no constraint")
	}
	// The classification carries the identifiers and not the data: the server's
	// message quotes the key, and this does not.
	rendered := fmt.Sprintf("%#v", info)
	if strings.Contains(rendered, "dup@example.com") {
		t.Errorf("the classification contains the value: %s", rendered)
	}

	// And the caller's error is still the server's.
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) {
		t.Errorf("the caller's error is not a PostgreSQL error: %v", err)
	}
}

// A cancelled operation is reported as cancelled.
func TestTrace_cancellation(t *testing.T) {
	db, rec, _ := traceDB(t)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, _ = db.Users.Query().All(ctx)

	if len(rec.ends) != 1 {
		t.Fatalf("%d end events", len(rec.ends))
	}
	if !rec.ends[0].Cancelled {
		t.Errorf("a cancelled query was not reported cancelled: %v", rec.ends[0].Err)
	}
	if observe.Classify(rec.ends[0].Err).Kind != "cancelled" {
		t.Errorf("the classification is %q", observe.Classify(rec.ends[0].Err).Kind)
	}
}

// A COPY is traced by what it copies, and its fingerprint is the COPY one.
func TestTrace_copy(t *testing.T) {
	db, rec, conn := traceDB(t)

	// The seed inserts explicit ids without advancing the identity sequence, so
	// it is advanced here: this test is about tracing, not about who assigns
	// primary keys.
	if _, err := conn.Exec(t.Context(),
		`SELECT setval(pg_get_serial_sequence('users', 'id'), 1000)`); err != nil {
		t.Fatal(err)
	}
	rec.reset()

	rows := make([]gendemo.User, 0, 20)
	for i := range 20 {
		rows = append(rows, gendemo.User{
			ID: int64(2000 + i), Email: fmt.Sprintf("copy%d@example.com", i),
			Age: 30, Active: true, State: gendemo.UserStateActive,
			Tags: []string{}, Settings: map[string]any{},
		})
	}
	n, err := db.Users.CopyFrom(t.Context(), rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != 20 {
		t.Fatalf("copied %d rows", n)
	}

	if len(rec.starts) != 1 {
		t.Fatalf("%d start events", len(rec.starts))
	}
	e := rec.starts[0]
	if e.Op != observe.OpCopy {
		t.Errorf("a COPY was reported as %s", e.Op)
	}
	if e.Table != "public.users" {
		t.Errorf("the COPY's table is %q", e.Table)
	}
	if len(e.Columns) == 0 {
		t.Error("the COPY event has no columns")
	}
	if e.SQL != "" {
		t.Errorf("a COPY has no SQL and the event carries %q", e.SQL)
	}
	want := orm.CopyFingerprint("public", "users", e.Columns).String()
	if e.Fingerprint != want {
		t.Errorf("the COPY's fingerprint is %q, want %q", e.Fingerprint, want)
	}
	if rec.ends[0].Rows != 20 {
		t.Errorf("the COPY reports %d rows", rec.ends[0].Rows)
	}
	// And no email reached the tracer.
	if strings.Contains(rec.text(), "copy1@example.com") {
		t.Error("a copied value reached the tracer")
	}
}

// A raw statement is marked as one, because this package cannot promise
// anything about literals inside SQL it did not write.
func TestTrace_rawIsMarked(t *testing.T) {
	// The Raw flag exists so an adapter can treat caller-written SQL
	// differently from SQL the ORM built — ormslog and ormotel both keep it
	// behind a switch of its own, because a caller's literal is something this
	// package cannot redact without parsing SQL.
	//
	// This asserts the flag rather than logging the event count. The earlier
	// version of this test printed the count and asserted nothing, and the
	// count was zero: the raw path emitted no event at all, so the flag was
	// never set and the switch governing it governed nothing.
	for _, c := range []struct {
		what string
		run  func(t *testing.T, db *gendemo.DB) error
		op   observe.Op
	}{
		{"All", func(t *testing.T, db *gendemo.DB) error {
			_, err := orm.Raw(db.Users,
				`SELECT * FROM users WHERE email = $1`, "raw@example.com").All(t.Context())
			return err
		}, observe.OpQuery},
		{"One", func(t *testing.T, db *gendemo.DB) error {
			_, err := orm.Raw(db.Users,
				`SELECT * FROM users WHERE email = $1`, "nobody@example.com").One(t.Context())
			// Not found is the expected answer and not the point.
			if errors.Is(err, orm.ErrNotFound) {
				return nil
			}
			return err
		}, observe.OpQuery},
		{"Rows", func(t *testing.T, db *gendemo.DB) error {
			for _, err := range orm.Raw(db.Users,
				`SELECT * FROM users WHERE email = $1`, "raw@example.com").Rows(t.Context()) {
				if err != nil {
					return err
				}
			}
			return nil
		}, observe.OpQuery},
	} {
		t.Run(c.what, func(t *testing.T) {
			db, rec, _ := traceDB(t)
			if err := c.run(t, db); err != nil {
				t.Fatal(err)
			}
			starts := rec.startEvents()
			if len(starts) != 1 {
				t.Fatalf("a raw %s produced %d events, want 1", c.what, len(starts))
			}
			e := starts[0]
			if !e.Raw {
				t.Error("the event is not marked raw, so an adapter cannot tell it from generated SQL")
			}
			if e.Op != c.op {
				t.Errorf("op = %q, want %q", e.Op, c.op)
			}
			if e.SQL == "" {
				t.Error("the event carries no SQL")
			}
			if e.Args != 1 {
				t.Errorf("args = %d, want 1", e.Args)
			}
			if e.Table == "" {
				t.Error("the event names no table")
			}
			// The argument is counted, never carried.
			if strings.Contains(e.SQL, "raw@example.com") || strings.Contains(e.SQL, "nobody@example.com") {
				t.Errorf("the event's SQL contains a bound value: %s", e.SQL)
			}
			if ends := rec.endEvents(); len(ends) != 1 {
				t.Errorf("a raw %s produced %d end events, want 1", c.what, len(ends))
			}
		})
	}
}

// A tracer runs inside a transaction and the event says so.
func TestTrace_transaction(t *testing.T) {
	db, rec, _ := traceDB(t)

	if err := db.Tx(t.Context(), func(tx *gendemo.DB) error {
		_, err := tx.Users.Query().All(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(rec.starts) != 1 {
		t.Fatalf("%d start events", len(rec.starts))
	}
	if !rec.starts[0].InTransaction {
		t.Error("a query inside a transaction was not reported as being in one")
	}

	rec.reset()
	if _, err := db.Users.Query().All(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rec.starts[0].InTransaction {
		t.Error("a query outside a transaction was reported as being in one")
	}
}

// An untraced executor is unchanged, so a program that does not trace pays
// nothing but a nil check.
func TestTrace_nilTracer(t *testing.T) {
	db, _ := explainDB(t)
	if _, err := db.Users.Query().All(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Traced with a nil tracer returns the executor itself.
	conn := db.Executor()
	if got := orm.Traced(conn, nil); got != conn {
		t.Error("Traced with no tracer wrapped the executor")
	}
}

// One tracer, many goroutines. The ORM introduces no race of its own; keeping
// the tracer's own state safe is the tracer's job, and this one uses a mutex.
func TestTrace_concurrent(t *testing.T) {
	// A pool rather than a single connection: one *pgx.Conn is not safe for
	// concurrent use, and the property under test is the ORM's, not pgx's.
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t)+seed)
	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	rec := &recorder{}
	db := gendemo.New(orm.Traced(pool, rec))

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 4 {
				if _, err := db.Users.Query().All(context.Background()); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(rec.starts) != 128 {
		t.Errorf("%d start events, want 128", len(rec.starts))
	}
	if len(rec.ends) != len(rec.starts) {
		t.Errorf("%d starts and %d ends", len(rec.starts), len(rec.ends))
	}
}
