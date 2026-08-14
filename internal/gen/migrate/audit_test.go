package migrate_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The migration engine, audited rather than demonstrated.
//
// These tests are the ones that try to break the guarantees: an operation whose
// SQL and whose state disagree, a diff that does not reach the schema it was
// computed from, a checksum that depends on where the repository was checked
// out, an artifact that panics the tool instead of being refused.

// ---------------------------------------------- state / SQL / reverse agree

// Every operation answers three questions — what it does to the state, what SQL
// it runs, and how to undo it — and the three have to be answers to the same
// question. This is the table that says so for every operation there is.
func TestAudit_everyOperationIsSelfConsistent(t *testing.T) {
	base := func() *schema.Schema {
		return &schema.Schema{
			Extensions: []schema.Extension{{Name: "citext"}},
			Enums:      []schema.Enum{{Schema: "public", Name: "st", Labels: []string{"a", "b"}}},
			Tables: []schema.Table{{
				Schema: "public", Name: "t",
				Columns: []schema.Column{
					{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
					{Name: "a", Type: schema.Type{Name: "text"}, Nullable: true},
					{Name: "b", Type: schema.Type{Name: "int4"}},
				},
				PrimaryKey:  &schema.PrimaryKey{Name: "t_pkey", Columns: []string{"id"}},
				Uniques:     []schema.Unique{{Name: "t_a_key", Columns: []string{"a"}, Constraint: true}},
				Checks:      []schema.Check{{Name: "t_b_ok", Expression: "b > 0"}},
				Indexes:     []schema.Index{{Name: "t_a_idx", Columns: []schema.IndexColumn{{Name: "a"}}}},
				ForeignKeys: nil,
			}, {
				Schema: "public", Name: "u",
				Columns: []schema.Column{
					{Name: "id", Type: schema.Type{Name: "int8"}},
					{Name: "t_id", Type: schema.Type{Name: "int8"}, Nullable: true},
				},
				PrimaryKey: &schema.PrimaryKey{Name: "u_pkey", Columns: []string{"id"}},
			}},
		}
	}

	for _, tt := range []struct {
		name string
		op   migrate.Operation
		// reversible says whether an inverse exists at all.
		reversible bool
		// safety is what the operation must classify itself as.
		safety migrate.Safety
		// sql says whether the operation renders statements. A state-only
		// operation renders none, and that is not a failure.
		sql bool
	}{
		{"create table", migrate.CreateTable{Table: schema.Table{
			Schema: "public", Name: "v",
			Columns:    []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}},
			PrimaryKey: &schema.PrimaryKey{Name: "v_pkey", Columns: []string{"id"}},
		}}, true, migrate.Safe, true},
		{"drop table", migrate.DropTable{Schema: "public", Name: "u"}, true, migrate.Destructive, true},
		{"rename table", migrate.RenameTable{Schema: "public", From: "u", To: "w"}, true, migrate.Safe, true},
		{"add column", migrate.AddColumn{Schema: "public", Table: "t", Column: schema.Column{
			Name: "c", Type: schema.Type{Name: "text"}, Nullable: true,
		}}, true, migrate.Safe, true},
		{"add a NOT NULL column with nothing to fill it", migrate.AddColumn{Schema: "public", Table: "t", Column: schema.Column{
			Name: "c", Type: schema.Type{Name: "text"},
		}}, true, migrate.RequiresData, true},
		{"drop column", migrate.DropColumn{Schema: "public", Table: "t", Name: "a"}, true, migrate.Destructive, true},
		{"rename column", migrate.RenameColumn{Schema: "public", Table: "t", From: "a", To: "z"}, true, migrate.Safe, true},
		{"widen a column", migrate.AlterColumn{
			Schema: "public", Table: "t", Name: "b",
			From: schema.Column{Name: "b", Type: schema.Type{Name: "int4"}},
			To:   schema.Column{Name: "b", Type: schema.Type{Name: "int8"}},
		}, true, migrate.Locking, true},
		{"make a column reject NULL", migrate.AlterColumn{
			Schema: "public", Table: "t", Name: "a",
			From: schema.Column{Name: "a", Type: schema.Type{Name: "text"}, Nullable: true},
			To:   schema.Column{Name: "a", Type: schema.Type{Name: "text"}},
		}, true, migrate.RequiresData, true},
		{"drop the primary key", migrate.DropPrimaryKey{Schema: "public", Table: "t", Name: "t_pkey"},
			true, migrate.Destructive, true},
		{"add a unique constraint", migrate.AddUnique{Schema: "public", Table: "u", Unique: schema.Unique{
			Name: "u_t_id_key", Columns: []string{"t_id"}, Constraint: true,
		}}, true, migrate.Locking, true},
		{"drop a unique constraint", migrate.DropUnique{
			Schema: "public", Table: "t", Name: "t_a_key", Constraint: true,
		}, true, migrate.Destructive, true},
		{"add a foreign key", migrate.AddForeignKey{Schema: "public", Table: "u", ForeignKey: schema.ForeignKey{
			Name: "u_t_id_fkey", Columns: []string{"t_id"},
			RefSchema: "public", RefTable: "t", RefColumns: []string{"id"}, OnDelete: schema.Cascade,
		}}, true, migrate.Locking, true},
		{"add a check", migrate.AddCheck{Schema: "public", Table: "u", Check: schema.Check{
			Name: "u_ok", Expression: "id > 0",
		}}, true, migrate.Locking, true},
		{"drop a check", migrate.DropCheck{Schema: "public", Table: "t", Name: "t_b_ok"},
			true, migrate.Destructive, true},
		{"create an index", migrate.CreateIndex{Schema: "public", Table: "u", Index: schema.Index{
			Name: "u_t_id_idx", Columns: []schema.IndexColumn{{Name: "t_id"}},
		}}, true, migrate.Locking, true},
		{"drop an index", migrate.DropIndex{Schema: "public", Table: "t", Name: "t_a_idx"},
			true, migrate.Locking, true},
		{"drop an index concurrently", migrate.DropIndex{
			Schema: "public", Table: "t", Name: "t_a_idx", Concurrently: true,
		}, true, migrate.Safe, true},
		{"rename an index", migrate.RenameIndex{Schema: "public", Table: "t", From: "t_a_idx", To: "t_z_idx"},
			true, migrate.Safe, true},
		{"create an enum", migrate.CreateEnum{Enum: schema.Enum{
			Schema: "public", Name: "st2", Labels: []string{"x"},
		}}, true, migrate.Safe, true},
		{"drop an enum", migrate.DropEnum{Schema: "public", Name: "st"}, true, migrate.Destructive, true},
		{"add an enum label", migrate.AddEnumValue{Schema: "public", Name: "st", Value: "c", After: "b"},
			false, migrate.Safe, true},
		{"rename an enum label", migrate.RenameEnumValue{Schema: "public", Name: "st", From: "a", To: "z"},
			true, migrate.Safe, true},
		{"rename an enum", migrate.RenameEnum{Schema: "public", From: "st", To: "state"}, true, migrate.Safe, true},
		// Dropping an extension takes every object that depends on it, so
		// creating one has no inverse this engine will invent.
		{"create an extension", migrate.CreateExtension{Extension: schema.Extension{Name: "hstore"}},
			false, migrate.Safe, true},
		{"raw SQL with an inverse", migrate.RawSQL{Up: "UPDATE t SET b = 1", Down: "UPDATE t SET b = 0", Atomic: true},
			true, migrate.Destructive, true},
		{"raw SQL without one", migrate.RawSQL{Up: "UPDATE t SET b = 1", Atomic: true},
			false, migrate.Destructive, true},
		{"a state-only change", migrate.StateOnly{Op: migrate.DropColumn{Schema: "public", Table: "t", Name: "a"}},
			true, migrate.Safe, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := base()

			// Applying changes the state, and it must not change the state it
			// was handed a copy of.
			after := before.Clone()
			if err := tt.op.Apply(after); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if diffs := schema.Diff(before, base()); len(diffs) > 0 {
				t.Errorf("Apply mutated the schema it was given: %v", diffs)
			}

			if got := tt.op.Safety(); got != tt.safety {
				t.Errorf("Safety = %v, want %v", got, tt.safety)
			}

			statements, err := tt.op.SQL()
			if err != nil {
				t.Fatalf("SQL: %v", err)
			}
			if tt.sql && len(statements) == 0 {
				t.Error("the operation renders no SQL")
			}
			if !tt.sql && len(statements) != 0 {
				t.Errorf("a state-only operation rendered %v", statements)
			}

			// The reverse is computed against the state as it was before, which
			// is where the definitions a drop threw away still live.
			rev, err := tt.op.Reverse(before)
			if !tt.reversible {
				if err == nil {
					t.Fatalf("an operation with no inverse produced %s", rev.Describe())
				}
				var irr *migrate.ErrIrreversible
				if !errors.As(err, &irr) {
					t.Errorf("Reverse = %v, want an irreversible error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reverse: %v", err)
			}
			// Undoing it returns to where it started. This is the property that
			// makes a rollback plan mean anything.
			back := after.Clone()
			if err := rev.Apply(back); err != nil {
				t.Fatalf("applying the reverse: %v", err)
			}
			// PostgreSQL has no way to put a column back where it was, so the
			// reverse of a drop appends it. The migration state says the same
			// thing the database will, which is the property that matters; the
			// position is a difference neither of them can undo.
			diffs := slices.DeleteFunc(schema.Diff(before, back), func(d string) bool {
				return strings.Contains(d, "declares its columns in a different order")
			})
			if len(diffs) > 0 {
				t.Errorf("apply then reverse did not return to the start:\n    %s", strings.Join(diffs, "\n    "))
			}
			// The reverse renders too, or a rollback would fail at execution
			// time rather than at planning time.
			if tt.sql {
				if _, err := rev.SQL(); err != nil {
					t.Errorf("the reverse renders no SQL: %v", err)
				}
			}
		})
	}
}

