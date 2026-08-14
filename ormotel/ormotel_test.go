package ormotel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AlexAli29/orm/observe"
	"github.com/AlexAli29/orm/ormotel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// M15.3: the OpenTelemetry adapter.
//
// The claim that matters is the same one the slog adapter makes and for the same
// structural reason: the events this adapter receives have no field holding a
// bind value, so there is nothing to export. What it does carry is asserted
// alongside, so the absence is not achieved by exporting nothing.

const secret = "m15-otel-sentinel-7c31"

func recorder(t *testing.T, opts ...ormotel.Option) (*ormotel.Tracer, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return ormotel.New(tp.Tracer("test"), opts...), rec
}

func run(tr *ormotel.Tracer, start observe.StartEvent, end observe.EndEvent) {
	ctx := tr.Start(context.Background(), start)
	tr.End(ctx, end)
}

// attrs flattens a span's attributes for searching.
func attrs(t *testing.T, rec *tracetest.SpanRecorder) (map[string]string, string) {
	t.Helper()
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("%d spans ended, want exactly one", len(spans))
	}
	out := map[string]string{}
	var all strings.Builder
	all.WriteString(spans[0].Name())
	for _, kv := range spans[0].Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
		all.WriteString(" " + string(kv.Key) + "=" + kv.Value.Emit())
	}
	for _, ev := range spans[0].Events() {
		all.WriteString(" event:" + ev.Name)
		for _, kv := range ev.Attributes {
			all.WriteString(" " + string(kv.Key) + "=" + kv.Value.Emit())
		}
	}
	return out, all.String()
}

// Scenario G: one operation, one span, carrying structure and no values.
func TestOtel_spanCarriesStructureNotValues(t *testing.T) {
	tr, rec := recorder(t)

	run(tr, observe.StartEvent{
		Op:   observe.OpQuery,
		SQL:  `SELECT "users"."id" FROM "users" WHERE "users"."email" = $1`,
		Args: 1, Fingerprint: "v1:abc", Table: "public.users", Entity: "domain.User",
		InTransaction: true, StartedAt: time.Now(),
	}, observe.EndEvent{
		Op: observe.OpQuery, Fingerprint: "v1:abc",
		Duration: time.Millisecond, Rows: 3, RowsKnown: true,
	})

	got, all := attrs(t, rec)
	if strings.Contains(all, secret) {
		t.Fatalf("a span carries a value:\n%s", all)
	}
	for key, want := range map[string]string{
		"db.system":             "postgresql",
		"orm.operation":         "query",
		"orm.query.fingerprint": "v1:abc",
		"db.collection.name":    "public.users",
		"orm.entity":            "domain.User",
		"orm.rows":              "3",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
	if sql := got["db.query.text"]; !strings.Contains(sql, "$1") {
		t.Errorf("the SQL lost its placeholder: %q", sql)
	}
}

// A COPY is one span, whatever the number of rows. A span per row would make a
// bulk load unusable in any backend.
func TestOtel_copyIsOneSpan(t *testing.T) {
	tr, rec := recorder(t)
	run(tr, observe.StartEvent{
		Op: observe.OpCopy, Table: "public.users",
		Columns: []string{"email", "age"},
	}, observe.EndEvent{Op: observe.OpCopy, Rows: 100_000, RowsKnown: true})

	got, _ := attrs(t, rec) // fails unless exactly one span ended
	if got["orm.copy.columns"] != "2" {
		t.Errorf("copy columns = %q", got["orm.copy.columns"])
	}
	if got["orm.rows"] != "100000" {
		t.Errorf("rows = %q", got["orm.rows"])
	}
}

// Relation loading is one span per statement, not per row, and each says which
// relation it loaded.
func TestOtel_relationSpans(t *testing.T) {
	tr, rec := recorder(t)
	for _, path := range []string{"Posts", "Posts.Comments"} {
		run(tr, observe.StartEvent{Op: observe.OpRelation, Table: "public.posts", Relation: path},
			observe.EndEvent{Op: observe.OpRelation})
	}
	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want one per relation statement", len(spans))
	}
	if !strings.Contains(spans[1].Name(), "Posts.Comments") {
		t.Errorf("the nested relation's span is named %q", spans[1].Name())
	}
}

