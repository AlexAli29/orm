package migrate_test

import (
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Warning codes are public API: a team writes one down in a runbook and CI
// matches on it, so what a code means has to stay what it meant.
func TestWarnings_codes(t *testing.T) {
	for _, tt := range []struct {
		name string
		op   migrate.Operation
		want string
	}{
		{"a plain index build", migrate.CreateIndex{Schema: "public", Table: "t", Index: schema.Index{
			Name: "t_a_idx", Columns: []schema.IndexColumn{{Name: "a"}},
		}}, migrate.WIndexNotConcurrent},
		{"a concurrent index build", migrate.CreateIndex{Schema: "public", Table: "t", Index: schema.Index{
			Name: "t_a_idx", Columns: []schema.IndexColumn{{Name: "a"}}, Concurrently: true,
		}}, ""},
		{"a dropped column", migrate.DropColumn{Schema: "public", Table: "t", Name: "a"}, migrate.WDropColumn},
		{"a dropped table", migrate.DropTable{Schema: "public", Name: "t"}, migrate.WDropTable},
		{"a dropped constraint", migrate.DropCheck{Schema: "public", Table: "t", Name: "t_ok"}, migrate.WDropConstraint},
		{"a NOT NULL column with a default", migrate.AddColumn{Schema: "public", Table: "t", Column: schema.Column{
			Name: "a", Type: schema.Type{Name: "text"}, Default: "''",
		}}, ""},
		{"a NOT NULL column without one", migrate.AddColumn{Schema: "public", Table: "t", Column: schema.Column{
			Name: "a", Type: schema.Type{Name: "text"},
		}}, migrate.WNotNullNoDefault},
		{"SET NOT NULL", migrate.AlterColumn{
			Schema: "public", Table: "t", Name: "a",
			From: schema.Column{Name: "a", Type: schema.Type{Name: "text"}, Nullable: true},
			To:   schema.Column{Name: "a", Type: schema.Type{Name: "text"}},
		}, migrate.WSetNotNull},
		{"a type change", migrate.AlterColumn{
			Schema: "public", Table: "t", Name: "a",
			From: schema.Column{Name: "a", Type: schema.Type{Name: "int4"}},
			To:   schema.Column{Name: "a", Type: schema.Type{Name: "int8"}},
		}, migrate.WTypeRewrite},
		{"a validated foreign key", migrate.AddForeignKey{Schema: "public", Table: "t", ForeignKey: schema.ForeignKey{
			Name: "t_u_fkey", Columns: []string{"u"}, RefSchema: "public", RefTable: "u", RefColumns: []string{"id"},
		}}, migrate.WValidatesRows},
		{"a NOT VALID foreign key", migrate.AddForeignKey{Schema: "public", Table: "t", ForeignKey: schema.ForeignKey{
			Name: "t_u_fkey", Columns: []string{"u"}, RefSchema: "public", RefTable: "u", RefColumns: []string{"id"},
			NotValid: true,
		}}, ""},
		{"raw SQL", migrate.RawSQL{Up: "UPDATE t SET a = 1", Atomic: true}, migrate.WRawSQL},
	} {
		got := migrate.Warnings([]migrate.Operation{tt.op})
		switch {
		case tt.want == "" && len(got) != 0:
			t.Errorf("%s warned: %v", tt.name, got)
		case tt.want == "":
		case len(got) == 0:
			t.Errorf("%s did not warn, want %s", tt.name, tt.want)
		case got[0].Code != tt.want:
			t.Errorf("%s warned %s, want %s", tt.name, got[0].Code, tt.want)
		}
	}
}

// A non-atomic migration is a property of the run rather than of any one
// operation, so it is warned about where the run is described.
func TestPlanWarnings_nonAtomic(t *testing.T) {
	m := &migrate.Migration{ID: "0001_index", Atomic: false, Operations: []migrate.Operation{
		migrate.CreateIndex{Schema: "public", Table: "t", Index: schema.Index{
			Name: "t_a_idx", Columns: []schema.IndexColumn{{Name: "a"}}, Concurrently: true,
		}},
	}}
	set := newSet(t, m)
	plan, err := migrate.PlanTarget(set, nil, "")
	if err != nil {
		t.Fatalf("PlanTarget: %v", err)
	}
	ws := migrate.PlanWarnings(plan)
	if len(ws) != 1 || ws[0].Code != migrate.WNonAtomic {
		t.Fatalf("warnings = %v", ws)
	}
	if !strings.Contains(migrate.RenderWarnings(ws, ""), migrate.WNonAtomic) {
		t.Error("the rendered block does not carry the code")
	}
}

// The summary groups by object, because what a reviewer needs is what happens
// to each table rather than the operation list in declaration order.
func TestRenderSummary_groupsByObject(t *testing.T) {
	ops := []migrate.Operation{
		migrate.AddColumn{Schema: "public", Table: "users", Column: schema.Column{
			Name: "status", Type: schema.Type{Schema: "public", Name: "user_status"}, Default: "'pending'",
		}},
		migrate.RenameColumn{Schema: "public", Table: "users", From: "email", To: "email_address"},
		migrate.CreateIndex{Schema: "public", Table: "posts", Index: schema.Index{
			Name:    "posts_feed_idx",
			Columns: []schema.IndexColumn{{Name: "author_id"}, {Name: "created_at", Direction: schema.Desc}},
			Include: []string{"title"}, Where: "published = true",
		}},
	}
	got := migrate.RenderSummary(ops, "")
	want := `users
  + status public.user_status NOT NULL DEFAULT 'pending'
  ~ rename email -> email_address

posts
  + index posts_feed_idx
      (author_id, created_at DESC)
      WHERE published = true
      INCLUDE (title)
`
	if got != want {
		t.Errorf("summary =\n%s\nwant\n%s", got, want)
	}
}

// Removing an enum label is refused rather than turned into a drop and a
// recreate, which would be a guess about data.
func TestEditable_refusesARemovedEnumLabel(t *testing.T) {
	from := &schema.Schema{Enums: []schema.Enum{{Schema: "public", Name: "s", Labels: []string{"a", "b"}}}}
	to := &schema.Schema{Enums: []schema.Enum{{Schema: "public", Name: "s", Labels: []string{"a"}}}}
	d, err := migrate.Compute(from, to, migrate.Options{})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	err = migrate.Editable(&migrate.Migration{ID: "0001_x", Atomic: true, Operations: d.Operations})
	if err == nil {
		t.Fatal("a removed enum label produced a writable migration")
	}
	if !strings.Contains(err.Error(), "PostgreSQL cannot remove one") {
		t.Errorf("the refusal does not say why: %v", err)
	}
	if !strings.Contains(err.Error(), "Write that migration by hand") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}