// ------------------------------------------------------- diff properties

// The property the whole engine rests on: applying a diff reaches the schema it
// was computed from. This generates the pairs rather than listing them, so it
// covers combinations nobody thought to write down.
func TestAudit_diffReachesItsTarget(t *testing.T) {
	variants := schemaVariants()
	for i, from := range variants {
		for j, to := range variants {
			if i == j {
				continue
			}
			name := fmt.Sprintf("%s -> %s", from.name, to.name)
			d, err := migrate.Compute(from.schema, to.schema, migrate.Options{})
			if err != nil {
				t.Fatalf("%s: Compute: %v", name, err)
			}
			// The contract is not that every difference can be migrated: it is
			// that a difference is either migrated correctly or refused. A
			// removed enum label is the refusal, and a diff carrying one must
			// not also claim to reach the target.
			if err := migrate.Editable(&migrate.Migration{
				ID: "0001_x", Atomic: true, Operations: d.Operations,
			}); err != nil {
				continue
			}
			// A rename is never guessed, so a property test must not resolve
			// one either: the drop-and-add the diff produced is the answer
			// under test.
			state := from.schema.Clone()
			for k, op := range d.Operations {
				if err := op.Apply(state); err != nil {
					t.Fatalf("%s: applying %s (%d): %v", name, op.Describe(), k, err)
				}
			}
			if diffs := schema.Diff(state, to.schema); len(diffs) > 0 {
				t.Errorf("%s: applying the diff did not reach the target:\n    %s",
					name, strings.Join(diffs, "\n    "))
			}
			// The same states diffed again produce nothing, which is what stops
			// a project generating the same migration for ever.
			again, err := migrate.Compute(state, to.schema, migrate.Options{})
			if err != nil {
				t.Fatalf("%s: Compute: %v", name, err)
			}
			if !again.Empty() {
				t.Errorf("%s: diffing the reached state again produced %d operations", name, len(again.Operations))
			}
		}
	}
}

