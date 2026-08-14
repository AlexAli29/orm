package gendemo_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm"
	"github.com/AlexAli29/orm/internal/gendemo"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The COPY audit.
//
// The claims worth attacking are the ones a wrong implementation would still
// pass a happy-path test with: that the columns go in the caller's order, that
// a failed COPY reports nothing as written, and that the protocol is really
// COPY rather than an INSERT with a different name on the wrapper.

// Release-critical: selected columns are sent in the caller's order, and the
// values follow them.
//
// Two same-typed columns read in the opposite order from their declaration is
// the case a positional mistake survives: the types match, PostgreSQL accepts
// it, and the data is silently transposed.
func TestAudit_copyColumnOrderIsTheCallersOrder(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	// title and body are both text; the entity declares title first.
	rows := []gendemo.Article{
		{Title: "TITLE-ONE", Body: "BODY-ONE"},
		{Title: "TITLE-TWO", Body: "BODY-TWO"},
	}
	// Name them in the opposite order.
	if _, err := orm.CopyColumns(t.Context(), db.Articles, rows,
		gendemo.Articles.Body, gendemo.Articles.Title); err != nil {
		t.Fatalf("CopyColumns: %v", err)
	}

	got, err := db.Articles.Query().OrderBy(gendemo.Articles.Title.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read back %d rows", len(got))
	}
	for _, a := range got {
		if !strings.HasPrefix(a.Title, "TITLE-") {
			t.Errorf("title and body were transposed: %+v", a)
		}
		if !strings.HasPrefix(a.Body, "BODY-") {
			t.Errorf("title and body were transposed: %+v", a)
		}
	}
	// And the COPY named them in the order asked for.
	rec := &recordingCopyExecutor{inner: conn}
	if _, err := orm.CopyColumns(t.Context(), gendemo.New(rec).Articles, nil,
		gendemo.Articles.Body, gendemo.Articles.Title); err != nil {
		t.Fatalf("CopyColumns: %v", err)
	}
	if got := strings.Join(rec.columns, ","); got != "body,title" {
		t.Errorf("COPY named columns %q, want the caller's order", got)
	}
}

// Release-critical: a COPY that PostgreSQL rejected wrote nothing, and the
// count reported alongside the error says so.
func TestAudit_copyReportsNothingWrittenWhenItFails(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	// 500 good rows and one that violates NOT NULL, outside any transaction of
	// the caller's: the COPY is one statement and PostgreSQL rolls it back.
	rows := make([]gendemo.Article, 501)
	for i := range rows {
		rows[i] = gendemo.Article{Title: fmt.Sprintf("t%d", i), Body: "b"}
	}
	n, err := orm.CopyColumns(t.Context(), db.Articles, rows, gendemo.Articles.Title)
	// body is NOT NULL with no default, so omitting it fails every row.
	if err == nil {
		t.Fatal("a COPY omitting a NOT NULL column with no default succeeded")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		t.Fatalf("error = %v (%T), want a *pgconn.PgError", err, err)
	}
	if n != 0 {
		t.Errorf("a failed COPY reported %d rows written; PostgreSQL rolled the statement back", n)
	}
	var count int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM articles`).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != 0 {
		t.Errorf("%d rows survived a failed COPY", count)
	}
}

// The same for a source that fails part way: nothing is written, and the count
// is zero rather than the number sent before the failure.
func TestAudit_copySourceErrorReportsNothingWritten(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	boom := errors.New("the source gave up")
	for _, at := range []int{0, 250, 500} {
		t.Run(fmt.Sprintf("fails at row %d", at), func(t *testing.T) {
			m11exec(t, conn, `DELETE FROM articles`)
			seq := func(yield func(gendemo.Article, error) bool) {
				for i := range 500 {
					if i == at {
						yield(gendemo.Article{}, boom)
						return
					}
					if !yield(gendemo.Article{Title: fmt.Sprintf("t%d", i), Body: "b"}, nil) {
						return
					}
				}
				// Failing after the last row is also a source error.
				if at >= 500 {
					yield(gendemo.Article{}, boom)
				}
			}
			n, err := db.Articles.CopyFromSeq(t.Context(), seq)
			if !errors.Is(err, boom) {
				t.Fatalf("error = %v, want it to wrap the source's", err)
			}
			if n != 0 {
				t.Errorf("a failed COPY reported %d rows written", n)
			}
			var count int64
			if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM articles`).Scan(&count); err != nil {
				t.Fatalf("counting: %v", err)
			}
			if count != 0 {
				t.Errorf("%d rows survived", count)
			}
			// The connection is still usable.
			if _, err := db.Articles.Query().All(t.Context()); err != nil {
				t.Errorf("the connection is unusable after a source error: %v", err)
			}
		})
	}
}

