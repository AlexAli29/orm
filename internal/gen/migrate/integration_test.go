package migrate_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/pgintro"
	"github.com/AlexAli29/orm/internal/gen/schema"
	"github.com/AlexAli29/orm/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The migration engine against real PostgreSQL.
//
// The test that matters most is the round trip: a desired schema becomes
// migrations, the migrations run, and what the database ended up with is read
// back and compared. Everything else the engine does is only worth anything if
// that holds.

func connect(t *testing.T) *pgx.Conn {
	t.Helper()
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "")

	conn, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(t.Context())) })
	return conn
}

func newSet(t *testing.T, migrations ...*migrate.Migration) *migrate.Set {
	t.Helper()
	set, err := migrate.NewSet(migrations)
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

// migrationFor turns a desired schema into one migration that builds it.
func migrationFor(t *testing.T, id string, from, to *schema.Schema, dependsOn ...string) *migrate.Migration {
	t.Helper()
	d, err := migrate.Compute(from, to, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if d.Empty() {
		t.Fatal("the two schemas do not differ")
	}
	m := &migrate.Migration{ID: id, DependsOn: dependsOn, Operations: d.Operations, Atomic: true}
	// A migration containing an operation that cannot be atomic says so, which
	// is what the generator will do for a caller later.
	for _, op := range d.Operations {
		if !op.Transactional() {
			m.Atomic = false
		}
	}
	return m
}

func introspect(t *testing.T, conn *pgx.Conn) *schema.Schema {
	t.Helper()
	s, err := pgintro.Canonical(t.Context(), conn, []string{"public"})
	if err != nil {
		t.Fatalf("Canonical: %v", err)
	}
	// The engine's own bookkeeping — the migration history and the record of
	// what each view's definition was when it was applied — is not part of the
	// schema anybody declared.
	s.Tables = deleteTable(s.Tables, migrate.HistorySchema, migrate.HistoryTable)
	s.Tables = deleteTable(s.Tables, migrate.HistorySchema, migrate.ViewStateTable)
	return s
}

func deleteTable(tables []schema.Table, schemaName, name string) []schema.Table {
	out := tables[:0]
	for _, t := range tables {
		if t.Schema == schemaName && t.Name == name {
			continue
		}
		out = append(out, t)
	}
	return out
}

func assertEqual(t *testing.T, want, got *schema.Schema) {
	t.Helper()
	if diffs := schema.Diff(want, got); len(diffs) > 0 {
		t.Fatalf("the live schema is not what the migrations describe:\n    %s", strings.Join(diffs, "\n    "))
	}
}

// blogSchema exercises every canonical feature the engine claims to support.
func blogSchema() *schema.Schema {
	s := &schema.Schema{
		Enums: []schema.Enum{{
			Schema: "public", Name: "post_status",
			Labels: []string{"draft", "published", "archived"},
		}},
		Tables: []schema.Table{
			{
				Schema: "public", Name: "users",
				Columns: []schema.Column{
					{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
					{Name: "email", Type: schema.Type{Name: "text"}},
					{Name: "nickname", Type: schema.Type{Name: "text"}, Nullable: true},
					{Name: "active", Type: schema.Type{Name: "bool"}, Default: "true"},
					{Name: "created_at", Type: schema.Type{Name: "timestamptz"}, Default: "now()"},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
				Uniques: []schema.Unique{
					{Name: "users_email_key", Columns: []string{"email"}, Constraint: true},
				},
				Checks: []schema.Check{
					{Name: "users_email_not_blank", Expression: "email <> ''"},
				},
			},
			{
				Schema: "public", Name: "posts",
				Columns: []schema.Column{
					{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
					{Name: "author_id", Type: schema.Type{Name: "int8"}},
					{Name: "title", Type: schema.Type{Name: "text"}},
					{Name: "status", Type: schema.Type{Schema: "public", Name: "post_status"}, Default: "'draft'"},
					{Name: "published", Type: schema.Type{Name: "bool"}, Default: "false"},
					{Name: "created_at", Type: schema.Type{Name: "timestamptz"}, Default: "now()"},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "posts_pkey", Columns: []string{"id"}},
				ForeignKeys: []schema.ForeignKey{{
					Name: "posts_author_id_fkey", Columns: []string{"author_id"},
					RefSchema: "public", RefTable: "users", RefColumns: []string{"id"},
					OnDelete: schema.Cascade,
				}},
				Indexes: []schema.Index{{
					Name: "posts_feed_idx",
					Columns: []schema.IndexColumn{
						{Name: "author_id"},
						{Name: "created_at", Direction: schema.Desc},
					},
					Include: []string{"title"},
					Where:   "published = true",
				}},
			},
			{
				// A composite primary key, whose column order is meaning.
				Schema: "public", Name: "tenants",
				Columns: []schema.Column{
					{Name: "region", Type: schema.Type{Name: "text"}},
					{Name: "code", Type: schema.Type{Name: "text"}},
					{Name: "name", Type: schema.Type{Name: "text"}},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "tenants_pkey", Columns: []string{"region", "code"}},
			},
		},
	}
	s.Normalize()
	return s
}

// The round trip that closes the loop: a desired schema, turned into
// migrations, applied, read back, and compared.
func TestRoundTrip_emptyDatabase(t *testing.T) {
	conn := connect(t)
	want := blogSchema()

	set := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, want))
	m := migrate.New(conn, set)

	plan, err := m.Migrate(t.Context(), "")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("plan has %d steps, want one", len(plan.Steps))
	}
	assertEqual(t, want, introspect(t, conn))

	// The state the migrations describe and the state the database has are the
	// same thing, which is what makes the next migration computable offline.
	state, err := set.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	assertEqual(t, state, introspect(t, conn))

	// Running again finds nothing to do.
	plan, err = m.Migrate(t.Context(), "")
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if !plan.Empty() {
		t.Errorf("a second run planned %d steps", len(plan.Steps))
	}
}

// The same invariant across a change rather than from nothing.
func TestRoundTrip_evolution(t *testing.T) {
	conn := connect(t)
	s1 := blogSchema()

	s2 := blogSchema()
	// A new column, a changed default, a widened column, a new index, a new
	// enum label and a dropped constraint — more than an AddColumn.
	users := &s2.Tables[len(s2.Tables)-1]
	if users.Name != "users" {
		for i := range s2.Tables {
			if s2.Tables[i].Name == "users" {
				users = &s2.Tables[i]
			}
		}
	}
	users.Columns = append(users.Columns, schema.Column{
		Name: "status", Type: schema.Type{Name: "text"}, Default: "'new'",
	})
	users.Columns[3].Default = "false"
	users.Checks = nil
	users.Indexes = []schema.Index{{
		Name:    "users_status_idx",
		Columns: []schema.IndexColumn{{Name: "status"}, {Name: "created_at", Direction: schema.Desc}},
	}}
	for i := range s2.Enums {
		if s2.Enums[i].Name == "post_status" {
			s2.Enums[i].Labels = append(s2.Enums[i].Labels, "deleted")
		}
	}
	s2.Normalize()

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, s1),
		migrationFor(t, "0002_evolve", s1, s2, "0001_initial"),
	)
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertEqual(t, s2, introspect(t, conn))
}

