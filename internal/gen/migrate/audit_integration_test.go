package migrate_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// The migration engine against PostgreSQL, adversarially.
//
// The in-memory tests prove the engine is consistent with itself. These prove
// it is consistent with the server: that the SQL it renders builds the schema
// its state claims, that what PostgreSQL renders back compares equal to what
// was asked for, and that a failure leaves the history saying what actually
// happened.

// applied lists the migration history a database records.
func appliedIDs(t *testing.T, conn *pgx.Conn, set *migrate.Set) []string {
	t.Helper()
	rows, err := migrate.New(conn, set).Applied(t.Context())
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, a := range rows {
		out = append(out, a.ID)
	}
	return out
}

// -------------------------------------------------------------- indexes

// Every index shape the canonical model can express survives the round trip:
// rendered to SQL, built by PostgreSQL, read back through the catalog, and
// compared with what was asked for.
//
// The four kinds of uniqueness are the part most likely to collapse into one:
// a UNIQUE constraint, a unique index, a partial unique index and an expression
// unique index are four different objects, and PostgreSQL treats them
// differently — only the first can be a foreign key's target, and only the
// last two can be partial.
func TestAudit_indexRoundTrip(t *testing.T) {
	conn := connect(t)

	want := &schema.Schema{
		Extensions: []schema.Extension{{Name: "btree_gin"}},
		Tables: []schema.Table{{
			Schema: "public", Name: "docs",
			Columns: []schema.Column{
				{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
				{Name: "owner", Type: schema.Type{Name: "text"}},
				{Name: "title", Type: schema.Type{Name: "text"}},
				{Name: "score", Type: schema.Type{Name: "int4"}},
				{Name: "tags", Type: schema.Type{Name: "text", Array: true}, Nullable: true},
				{Name: "live", Type: schema.Type{Name: "bool"}, Default: "false"},
			},
			PrimaryKey: &schema.PrimaryKey{Name: "docs_pkey", Columns: []string{"id"}},
			// A UNIQUE constraint, which owns an index and can be a foreign
			// key's target. A bare unique index is not one of these — it is an
			// index, and lives below with the rest of them.
			Uniques: []schema.Unique{
				{Name: "docs_owner_title_key", Columns: []string{"owner", "title"}, Constraint: true},
			},
			Indexes: []schema.Index{
				{Name: "docs_owner_idx", Columns: []schema.IndexColumn{{Name: "owner"}}},
				{Name: "docs_score_uidx", Unique: true, Columns: []schema.IndexColumn{{Name: "score"}}},
				{Name: "docs_desc_idx", Columns: []schema.IndexColumn{
					{Name: "owner"}, {Name: "score", Direction: schema.Desc},
				}},
				{Name: "docs_nulls_idx", Columns: []schema.IndexColumn{
					{Name: "score", Nulls: schema.NullsFirst},
				}},
				{Name: "docs_partial_idx", Columns: []schema.IndexColumn{{Name: "score"}}, Where: "live"},
				{Name: "docs_covering_idx",
					Columns: []schema.IndexColumn{{Name: "owner"}, {Name: "score", Direction: schema.Desc}},
					Include: []string{"title"}, Where: "live"},
				{Name: "docs_opclass_idx", Columns: []schema.IndexColumn{
					{Name: "title", OpClass: "text_pattern_ops"},
				}},
				{Name: "docs_expr_idx", Columns: []schema.IndexColumn{{Expression: "lower(title)"}}},
				{Name: "docs_partial_unique_idx", Unique: true,
					Columns: []schema.IndexColumn{{Name: "title"}}, Where: "live"},
				{Name: "docs_expr_unique_idx", Unique: true,
					Columns: []schema.IndexColumn{{Expression: "lower(owner)"}}},
				{Name: "docs_tags_idx", Method: "gin", Columns: []schema.IndexColumn{{Name: "tags"}}},
				{Name: "docs_hash_idx", Method: "hash", Columns: []schema.IndexColumn{{Name: "owner"}}},
				{Name: "docs_brin_idx", Method: "brin", Columns: []schema.IndexColumn{{Name: "score"}}},
			},
		}},
	}
	want.Normalize()

	set := newSet(t, migrationFor(t, "0001_indexes", &schema.Schema{}, want))
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := introspect(t, conn)
	if diffs := schema.Diff(want, got); len(diffs) > 0 {
		t.Errorf("the schema PostgreSQL built is not the one asked for:\n    %s", strings.Join(diffs, "\n    "))
	}

	// The four kinds of uniqueness stayed four distinct objects: one
	// constraint, and three indexes that are unique in three different ways.
	docs, _ := got.Table("public", "docs")
	if len(docs.Uniques) != 1 || !docs.Uniques[0].Constraint {
		t.Errorf("uniques = %+v, want exactly the one UNIQUE constraint", docs.Uniques)
	}
	var plainUnique, partialUnique, exprUnique bool
	for _, i := range docs.Indexes {
		if i.Name == "docs_score_uidx" {
			plainUnique = i.Unique && i.Where.Empty()
		}
	}
	if !plainUnique {
		t.Errorf("a bare unique index was not read back as a unique index: %+v", docs.Indexes)
	}
	for _, i := range docs.Indexes {
		switch i.Name {
		case "docs_partial_unique_idx":
			partialUnique = i.Unique && !i.Where.Empty()
		case "docs_expr_unique_idx":
			exprUnique = i.Unique && len(i.Columns) == 1 && i.Columns[0].Name == "" && !i.Columns[0].Expression.Empty()
		}
	}
	if !partialUnique || !exprUnique {
		t.Errorf("a partial or expression unique index was not read back as one: %+v", docs.Indexes)
	}

	// Diffing the live schema against what was asked for produces nothing, or
	// a project would generate the same migration for ever.
	d, err := migrate.Compute(got, want, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !d.Empty() {
		var lines []string
		for _, op := range d.Operations {
			lines = append(lines, op.Describe())
		}
		t.Errorf("re-diffing the built schema produced %d operations:\n    %s", len(d.Operations), strings.Join(lines, "\n    "))
	}
}

// Whether an index was built concurrently is how it was made, not what it is.
// PostgreSQL does not record it, so the schema read back must still equal the
// one that asked for it.
func TestAudit_concurrentlyIsNotPartOfTheSchema(t *testing.T) {
	conn := connect(t)

	plain := &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "t",
		Columns:    []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}, {Name: "a", Type: schema.Type{Name: "text"}}},
		PrimaryKey: &schema.PrimaryKey{Name: "t_pkey", Columns: []string{"id"}},
	}}}
	plain.Normalize()

	concurrent := plain.Clone()
	concurrent.Tables[0].Indexes = []schema.Index{{
		Name: "t_a_idx", Columns: []schema.IndexColumn{{Name: "a"}}, Concurrently: true,
	}}
	concurrent.Normalize()

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, plain),
		migrationFor(t, "0002_index", plain, concurrent, "0001_initial"),
	)
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	got := introspect(t, conn)
	if diffs := schema.Diff(concurrent, got); len(diffs) > 0 {
		t.Errorf("a concurrently-built index did not compare equal:\n    %s", strings.Join(diffs, "\n    "))
	}
	// And the same schema asked for again is not a change.
	d, err := migrate.Compute(got, concurrent, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !d.Empty() {
		t.Errorf("CONCURRENTLY leaked into schema equality: %d operations", len(d.Operations))
	}
}

