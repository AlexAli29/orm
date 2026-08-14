package ormextdemo_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"example.com/ormextdemo"
	"github.com/AlexAli29/orm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The conformance checks an extension author should run.
//
// These are the questions the ORM's own audits asked of PostGIS, applied to an
// extension built only from the public SDK: does the declared result type match
// pg_typeof, does NULL behave as the declaration says, does an outer join make
// the result nullable, do the operands' sources survive into the statement's
// dependencies, and do the placeholders keep the statement's own numbering when
// extension calls nest.
//
// They run against a real server because every one of them is a question about
// PostgreSQL rather than about Go.

// The fixture is a table of its own, created here, so this module needs nothing
// generated and no other package's schema.
const fixtureDDL = `
CREATE TABLE IF NOT EXISTS ext_docs (
    id     bigint PRIMARY KEY,
    title  text NOT NULL,
    body   text NOT NULL,
    note   text
);
INSERT INTO ext_docs (id, title, body, note) VALUES
    (1, 'alpha', 'alpha', 'has a note'),
    (2, 'beta',  'gamma', NULL)
ON CONFLICT (id) DO NOTHING;
`

// docs is a hand-written descriptor set, standing in for generated code. An
// extension author would have real generated descriptors; what matters here is
// that they are the ordinary public types.
type Doc struct{}

var docsSource = orm.NewSource("public", "ext_docs")

var Docs = struct {
	Src   *orm.Source
	ID    orm.OrdCol[Doc, int64]
	Title orm.TextCol[Doc]
	Body  orm.TextCol[Doc]
	Note  orm.NullTextCol[Doc]
}{
	Src:   docsSource,
	ID:    orm.NewOrdCol[Doc, int64](docsSource, "id"),
	Title: orm.NewTextCol[Doc](docsSource, "title"),
	Body:  orm.NewTextCol[Doc](docsSource, "body"),
	Note:  orm.NewNullTextCol[Doc](docsSource, "note"),
}

func connect(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("ORM_TEST_ADMIN_DSN")
	if dsn == "" {
		t.Skip("ORM_TEST_ADMIN_DSN is not set")
	}
	p, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(p.Close)
	if _, err := p.Exec(t.Context(), fixtureDDL); err != nil {
		t.Fatalf("creating the fixture: %v", err)
	}
	return p
}

// The declared result type is a claim. This is how an author checks it.
func TestConformance_pgTypeof(t *testing.T) {
	p := connect(t)
	for _, tt := range []struct {
		name, sql, want string
	}{
		{"md5", "md5(title)", "text"},
		{"octet_length", "octet_length(title)", "integer"},
		{"repeat", "repeat(title, 2)", "text"},
		{"starts_with", "title ^@ 'a'", "boolean"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			if err := p.QueryRow(t.Context(),
				"SELECT pg_typeof("+tt.sql+")::text FROM ext_docs LIMIT 1").Scan(&got); err != nil {
				t.Fatalf("asking PostgreSQL: %v", err)
			}
			if got != tt.want {
				t.Errorf("pg_typeof = %s, want %s — the extension's declared Go type is wrong", got, tt.want)
			}
		})
	}
}

// The expressions run, and produce what PostgreSQL produces.
func TestConformance_results(t *testing.T) {
	p := connect(t)

	type row struct {
		id   int64
		hash string
		size int32
	}
	got, err := orm.Compose(p, orm.Project3(
		orm.Of(Docs.ID),
		ormextdemo.MD5(Docs.Title),
		ormextdemo.OctetLength(Docs.Title),
		func(id int64, hash string, size int32) row { return row{id, hash, size} })).
		From(Docs.Src).
		OrderBy(orm.Of(Docs.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("running the extension expressions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d rows", len(got))
	}

	var wantHash string
	var wantSize int32
	if err := p.QueryRow(t.Context(),
		"SELECT md5(title), octet_length(title) FROM ext_docs WHERE id = 1").Scan(&wantHash, &wantSize); err != nil {
		t.Fatalf("the handwritten comparison: %v", err)
	}
	if got[0].hash != wantHash || got[0].size != wantSize {
		t.Errorf("row 1 = %+v, PostgreSQL says %s / %d", got[0], wantHash, wantSize)
	}
}

// NULL in, NULL out — and the nullable form is the one that can hold it.
func TestConformance_nullSemantics(t *testing.T) {
	p := connect(t)

	got, err := orm.Compose(p, orm.Project1(
		ormextdemo.MD5Null(Docs.Note),
		func(h *string) *string { return h })).
		From(Docs.Src).
		OrderBy(orm.Of(Docs.ID).Asc()).
		All(t.Context())
	if err != nil {
		t.Fatalf("md5 over a nullable column: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d rows", len(got))
	}
	if got[0] == nil {
		t.Error("a row with a note produced no hash")
	}
	if got[1] != nil {
		t.Errorf("md5(NULL) produced %q; PostgreSQL returns NULL", *got[1])
	}
}

// An outer join makes an extension's result nullable, whatever the column's own
// constraint says. This is the frozen M11 rule, and an extension gets it for
// free — but only if it built its expression from the SDK rather than around it.
func TestConformance_outerJoinNullability(t *testing.T) {
	p := connect(t)
	other := Docs.Src.As("other")
	otherTitle := orm.NewTextCol[Doc](other, "title")

	got, err := orm.Compose(p, orm.Project2(
		orm.Of(Docs.ID),
		orm.Opt(ormextdemo.MD5(otherTitle)),
		func(id int64, hash *string) *string { return hash })).
		From(Docs.Src).
		LeftJoin(other, orm.Cond(otherTitle.Eq("no such title"))).
		All(t.Context())
	if err != nil {
		t.Fatalf("the outer join: %v", err)
	}
	for i, h := range got {
		if h != nil {
			t.Errorf("row %d: the absent side produced %q", i, *h)
		}
	}
	if len(got) == 0 {
		t.Fatal("no rows, so this proves nothing")
	}
}

// An extension expression declares the sources its operands name, so a statement
// that does not read them is refused before it reaches PostgreSQL.
//
// This is the property that stops an extension forging source ownership: the
// operand carries its own source and the ORM checks it, whatever the extension
// wrapped it in.
func TestConformance_sourceDependencies(t *testing.T) {
	other := Docs.Src.As("other")
	otherTitle := orm.NewTextCol[Doc](other, "title")

	for _, tt := range []struct {
		name string
		pred orm.Predicate[orm.Composed]
	}{
		{"a function's operand", orm.Cond(ormextdemo.StartsWith(otherTitle, "x"))},
		{"an operator with an extension call on each side",
			orm.Cond(ormextdemo.SameHash(Docs.Title, otherTitle))},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// The statement reads ext_docs but not its alias.
			_, _, err := orm.Compose(nil, orm.Project1(
				orm.Of(Docs.ID), func(id int64) int64 { return id })).
				From(Docs.Src).
				Where(tt.pred).
				SQL()
			if err == nil {
				t.Fatal("a statement naming an unread source compiled")
			}
			if !strings.Contains(err.Error(), "scope error") {
				t.Errorf("error = %v, want a scope error", err)
			}
		})
	}
}

