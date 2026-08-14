package migrate_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The artifact, the graph and the order things happen in.
//
// None of this touches a database. A migration set is a history, and what a
// history means has to be computable from the artifacts alone — otherwise two
// developers on one branch get different answers depending on which database
// they happen to be pointed at.

func addColumn(table, name string) migrate.Operation {
	return migrate.AddColumn{
		Schema: "public", Table: table,
		Column: schema.Column{Name: name, Type: schema.Type{Name: "text"}, Nullable: true},
	}
}

func createUsers() migrate.Operation {
	return migrate.CreateTable{Table: schema.Table{
		Schema: "public", Name: "users",
		Columns: []schema.Column{
			{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
		},
		PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}},
	}}
}

func mig(id string, deps []string, ops ...migrate.Operation) *migrate.Migration {
	return &migrate.Migration{ID: id, DependsOn: deps, Operations: ops, Atomic: true}
}

// The same artifact fingerprints the same every time, and two artifacts that
// differ in anything semantic fingerprint differently.
func TestChecksum_isDeterministic(t *testing.T) {
	m := mig("0001", nil, createUsers(), addColumn("users", "email"))
	first, err := m.Checksum()
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}
	if len(first) != 64 {
		t.Fatalf("checksum = %q, want a sha256 digest", first)
	}
	for range 50 {
		again, err := m.Checksum()
		if err != nil {
			t.Fatalf("Checksum: %v", err)
		}
		if again != first {
			t.Fatalf("checksum = %s, want %s", again, first)
		}
	}
}

func TestChecksum_coversWhatMatters(t *testing.T) {
	base := mig("0001", nil, createUsers(), addColumn("users", "email"))
	sum, err := base.Checksum()
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}

	tests := []struct {
		name string
		m    *migrate.Migration
	}{
		{name: "the ID", m: mig("0002", nil, createUsers(), addColumn("users", "email"))},
		{name: "a dependency", m: mig("0001", []string{"0000"}, createUsers(), addColumn("users", "email"))},
		{name: "an operation's argument", m: mig("0001", nil, createUsers(), addColumn("users", "address"))},
		{name: "the operation order", m: mig("0001", nil, addColumn("users", "email"), createUsers())},
		{name: "an extra operation", m: mig("0001", nil, createUsers(), addColumn("users", "email"), addColumn("users", "x"))},
		{
			name: "the atomicity",
			m:    &migrate.Migration{ID: "0001", Operations: []migrate.Operation{createUsers(), addColumn("users", "email")}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.m.Checksum()
			if err != nil {
				t.Fatalf("Checksum: %v", err)
			}
			if got == sum {
				t.Error("the checksum did not change, so an edit would go unnoticed")
			}
		})
	}

	// Naming the same dependencies in the other order is the same statement
	// about ordering, so it is not a change.
	a := mig("0003", []string{"0001", "0002"}, addColumn("users", "x"))
	b := mig("0003", []string{"0002", "0001"}, addColumn("users", "x"))
	sa, _ := a.Checksum()
	sb, _ := b.Checksum()
	if sa != sb {
		t.Error("reordering the dependency list changed the checksum")
	}
}

// A function has no stable representation to hash, so it has to be named.
func TestChecksum_runFuncNeedsAName(t *testing.T) {
	m := mig("0001", nil, migrate.RunFunc{Up: func(context.Context, migrate.SQLRunner) error { return nil }})
	if _, err := m.Checksum(); err == nil {
		t.Error("an unnamed data migration was checksummed")
	}
}