// ------------------------------------------------------------- defaults

// PostgreSQL does not keep the text somebody typed: it keeps a parse tree and
// renders it back, adding casts and parentheses. A comparison that saw those as
// changes would generate the same migration for ever, which is the single most
// effective way to make a migration tool untrustworthy.
func TestAudit_defaultsDoNotChurn(t *testing.T) {
	conn := connect(t)

	want := &schema.Schema{
		Enums: []schema.Enum{{Schema: "public", Name: "st", Labels: []string{"a", "b"}}},
		Tables: []schema.Table{{
			Schema: "public", Name: "t",
			Columns: []schema.Column{
				{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
				{Name: "made", Type: schema.Type{Name: "timestamptz"}, Default: "now()"},
				{Name: "stamped", Type: schema.Type{Name: "timestamptz"}, Default: "CURRENT_TIMESTAMP"},
				{Name: "flag", Type: schema.Type{Name: "bool"}, Default: "true"},
				{Name: "n", Type: schema.Type{Name: "int4"}, Default: "0"},
				{Name: "big", Type: schema.Type{Name: "numeric(10,2)"}, Default: "0.00"},
				{Name: "label", Type: schema.Type{Name: "text"}, Default: "'none'"},
				{Name: "state", Type: schema.Type{Schema: "public", Name: "st"}, Default: "'a'"},
				{Name: "list", Type: schema.Type{Name: "text", Array: true}, Default: "'{}'"},
				{Name: "doc", Type: schema.Type{Name: "jsonb"}, Default: "'{}'"},
				{Name: "computed", Type: schema.Type{Name: "int4"}, Default: "(1 + 1)"},
			},
			PrimaryKey: &schema.PrimaryKey{Name: "t_pkey", Columns: []string{"id"}},
			Checks: []schema.Check{
				{Name: "t_n_ok", Expression: "n >= 0"},
				{Name: "t_label_ok", Expression: "label <> ''"},
			},
		}},
	}
	want.Normalize()

	set := newSet(t, migrationFor(t, "0001_defaults", &schema.Schema{}, want))
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got := introspect(t, conn)

	// The catalog's rendering is not the declaration's text — that is the
	// point — so the comparison has to be semantic on both sides.
	d, err := migrate.Compute(got, want, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if !d.Empty() {
		var lines []string
		for _, op := range d.Operations {
			lines = append(lines, op.Describe())
		}
		t.Errorf("the same schema diffed against itself through PostgreSQL produced:\n    %s",
			strings.Join(lines, "\n    "))
	}
	if diffs := schema.Diff(want, got); len(diffs) > 0 {
		t.Errorf("declared and introspected defaults differ:\n    %s", strings.Join(diffs, "\n    "))
	}
}

// ------------------------------------------------------------ evolution

// Ten migrations in a row, each checked against the database it produced.
//
// Cumulative drift is what isolated tests miss: every step here is ordinary,
// and the failure being looked for is the one that only appears after the
// eighth of them.
func TestAudit_evolutionStaysExact(t *testing.T) {
	conn := connect(t)

	col := func(name, typ string) schema.Column {
		return schema.Column{Name: name, Type: schema.Type{Name: typ}}
	}
	states := []*schema.Schema{}
	s := &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "people",
		Columns:    []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault}, col("email", "text")},
		PrimaryKey: &schema.PrimaryKey{Name: "people_pkey", Columns: []string{"id"}},
	}}}
	next := func(mut func(*schema.Schema)) {
		c := s.Clone()
		mut(c)
		c.Normalize()
		states = append(states, c)
		s = c
	}
	people := func(x *schema.Schema) *schema.Table {
		for i := range x.Tables {
			if x.Tables[i].Name == "people" {
				return &x.Tables[i]
			}
		}
		panic("no people")
	}

	s.Normalize()
	states = append(states, s.Clone())
	next(func(x *schema.Schema) { // a nullable column
		p := people(x)
		p.Columns = append(p.Columns, schema.Column{Name: "nickname", Type: schema.Type{Name: "text"}, Nullable: true})
	})
	next(func(x *schema.Schema) { // a column with a default
		p := people(x)
		p.Columns = append(p.Columns, schema.Column{Name: "active", Type: schema.Type{Name: "bool"}, Default: "true"})
	})
	next(func(x *schema.Schema) { // make it reject NULL
		people(x).Columns[2].Nullable = false
		people(x).Columns[2].Default = "''"
	})
	next(func(x *schema.Schema) { // a unique constraint
		p := people(x)
		p.Uniques = []schema.Unique{{Name: "people_email_key", Columns: []string{"email"}, Constraint: true}}
	})
	next(func(x *schema.Schema) { // a second table with a foreign key
		x.Tables = append(x.Tables, schema.Table{
			Schema: "public", Name: "notes",
			Columns: []schema.Column{
				{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
				col("person_id", "int8"), col("body", "text"),
			},
			PrimaryKey: &schema.PrimaryKey{Name: "notes_pkey", Columns: []string{"id"}},
			ForeignKeys: []schema.ForeignKey{{
				Name: "notes_person_id_fkey", Columns: []string{"person_id"},
				RefSchema: "public", RefTable: "people", RefColumns: []string{"id"}, OnDelete: schema.Cascade,
			}},
		})
	})
	next(func(x *schema.Schema) { // an enum and a column using it
		x.Enums = append(x.Enums, schema.Enum{Schema: "public", Name: "kind", Labels: []string{"note", "todo"}})
		for i := range x.Tables {
			if x.Tables[i].Name == "notes" {
				x.Tables[i].Columns = append(x.Tables[i].Columns, schema.Column{
					Name: "kind", Type: schema.Type{Schema: "public", Name: "kind"}, Default: "'note'",
				})
			}
		}
	})
	next(func(x *schema.Schema) { // a new enum label
		x.Enums[0].Labels = append(x.Enums[0].Labels, "done")
	})
	next(func(x *schema.Schema) { // a partial covering index
		for i := range x.Tables {
			if x.Tables[i].Name == "notes" {
				x.Tables[i].Indexes = []schema.Index{{
					Name:    "notes_person_idx",
					Columns: []schema.IndexColumn{{Name: "person_id"}, {Name: "id", Direction: schema.Desc}},
					Include: []string{"body"}, Where: "kind = 'todo'",
				}}
			}
		}
	})
	next(func(x *schema.Schema) { // a check, added NOT VALID
		people(x).Checks = []schema.Check{{Name: "people_email_ok", Expression: "email <> ''", NotValid: true}}
	})
	next(func(x *schema.Schema) { // then validated
		people(x).Checks[0].NotValid = false
	})
	next(func(x *schema.Schema) { // widen a column and drop one
		p := people(x)
		p.Columns[1].Type = schema.Type{Name: "varchar(320)"}
		p.Columns = append(p.Columns[:3], p.Columns[4:]...)
	})

	var (
		migrations []*migrate.Migration
		prev       = &schema.Schema{}
	)
	for i, want := range states {
		id := fmt.Sprintf("%04d_step", i+1)
		var deps []string
		if i > 0 {
			deps = []string{fmt.Sprintf("%04d_step", i)}
		}
		migrations = append(migrations, migrationFor(t, id, prev, want, deps...))
		prev = want
	}

	set := newSet(t, migrations...)
	m := migrate.New(conn, set)
	for i, want := range states {
		id := fmt.Sprintf("%04d_step", i+1)
		if _, err := m.Migrate(t.Context(), id); err != nil {
			t.Fatalf("migrating to %s: %v", id, err)
		}
		got := introspect(t, conn)
		if diffs := schema.Diff(want, got); len(diffs) > 0 {
			t.Fatalf("after %s the database is not what the migrations describe:\n    %s",
				id, strings.Join(diffs, "\n    "))
		}
		// And the state the migrations reconstruct in memory agrees with both.
		state, err := set.StateAt(id)
		if err != nil {
			t.Fatalf("StateAt(%s): %v", id, err)
		}
		if diffs := schema.Diff(state, got); len(diffs) > 0 {
			t.Fatalf("after %s the reconstructed state is not the database:\n    %s",
				id, strings.Join(diffs, "\n    "))
		}
	}

	// Rolling back past the validate is refused, because PostgreSQL cannot mark
	// a validated constraint NOT VALID again — and it is refused before
	// anything moves rather than part way through.
	full := introspect(t, conn)
	historyBefore := appliedIDs(t, conn, set)
	if _, err := m.Migrate(t.Context(), "0001_step"); err == nil {
		t.Error("a rollback through an irreversible operation was allowed")
	} else if !strings.Contains(err.Error(), "cannot be reversed") {
		t.Errorf("err = %v, want it to name the irreversible operation", err)
	}
	if diffs := schema.Diff(full, introspect(t, conn)); len(diffs) > 0 {
		t.Errorf("the refused rollback changed the schema:\n    %s", strings.Join(diffs, "\n    "))
	}
	if got := appliedIDs(t, conn, set); fmt.Sprint(got) != fmt.Sprint(historyBefore) {
		t.Errorf("the refused rollback changed the history: %v -> %v", historyBefore, got)
	}

	// A rollback that stops short of it works, one step at a time.
	target := fmt.Sprintf("%04d_step", len(states)-1)
	if _, err := m.Migrate(t.Context(), target); err != nil {
		t.Fatalf("rolling back to %s: %v", target, err)
	}
	if diffs := schema.Diff(states[len(states)-2], introspect(t, conn)); len(diffs) > 0 {
		t.Errorf("rolling back one step did not reach the state before it:\n    %s",
			strings.Join(diffs, "\n    "))
	}
}