type namedSchema struct {
	name   string
	schema *schema.Schema
}

// schemaVariants builds a family of schemas that differ one property at a time.
//
// One property at a time is the point: a pair differing in everything proves
// only that a rebuild works, and the failures worth finding are the ones where
// a single changed attribute produces an operation that does not quite undo the
// difference.
func schemaVariants() []namedSchema {
	col := func(name, typ string, mods ...func(*schema.Column)) schema.Column {
		c := schema.Column{Name: name, Type: schema.Type{Name: typ}}
		for _, m := range mods {
			m(&c)
		}
		return c
	}
	nullable := func(c *schema.Column) { c.Nullable = true }
	def := func(e schema.Expr) func(*schema.Column) { return func(c *schema.Column) { c.Default = e } }
	identity := func(c *schema.Column) { c.Identity = schema.IdentityByDefault }

	build := func(mut func(*schema.Schema)) *schema.Schema {
		s := &schema.Schema{
			Enums: []schema.Enum{{Schema: "public", Name: "st", Labels: []string{"a", "b"}}},
			Tables: []schema.Table{{
				Schema: "public", Name: "parent",
				Columns:    []schema.Column{col("id", "int8", identity), col("name", "text")},
				PrimaryKey: &schema.PrimaryKey{Name: "parent_pkey", Columns: []string{"id"}},
			}, {
				Schema: "public", Name: "child",
				Columns: []schema.Column{
					col("id", "int8", identity), col("parent_id", "int8"),
					col("note", "text", nullable), col("n", "int4", def("0")),
				},
				PrimaryKey: &schema.PrimaryKey{Name: "child_pkey", Columns: []string{"id"}},
				ForeignKeys: []schema.ForeignKey{{
					Name: "child_parent_id_fkey", Columns: []string{"parent_id"},
					RefSchema: "public", RefTable: "parent", RefColumns: []string{"id"},
				}},
			}},
		}
		if mut != nil {
			mut(s)
		}
		s.Normalize()
		return s
	}
	table := func(s *schema.Schema, name string) *schema.Table {
		for i := range s.Tables {
			if s.Tables[i].Name == name {
				return &s.Tables[i]
			}
		}
		panic("no table " + name)
	}

	return []namedSchema{
		{"empty", &schema.Schema{}},
		{"base", build(nil)},
		{"an added column", build(func(s *schema.Schema) {
			t := table(s, "child")
			t.Columns = append(t.Columns, col("extra", "text", nullable))
		})},
		{"a dropped column", build(func(s *schema.Schema) {
			t := table(s, "child")
			t.Columns = t.Columns[:len(t.Columns)-1]
		})},
		{"a widened column", build(func(s *schema.Schema) {
			table(s, "child").Columns[3].Type = schema.Type{Name: "int8"}
		})},
		{"a column that now rejects NULL", build(func(s *schema.Schema) {
			table(s, "child").Columns[2].Nullable = false
		})},
		{"a changed default", build(func(s *schema.Schema) {
			table(s, "child").Columns[3].Default = "1"
		})},
		{"a dropped default", build(func(s *schema.Schema) {
			table(s, "child").Columns[3].Default = ""
		})},
		{"a unique constraint", build(func(s *schema.Schema) {
			t := table(s, "parent")
			t.Uniques = []schema.Unique{{Name: "parent_name_key", Columns: []string{"name"}, Constraint: true}}
		})},
		{"a unique index instead", build(func(s *schema.Schema) {
			t := table(s, "parent")
			t.Uniques = []schema.Unique{{Name: "parent_name_key", Columns: []string{"name"}}}
		})},
		{"a check", build(func(s *schema.Schema) {
			table(s, "child").Checks = []schema.Check{{Name: "child_n_ok", Expression: "n >= 0"}}
		})},
		{"a NOT VALID check", build(func(s *schema.Schema) {
			table(s, "child").Checks = []schema.Check{{Name: "child_n_ok", Expression: "n >= 0", NotValid: true}}
		})},
		{"a cascading foreign key", build(func(s *schema.Schema) {
			table(s, "child").ForeignKeys[0].OnDelete = schema.Cascade
		})},
		{"no foreign key", build(func(s *schema.Schema) {
			table(s, "child").ForeignKeys = nil
		})},
		{"an index", build(func(s *schema.Schema) {
			table(s, "child").Indexes = []schema.Index{{
				Name: "child_parent_idx", Columns: []schema.IndexColumn{{Name: "parent_id"}},
			}}
		})},
		{"a partial covering index", build(func(s *schema.Schema) {
			table(s, "child").Indexes = []schema.Index{{
				Name:    "child_parent_idx",
				Columns: []schema.IndexColumn{{Name: "parent_id"}, {Name: "n", Direction: schema.Desc}},
				Include: []string{"note"}, Where: "n > 0",
			}}
		})},
		{"an extra enum label", build(func(s *schema.Schema) {
			s.Enums[0].Labels = []string{"a", "b", "c"}
		})},
		{"an extra table", build(func(s *schema.Schema) {
			s.Tables = append(s.Tables, schema.Table{
				Schema: "public", Name: "extra",
				Columns:    []schema.Column{col("id", "int8", identity)},
				PrimaryKey: &schema.PrimaryKey{Name: "extra_pkey", Columns: []string{"id"}},
			})
		})},
		{"an extension", build(func(s *schema.Schema) {
			s.Extensions = []schema.Extension{{Name: "citext"}}
		})},
	}
}