// Everything wrong with a set as a whole is reported when it is built, before
// anything is planned.
func TestNewSet_validation(t *testing.T) {
	tests := []struct {
		name       string
		migrations []*migrate.Migration
		want       string
	}{
		{
			name:       "duplicate ID",
			migrations: []*migrate.Migration{mig("0001", nil, createUsers()), mig("0001", nil, addColumn("users", "x"))},
			want:       "more than one migration",
		},
		{
			name:       "missing dependency",
			migrations: []*migrate.Migration{mig("0002", []string{"0001"}, addColumn("users", "x"))},
			want:       "name no migration",
		},
		{
			name: "cycle",
			migrations: []*migrate.Migration{
				mig("0001", []string{"0002"}, addColumn("users", "a")),
				mig("0002", []string{"0001"}, addColumn("users", "b")),
			},
			want: "cycle",
		},
		{
			name:       "no operations",
			migrations: []*migrate.Migration{{ID: "0001", Atomic: true}},
			want:       "no operations",
		},
		{
			name:       "depends on itself",
			migrations: []*migrate.Migration{mig("0001", []string{"0001"}, createUsers())},
			want:       "depends on itself",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := migrate.NewSet(tt.migrations)
			if err == nil {
				t.Fatalf("NewSet succeeded, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}

// A migration from a newer format may mean something this build does not
// implement, so it is refused rather than reinterpreted.
func TestNewSet_futureFormat(t *testing.T) {
	m := mig("0001", nil, createUsers())
	m.Format = migrate.FormatVersion + 1

	_, err := migrate.NewSet([]*migrate.Migration{m})
	var unsupported *migrate.ErrUnsupportedFormat
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want ErrUnsupportedFormat", err)
	}
	if unsupported.Format != migrate.FormatVersion+1 {
		t.Errorf("error = %+v", unsupported)
	}
}

// The order migrations run in is a property of the graph, not of the order the
// artifacts arrived in.
func TestNewSet_orderIsDeterministic(t *testing.T) {
	build := func(order []int) []*migrate.Migration {
		all := []*migrate.Migration{
			mig("0001", nil, createUsers()),
			mig("0002", []string{"0001"}, addColumn("users", "b")),
			mig("0003", []string{"0001"}, addColumn("users", "c")),
			mig("0004", []string{"0002", "0003"}, addColumn("users", "d")),
		}
		out := make([]*migrate.Migration, 0, len(order))
		for _, i := range order {
			out = append(out, all[i])
		}
		return out
	}

	want := ""
	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2}} {
		set, err := migrate.NewSet(build(order))
		if err != nil {
			t.Fatalf("NewSet: %v", err)
		}
		var ids []string
		for _, m := range set.Migrations() {
			ids = append(ids, m.ID)
		}
		got := strings.Join(ids, ",")
		if want == "" {
			want = got
		}
		if got != want {
			t.Fatalf("order = %s, want %s; the arrival order changed the plan", got, want)
		}
	}
	// Two independent branches are ordered by ID, which is the tie-breaker that
	// makes the result the same rather than merely valid.
	if want != "0001,0002,0003,0004" {
		t.Errorf("order = %s", want)
	}
}

// The state a migration set describes is computed from the artifacts alone.
func TestSet_state(t *testing.T) {
	set, err := migrate.NewSet([]*migrate.Migration{
		mig("0001", nil, createUsers()),
		mig("0002", []string{"0001"}, addColumn("users", "email")),
		mig("0003", []string{"0002"}, addColumn("users", "status")),
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}

	full, err := set.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	users, ok := full.Table("public", "users")
	if !ok || len(users.Columns) != 3 {
		t.Fatalf("state = %+v, want three columns", users)
	}

	// And as of a point in the history, which is what a reverse is computed
	// against.
	at, err := set.StateAt("0002")
	if err != nil {
		t.Fatalf("StateAt: %v", err)
	}
	users, _ = at.Table("public", "users")
	if len(users.Columns) != 2 {
		t.Errorf("state at 0002 has %d columns, want two", len(users.Columns))
	}

	if _, err := set.StateAt("nope"); err == nil {
		t.Error("StateAt accepted a target that names no migration")
	}
}

// Planning is deterministic too: the same set and the same history produce the
// same plan, rendered the same way.
func TestPlanTarget_isDeterministic(t *testing.T) {
	set, err := migrate.NewSet([]*migrate.Migration{
		mig("0001", nil, createUsers()),
		mig("0002", []string{"0001"}, addColumn("users", "email")),
		mig("0003", []string{"0002"}, addColumn("users", "status")),
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	sum, _ := set.Checksum("0001")
	applied := []migrate.Applied{{ID: "0001", Checksum: sum}}

	want := ""
	for range 20 {
		plan, err := migrate.PlanTarget(set, applied, "")
		if err != nil {
			t.Fatalf("PlanTarget: %v", err)
		}
		got := plan.Describe()
		if want == "" {
			want = got
		}
		if got != want {
			t.Fatalf("plan differs between runs:\n%s\n\n%s", want, got)
		}
	}
	if !strings.Contains(want, "0002") || !strings.Contains(want, "0003") || strings.Contains(want, "0001\n") {
		t.Errorf("plan = %s, want the two unapplied migrations", want)
	}

	// The set's own rendering is stable as well, so a listing can be diffed.
	first := set.Describe()
	for range 20 {
		if set.Describe() != first {
			t.Fatal("the set describes itself differently between runs")
		}
	}
}

// History that names a migration nobody has is a history from another codebase,
// and is refused rather than worked around.
func TestPlanTarget_historyValidation(t *testing.T) {
	set, err := migrate.NewSet([]*migrate.Migration{
		mig("0001", nil, createUsers()),
		mig("0002", []string{"0001"}, addColumn("users", "email")),
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	sum1, _ := set.Checksum("0001")

	t.Run("unknown migration", func(t *testing.T) {
		_, err := migrate.PlanTarget(set, []migrate.Applied{{ID: "9999"}}, "")
		var hist *migrate.ErrHistory
		if !errors.As(err, &hist) {
			t.Fatalf("error = %v, want ErrHistory", err)
		}
	})

	t.Run("modified checksum", func(t *testing.T) {
		_, err := migrate.PlanTarget(set, []migrate.Applied{{ID: "0001", Checksum: "different"}}, "")
		var mod *migrate.ErrMigrationModified
		if !errors.As(err, &mod) {
			t.Fatalf("error = %v, want ErrMigrationModified", err)
		}
		if mod.ID != "0001" || mod.Applied != "different" {
			t.Errorf("error = %+v", mod)
		}
	})

	t.Run("dependency not applied", func(t *testing.T) {
		sum2, _ := set.Checksum("0002")
		_, err := migrate.PlanTarget(set, []migrate.Applied{{ID: "0002", Checksum: sum2}}, "")
		var hist *migrate.ErrHistory
		if !errors.As(err, &hist) {
			t.Fatalf("error = %v, want ErrHistory", err)
		}
	})

	t.Run("unknown target", func(t *testing.T) {
		_, err := migrate.PlanTarget(set, []migrate.Applied{{ID: "0001", Checksum: sum1}}, "nope")
		var target *migrate.ErrUnknownTarget
		if !errors.As(err, &target) {
			t.Fatalf("error = %v, want ErrUnknownTarget", err)
		}
	})

	t.Run("already there", func(t *testing.T) {
		sum2, _ := set.Checksum("0002")
		plan, err := migrate.PlanTarget(set,
			[]migrate.Applied{{ID: "0001", Checksum: sum1}, {ID: "0002", Checksum: sum2}}, "")
		if err != nil {
			t.Fatalf("PlanTarget: %v", err)
		}
		if !plan.Empty() {
			t.Errorf("plan = %s, want nothing to do", plan.Describe())
		}
	})
}

// A reverse path is checked in full before any of it is offered, so an
// irreversible step is found while planning rather than mid-rollback.
func TestPlanTarget_irreversibleIsFoundWhilePlanning(t *testing.T) {
	set, err := migrate.NewSet([]*migrate.Migration{
		mig("0001", nil, createUsers()),
		mig("0002", []string{"0001"}, migrate.RawSQL{Up: "SELECT 1", Atomic: true}),
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	s1, _ := set.Checksum("0001")
	s2, _ := set.Checksum("0002")

	_, err = migrate.PlanTarget(set,
		[]migrate.Applied{{ID: "0001", Checksum: s1}, {ID: "0002", Checksum: s2}}, "0001")
	var irr *migrate.ErrIrreversible
	if !errors.As(err, &irr) {
		t.Fatalf("error = %v, want ErrIrreversible", err)
	}
}

// The plan says what it is about to do, including the parts a reader most needs
// to see before saying yes.
func TestPlan_warnings(t *testing.T) {
	set, err := migrate.NewSet([]*migrate.Migration{
		mig("0001", nil, createUsers()),
		{
			ID: "0002", DependsOn: []string{"0001"}, Atomic: false,
			Operations: []migrate.Operation{
				migrate.DropColumn{Schema: "public", Table: "users", Name: "id"},
				migrate.CreateIndex{Schema: "public", Table: "users", Index: schema.Index{
					Name: "i", Columns: []schema.IndexColumn{{Name: "id"}}, Concurrently: true,
				}},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	plan, err := migrate.PlanTarget(set, nil, "")
	if err != nil {
		t.Fatalf("PlanTarget: %v", err)
	}
	out := plan.Describe()
	for _, want := range []string{"non-atomic", "DESTRUCTIVE", "outside a transaction", "discards data"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan does not mention %q:\n%s", want, out)
		}
	}
}