// ------------------------------------------------------------ the lock

// Two migrators on one database serialise; two on different databases do not.
//
// An advisory lock is scoped to a database, so a project that blocked every
// other project on the same server would be a shared-hosting outage rather than
// a safety feature.
func TestAudit_advisoryLockIsPerDatabase(t *testing.T) {
	testdb.AdminDSN(t)

	build := func() (*pgx.Conn, *migrate.Set) {
		dsn := testdb.Create(t, "")
		conn, err := pgx.Connect(t.Context(), dsn)
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
		s := &schema.Schema{Tables: []schema.Table{{
			Schema: "public", Name: "t",
			Columns: []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}},
		}}}
		s.Normalize()
		return conn, newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, s))
	}

	// Two databases, migrated at the same time. If the lock were global this
	// would still pass, so the real assertion is the one below: a holder on one
	// database does not stop the other.
	connA, setA := build()
	connB, setB := build()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = migrate.New(connA, setA).Migrate(t.Context(), "") }()
	go func() { defer wg.Done(); _, errs[1] = migrate.New(connB, setB).Migrate(t.Context(), "") }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("migrator %d: %v", i, err)
		}
	}

	// Hold the lock on A by hand, then prove B is unaffected and A is not.
	holder, err := pgx.Connect(t.Context(), connA.Config().ConnString())
	if err != nil {
		t.Fatalf("connecting a second time: %v", err)
	}
	defer func() { _ = holder.Close(context.WithoutCancel(t.Context())) }()
	if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_lock($1)", migrate.LockKeyForTest()); err != nil {
		t.Fatalf("taking the lock: %v", err)
	}

	// B, on another database, is not blocked.
	done := make(chan error, 1)
	go func() {
		_, err := migrate.New(connB, setB).Plan(t.Context(), "")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a migrator on another database failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a migrator on another database blocked on a lock held elsewhere")
	}

	// A, on the same database, waits — and reports a lock rather than an
	// unreachable server when it gives up.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = migrate.New(connA, setA).Plan(ctx, "")
	if err == nil {
		t.Fatal("a migrator took a lock another session was holding")
	}
	if !strings.Contains(err.Error(), "lock") {
		t.Errorf("err = %v, want it to name the lock", err)
	}

	// Releasing it lets the next attempt through. It takes a new connection:
	// giving up on a query mid-flight costs pgx the connection it was on, which
	// is why the engine documents that a caller must reconnect rather than
	// pretending the timed-out one is still good.
	if _, err := holder.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", migrate.LockKeyForTest()); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	fresh, err := pgx.Connect(t.Context(), holder.Config().ConnString())
	if err != nil {
		t.Fatalf("reconnecting: %v", err)
	}
	defer func() { _ = fresh.Close(context.WithoutCancel(t.Context())) }()
	if _, err := migrate.New(fresh, setA).Plan(t.Context(), ""); err != nil {
		t.Errorf("after the lock was released: %v", err)
	}
}