// ------------------------------------------------- canonical model properties

func TestAudit_canonicalModelProperties(t *testing.T) {
	for _, v := range schemaVariants() {
		// Normalising is idempotent, or the state a migration reconstructs
		// would depend on how many times it had been touched.
		once := v.schema.Clone()
		once.Normalize()
		twice := once.Clone()
		twice.Normalize()
		if diffs := schema.Diff(once, twice); len(diffs) > 0 {
			t.Errorf("%s: Normalize is not idempotent: %v", v.name, diffs)
		}

		// A schema equals itself.
		if diffs := schema.Diff(v.schema, v.schema.Clone()); len(diffs) > 0 {
			t.Errorf("%s: a schema differs from its own clone: %v", v.name, diffs)
		}

		// A clone is deep: mutating it cannot reach the original. Every nested
		// slice is touched, because one shared backing array is all it takes.
		clone := v.schema.Clone()
		for i := range clone.Tables {
			tbl := &clone.Tables[i]
			tbl.Name += "_x"
			for j := range tbl.Columns {
				tbl.Columns[j].Name += "_x"
				tbl.Columns[j].Type.Name = "bogus"
			}
			if tbl.PrimaryKey != nil {
				tbl.PrimaryKey.Name += "_x"
				for j := range tbl.PrimaryKey.Columns {
					tbl.PrimaryKey.Columns[j] += "_x"
				}
			}
			for j := range tbl.ForeignKeys {
				tbl.ForeignKeys[j].Name += "_x"
				for k := range tbl.ForeignKeys[j].Columns {
					tbl.ForeignKeys[j].Columns[k] += "_x"
				}
			}
			for j := range tbl.Indexes {
				tbl.Indexes[j].Name += "_x"
				for k := range tbl.Indexes[j].Columns {
					tbl.Indexes[j].Columns[k].Name += "_x"
				}
				for k := range tbl.Indexes[j].Include {
					tbl.Indexes[j].Include[k] += "_x"
				}
			}
			for j := range tbl.Uniques {
				tbl.Uniques[j].Name += "_x"
				for k := range tbl.Uniques[j].Columns {
					tbl.Uniques[j].Columns[k] += "_x"
				}
			}
			for j := range tbl.Checks {
				tbl.Checks[j].Name += "_x"
			}
		}
		for i := range clone.Enums {
			clone.Enums[i].Name += "_x"
			for j := range clone.Enums[i].Labels {
				clone.Enums[i].Labels[j] += "_x"
			}
		}
		for i := range clone.Extensions {
			clone.Extensions[i].Name += "_x"
		}
		fresh := schemaVariants()
		if diffs := schema.Diff(v.schema, fresh[indexOf(fresh, v.name)].schema); len(diffs) > 0 {
			t.Errorf("%s: mutating a clone reached the original:\n    %s", v.name, strings.Join(diffs, "\n    "))
		}
	}
}

