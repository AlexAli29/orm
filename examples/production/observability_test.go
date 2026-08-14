package production_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"example.com/production/domain"
	"example.com/production/observability"
	"example.com/production/postgres"
	"example.com/production/service"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5/pgxpool"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Observability, and the one thing it must never do.
//
// An application log and a trace backend have different audiences and different
// retention from the database, and both are places a value goes to live for a
// year. So the rule the ORM states — bind values never reach a trace event — is
// checked here from the outside, through the application, against both adapters
// at once, with values chosen to be unmistakable if they appear.

// secrets is the corpus. Each is written through the application and then
// searched for in everything the adapters produced.
var secrets = []struct {
	what  string
	value string
}{
	{"an email address", "leak-probe-8f21@example.com"},
	{"a password-shaped string", "hunter2-Tr0ub4dor&3-sentinel"},
	// Deliberately not shaped like any real provider's key. The point of this
	// corpus is to be an unmistakable needle, and a string that looks like a
	// live Stripe or AWS credential is unmistakable to a secret scanner too:
	// it blocks the push, and every reader who greps the repository afterwards
	// has to work out that it was always fake.
	{"an API token", "token-sentinel-4f2b9c1e-not-a-real-credential"},
	{"a name with punctuation", "O'Brien; DROP TABLE users--"},
}

// captured is everything the two adapters emitted during one test.
type captured struct {
	mu    sync.Mutex
	log   bytes.Buffer
	spans *tracetest.InMemoryExporter
}

// text renders everything for searching: the log verbatim, and every span's
// name and attributes.
func (c *captured) text(t *testing.T) string {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	var b strings.Builder
	b.WriteString(c.log.String())
	for _, s := range c.spans.GetSpans() {
		fmt.Fprintf(&b, "\nspan %s status=%v", s.Name, s.Status)
		for _, a := range s.Attributes {
			fmt.Fprintf(&b, "\n  %s = %s", a.Key, a.Value.Emit())
		}
		for _, e := range s.Events {
			fmt.Fprintf(&b, "\n  event %s", e.Name)
			for _, a := range e.Attributes {
				fmt.Fprintf(&b, "\n    %s = %s", a.Key, a.Value.Emit())
			}
		}
	}
	return b.String()
}