// ------------------------------------------------------- failure honesty

// A concurrent index that fails leaves an invalid index behind, and the engine
// must not claim otherwise: the migration is not recorded, and the catalog
// still shows what happened.
func TestAudit_failedConcurrentIndexIsNotRecorded(t *testing.T) {
	conn := connect(t)

	base := &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "t",
		Columns: []schema.Column{
			{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
			{Name: "a", Type: schema.Type{Name: "int4"}},
		},
		PrimaryKey: &schema.PrimaryKey{Name: "t_pkey", Columns: []string{"id"}},
	}}}
	base.Normalize()

	set := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, base))
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// Duplicate values, so a unique index cannot be built.
	if _, err := conn.Exec(t.Context(), "INSERT INTO t (a) VALUES (1), (1)"); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	withIndex := base.Clone()
	withIndex.Tables[0].Indexes = []schema.Index{{
		Name: "t_a_uidx", Unique: true, Concurrently: true,
		Columns: []schema.IndexColumn{{Name: "a"}},
	}}
	withIndex.Normalize()

	full := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		migrationFor(t, "0002_index", base, withIndex, "0001_initial"),
	)
	m := migrate.New(conn, full)
	_, err := m.Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("a unique index over duplicate values was built")
	}
	var exec *migrate.ErrExecution
	if !errors.As(err, &exec) {
		t.Fatalf("err = %v, want an execution error", err)
	}
	if exec.Atomic {
		t.Error("a concurrent index was reported as running inside a transaction")
	}
	// The message has to say the state is uncertain rather than clean.
	if !strings.Contains(err.Error(), "no migration describes") {
		t.Errorf("the error does not say the database may be in a partial state: %v", err)
	}

	// The migration is not recorded, whatever PostgreSQL left behind.
	if got := appliedIDs(t, conn, full); len(got) != 1 || got[0] != "0001_initial" {
		t.Errorf("history = %v, want only the first migration", got)
	}
	// PostgreSQL leaves the failed index in place, marked invalid. The engine
	// does not pretend it is gone, and the next check will see it.
	var invalid bool
	if err := conn.QueryRow(t.Context(),
		`SELECT count(*) > 0 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = 't_a_uidx' AND NOT i.indisvalid`).Scan(&invalid); err != nil {
		t.Fatalf("looking for the failed index: %v", err)
	}
	if !invalid {
		t.Log("PostgreSQL cleaned up the failed index on this version; nothing to reconcile")
	}
}