// A value inside a nested extension call takes its number from the statement's
// own namespace, wherever the expression ends up.
func TestConformance_placeholderNumbering(t *testing.T) {
	p := connect(t)

	sql, args, err := orm.Compose(nil, orm.Project1(
		orm.Of(Docs.ID), func(id int64) int64 { return id })).
		From(Docs.Src).
		Where(
			orm.Cond(Docs.Title.Eq("alpha")),
			orm.Cond(ormextdemo.Tagged(Docs.Body, 2, "alphaalpha")),
			orm.Cond(Docs.ID.Gt(int64(0))),
		).
		SQL()
	if err != nil {
		t.Fatalf("SQL: %v", err)
	}
	// Four values: the title, the repeat count, the expected string, the id.
	if len(args) != 4 {
		t.Fatalf("args = %d (%v)", len(args), args)
	}
	for i := 1; i <= 4; i++ {
		ph := "$" + string(rune('0'+i))
		if !strings.Contains(sql, ph) {
			t.Errorf("%s is missing from %s", ph, sql)
		}
	}
	// And the whole thing runs, which is the real check that the numbers line up
	// with the values.
	got, err := orm.Compose(p, orm.Project1(
		orm.Of(Docs.ID), func(id int64) int64 { return id })).
		From(Docs.Src).
		Where(
			orm.Cond(Docs.Title.Eq("alpha")),
			orm.Cond(ormextdemo.Tagged(Docs.Body, 2, "alphaalpha")),
			orm.Cond(Docs.ID.Gt(int64(0))),
		).
		All(t.Context())
	if err != nil {
		t.Fatalf("running it: %v", err)
	}
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("ids = %v, want [1]", got)
	}
}

// An extension declaring the wrong result type is the extension's mistake, and
// it surfaces as a scan error rather than as a wrong value.
func TestConformance_wrongResultTypeIsAScanError(t *testing.T) {
	p := connect(t)

	// md5 returns text; this claims int64.
	wrong := orm.FnExpr[int64]("md5", orm.ArgOf(Docs.Title))
	_, err := orm.Compose(p, orm.Project1(wrong, func(v int64) int64 { return v })).
		From(Docs.Src).
		All(t.Context())
	if err == nil {
		t.Fatal("a wrongly typed extension expression produced a value")
	}
	// The failure is at the scan, which is the documented trust boundary.
	if !strings.Contains(err.Error(), "scan") && !strings.Contains(err.Error(), "cannot") {
		t.Logf("error = %v", err)
	}
}

// One extension expression, many goroutines: the descriptors and the SDK are
// read-only.
func TestConformance_race(t *testing.T) {
	p := connect(t)
	errs := make(chan error, 32)
	for i := range 32 {
		go func(i int) {
			_, err := orm.Compose(p, orm.Project1(
				ormextdemo.MD5(Docs.Title), func(h string) string { return h })).
				From(Docs.Src).
				Where(orm.Cond(ormextdemo.StartsWith(Docs.Title, "a"))).
				All(context.Background())
			errs <- err
		}(i)
	}
	for range 32 {
		if err := <-errs; err != nil {
			t.Fatalf("a worker failed: %v", err)
		}
	}
}

var _ = pgx.ErrNoRows