func indexOf(all []namedSchema, name string) int {
	for i, v := range all {
		if v.name == name {
			return i
		}
	}
	panic("no variant " + name)
}

// Normalization must not touch the orders that carry meaning.
func TestAudit_normalizeKeepsMeaningfulOrder(t *testing.T) {
	s := &schema.Schema{
		Enums: []schema.Enum{{Schema: "public", Name: "st", Labels: []string{"z", "a", "m"}}},
		Tables: []schema.Table{{
			Schema: "public", Name: "t",
			// Column order is what SELECT * returns and what a positional scan
			// reads; sorting it would silently change both.
			Columns: []schema.Column{
				{Name: "z", Type: schema.Type{Name: "int8"}},
				{Name: "a", Type: schema.Type{Name: "int8"}},
				{Name: "m", Type: schema.Type{Name: "int8"}},
			},
			PrimaryKey: &schema.PrimaryKey{Name: "t_pkey", Columns: []string{"z", "a"}},
			ForeignKeys: []schema.ForeignKey{{
				Name: "t_fk", Columns: []string{"z", "a"},
				RefSchema: "public", RefTable: "t", RefColumns: []string{"m", "z"},
			}},
			Indexes: []schema.Index{{
				Name:    "t_idx",
				Columns: []schema.IndexColumn{{Name: "m"}, {Name: "a", Direction: schema.Desc}},
			}},
			Uniques: []schema.Unique{{Name: "t_u", Columns: []string{"m", "z"}, Constraint: true}},
		}},
	}
	s.Normalize()

	tbl := s.Tables[0]
	if got := names(tbl.Columns, func(c schema.Column) string { return c.Name }); got != "z,a,m" {
		t.Errorf("column order = %s, want the declared order", got)
	}
	if strings.Join(s.Enums[0].Labels, ",") != "z,a,m" {
		t.Errorf("enum labels = %v, want the declared order", s.Enums[0].Labels)
	}
	if strings.Join(tbl.PrimaryKey.Columns, ",") != "z,a" {
		t.Errorf("primary key columns = %v", tbl.PrimaryKey.Columns)
	}
	if strings.Join(tbl.ForeignKeys[0].Columns, ",") != "z,a" ||
		strings.Join(tbl.ForeignKeys[0].RefColumns, ",") != "m,z" {
		t.Errorf("foreign key column pairing was reordered: %+v", tbl.ForeignKeys[0])
	}
	if got := names(tbl.Indexes[0].Columns, func(c schema.IndexColumn) string { return c.Name }); got != "m,a" {
		t.Errorf("index keys = %s, want the declared order", got)
	}
	if strings.Join(tbl.Uniques[0].Columns, ",") != "m,z" {
		t.Errorf("unique columns = %v", tbl.Uniques[0].Columns)
	}
}