// An atomic migration that fails leaves nothing at all, and the database says
// so rather than the engine.
func TestAudit_atomicFailureLeavesTheDatabaseUntouched(t *testing.T) {
	conn := connect(t)

	base := &schema.Schema{Tables: []schema.Table{{
		Schema: "public", Name: "t",
		Columns:    []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault}},
		PrimaryKey: &schema.PrimaryKey{Name: "t_pkey", Columns: []string{"id"}},
	}}}
	base.Normalize()

	broken := &migrate.Migration{
		ID: "0002_broken", DependsOn: []string{"0001_initial"}, Atomic: true,
		Operations: []migrate.Operation{
			migrate.CreateTable{Table: schema.Table{
				Schema: "public", Name: "landed",
				Columns: []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}},
			}},
			migrate.RawSQL{Up: "THIS IS NOT SQL", Atomic: true, Description: "a statement the server refuses"},
		},
	}
	set := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, base), broken)

	before := introspect(t, conn)
	_, err := migrate.New(conn, set).Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("a broken migration succeeded")
	}
	var exec *migrate.ErrExecution
	if !errors.As(err, &exec) || !exec.Atomic {
		t.Fatalf("err = %v, want an atomic execution error", err)
	}

	after := introspect(t, conn)
	// The first migration ran; the second left nothing.
	if _, ok := after.Table("public", "landed"); ok {
		t.Error("the first operation of a rolled-back atomic migration survived")
	}
	if diffs := schema.Diff(base, after); len(diffs) > 0 {
		t.Errorf("a rolled-back migration changed the schema:\n    %s", strings.Join(diffs, "\n    "))
	}
	_ = before
	if got := appliedIDs(t, conn, set); len(got) != 1 {
		t.Errorf("history = %v, want only the first migration", got)
	}
}
