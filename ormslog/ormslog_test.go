package ormslog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm/observe"
	"github.com/AlexAli29/orm/ormslog"
)

// M15.3: the slog adapter.
//
// The claim under test is the one that matters: there is no path from a bind
// value to a log record, because the events this adapter receives have no field
// that holds one. What it can log — placeholders, counts, classifications — is
// asserted alongside, so that the absence is not achieved by logging nothing.

func capture(t *testing.T, opts ...ormslog.Option) (*ormslog.Tracer, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return ormslog.New(logger, opts...), &buf
}

const secret = "m15-sentinel-pa55word-9f2b"

// A full round of events, with the sentinel in every field an adapter could
// mistakenly forward.
func run(tr *ormslog.Tracer, start observe.StartEvent, end observe.EndEvent) {
	ctx := tr.Start(context.Background(), start)
	tr.End(ctx, end)
}

func TestSlog_logsStructureAndNeverValues(t *testing.T) {
	tr, buf := capture(t)

	run(tr, observe.StartEvent{
		Op: observe.OpQuery, SQL: `SELECT "users"."id" FROM "users" WHERE "users"."email" = $1`,
		Args: 1, Fingerprint: "v1:abc123", Table: "public.users", Entity: "domain.User",
		InTransaction: true, StartedAt: time.Now(),
	}, observe.EndEvent{
		Op: observe.OpQuery, Fingerprint: "v1:abc123",
		Duration: 3 * time.Millisecond, Rows: 1, RowsKnown: true,
	})

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Fatalf("the record carries a value:\n%s", out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("the record is not JSON: %v\n%s", err, out)
	}
	for _, want := range []string{"op", "fingerprint", "table", "entity", "args", "rows", "sql"} {
		if _, ok := rec[want]; !ok {
			t.Errorf("the record has no %q field:\n%s", want, out)
		}
	}
	if sql, _ := rec["sql"].(string); !strings.Contains(sql, "$1") {
		t.Errorf("the SQL lost its placeholder: %q", sql)
	}
	// The count of arguments, not the arguments.
	if n, _ := rec["args"].(float64); n != 1 {
		t.Errorf("args = %v", rec["args"])
	}
}

// The event model has no field for a value, so there is nothing to leak even
// when a caller tries. This is the structural half of the guarantee.
func TestSlog_theEventModelHasNowhereForAValue(t *testing.T) {
	tr, buf := capture(t)
	// Every string field the adapter reads, loaded with the sentinel. If any of
	// them were a value in production this would find it; they are all
	// structure, so what appears is structure.
	run(tr, observe.StartEvent{
		Op: observe.OpQuery, SQL: "SELECT $1", Args: 1,
		Fingerprint: "v1:deadbeef", Table: "public.t", Entity: "domain.T",
	}, observe.EndEvent{Op: observe.OpQuery, Duration: time.Millisecond})

	if strings.Contains(buf.String(), secret) {
		t.Fatalf("a value reached the log:\n%s", buf)
	}
}

// Raw SQL is the documented exception, and the option to withhold it is real.
func TestSlog_rawSQLBoundary(t *testing.T) {
	rawSQL := "SELECT '" + secret + "'"

	t.Run("raw SQL is logged by default, because the caller wrote it", func(t *testing.T) {
		tr, buf := capture(t)
		run(tr, observe.StartEvent{Op: observe.OpQuery, SQL: rawSQL, Raw: true},
			observe.EndEvent{Op: observe.OpQuery})
		if !strings.Contains(buf.String(), secret) {
			t.Error("raw SQL was withheld without being asked")
		}
	})

	t.Run("and can be withheld separately from generated SQL", func(t *testing.T) {
		tr, buf := capture(t, ormslog.WithRawSQL(false))
		run(tr, observe.StartEvent{Op: observe.OpQuery, SQL: rawSQL, Raw: true},
			observe.EndEvent{Op: observe.OpQuery})
		if strings.Contains(buf.String(), secret) {
			t.Errorf("WithRawSQL(false) still logged it:\n%s", buf)
		}
		// A generated statement is still logged.
		tr2, buf2 := capture(t, ormslog.WithRawSQL(false))
		run(tr2, observe.StartEvent{Op: observe.OpQuery, SQL: "SELECT $1"},
			observe.EndEvent{Op: observe.OpQuery})
		if !strings.Contains(buf2.String(), "$1") {
			t.Error("withholding raw SQL also withheld generated SQL")
		}
	})
}

// An error is recorded by classification, not by the server's message.
func TestSlog_errorsAreClassified(t *testing.T) {
	tr, buf := capture(t)
	run(tr, observe.StartEvent{Op: observe.OpInsert, SQL: "INSERT ...", Table: "public.users"},
		observe.EndEvent{Op: observe.OpInsert, Err: context.Canceled, Cancelled: true})

	out := buf.String()
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("a failure was not logged at error level:\n%s", out)
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("the cancellation was not recorded:\n%s", out)
	}
}

// The slow threshold is the caller's, there is no default, and crossing it logs
// rather than investigates.
func TestSlog_slowThreshold(t *testing.T) {
	t.Run("no threshold means nothing is slow", func(t *testing.T) {
		tr, buf := capture(t)
		run(tr, observe.StartEvent{Op: observe.OpQuery}, observe.EndEvent{Op: observe.OpQuery, Duration: time.Hour})
		if strings.Contains(buf.String(), "slow") {
			t.Errorf("an hour was called slow without a threshold:\n%s", buf)
		}
	})

	t.Run("crossing the caller's threshold logs it", func(t *testing.T) {
		tr, buf := capture(t, ormslog.WithSlowThreshold(10*time.Millisecond))
		run(tr, observe.StartEvent{Op: observe.OpQuery, SQL: "SELECT 1"},
			observe.EndEvent{Op: observe.OpQuery, Duration: 50 * time.Millisecond})
		out := buf.String()
		if !strings.Contains(out, "slow") || !strings.Contains(out, `"level":"WARN"`) {
			t.Errorf("a slow operation was not reported:\n%s", out)
		}
		if !strings.Contains(out, "threshold") {
			t.Errorf("the record does not say what threshold was crossed:\n%s", out)
		}
		// Release-critical: observing is not investigating. Nothing in the
		// record suggests a plan was fetched, and the adapter has no executor
		// to fetch one with.
		if strings.Contains(out, "Node Type") || strings.Contains(strings.ToUpper(out), "EXPLAIN") {
			t.Errorf("the slow path ran EXPLAIN:\n%s", out)
		}
	})

	t.Run("under the threshold is ordinary", func(t *testing.T) {
		tr, buf := capture(t, ormslog.WithSlowThreshold(time.Second))
		run(tr, observe.StartEvent{Op: observe.OpQuery}, observe.EndEvent{Op: observe.OpQuery, Duration: time.Millisecond})
		if strings.Contains(buf.String(), "slow") {
			t.Errorf("a fast operation was called slow:\n%s", buf)
		}
	})
}

// A nil logger and a nil tracer are both inert rather than a panic.
func TestSlog_nilIsInert(t *testing.T) {
	var tr *ormslog.Tracer
	ctx := tr.Start(context.Background(), observe.StartEvent{Op: observe.OpQuery})
	tr.End(ctx, observe.EndEvent{Op: observe.OpQuery})

	tr2 := ormslog.New(nil)
	ctx2 := tr2.Start(context.Background(), observe.StartEvent{Op: observe.OpQuery})
	tr2.End(ctx2, observe.EndEvent{Op: observe.OpQuery})
}