func names[T any](items []T, f func(T) string) string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, f(i))
	}
	return strings.Join(out, ",")
}

// ------------------------------------------------------------ checksums

// A checksum is a statement about a migration, not about the machine it was
// read on. This checks it survives everything about the environment that could
// differ between a laptop and CI.
func TestAudit_checksumIsIndependentOfTheFilesystem(t *testing.T) {
	migrations := []*migrate.Migration{
		{ID: "0001_initial", Atomic: true, Operations: everyOperation()[:8]},
		{ID: "0002_more", DependsOn: []string{"0001_initial"}, Atomic: true, Operations: everyOperation()[8:12]},
	}
	migrations[0].Atomic = false
	migrations[1].Atomic = false

	write := func(dir string) *migrate.Set {
		t.Helper()
		store := migrate.NewStore(dir)
		for _, m := range migrations {
			if _, err := store.Write(m); err != nil {
				t.Fatalf("Write: %v", err)
			}
		}
		set, err := store.Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		return set
	}

	// Two different absolute paths, which is what a fresh clone is.
	first := write(filepath.Join(t.TempDir(), "a", "migrations"))
	second := write(filepath.Join(t.TempDir(), "deeper", "nested", "elsewhere", "migrations"))

	for _, m := range migrations {
		a, _ := first.Checksum(m.ID)
		b, _ := second.Checksum(m.ID)
		if a == "" || a != b {
			t.Errorf("%s checksums differ between checkouts: %s and %s", m.ID, a, b)
		}
	}

	// Reformatting the file changes its bytes and not its meaning.
	dir := filepath.Join(t.TempDir(), "reformatted")
	third := write(dir)
	path := filepath.Join(dir, "0001_initial.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compact := strings.ReplaceAll(strings.ReplaceAll(string(data), "\n", ""), "  ", "")
	if err := os.WriteFile(path, []byte(compact), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded, err := migrate.NewStore(dir).Load()
	if err != nil {
		t.Fatalf("Load after reformatting: %v", err)
	}
	a, _ := third.Checksum("0001_initial")
	b, _ := reloaded.Checksum("0001_initial")
	if a != b {
		t.Errorf("reformatting the file changed its checksum: %s -> %s", a, b)
	}

	// Changing what it does always changes it.
	edited := *migrations[0]
	edited.Operations = append(slices.Clone(edited.Operations),
		migrate.DropTable{Schema: "public", Name: "gone"})
	before, _ := migrations[0].Checksum()
	after, _ := edited.Checksum()
	if before == after {
		t.Error("adding an operation did not change the checksum")
	}
}

// ------------------------------------------------------- history integrity

// Every way history can stop making sense has to be found while planning,
// before a statement runs.
func TestAudit_brokenHistoryIsRefusedBeforeAnythingRuns(t *testing.T) {
	set := newSet(t,
		&migrate.Migration{ID: "0001_a", Atomic: true, Operations: []migrate.Operation{
			migrate.CreateTable{Table: schema.Table{Schema: "public", Name: "a",
				Columns: []schema.Column{{Name: "id", Type: schema.Type{Name: "int8"}}}}},
		}},
		&migrate.Migration{ID: "0002_b", DependsOn: []string{"0001_a"}, Atomic: true, Operations: []migrate.Operation{
			migrate.AddColumn{Schema: "public", Table: "a", Column: schema.Column{
				Name: "x", Type: schema.Type{Name: "text"}, Nullable: true}},
		}},
	)
	sum1, _ := set.Checksum("0001_a")

	for _, tt := range []struct {
		name    string
		applied []migrate.Applied
		want    string
	}{
		{"a migration nobody has", []migrate.Applied{{ID: "0009_ghost", Checksum: "x"}},
			"are not among the migrations present"},
		{"a dependency that was never applied", []migrate.Applied{{ID: "0002_b", Checksum: mustSum(t, set, "0002_b")}},
			"which it depends on, is not"},
		{"an edited migration", []migrate.Applied{{ID: "0001_a", Checksum: "0000000000000000"}},
			"was modified after it was applied"},
	} {
		_, err := migrate.PlanTarget(set, tt.applied, "")
		if err == nil {
			t.Errorf("%s: planning succeeded", tt.name)
			continue
		}
		if !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tt.name, err, tt.want)
		}
	}

	// A history that does make sense plans normally.
	if _, err := migrate.PlanTarget(set, []migrate.Applied{{ID: "0001_a", Checksum: sum1}}, ""); err != nil {
		t.Errorf("a valid history was refused: %v", err)
	}
}

