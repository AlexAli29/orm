package migrate_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The diff and the state it operates on.
//
// Everything here runs without a database, which is the point: a migration is
// computed from what the migrations themselves say the schema is, so the whole
// path from two states to a set of operations has to work with no network at
// all.

func col(name, typ string, opts ...func(*schema.Column)) schema.Column {
	c := schema.Column{Name: name, Type: schema.Type{Name: typ}}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func nullable(c *schema.Column)         { c.Nullable = true }
func def(e string) func(*schema.Column) { return func(c *schema.Column) { c.Default = schema.Expr(e) } }
func identity(c *schema.Column)         { c.Identity = schema.IdentityByDefault }

// users is the starting state most of these tests diff against.
func users() *schema.Schema {
	s := &schema.Schema{Tables: []schema.Table{{
		Schema:     "public",
		Name:       "users",
		Columns:    []schema.Column{col("id", "int8", identity), col("email", "text")},
		PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
	}}}
	s.Normalize()
	return s
}

func describe(t *testing.T, d migrate.Diff) []string {
	t.Helper()
	out := make([]string, 0, len(d.Operations))
	for _, op := range d.Operations {
		out = append(out, op.Describe())
	}
	return out
}

func compute(t *testing.T, from, to *schema.Schema, renames ...migrate.Rename) migrate.Diff {
	t.Helper()
	d, err := migrate.Compute(from, to, migrate.Options{Renames: renames})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	return d
}

// Two states that say the same thing produce nothing. This is the property that
// stops a project from generating a migration on every run.
func TestCompute_noChanges(t *testing.T) {
	if d := compute(t, users(), users()); !d.Empty() {
		t.Errorf("diffing a schema against itself produced %v", describe(t, d))
	}
}

// Order is not meaning for constraints and indexes, so a schema assembled in a
// different order is the same schema.
func TestCompute_orderIsNotAChange(t *testing.T) {
	a := users()
	a.Tables[0].Indexes = []schema.Index{
		{Name: "b_idx", Columns: []schema.IndexColumn{{Name: "email"}}},
		{Name: "a_idx", Columns: []schema.IndexColumn{{Name: "id"}}},
	}
	b := users()
	b.Tables[0].Indexes = []schema.Index{
		{Name: "a_idx", Columns: []schema.IndexColumn{{Name: "id"}}},
		{Name: "b_idx", Columns: []schema.IndexColumn{{Name: "email"}}},
	}
	if d := compute(t, a, b); !d.Empty() {
		t.Errorf("reordering indexes produced %v", describe(t, d))
	}
}

func TestCompute_addAndDropColumn(t *testing.T) {
	to := users()
	to.Tables[0].Columns = append(to.Tables[0].Columns, col("status", "text", nullable))

	d := compute(t, users(), to)
	if got := describe(t, d); len(got) != 1 || !strings.HasPrefix(got[0], "add column") {
		t.Fatalf("operations = %v", got)
	}

	// And the other way round.
	d = compute(t, to, users())
	if got := describe(t, d); len(got) != 1 || !strings.HasPrefix(got[0], "drop column") {
		t.Fatalf("operations = %v", got)
	}
	if d.Operations[0].Safety() != migrate.Destructive {
		t.Error("dropping a column is not classified as destructive")
	}
}

// A column whose properties changed is altered rather than replaced, because
// replacing it would throw the data away.
func TestCompute_alterColumn(t *testing.T) {
	tests := []struct {
		name string
		to   func(*schema.Column)
		want string
	}{
		{name: "type", to: func(c *schema.Column) { c.Type = schema.Type{Name: "varchar"} }, want: "type text -> varchar"},
		{name: "nullability", to: nullable, want: "drop not null"},
		{name: "default", to: def("'x'"), want: "default 'x'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			to := users()
			tt.to(&to.Tables[0].Columns[1])

			d := compute(t, users(), to)
			if len(d.Operations) != 1 {
				t.Fatalf("operations = %v", describe(t, d))
			}
			if got := d.Operations[0].Describe(); !strings.Contains(got, tt.want) {
				t.Errorf("operation = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}

// Making a column reject NULL cannot be classified as safe: PostgreSQL scans
// every row and fails on the first NULL, and nothing here knows whether there
// is one.
func TestCompute_setNotNullNeedsData(t *testing.T) {
	from := users()
	from.Tables[0].Columns[1].Nullable = true

	d := compute(t, from, users())
	if len(d.Operations) != 1 {
		t.Fatalf("operations = %v", describe(t, d))
	}
	if got := d.Operations[0].Safety(); got != migrate.RequiresData {
		t.Errorf("safety = %v, want it to require a data migration", got)
	}
}

// A NOT NULL column with no default cannot be added to a table that has rows.
// One with a default can, and the difference has to be visible.
func TestCompute_addColumnSafety(t *testing.T) {
	withDefault := users()
	withDefault.Tables[0].Columns = append(withDefault.Tables[0].Columns, col("status", "text", def("'new'")))
	if got := compute(t, users(), withDefault).Operations[0].Safety(); got != migrate.Safe {
		t.Errorf("safety = %v, want safe for a column with a default", got)
	}

	without := users()
	without.Tables[0].Columns = append(without.Tables[0].Columns, col("status", "text"))
	if got := compute(t, users(), without).Operations[0].Safety(); got != migrate.RequiresData {
		t.Errorf("safety = %v, want a NOT NULL column with no default to require data", got)
	}
}

// Nothing is guessed. A column that vanished and one that appeared are reported
// as a candidate and diffed as a drop and an add until somebody confirms.
func TestCompute_renameIsNeverGuessed(t *testing.T) {
	to := users()
	to.Tables[0].Columns[1].Name = "email_address"

	d := compute(t, users(), to)
	if len(d.Candidates) != 1 {
		t.Fatalf("candidates = %v, want the pair to be reported", d.Candidates)
	}
	c := d.Candidates[0]
	if c.From != "email" || c.To != "email_address" || c.Table != "users" {
		t.Errorf("candidate = %+v", c)
	}
	got := describe(t, d)
	if len(got) != 2 {
		t.Fatalf("operations = %v, want an add and a drop until the rename is confirmed", got)
	}

	// Confirmed, it becomes one operation that keeps the data.
	d = compute(t, users(), to, migrate.Rename{Schema: "public", Table: "users", From: "email", To: "email_address"})
	if got := describe(t, d); len(got) != 1 || !strings.HasPrefix(got[0], "rename column") {
		t.Fatalf("operations = %v, want a single rename", got)
	}
	if len(d.Candidates) != 0 {
		t.Errorf("candidates = %v, want none once the rename is confirmed", d.Candidates)
	}
}

// A rename is only a candidate when the two columns are otherwise identical.
// Different types are two different columns.
func TestCompute_renameCandidatesNeedTheSameShape(t *testing.T) {
	to := users()
	to.Tables[0].Columns[1] = col("email_address", "int8")

	if d := compute(t, users(), to); len(d.Candidates) != 0 {
		t.Errorf("candidates = %v, want none for columns of different types", d.Candidates)
	}
}

func TestCompute_tableRename(t *testing.T) {
	to := users()
	to.Tables[0].Name = "accounts"

	d := compute(t, users(), to)
	if len(d.Candidates) != 1 || d.Candidates[0].Table != "" {
		t.Fatalf("candidates = %v, want the table pair", d.Candidates)
	}

	d = compute(t, users(), to, migrate.Rename{Schema: "public", From: "users", To: "accounts"})
	if got := describe(t, d); len(got) != 1 || !strings.HasPrefix(got[0], "rename table") {
		t.Fatalf("operations = %v", got)
	}
}

// An index's key order is its meaning, so reordering it is a different index.
func TestCompute_indexKeyOrderMatters(t *testing.T) {
	from := users()
	from.Tables[0].Indexes = []schema.Index{{
		Name:    "users_a_b_idx",
		Columns: []schema.IndexColumn{{Name: "id"}, {Name: "email"}},
	}}
	to := users()
	to.Tables[0].Indexes = []schema.Index{{
		Name:    "users_a_b_idx",
		Columns: []schema.IndexColumn{{Name: "email"}, {Name: "id"}},
	}}

	d := compute(t, from, to)
	if len(d.Operations) != 2 {
		t.Fatalf("operations = %v, want the index dropped and recreated", describe(t, d))
	}
}

// A partial index is not the same object as an unqualified one over the same
// columns, and treating them as equal would silently widen a constraint.
func TestCompute_partialIndexIsADifferentIndex(t *testing.T) {
	from := users()
	from.Tables[0].Indexes = []schema.Index{{Name: "i", Columns: []schema.IndexColumn{{Name: "email"}}}}
	to := users()
	to.Tables[0].Indexes = []schema.Index{{
		Name:    "i",
		Columns: []schema.IndexColumn{{Name: "email"}},
		Where:   "deleted_at IS NULL",
	}}

	if d := compute(t, from, to); len(d.Operations) != 2 {
		t.Errorf("operations = %v, want the predicate to count as a change", describe(t, d))
	}
}

// An unset referential action and an explicit NO ACTION are the same thing to
// PostgreSQL, so they must not diff forever against each other.
func TestCompute_foreignKeyActionNormalization(t *testing.T) {
	base := func(onDelete schema.Action) *schema.Schema {
		s := users()
		s.Tables = append(s.Tables, schema.Table{
			Schema:  "public",
			Name:    "posts",
			Columns: []schema.Column{col("id", "int8", identity), col("author_id", "int8", nullable)},
			ForeignKeys: []schema.ForeignKey{{
				Name: "posts_author_id_fkey", Columns: []string{"author_id"},
				RefSchema: "public", RefTable: "users", RefColumns: []string{"id"},
				OnDelete: onDelete,
			}},
		})
		s.Normalize()
		return s
	}
	if d := compute(t, base(""), base(schema.NoAction)); !d.Empty() {
		t.Errorf("an unset action differs from NO ACTION: %v", describe(t, d))
	}
	if d := compute(t, base(""), base(schema.Cascade)); len(d.Operations) != 2 {
		t.Errorf("operations = %v, want the constraint replaced", describe(t, d))
	}
}

// A constraint added NOT VALID and later expected to be valid is validated
// rather than rebuilt. That is the whole point of the staged pattern: rebuilding
// would take the lock the pattern exists to avoid.
func TestCompute_notValidThenValidate(t *testing.T) {
	from := users()
	from.Tables[0].Checks = []schema.Check{{Name: "c", Expression: "id > 0", NotValid: true}}
	to := users()
	to.Tables[0].Checks = []schema.Check{{Name: "c", Expression: "id > 0"}}

	d := compute(t, from, to)
	if got := describe(t, d); len(got) != 1 || !strings.HasPrefix(got[0], "validate constraint") {
		t.Fatalf("operations = %v, want a single validate", got)
	}
}

// Adding an enum label is one statement; removing one is not something
// PostgreSQL can do, and the diff has to say so rather than emitting a
// destructive rebuild.
func TestCompute_enumLabels(t *testing.T) {
	from := &schema.Schema{Enums: []schema.Enum{{Schema: "public", Name: "status", Labels: []string{"a", "b"}}}}
	to := &schema.Schema{Enums: []schema.Enum{{Schema: "public", Name: "status", Labels: []string{"a", "b", "c"}}}}

	d := compute(t, from, to)
	if got := describe(t, d); len(got) != 1 || !strings.Contains(got[0], `add value "c"`) {
		t.Fatalf("operations = %v", got)
	}
	if _, err := d.Operations[0].Reverse(from); err == nil {
		t.Error("adding an enum label claimed to be reversible")
	}

	// Removing one is refused where it would be applied.
	d = compute(t, to, from)
	if len(d.Operations) != 1 {
		t.Fatalf("operations = %v", describe(t, d))
	}
	_, err := d.Operations[0].SQL()
	if err == nil {
		t.Fatal("removing an enum label rendered SQL")
	}
	if !strings.Contains(err.Error(), "cannot remove") || !strings.Contains(err.Error(), "by hand") {
		t.Errorf("error = %v, want it to explain why and what to do", err)
	}
}

// Applying a diff to the state it was computed from must produce the state it
// was computed against. This is the invariant the whole engine rests on: if it
// did not hold, the next migration would be computed from a state that never
// existed.
func TestCompute_applyingADiffReachesTheTarget(t *testing.T) {
	from := users()
	to := users()
	to.Tables[0].Columns = append(to.Tables[0].Columns, col("status", "text", def("'new'")))
	to.Tables[0].Indexes = []schema.Index{{
		Name:    "users_status_idx",
		Columns: []schema.IndexColumn{{Name: "status"}, {Name: "email", Direction: schema.Desc}},
		Where:   "status <> 'archived'",
		Include: []string{"id"},
	}}
	to.Tables = append(to.Tables, schema.Table{
		Schema:     "public",
		Name:       "profiles",
		Columns:    []schema.Column{col("id", "int8", identity), col("user_id", "int8")},
		PrimaryKey: &schema.PrimaryKey{Name: "profiles_pkey", Columns: []string{"id"}},
		Uniques:    []schema.Unique{{Name: "profiles_user_id_key", Columns: []string{"user_id"}, Constraint: true}},
		ForeignKeys: []schema.ForeignKey{{
			Name: "profiles_user_id_fkey", Columns: []string{"user_id"},
			RefSchema: "public", RefTable: "users", RefColumns: []string{"id"},
			OnDelete: schema.Cascade,
		}},
	})
	to.Normalize()

	d := compute(t, from, to)
	if d.Empty() {
		t.Fatal("no operations")
	}

	state := from.Clone()
	for _, op := range d.Operations {
		if err := op.Apply(state); err != nil {
			t.Fatalf("applying %s: %v", op.Describe(), err)
		}
	}
	state.Normalize()

	// The proof is that a second diff finds nothing left to do.
	if again := compute(t, state, to); !again.Empty() {
		t.Errorf("applying the diff did not reach the target; still to do: %v", describe(t, again))
	}
}

// Reversing a diff and applying it must return the state it started from.
func TestCompute_reverseReturnsToTheStart(t *testing.T) {
	from := users()
	to := users()
	to.Tables[0].Columns = append(to.Tables[0].Columns, col("status", "text", nullable))
	to.Tables[0].Indexes = []schema.Index{{Name: "i", Columns: []schema.IndexColumn{{Name: "status"}}}}

	d := compute(t, from, to)
	forward := from.Clone()
	for _, op := range d.Operations {
		if err := op.Apply(forward); err != nil {
			t.Fatalf("applying: %v", err)
		}
	}

	// Reversed in the opposite order, each against the state it will undo.
	state := forward.Clone()
	for i := len(d.Operations) - 1; i >= 0; i-- {
		rev, err := d.Operations[i].Reverse(state)
		if err != nil {
			t.Fatalf("reversing %s: %v", d.Operations[i].Describe(), err)
		}
		if err := rev.Apply(state); err != nil {
			t.Fatalf("applying the reverse of %s: %v", d.Operations[i].Describe(), err)
		}
	}
	state.Normalize()

	if again := compute(t, state, from); !again.Empty() {
		t.Errorf("reversing did not return to the start; still differs by: %v", describe(t, again))
	}
}

// A concurrent index decides how the migration containing it runs, so the
// operation has to say it cannot be in a transaction.
func TestOperation_concurrentIndexIsNotTransactional(t *testing.T) {
	op := migrate.CreateIndex{
		Schema: "public", Table: "users",
		Index: schema.Index{Name: "i", Columns: []schema.IndexColumn{{Name: "email"}}, Concurrently: true},
	}
	if op.Transactional() {
		t.Error("a concurrent index claims it can run inside a transaction")
	}
	if op.Safety() != migrate.Safe {
		t.Error("a concurrent index is classified as locking")
	}

	plain := op
	plain.Index.Concurrently = false
	if !plain.Transactional() {
		t.Error("an ordinary index claims it cannot run inside a transaction")
	}
	if plain.Safety() != migrate.Locking {
		t.Error("an ordinary index is not classified as locking")
	}
}

// Raw SQL changes no state, which is why it comes with a way to say what it
// did. Without one the state would quietly stop describing the database.
func TestOperation_rawSQLAndState(t *testing.T) {
	raw := migrate.RawSQL{Up: "UPDATE users SET email = lower(email)", Atomic: true}
	state := users()
	if err := raw.Apply(state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := raw.Reverse(state); err == nil {
		t.Error("raw SQL with no reverse claimed to be reversible")
	}

	withDown := migrate.RawSQL{Up: "a", Down: "b", Atomic: true}
	if _, err := withDown.Reverse(state); err != nil {
		t.Errorf("raw SQL with a reverse: %v", err)
	}

	// The state-only half changes the state and runs nothing.
	only := migrate.StateOnly{Op: migrate.AddColumn{
		Schema: "public", Table: "users", Column: col("nickname", "text", nullable),
	}}
	if err := only.Apply(state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if sql, err := only.SQL(); err != nil || len(sql) != 0 {
		t.Errorf("SQL = %v, %v; want nothing to run", sql, err)
	}
	if _, ok := state.Tables[0].Column("nickname"); !ok {
		t.Error("the state-only operation did not change the state")
	}
}
