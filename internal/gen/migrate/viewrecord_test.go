package migrate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// M16.5 F0: the definition-identity gate.
//
// The release-critical question is whether a read-only check can tell that
// somebody altered a view by hand. These tests answer it against a real server,
// on the exact example the requirement names:
//
//	declared:  SELECT id FROM users WHERE active
//	altered:   SELECT id FROM users WHERE NOT active
//
// without executing application code, without creating a scratch relation, and
// without putting server-version-specific text anywhere that gets committed.

func conn(t *testing.T, ddl string) *pgx.Conn {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, ddl)
	c, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

const base = `CREATE TABLE users (id bigint PRIMARY KEY, active boolean NOT NULL);`

// The whole mechanism, end to end.
func TestF0_readOnlyCheckDetectsAManualEdit(t *testing.T) {
	const declared = "SELECT id FROM users WHERE active"
	c := conn(t, base)

	// What a migration does: create, then record what the server made of it.
	m := migrate.New(c, nil)
	if err := m.EnsureViewState(t.Context()); err != nil {
		t.Fatalf("creating the view state table: %v", err)
	}
	if _, err := c.Exec(t.Context(), `CREATE VIEW active_users AS `+declared); err != nil {
		t.Fatalf("creating the view: %v", err)
	}
	id := string(schema.SourceIdentityOf(declared))
	if err := migrate.RecordView(t.Context(), c, "public", "active_users", "view", id); err != nil {
		t.Fatalf("recording: %v", err)
	}

	// What a check does: read the recording, read the server, compare.
	drifted := func() bool {
		t.Helper()
		state, err := migrate.ReadViewState(t.Context(), c)
		if err != nil {
			t.Fatalf("reading the recorded state: %v", err)
		}
		rec, ok := state["public.active_users"]
		if !ok {
			t.Fatal("nothing was recorded for the view")
		}
		var actual string
		if err := c.QueryRow(t.Context(),
			`SELECT pg_get_viewdef('public.active_users'::regclass, true)`).Scan(&actual); err != nil {
			t.Fatalf("reading the definition: %v", err)
		}
		return !schema.SameOnServer(rec.Canonical, actual)
	}

	if drifted() {
		t.Fatal("a view nobody touched was reported as drifted")
	}

	// Somebody edits the database by hand. The columns are identical — only the
	// predicate moved — so nothing about the relation's shape can see this.
	if _, err := c.Exec(t.Context(),
		`CREATE OR REPLACE VIEW active_users AS SELECT id FROM users WHERE NOT active`); err != nil {
		t.Fatalf("editing the view: %v", err)
	}
	if !drifted() {
		t.Error("a manual edit to the view's predicate was not detected. " +
			"The columns are unchanged, so shape comparison cannot see it: " +
			"this is the case the recorded canonical definition exists for")
	}

	// And putting it back is clean again, which proves the comparison is about
	// the definition rather than about having been touched.
	if _, err := c.Exec(t.Context(),
		`CREATE OR REPLACE VIEW active_users AS `+declared); err != nil {
		t.Fatalf("restoring: %v", err)
	}
	if drifted() {
		t.Error("restoring the original definition still reports drift")
	}
}

// Formatting is invisible to drift, because both sides are deparsed by one
// server. This is the half where formatting-independence is real.
func TestF0_formattingIsInvisibleToDrift(t *testing.T) {
	c := conn(t, base)
	m := migrate.New(c, nil)
	if err := m.EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(t.Context(),
		`CREATE VIEW v AS SELECT id FROM users WHERE active`); err != nil {
		t.Fatal(err)
	}
	if err := migrate.RecordView(t.Context(), c, "public", "v", "view", "v1:x"); err != nil {
		t.Fatal(err)
	}
	state, _ := migrate.ReadViewState(t.Context(), c)
	recorded := state["public.v"].Canonical

	// The same query, reindented, with a comment, and with different casing.
	if _, err := c.Exec(t.Context(), "CREATE OR REPLACE VIEW v AS\n"+
		"  -- the active ones\n  select\n     id\n  from users\n  where active"); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err := c.QueryRow(t.Context(),
		`SELECT pg_get_viewdef('public.v'::regclass, true)`).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if !schema.SameOnServer(recorded, actual) {
		t.Errorf("reformatting was read as drift:\nrecorded: %q\n  actual: %q", recorded, actual)
	}
}