// A concurrent index cannot run in a transaction, and the engine has to notice
// before it starts rather than let PostgreSQL refuse it halfway through.
func TestConcurrentIndex(t *testing.T) {
	conn := connect(t)
	base := blogSchema()

	withIndex := blogSchema()
	for i := range withIndex.Tables {
		if withIndex.Tables[i].Name != "users" {
			continue
		}
		withIndex.Tables[i].Indexes = append(withIndex.Tables[i].Indexes, schema.Index{
			Name:         "users_email_lower_idx",
			Columns:      []schema.IndexColumn{{Name: "email"}},
			Concurrently: true,
		})
	}
	withIndex.Normalize()

	concurrent := migrationFor(t, "0002_concurrent", base, withIndex, "0001_initial")
	if concurrent.Atomic {
		t.Fatal("a migration with a concurrent index was planned as atomic")
	}

	// Marked atomic anyway, it is refused while the set is validated.
	bad := *concurrent
	bad.Atomic = true
	if _, err := migrate.NewSet([]*migrate.Migration{&bad}); err == nil {
		t.Error("an atomic migration containing a concurrent index was accepted")
	} else {
		var atom *migrate.ErrAtomicity
		if !errors.As(err, &atom) {
			t.Errorf("error = %v, want an atomicity error", err)
		}
	}

	set := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, base), concurrent)
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertEqual(t, withIndex, introspect(t, conn))
}

// An atomic migration that fails leaves nothing behind, and the history does
// not claim it ran.
func TestAtomicFailure(t *testing.T) {
	conn := connect(t)
	base := blogSchema()

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{
			ID: "0002_fails", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.AddColumn{Schema: "public", Table: "users",
					Column: schema.Column{Name: "ok", Type: schema.Type{Name: "text"}, Nullable: true}},
				// A column of a type that does not exist: PostgreSQL refuses it
				// and the transaction takes the one before it with it.
				migrate.AddColumn{Schema: "public", Table: "users",
					Column: schema.Column{Name: "bad", Type: schema.Type{Name: "no_such_type"}, Nullable: true}},
			},
		},
	)
	m := migrate.New(conn, set)
	_, err := m.Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("the migration succeeded")
	}

	var exec *migrate.ErrExecution
	if !errors.As(err, &exec) {
		t.Fatalf("error = %v, want an execution error", err)
	}
	if !exec.Atomic || exec.Index != 1 {
		t.Errorf("error = %+v, want the second operation of an atomic migration", exec)
	}
	// PostgreSQL's own error survives everything the engine wrapped around it.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Errorf("error = %v, want a *pgconn.PgError to be reachable", err)
	}

	// The database is exactly what it was before the migration.
	assertEqual(t, base, introspect(t, conn))
	applied, err := m.Applied(t.Context())
	if err != nil {
		t.Fatalf("Applied: %v", err)
	}
	if len(applied) != 1 || applied[0].ID != "0001_initial" {
		t.Errorf("history = %+v, want only the first migration", applied)
	}
}