// Raw SQL is the documented exception, and withholding it is real.
func TestOtel_rawSQLBoundary(t *testing.T) {
	rawSQL := "SELECT '" + secret + "'"

	tr, rec := recorder(t)
	run(tr, observe.StartEvent{Op: observe.OpQuery, SQL: rawSQL, Raw: true},
		observe.EndEvent{Op: observe.OpQuery})
	if _, all := attrs(t, rec); !strings.Contains(all, secret) {
		t.Error("raw SQL was withheld without being asked")
	}

	tr2, rec2 := recorder(t, ormotel.WithRawSQL(false))
	run(tr2, observe.StartEvent{Op: observe.OpQuery, SQL: rawSQL, Raw: true},
		observe.EndEvent{Op: observe.OpQuery})
	if _, all := attrs(t, rec2); strings.Contains(all, secret) {
		t.Errorf("WithRawSQL(false) still exported it:\n%s", all)
	}
}

// An error is recorded by classification. The server's message is a value-bearing
// surface and is off by default.
func TestOtel_errorsAreClassifiedNotQuoted(t *testing.T) {
	// An error whose message carries something sensitive, as PostgreSQL's do.
	err := errors.New(`duplicate key value violates unique constraint "users_email_key" DETAIL: Key (email)=(` + secret + `) already exists.`)

	tr, rec := recorder(t)
	run(tr, observe.StartEvent{Op: observe.OpInsert, Table: "public.users"},
		observe.EndEvent{Op: observe.OpInsert, Err: err})
	if _, all := attrs(t, rec); strings.Contains(all, secret) {
		t.Errorf("the server's message reached the span by default:\n%s", all)
	}

	// And opting in brings it back, for a caller who decided that is acceptable.
	tr2, rec2 := recorder(t, ormotel.WithErrorMessages(true))
	run(tr2, observe.StartEvent{Op: observe.OpInsert}, observe.EndEvent{Op: observe.OpInsert, Err: err})
	if _, all := attrs(t, rec2); !strings.Contains(all, secret) {
		t.Error("WithErrorMessages(true) did not record the message")
	}
}

// A plan never reaches a span. An explain operation gets a span saying it
// happened; the plan goes back to the caller.
func TestOtel_explainSpansCarryNoPlan(t *testing.T) {
	tr, rec := recorder(t)
	run(tr, observe.StartEvent{
		Op: observe.OpExplain, SQL: "SELECT * FROM users WHERE email = $1", Args: 1,
	}, observe.EndEvent{Op: observe.OpExplain, Rows: 1, RowsKnown: true})

	got, all := attrs(t, rec)
	if got["orm.operation"] != "explain" {
		t.Errorf("operation = %q", got["orm.operation"])
	}
	for _, forbidden := range []string{"Node Type", "Index Cond", "Seq Scan", "Plan Rows"} {
		if strings.Contains(all, forbidden) {
			t.Errorf("a plan reached the span (%q):\n%s", forbidden, all)
		}
	}
}

// Context propagation: the ORM's span is a child of whatever the caller's
// context already carried, with no global provider involved.
func TestOtel_spanIsAChildOfTheCallersSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := trace.NewTracerProvider(trace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	parentCtx, parent := tp.Tracer("http").Start(context.Background(), "GET /users")
	tr := ormotel.New(tp.Tracer("db"))
	ctx := tr.Start(parentCtx, observe.StartEvent{Op: observe.OpQuery, Table: "public.users"})
	tr.End(ctx, observe.EndEvent{Op: observe.OpQuery})
	parent.End()

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("%d spans", len(spans))
	}
	// The ORM's span ended first and names the request's span as its parent.
	if spans[0].Parent().SpanID() != spans[1].SpanContext().SpanID() {
		t.Error("the ORM's span is not a child of the caller's")
	}
}

// A nil tracer is inert rather than a panic.
func TestOtel_nilIsInert(t *testing.T) {
	var tr *ormotel.Tracer
	ctx := tr.Start(context.Background(), observe.StartEvent{Op: observe.OpQuery})
	tr.End(ctx, observe.EndEvent{Op: observe.OpQuery})

	tr2 := ormotel.New(nil)
	ctx2 := tr2.Start(context.Background(), observe.StartEvent{Op: observe.OpQuery})
	tr2.End(ctx2, observe.EndEvent{Op: observe.OpQuery})
}