func mustSum(t *testing.T, set *migrate.Set, id string) string {
	t.Helper()
	sum, ok := set.Checksum(id)
	if !ok {
		t.Fatalf("no checksum for %s", id)
	}
	return sum
}

// ------------------------------------------------------------ the parser

// The artifact parser reads bytes somebody may have edited, so it has to refuse
// rather than fail in a way that takes the process with it.
func TestAudit_parserRefusesMalformedArtifacts(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
	}{
		{"not JSON", `{`},
		{"an empty document", ``},
		{"no operations", `{"format":1,"id":"0001_x","atomic":true,"operations":[]}`},
		{"no ID", `{"format":1,"id":"","atomic":true,"operations":[{"op":"drop_table","args":{"Schema":"public","Name":"t"}}]}`},
		{"an operation with no kind", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"args":{}}]}`},
		{"an unknown operation", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"teleport","args":{}}]}`},
		{"an unknown field", `{"format":1,"id":"0001_x","atomic":true,"operations":[],"surprise":1}`},
		{"an unknown argument", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"drop_table","args":{"Schema":"public","Name":"t","Extra":1}}]}`},
		{"a future format", `{"format":9999,"id":"0001_x","atomic":true,"operations":[]}`},
		{"an empty table name", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"drop_table","args":{"Schema":"public","Name":""}}]}`},
		{"an identifier with a NUL", "{\"format\":1,\"id\":\"0001_x\",\"atomic\":true,\"operations\":[{\"op\":\"drop_table\",\"args\":{\"Schema\":\"public\",\"Name\":\"a\\u0000b\"}}]}"},
		{"an empty column name", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"add_column","args":{"Schema":"public","Table":"t","Column":{"Name":"","Type":{"Schema":"","Name":"text","Array":false},"Nullable":true,"Default":"","Identity":0,"Generated":"","Collation":""}}}]}`},
		{"a self-dependency", `{"format":1,"id":"0001_x","dependsOn":["0001_x"],"atomic":true,"operations":[{"op":"drop_table","args":{"Schema":"public","Name":"t"}}]}`},
		{"an atomic migration containing a concurrent index", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"drop_index","args":{"Schema":"public","Table":"t","Name":"i","Concurrently":true}}]}`},
		{"raw SQL with no statement", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"raw_sql","args":{"Up":"","Down":"","Atomic":true,"Description":"x"}}]}`},
		{"deeply nested state_only", `{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"state_only","args":{"op":"state_only","args":{"op":"state_only","args":{"op":"nope","args":{}}}}}]}`},
	} {
		if _, err := migrate.Parse([]byte(tt.data)); err == nil {
			t.Errorf("%s was accepted", tt.name)
		}
	}
}

// FuzzArtifact drives the parser with arbitrary bytes.
//
// The property is narrow and absolute: whatever the input, Parse returns or
// errors. It never panics, and it never produces a migration that panics when
// asked for its SQL — which is the failure that would otherwise reach a person
// as a stack trace from `orm migrate`.
func FuzzArtifact(f *testing.F) {
	f.Add(`{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"drop_table","args":{"Schema":"public","Name":"t"}}]}`)
	f.Add(`{"format":1,"id":"0001_x","atomic":false,"operations":[{"op":"create_index","args":{"Schema":"public","Table":"t","Index":{"Name":"i","Columns":[{"Name":"a","Expression":"","Direction":0,"Nulls":0,"OpClass":""}],"Include":null,"Unique":false,"Method":"","Where":"","Concurrently":true}}}]}`)
	f.Add(`{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"state_only","args":{"op":"drop_column","args":{"Schema":"p","Table":"t","Name":"c"}}}]}`)
	f.Add(`{}`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, data string) {
		m, err := migrate.Parse([]byte(data))
		if err != nil {
			return
		}
		if m == nil {
			t.Fatal("Parse returned no migration and no error")
		}
		// Everything a command would do with it, none of which may panic.
		for _, op := range m.Operations {
			_ = op.Describe()
			_ = op.Safety()
			_ = op.Transactional()
			_, _ = op.SQL()
			_ = op.Apply(&schema.Schema{})
			_, _ = op.Reverse(&schema.Schema{})
		}
		if _, err := m.Checksum(); err != nil {
			t.Fatalf("a parsed migration cannot be checksummed: %v", err)
		}
		// It also has to survive being written back out and read again.
		out, err := migrate.Render(m)
		if err != nil {
			t.Fatalf("a parsed migration cannot be rendered: %v", err)
		}
		if _, err := migrate.Parse(out); err != nil {
			t.Fatalf("a rendered migration cannot be parsed: %v", err)
		}
	})
}

// A file name is an ID, and an ID is not a path. Nothing a migration says about
// itself may decide where the file goes.
func TestAudit_storeRefusesPathTraversal(t *testing.T) {
	dir := t.TempDir()
	store := migrate.NewStore(filepath.Join(dir, "migrations"))
	// An ID is a name, not a location, and not a way to hide a file from the
	// loader either.
	for _, id := range []string{"../escape", "a/b", "..", ".", ".hidden", "0001_x.json", ""} {
		m := &migrate.Migration{ID: id, Atomic: true, Operations: []migrate.Operation{
			migrate.DropTable{Schema: "public", Name: "t"},
		}}
		path, err := store.Write(m)
		if err != nil {
			continue
		}
		t.Errorf("a migration named %q was written, to %s", id, path)
	}
}