// A non-atomic migration that fails leaves what ran, and says so. Claiming a
// rollback that did not happen would be worse than the failure.
func TestNonAtomicFailure(t *testing.T) {
	conn := connect(t)
	base := blogSchema()

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{
			ID: "0002_partial", DependsOn: []string{"0001_initial"}, Atomic: false,
			Operations: []migrate.Operation{
				migrate.CreateIndex{Schema: "public", Table: "users", Index: schema.Index{
					Name: "users_nickname_idx", Columns: []schema.IndexColumn{{Name: "nickname"}},
					Concurrently: true,
				}},
				migrate.AddColumn{Schema: "public", Table: "users",
					Column: schema.Column{Name: "bad", Type: schema.Type{Name: "no_such_type"}, Nullable: true}},
			},
		},
	)
	m := migrate.New(conn, set)
	_, err := m.Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("the migration succeeded")
	}

	var exec *migrate.ErrExecution
	if !errors.As(err, &exec) {
		t.Fatalf("error = %v, want an execution error", err)
	}
	if exec.Atomic {
		t.Error("the failure was reported as atomic")
	}
	if exec.Completed != 1 {
		t.Errorf("completed = %d, want the one operation that succeeded", exec.Completed)
	}
	if !strings.Contains(err.Error(), "still applied") {
		t.Errorf("error = %v, want it to say what remains", err)
	}

	// The index really is still there, which is what the message claims.
	live := introspect(t, conn)
	found := false
	for _, tbl := range live.Tables {
		for _, idx := range tbl.Indexes {
			if idx.Name == "users_nickname_idx" {
				found = true
			}
		}
	}
	if !found {
		t.Error("the operation that succeeded was rolled back, which a non-atomic migration cannot do")
	}
	applied, _ := m.Applied(t.Context())
	if len(applied) != 1 {
		t.Errorf("history = %+v, want the failed migration not to be recorded", applied)
	}
}

// A migration edited after it was applied describes something that never
// happened, and is refused before anything else runs.
func TestChecksumMismatch(t *testing.T) {
	conn := connect(t)
	base := blogSchema()

	first := migrationFor(t, "0001_initial", &schema.Schema{}, base)
	if _, err := migrate.New(conn, newSet(t, first)).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// The same ID with different contents.
	edited := &migrate.Migration{
		ID: "0001_initial", Atomic: true,
		Operations: append(append([]migrate.Operation{}, first.Operations...),
			migrate.AddColumn{Schema: "public", Table: "users",
				Column: schema.Column{Name: "sneaked_in", Type: schema.Type{Name: "text"}, Nullable: true}}),
	}
	m := migrate.New(conn, newSet(t, edited))
	_, err := m.Migrate(t.Context(), "")
	if err == nil {
		t.Fatal("an edited migration was applied")
	}
	var modified *migrate.ErrMigrationModified
	if !errors.As(err, &modified) {
		t.Fatalf("error = %v, want ErrMigrationModified", err)
	}
	if modified.ID != "0001_initial" || modified.Applied == modified.Current {
		t.Errorf("error = %+v", modified)
	}
	// Nothing ran: the column the edit added is not there.
	live := introspect(t, conn)
	for _, tbl := range live.Tables {
		if _, ok := tbl.Column("sneaked_in"); ok {
			t.Error("the edited migration's new operation was applied")
		}
	}
}