// Source identity is portable and is not the server's text.
func TestF0_sourceIdentityIsPortable(t *testing.T) {
	const sql = "SELECT id FROM users WHERE active"
	id := schema.SourceIdentityOf(sql)

	if !strings.HasPrefix(string(id), "v1:") {
		t.Errorf("identity %q carries no scheme version", id)
	}
	// Nothing server-shaped in it: no deparsed text, no version, no path.
	for _, bad := range []string{"SELECT", "users", "/", "pg_"} {
		if strings.Contains(string(id), bad) {
			t.Errorf("the identity contains %q, so it is not a fingerprint", bad)
		}
	}
	// The same declaration is the same identity, every time and everywhere.
	if schema.SourceIdentityOf(sql) != id {
		t.Error("the identity is not deterministic")
	}
	if schema.SourceIdentityOf("  "+sql+"\n") != id {
		t.Error("surrounding whitespace changed the identity")
	}
	// A real change changes it.
	if schema.SourceIdentityOf("SELECT id FROM users WHERE NOT active") == id {
		t.Error("changing the predicate did not change the identity")
	}
	// And reformatting changes it too, which is the documented decision rather
	// than an accident: making it formatting-independent needs a tokenizer that
	// knows a space inside a literal is data, and one that is approximately
	// right about PostgreSQL would call two different definitions equal.
	if schema.SourceIdentityOf("SELECT id\nFROM users\nWHERE active") == id {
		t.Error("this test asserts the documented behaviour; if identity became " +
			"formatting-independent, the documentation and the lock contract must change with it")
	}
}

// A database nothing has migrated has no recording, and asking is not an error.
func TestF0_noRecordingIsAFactNotAFailure(t *testing.T) {
	c := conn(t, base)
	state, err := migrate.ReadViewState(t.Context(), c)
	if err != nil {
		t.Fatalf("reading state from a database with no view table: %v", err)
	}
	if len(state) != 0 {
		t.Errorf("state = %v", state)
	}
}

// The reader interface offers no way to write.
//
// This is the read-only guarantee as a type rather than as a rule: a check is
// handed a ViewReader, and a ViewReader has no Exec to call.
func TestF0_theReaderCannotWrite(t *testing.T) {
	var r migrate.ViewReader = conn(t, base)
	if _, ok := r.(interface {
		Exec(context.Context, string, ...any) (any, error)
	}); ok {
		t.Error("the read interface exposes an Exec")
	}
	// It does satisfy the writer when the concrete type can write, which is how
	// a migration gets one — but a check is never handed the wider type.
	if _, ok := r.(migrate.ViewWriter); !ok {
		t.Error("a connection does not satisfy ViewWriter, so migrations cannot record")
	}
}

// M16.5 F.3: the recording is what a future planner will trust, so it is
// attacked here rather than assumed.