// observedService builds the application with both adapters attached, exactly
// as the server does.
func observedService(t *testing.T, e *env, logSQL bool) (*service.Service, *captured) {
	t.Helper()
	c := &captured{spans: tracetest.NewInMemoryExporter()}

	// Debug level, so nothing is dropped before it can be searched. A test that
	// swept a log the adapter had filtered out would prove nothing.
	log := slog.New(slog.NewJSONHandler(&lockedWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ex := orm.Traced(e.pool, observability.New(log, observability.Config{
		LogSQL: logSQL,
		Traces: tp.Tracer("production-example"),
	}))
	return service.New(ex), c
}

type lockedWriter struct{ c *captured }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	defer w.c.mu.Unlock()
	return w.c.log.Write(p)
}

// Release-critical: no value written through the application appears in a log
// line or a span attribute, from either adapter.
func TestObservability_noBindValueReachesTelemetry(t *testing.T) {
	e := newEnv(t)
	// SQL logging on, which is the widest setting that does not turn on raw
	// SQL. Sweeping with it off would be sweeping the easy case.
	svc, cap := observedService(t, e, true)
	ctx := t.Context()

	for _, s := range secrets {
		user, err := svc.CreateUser(ctx, domain.User{Email: s.value + ".user", Name: s.value})
		if err != nil {
			t.Fatalf("writing %s: %v", s.what, err)
		}
		// A read, so the value travels as a query argument as well as an insert.
		if _, err := svc.User(ctx, user.ID); err != nil {
			t.Fatalf("reading back after %s: %v", s.what, err)
		}
		// A transaction, so the relation and multi-statement paths are covered.
		if _, err := svc.CreateProject(ctx, domain.NewProject{
			OwnerID:   user.ID,
			Slug:      "slug-" + fmt.Sprint(user.ID),
			Name:      s.value,
			FirstTask: s.value,
		}); err != nil {
			t.Fatalf("creating a project with %s: %v", s.what, err)
		}
		// A failure, because the error path is where a driver message gets
		// copied into a log by accident.
		if _, err := svc.CreateUser(ctx, domain.User{Email: s.value + ".user", Name: "Duplicate"}); err == nil {
			t.Fatal("the duplicate email was accepted")
		}
	}

	// And a Raw statement carrying a literal, because that is the one case the
	// ORM cannot redact — it would have to parse SQL — and the adapters are
	// configured not to log raw SQL for exactly that reason. Turning
	// WithRawSQL on is what this asserts the cost of.
	rawEx := orm.Traced(e.pool, observability.New(
		slog.New(slog.NewJSONHandler(&lockedWriter{c: cap}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		observability.Config{LogSQL: true},
	))
	const rawLiteral = "raw-literal-sentinel-4c1e"
	if _, err := orm.Raw(postgres.New(rawEx).Users,
		"SELECT * FROM users WHERE name = $1 OR name = '"+rawLiteral+"'", "bound").
		All(ctx); err != nil {
		t.Fatalf("running the raw probe: %v", err)
	}

	out := cap.text(t)
	if out == "" {
		t.Fatal("the adapters produced nothing; this test would pass vacuously")
	}
	for _, s := range secrets {
		if strings.Contains(out, s.value) {
			t.Errorf("%s appears in telemetry:\n%s", s.what, excerpt(out, s.value))
		}
	}
	if strings.Contains(out, rawLiteral) {
		t.Errorf("a literal inside orm.Raw reached telemetry, so raw SQL logging is on:\n%s",
			excerpt(out, rawLiteral))
	}

	// The sweep is only meaningful if the adapters were in fact reporting. The
	// SQL should be there, with its placeholders.
	if !strings.Contains(out, "INSERT INTO") {
		t.Error("no SQL was logged, so the sweep searched an empty haystack")
	}
	if !strings.Contains(out, "$1") {
		t.Error("the logged SQL has no placeholders; that is what redaction looks like when it works")
	}
	// And spans were produced.
	if len(cap.spans.GetSpans()) == 0 {
		t.Error("no spans were exported")
	}
}

// The DSN is a credential, and it is in the pool's configuration on every
// connection. It must not travel with an event.
//
// The test connects through a role created for it, with a password chosen to be
// unmistakable. Sweeping for the container's own password would not work: it is
// a short generic word that occurs inside ordinary log messages, so the search
// would either report a match that is not a leak or be quietly weakened until
// it reported nothing at all.
func TestObservability_noCredentialInTelemetry(t *testing.T) {
	e := newEnv(t)

	const password = "sentinel-Pa55w0rd-c41e9b7f"
	if _, err := e.pool.Exec(t.Context(),
		`CREATE ROLE leak_probe LOGIN PASSWORD '`+password+`'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = e.pool.Exec(context.Background(), `DROP ROLE IF EXISTS leak_probe`)
	})
	if _, err := e.pool.Exec(t.Context(),
		`GRANT ALL ON ALL TABLES IN SCHEMA public TO leak_probe;
		 GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO leak_probe;
		 GRANT USAGE ON SCHEMA public TO leak_probe`); err != nil {
		t.Fatal(err)
	}

	cfg := e.pool.Config().Copy()
	cfg.ConnConfig.User = "leak_probe"
	cfg.ConnConfig.Password = password
	probe, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()

	c := &captured{spans: tracetest.NewInMemoryExporter()}
	log := slog.New(slog.NewJSONHandler(&lockedWriter{c: c}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(c.spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	svc := service.New(orm.Traced(probe, observability.New(log, observability.Config{
		LogSQL: true,
		Traces: tp.Tracer("production-example"),
	})))

	if _, err := svc.CreateUser(t.Context(), domain.User{Email: "dsn@example.com", Name: "DSN"}); err != nil {
		t.Fatal(err)
	}
	// A failure too: the error path is where a connection string most often
	// gets copied into a message.
	_, _ = svc.User(t.Context(), 999999)

	out := c.text(t)
	if out == "" {
		t.Fatal("the adapters produced nothing; this test would pass vacuously")
	}
	for _, forbidden := range []string{password, "postgres://", "password=", "leak_probe:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("telemetry contains %q:\n%s", forbidden, excerpt(out, forbidden))
		}
	}
}

// Both adapters receive the same events, which is what Multi is for.
//
// The failure this catches is the one the package comment warns about: an
// application that wraps the executor twice gets one tracer, silently.
func TestObservability_bothAdaptersSeeEveryOperation(t *testing.T) {
	e := newEnv(t)
	svc, cap := observedService(t, e, false)

	if _, err := svc.CreateUser(t.Context(), domain.User{Email: "multi@example.com", Name: "Multi"}); err != nil {
		t.Fatal(err)
	}

	cap.mu.Lock()
	logged := cap.log.String()
	spans := cap.spans.GetSpans()
	cap.mu.Unlock()

	if logged == "" {
		t.Error("the log adapter received nothing")
	}
	if len(spans) == 0 {
		t.Error("the trace adapter received nothing")
	}

	// Every log line is JSON with a fingerprint, which is the thing that groups
	// executions of one statement.
	var found bool
	for line := range strings.SplitSeq(strings.TrimSpace(logged), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if fp, ok := rec["fingerprint"].(string); ok && strings.HasPrefix(fp, "v1:") {
			found = true
		}
	}
	if !found {
		t.Errorf("no logged operation carried a versioned fingerprint:\n%s", logged)
	}
}

// A Multi with a nil member does not panic, because building the list from
// configuration is the reason it exists.
func TestObservability_multiSkipsNil(t *testing.T) {
	e := newEnv(t)
	ex := orm.Traced(e.pool, observability.Multi{nil, nil})
	svc := service.New(ex)
	if _, err := svc.CreateUser(t.Context(), domain.User{Email: "nil-tracer@example.com", Name: "Nil"}); err != nil {
		t.Fatalf("a Multi of nils broke the application: %v", err)
	}
}

// excerpt shows the neighbourhood of a match, so a failure names the line
// rather than printing the whole capture.
func excerpt(hay, needle string) string {
	i := strings.Index(hay, needle)
	if i < 0 {
		return ""
	}
	start := max(i-200, 0)
	end := min(i+len(needle)+200, len(hay))
	return "..." + hay[start:end] + "..."
}