// Migrating to an earlier target reverses the migrations above it, newest
// first, and the history and the schema both follow.
func TestReverseToTarget(t *testing.T) {
	conn := connect(t)
	s1 := blogSchema()

	s2 := blogSchema()
	for i := range s2.Tables {
		if s2.Tables[i].Name == "users" {
			s2.Tables[i].Columns = append(s2.Tables[i].Columns, schema.Column{
				Name: "status", Type: schema.Type{Name: "text"}, Nullable: true,
			})
		}
	}
	s2.Normalize()

	s3 := s2.Clone()
	for i := range s3.Tables {
		if s3.Tables[i].Name == "users" {
			s3.Tables[i].Indexes = append(s3.Tables[i].Indexes, schema.Index{
				Name: "users_status_idx", Columns: []schema.IndexColumn{{Name: "status"}},
			})
		}
	}
	s3.Normalize()

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, s1),
		migrationFor(t, "0002_status", s1, s2, "0001_initial"),
		migrationFor(t, "0003_index", s2, s3, "0002_status"),
	)
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	assertEqual(t, s3, introspect(t, conn))

	plan, err := m.Migrate(t.Context(), "0001_initial")
	if err != nil {
		t.Fatalf("reverse Migrate: %v", err)
	}
	if plan.Direction != migrate.Reverse || len(plan.Steps) != 2 {
		t.Fatalf("plan = %+v, want two reverse steps", plan)
	}
	if plan.Steps[0].Migration.ID != "0003_index" {
		t.Errorf("first reverse step is %s, want the newest migration", plan.Steps[0].Migration.ID)
	}

	assertEqual(t, s1, introspect(t, conn))
	applied, _ := m.Applied(t.Context())
	if len(applied) != 1 || applied[0].ID != "0001_initial" {
		t.Errorf("history = %+v, want only the target", applied)
	}
}

// A reverse path containing something that cannot be undone is refused before
// any of it runs, rather than discovered halfway through a rollback.
func TestIrreversibleTargetIsRefused(t *testing.T) {
	conn := connect(t)
	base := blogSchema()

	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{
			ID: "0002_enum_value", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				// PostgreSQL cannot remove an enum label, so adding one cannot
				// be undone.
				migrate.AddEnumValue{Schema: "public", Name: "post_status", Value: "hidden"},
			},
		},
	)
	m := migrate.New(conn, set)
	if _, err := m.Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err := m.Migrate(t.Context(), "0001_initial")
	if err == nil {
		t.Fatal("an irreversible migration was reversed")
	}
	var irr *migrate.ErrIrreversible
	if !errors.As(err, &irr) {
		t.Fatalf("error = %v, want ErrIrreversible", err)
	}

	// Nothing was undone: the label is still there and so is the history.
	live := introspect(t, conn)
	var labels []string
	for _, e := range live.Enums {
		if e.Name == "post_status" {
			labels = e.Labels
		}
	}
	if len(labels) != 4 {
		t.Errorf("labels = %v, want the added one to be untouched", labels)
	}
	applied, _ := m.Applied(t.Context())
	if len(applied) != 2 {
		t.Errorf("history = %+v, want both migrations still applied", applied)
	}
}

// Two migrators against one database must not run at the same time. The lock is
// PostgreSQL's, so the second waits for the first rather than racing it.
func TestAdvisoryLock(t *testing.T) {
	testdb.AdminDSN(t)
	dsn := testdb.Create(t, "")

	open := func() *pgx.Conn {
		c, err := pgx.Connect(t.Context(), dsn)
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		t.Cleanup(func() { _ = c.Close(context.WithoutCancel(t.Context())) })
		return c
	}

	base := blogSchema()
	set := newSet(t, migrationFor(t, "0001_initial", &schema.Schema{}, base))

	// Both start at once. Whichever gets the lock applies the migration; the
	// other waits, then finds nothing to do — rather than both trying to create
	// the same tables.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		applied int
		errs    []error
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			plan, err := migrate.New(open(), set).Migrate(t.Context(), "")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			applied += len(plan.Steps)
		}()
	}
	wg.Wait()

	for _, err := range errs {
		t.Errorf("a concurrent migrator failed: %v", err)
	}
	if applied != 1 {
		t.Errorf("the migration was applied %d times, want once", applied)
	}
	assertEqual(t, base, introspect(t, open()))
}

// A data migration is handed a narrow runner rather than the generated ORM, so
// that a migration written today still compiles after the entities change.
func TestRunFunc(t *testing.T) {
	conn := connect(t)
	base := blogSchema()

	var ran bool
	set := newSet(t,
		migrationFor(t, "0001_initial", &schema.Schema{}, base),
		&migrate.Migration{
			ID: "0002_backfill", DependsOn: []string{"0001_initial"}, Atomic: true,
			Operations: []migrate.Operation{
				migrate.RunFunc{
					Name: "seed a user",
					Up: func(ctx context.Context, ex migrate.SQLRunner) error {
						ran = true
						_, err := ex.Exec(ctx, `INSERT INTO users (email) VALUES ('seed@example.com')`)
						return err
					},
				},
			},
		},
	)
	if _, err := migrate.New(conn, set).Migrate(t.Context(), ""); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !ran {
		t.Fatal("the data migration did not run")
	}
	var n int
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if n != 1 {
		t.Errorf("users = %d, want the row the migration inserted", n)
	}
}