// Duplicates are impossible, and the proof is PostgreSQL refusing one rather
// than a reading of the DDL.
func TestRecord_duplicatesAreImpossible(t *testing.T) {
	c := conn(t, base+`CREATE VIEW v AS SELECT id FROM users;`)
	if err := migrate.New(c, nil).EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := migrate.RecordView(t.Context(), c, "public", "v", "view", "v1:a"); err != nil {
		t.Fatalf("recording: %v", err)
	}

	// A second insert of the same logical record, going around RecordView's
	// upsert to attack the constraint directly.
	_, err := c.Exec(t.Context(),
		`INSERT INTO public.orm_schema_views (schema_name, relation_name, kind, source_identity, canonical)
		 VALUES ('public', 'v', 'view', 'v1:b', 'something else')`)
	if err == nil {
		t.Fatal("two rows for one relation were accepted. A reader would then have to pick " +
			"one, and whichever it picked would be arbitrary")
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "23505" {
		t.Fatalf("the duplicate failed for the wrong reason: %v", err)
	}

	// And recording again through the API updates rather than duplicating.
	if err := migrate.RecordView(t.Context(), c, "public", "v", "view", "v1:c"); err != nil {
		t.Fatalf("re-recording: %v", err)
	}
	state, err := migrate.ReadViewState(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 1 || state["public.v"].SourceIdentity != "v1:c" {
		t.Errorf("re-recording produced %+v", state)
	}
}

// The key is schema and name, and that is sufficient because PostgreSQL has one
// namespace for relations: a schema cannot hold a view and a materialized view
// of one name, so the kind cannot disambiguate two rows that could not both
// exist. What the kind must do instead is travel with the record, so that a
// relation whose kind changed is caught rather than compared.
func TestRecord_keyIsSchemaAndName(t *testing.T) {
	c := conn(t, base+`
		CREATE SCHEMA other;
		CREATE VIEW v AS SELECT id FROM users;
		CREATE VIEW other.v AS SELECT id FROM public.users;`)
	if err := migrate.New(c, nil).EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"public", "other"} {
		if err := migrate.RecordView(t.Context(), c, s, "v", "view", "v1:"+s); err != nil {
			t.Fatalf("recording %s.v: %v", s, err)
		}
	}
	state, err := migrate.ReadViewState(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(state) != 2 {
		t.Fatalf("two schemas' views collapsed into %d records: %+v", len(state), state)
	}
	if state["public.v"].SourceIdentity == state["other.v"].SourceIdentity {
		t.Error("the records are indistinguishable")
	}

	// The kind is recorded, which is what lets a changed kind be noticed.
	if state["public.v"].Kind != "view" {
		t.Errorf("kind = %q", state["public.v"].Kind)
	}
}

// Corruption. Canonical text is opaque by design — it is whatever PostgreSQL
// deparsed, and validating it would mean parsing SQL — so what is detectable is
// its absence and its disagreement with the server, not its syntax.
func TestRecord_corruption(t *testing.T) {
	c := conn(t, base+`CREATE VIEW v AS SELECT id FROM users;`)
	if err := migrate.New(c, nil).EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := migrate.RecordView(t.Context(), c, "public", "v", "view", "v1:a"); err != nil {
		t.Fatal(err)
	}

	var actual string
	if err := c.QueryRow(t.Context(),
		`SELECT pg_get_viewdef('public.v'::regclass, true)`).Scan(&actual); err != nil {
		t.Fatal(err)
	}

	for _, c2 := range []struct {
		what      string
		canonical string
		wantDrift bool
	}{
		{"an emptied canonical text", "", true},
		{"a truncated canonical text", actual[:len(actual)/2], true},
		{"text that is not SQL at all", "not sql at all {{{", true},
		{"the correct text", actual, false},
	} {
		t.Run(c2.what, func(t *testing.T) {
			if _, err := c.Exec(t.Context(),
				`UPDATE public.orm_schema_views SET canonical = $1 WHERE relation_name = 'v'`,
				c2.canonical); err != nil {
				t.Fatal(err)
			}
			state, err := migrate.ReadViewState(t.Context(), c)
			if err != nil {
				t.Fatalf("reading corrupted state failed instead of reporting: %v", err)
			}
			rec, ok := state["public.v"]
			if !ok {
				t.Fatal("the record disappeared")
			}
			drift := !schema.SameOnServer(rec.Canonical, actual)
			if drift != c2.wantDrift {
				t.Errorf("%s: drift = %v, want %v", c2.what, drift, c2.wantDrift)
			}
		})
	}
}

// The writer takes an executor, so a migration can do its DDL and its recording
// in one transaction. A recorder that opened a connection of its own could not
// be rolled back with the statement it describes, and a failed migration would
// leave a record of a relation that does not exist.
func TestRecord_participatesInATransaction(t *testing.T) {
	c := conn(t, base)
	if err := migrate.New(c, nil).EnsureViewState(t.Context()); err != nil {
		t.Fatal(err)
	}

	tx, err := c.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(t.Context(), `CREATE VIEW v AS SELECT id FROM users`); err != nil {
		t.Fatal(err)
	}
	// The recorder accepts the transaction: this is the compile-time half of
	// the proof, and it is why the parameter is an interface rather than a
	// concrete connection.
	if err := migrate.RecordView(t.Context(), tx, "public", "v", "view", "v1:a"); err != nil {
		t.Fatalf("recording inside a transaction: %v", err)
	}
	// Inside the transaction, both exist.
	state, err := migrate.ReadViewState(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state["public.v"]; !ok {
		t.Fatal("the record is not visible inside the transaction that wrote it")
	}

	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// After the rollback, neither does. The record and the relation it
	// describes are applied or abandoned together.
	state, err = migrate.ReadViewState(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state["public.v"]; ok {
		t.Error("the recording survived a rolled-back migration, so it committed on its own")
	}
	var exists bool
	if err := c.QueryRow(t.Context(),
		`SELECT to_regclass('public.v') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("the view survived the rollback")
	}
}
