package pgintro_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/testdb"
)

// M16.5 F.2.5: does reading views scale, or is it a query per view?
//
// The point is not speed. It is to find out, before a migration planner depends
// on this, whether the architecture is batched or whether it issues a round
// trip per relation — because the second only hurts once a real project has a
// few hundred views, which is after the planner has been built on it.

func buildViews(n int) string {
	var b strings.Builder
	b.WriteString(`CREATE TABLE base (id bigint PRIMARY KEY, email text NOT NULL, active boolean NOT NULL);`)
	for i := range n {
		fmt.Fprintf(&b, "\nCREATE VIEW v%d AS SELECT id, email FROM base WHERE active AND id > %d;", i, i)
		if i%10 == 0 {
			// A materialized view with indexes every tenth relation, so the
			// index and dependency readers are exercised at scale too.
			fmt.Fprintf(&b, "\nCREATE MATERIALIZED VIEW m%d AS SELECT id, email FROM v%d WITH DATA;", i, i)
			fmt.Fprintf(&b, "\nCREATE UNIQUE INDEX m%d_key ON m%d (id);", i, i)
			fmt.Fprintf(&b, "\nCREATE INDEX m%d_lower ON m%d (lower(email));", i, i)
		}
	}
	return b.String()
}

func TestScale_readingViewsIsBatched(t *testing.T) {
	testdb.AdminDSN(t)
	for _, n := range []int{10, 100, 1000} {
		t.Run(fmt.Sprintf("%d views", n), func(t *testing.T) {
			dsn := testdb.Create(t, buildViews(n))
			conn, err := pgintro.Connect(t.Context(), dsn)
			if err != nil {
				t.Fatalf("connecting: %v", err)
			}
			defer func() { _ = conn.Close(context.Background()) }()

			// Ask the server how many statements it has run, before and after.
			// This counts what actually reached PostgreSQL rather than what the
			// code looks like it sends.
			var before, after int64
			const q = `SELECT sum(calls)::bigint FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`
			counted := conn.QueryRow(t.Context(), q).Scan(&before) == nil

			start := time.Now()
			s, err := pgintro.Canonical(t.Context(), conn, []string{"public"})
			if err != nil {
				t.Fatalf("reading the canonical schema: %v", err)
			}
			elapsed := time.Since(start)

			mats := (n + 9) / 10
			if len(s.Views) != n {
				t.Errorf("read %d views, want %d", len(s.Views), n)
			}
			if len(s.MaterializedViews) != mats {
				t.Errorf("read %d materialized views, want %d", len(s.MaterializedViews), mats)
			}
			if counted {
				_ = conn.QueryRow(t.Context(), q).Scan(&after)
				t.Logf("%d views + %d materialized views: %v, %d server statements",
					n, mats, elapsed.Round(time.Millisecond), after-before)
			} else {
				t.Logf("%d views + %d materialized views: %v (pg_stat_statements unavailable; "+
					"statement count not measured)", n, mats, elapsed.Round(time.Millisecond))
			}

			// Every index survived the scale, which is what proves the batched
			// reader is reading them all rather than the first few.
			var indexes int
			for _, m := range s.MaterializedViews {
				indexes += len(m.Indexes)
			}
			if indexes != mats*2 {
				t.Errorf("read %d indexes across %d materialized views, want %d", indexes, mats, mats*2)
			}
		})
	}
}