// The protocol claim, stated as strongly as the executor allows: COPY makes
// exactly one CopyFrom call and no Query calls, and the same executor proves
// InsertMany does the opposite.
func TestAudit_copyNeverFallsBackToAStatement(t *testing.T) {
	testdb.AdminDSN(t)
	_, conn := m12env(t)

	rec := &recordingCopyExecutor{inner: conn}
	db := gendemo.New(rec)
	rows := []gendemo.Category{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	if _, err := db.Categories.CopyFrom(t.Context(), rows); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if rec.copies != 1 || rec.queries != 0 {
		t.Errorf("COPY made %d CopyFrom and %d Query calls, want 1 and 0", rec.copies, rec.queries)
	}

	rec.copies, rec.queries = 0, 0
	if _, err := db.Categories.InsertMany(t.Context(), rows); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	if rec.copies != 0 || rec.queries != 1 {
		t.Errorf("InsertMany made %d CopyFrom and %d Query calls, want 0 and 1", rec.copies, rec.queries)
	}
}

// Zero and NULL stay apart through COPY, across every type the fixture has.
func TestAudit_copyZeroIsNotNull(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	zeroPrefix := netip.MustParsePrefix("0.0.0.0/0")
	mac, err := net.ParseMAC("00:00:00:00:00:00")
	if err != nil {
		t.Fatalf("parsing a MAC: %v", err)
	}
	fb := netip.MustParsePrefix("0.0.0.0/32")
	if _, err := db.Networks.CopyFrom(t.Context(), []gendemo.Network{
		{Label: "", Subnet: zeroPrefix, Host: netip.MustParsePrefix("0.0.0.0/32"),
			Fallback: &fb, Hardware: mac},
		{Label: "", Subnet: zeroPrefix, Host: netip.MustParsePrefix("0.0.0.0/32"),
			Hardware: mac},
	}); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	got, err := db.Networks.Query().OrderBy(gendemo.Networks.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read back %d rows", len(got))
	}
	// A zero-valued prefix is a value; an absent one is NULL. The two must not
	// have converged.
	if got[0].Fallback == nil {
		t.Error("a zero-valued network became NULL")
	}
	if got[1].Fallback != nil {
		t.Errorf("an absent network became %v", got[1].Fallback)
	}
	if got[0].Label != "" || got[0].Hardware.String() != "00:00:00:00:00:00" {
		t.Errorf("a zero row = %+v", got[0])
	}
	// The empty string reached the database as an empty string.
	var labels int64
	if err := conn.QueryRow(t.Context(),
		`SELECT count(*) FROM networks WHERE label = ''`).Scan(&labels); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if labels != 2 {
		t.Errorf("%d rows have an empty label, want 2", labels)
	}
}

// COPY with no rows is a COPY of nothing rather than an error or a panic.
func TestAudit_copyEdgeShapes(t *testing.T) {
	testdb.AdminDSN(t)
	db, _ := m12env(t)

	t.Run("no rows", func(t *testing.T) {
		n, err := db.Categories.CopyFrom(t.Context(), nil)
		if err != nil {
			t.Fatalf("CopyFrom of nothing: %v", err)
		}
		if n != 0 {
			t.Errorf("copied %d rows from an empty slice", n)
		}
	})
	t.Run("no columns named", func(t *testing.T) {
		_, err := orm.CopyColumns(t.Context(), db.Categories, []gendemo.Category{{Name: "x"}})
		// Naming no columns falls back to the whole writable set, which is the
		// same thing CopyFrom does. It must not build a malformed COPY.
		if err != nil {
			t.Fatalf("CopyColumns with no columns: %v", err)
		}
	})
	t.Run("an empty sequence", func(t *testing.T) {
		n, err := db.Categories.CopyFromSeq(t.Context(), func(yield func(gendemo.Category, error) bool) {})
		if err != nil {
			t.Fatalf("CopyFromSeq of nothing: %v", err)
		}
		if n != 0 {
			t.Errorf("copied %d rows from an empty sequence", n)
		}
	})
	t.Run("a nil sequence", func(t *testing.T) {
		if _, err := db.Categories.CopyFromSeq(t.Context(), nil); err == nil {
			t.Error("a nil sequence was accepted")
		}
	})
}

// A COPY inside a savepoint rolls back with the savepoint, and the outer
// transaction carries on.
func TestAudit_copyInsideASavepoint(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	sentinel := errors.New("roll back the savepoint")
	err := db.Tx(t.Context(), func(outer *gendemo.DB) error {
		if _, err := outer.Categories.CopyFrom(t.Context(),
			[]gendemo.Category{{Name: "outer"}}); err != nil {
			return err
		}
		nested := outer.Tx(t.Context(), func(inner *gendemo.DB) error {
			if _, err := inner.Categories.CopyFrom(t.Context(),
				[]gendemo.Category{{Name: "inner"}}); err != nil {
				return err
			}
			got, err := inner.Categories.Query().All(t.Context())
			if err != nil {
				return err
			}
			if len(got) != 2 {
				t.Errorf("inside the savepoint there are %d rows, want 2", len(got))
			}
			return sentinel
		})
		if !errors.Is(nested, sentinel) {
			t.Fatalf("the nested transaction returned %v", nested)
		}
		// The savepoint rolled back; the outer COPY survives.
		got, err := outer.Categories.Query().All(t.Context())
		if err != nil {
			return err
		}
		if len(got) != 1 || got[0].Name != "outer" {
			t.Errorf("after the savepoint rolled back the rows are %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	after, err := db.Categories.Query().All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(after) != 1 {
		t.Errorf("after commit there are %d rows, want 1", len(after))
	}
}

// Thousands of failing, cancelled and succeeding copies against a two-connection
// pool: the pool has to come back to zero every time.
func TestAudit_copyResourceStress(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, schema(t))
	pool := poolFor(t, dsn)
	db := gendemo.New(pool)

	boom := errors.New("source")
	for i := range 300 {
		switch i % 4 {
		case 0:
			if _, err := db.Categories.CopyFrom(t.Context(),
				[]gendemo.Category{{Name: "ok"}}); err != nil {
				t.Fatalf("CopyFrom: %v", err)
			}
		case 1:
			// A server error: body is NOT NULL with no default.
			if _, err := orm.CopyColumns(t.Context(), db.Articles,
				[]gendemo.Article{{Title: "t"}}, gendemo.Articles.Title); err == nil {
				t.Fatal("a NOT NULL violation succeeded")
			}
		case 2:
			if _, err := db.Categories.CopyFromSeq(t.Context(),
				func(yield func(gendemo.Category, error) bool) {
					yield(gendemo.Category{Name: "x"}, nil)
					yield(gendemo.Category{}, boom)
				}); !errors.Is(err, boom) {
				t.Fatalf("source error = %v", err)
			}
		case 3:
			ctx, cancel := context.WithCancel(t.Context())
			_, err := db.Categories.CopyFromSeq(ctx, func(yield func(gendemo.Category, error) bool) {
				for j := range 10000 {
					if j == 20 {
						cancel()
					}
					if !yield(gendemo.Category{Name: "y"}, nil) {
						return
					}
				}
			})
			cancel()
			if err == nil {
				t.Fatal("a cancelled COPY succeeded")
			}
		}
	}
	// A cancelled COPY leaves its connection broken, and pgxpool destroys a
	// broken connection on its own schedule rather than before the call that
	// broke it returns. So the question is whether the pool settles, not what
	// it reports the instant afterwards — a leak never settles.
	if n := settledAcquired(t, pool); n != 0 {
		t.Errorf("%d connections still acquired after 300 copies", n)
	}
	if _, err := db.Categories.Query().All(t.Context()); err != nil {
		t.Fatalf("the pool is unusable: %v", err)
	}
}

// A large COPY streams: the source is pulled one row at a time and the whole
// thing lands.
func TestAudit_copyLarge(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	const rows = 100000
	peak, live := 0, 0
	seq := func(yield func(gendemo.Category, error) bool) {
		for i := range rows {
			live++
			if live > peak {
				peak = live
			}
			ok := yield(gendemo.Category{Name: "c"}, nil)
			live--
			if !ok {
				return
			}
			_ = i
		}
	}
	start := time.Now()
	n, err := db.Categories.CopyFromSeq(t.Context(), seq)
	if err != nil {
		t.Fatalf("CopyFromSeq: %v", err)
	}
	if n != rows {
		t.Fatalf("copied %d rows, want %d", n, rows)
	}
	if peak != 1 {
		t.Errorf("%d rows were live at once", peak)
	}
	var count int64
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM categories`).Scan(&count); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if count != rows {
		t.Errorf("the table holds %d rows", count)
	}
	t.Logf("100k rows in %v", time.Since(start))
}

// COPY and InsertMany leave the same table state for the same rows.
func TestAudit_copyAgreesWithInsertMany(t *testing.T) {
	testdb.AdminDSN(t)
	db, conn := m12env(t)

	rows := []gendemo.Article{
		{Title: "a", Body: ""},
		{Title: "", Body: "b"},
		{Title: "unicode ünï", Body: "quote \" and backslash \\"},
	}
	if _, err := db.Articles.CopyFrom(t.Context(), rows); err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	viaCopy, err := db.Articles.Query().OrderBy(gendemo.Articles.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	m11exec(t, conn, `DELETE FROM articles`)
	if _, err := db.Articles.InsertMany(t.Context(), rows); err != nil {
		t.Fatalf("InsertMany: %v", err)
	}
	viaInsert, err := db.Articles.Query().OrderBy(gendemo.Articles.ID.Asc()).All(t.Context())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(viaCopy) != len(viaInsert) {
		t.Fatalf("COPY wrote %d rows, InsertMany %d", len(viaCopy), len(viaInsert))
	}
	for i := range viaCopy {
		if viaCopy[i].Title != viaInsert[i].Title || viaCopy[i].Body != viaInsert[i].Body {
			t.Errorf("row %d: COPY wrote %+v, InsertMany wrote %+v", i, viaCopy[i], viaInsert[i])
		}
		// The generated vector followed either way.
		if (viaCopy[i].Search == nil) != (viaInsert[i].Search == nil) {
			t.Errorf("row %d: the generated column differs", i)
		}
	}
}

var _ = pgx.Identifier{}
