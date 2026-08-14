package migrate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/internal/gen/migrate"
	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The migration artifact.
//
// A migration file is a permanent record, and the property that makes it one is
// that reading it back produces the migration that was written. Everything else
// here — determinism, the file name, the refusal to write what cannot be read —
// is in service of that.

// everyOperation is one of each kind the artifact format can hold.
//
// It is deliberately exhaustive: an operation nobody added a case for would
// otherwise be discovered when somebody's migration silently lost a step.
func everyOperation() []migrate.Operation {
	table := schema.Table{
		Schema: "public", Name: "posts",
		Columns: []schema.Column{
			{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
			{Name: "title", Type: schema.Type{Name: "text"}},
			{Name: "status", Type: schema.Type{Schema: "public", Name: "post_status"}, Default: "'draft'"},
			{Name: "search", Type: schema.Type{Name: "tsvector"}, Nullable: true, Generated: "to_tsvector('english', title)"},
		},
		PrimaryKey: &schema.PrimaryKey{Name: "posts_pkey", Columns: []string{"id"}},
	}
	index := schema.Index{
		Name:    "posts_feed_idx",
		Columns: []schema.IndexColumn{{Name: "status"}, {Name: "id", Direction: schema.Desc, Nulls: schema.NullsLast, OpClass: "int8_ops"}},
		Include: []string{"title"}, Where: "status = 'published'", Method: "btree",
	}
	return []migrate.Operation{
		migrate.CreateExtension{Extension: schema.Extension{Name: "citext", Schema: "public"}},
		migrate.CreateEnum{Enum: schema.Enum{Schema: "public", Name: "post_status", Labels: []string{"draft", "published"}}},
		migrate.AddEnumValue{Schema: "public", Name: "post_status", Value: "archived", After: "published"},
		migrate.RenameEnumValue{Schema: "public", Name: "post_status", From: "draft", To: "unpublished"},
		migrate.RenameEnum{Schema: "public", From: "post_status", To: "status"},
		migrate.CreateTable{Table: table},
		migrate.RenameTable{Schema: "public", From: "posts", To: "articles"},
		migrate.AddColumn{Schema: "public", Table: "articles", Column: table.Columns[1]},
		migrate.RenameColumn{Schema: "public", Table: "articles", From: "title", To: "headline"},
		migrate.AlterColumn{
			Schema: "public", Table: "articles", Name: "headline",
			From:  schema.Column{Name: "headline", Type: schema.Type{Name: "text"}, Nullable: true},
			To:    schema.Column{Name: "headline", Type: schema.Type{Name: "varchar(200)"}},
			Using: "headline::varchar(200)",
		},
		migrate.DropColumn{Schema: "public", Table: "articles", Name: "headline"},
		migrate.AddPrimaryKey{Schema: "public", Table: "articles", Key: schema.PrimaryKey{Name: "articles_pkey", Columns: []string{"id"}}},
		migrate.DropPrimaryKey{Schema: "public", Table: "articles", Name: "articles_pkey"},
		migrate.AddUnique{Schema: "public", Table: "articles", Unique: schema.Unique{
			Name: "articles_slug_key", Columns: []string{"slug"}, Constraint: true, NullsNotDistinct: true,
		}},
		migrate.DropUnique{Schema: "public", Table: "articles", Name: "articles_slug_key", Constraint: true},
		migrate.AddForeignKey{Schema: "public", Table: "articles", ForeignKey: schema.ForeignKey{
			Name: "articles_author_id_fkey", Columns: []string{"author_id"},
			RefSchema: "public", RefTable: "users", RefColumns: []string{"id"},
			OnDelete: schema.Cascade, Deferrable: true, InitiallyDeferred: true, NotValid: true,
		}},
		migrate.ValidateConstraint{Schema: "public", Table: "articles", Name: "articles_author_id_fkey"},
		migrate.DropForeignKey{Schema: "public", Table: "articles", Name: "articles_author_id_fkey"},
		migrate.AddCheck{Schema: "public", Table: "articles", Check: schema.Check{Name: "articles_ok", Expression: "id > 0", NotValid: true}},
		migrate.DropCheck{Schema: "public", Table: "articles", Name: "articles_ok"},
		migrate.CreateIndex{Schema: "public", Table: "articles", Index: index},
		migrate.RenameIndex{Schema: "public", Table: "articles", From: "posts_feed_idx", To: "articles_feed_idx"},
		migrate.DropIndex{Schema: "public", Table: "articles", Name: "articles_feed_idx", Concurrently: true},
		migrate.DropTable{Schema: "public", Name: "articles"},
		migrate.DropEnum{Schema: "public", Name: "status"},
		migrate.RawSQL{Up: "UPDATE t SET a = b WHERE a IS NULL", Down: "", Atomic: true, Description: "backfill a"},
		migrate.StateOnly{Op: migrate.DropColumn{Schema: "public", Table: "t", Name: "gone"}},
	}
}

func TestArtifact_roundTripsEveryOperation(t *testing.T) {
	// Not atomic: the set includes DROP INDEX CONCURRENTLY, which PostgreSQL
	// refuses inside a transaction block.
	m := &migrate.Migration{ID: "0001_everything", DependsOn: []string{"0000_root"}, Atomic: false, Operations: everyOperation()}

	data, err := migrate.Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	back, err := migrate.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The checksum covers every operation and every argument, so two migrations
	// with the same checksum are the same migration.
	want, err := m.Checksum()
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}
	got, err := back.Checksum()
	if err != nil {
		t.Fatalf("Checksum: %v", err)
	}
	if want != got {
		t.Errorf("the migration changed meaning:\n    written: %s\n    read:    %s", want, got)
	}
	if len(back.Operations) != len(m.Operations) {
		t.Errorf("read %d operations, wrote %d", len(back.Operations), len(m.Operations))
	}
	for i := range m.Operations {
		if a, b := m.Operations[i].Describe(), back.Operations[i].Describe(); a != b {
			t.Errorf("operation %d: wrote %q, read %q", i, a, b)
		}
	}
}

// The same migration renders to the same bytes every time, or a repository
// could not tell a regenerated migration from an edited one.
func TestArtifact_isDeterministic(t *testing.T) {
	m := &migrate.Migration{ID: "0001_everything", Atomic: false, Operations: everyOperation()}
	first, err := migrate.Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for range 20 {
		again, err := migrate.Render(m)
		if err != nil {
			t.Fatalf("Render: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("two renders of one migration differ")
		}
	}
	// Schema SQL routinely contains <, > and &, which HTML escaping would turn
	// into entities PostgreSQL does not accept.
	sql := &migrate.Migration{ID: "0001_check", Atomic: true, Operations: []migrate.Operation{
		migrate.AddCheck{Schema: "public", Table: "t", Check: schema.Check{Name: "t_ok", Expression: "a <> '' AND b > 0"}},
	}}
	data, err := migrate.Render(sql)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(string(data), "a <> '' AND b > 0") {
		t.Errorf("the expression was escaped:\n%s", data)
	}
}

// A Go function has no representation in a file, and pretending otherwise would
// produce a migration that silently does nothing on another machine.
func TestArtifact_refusesAGoFunction(t *testing.T) {
	m := &migrate.Migration{ID: "0001_backfill", Atomic: true, Operations: []migrate.Operation{
		migrate.RunFunc{Name: "backfill", Up: func(context.Context, migrate.SQLRunner) error { return nil }},
	}}
	_, err := migrate.Render(m)
	if err == nil {
		t.Fatal("a Go data migration was written to a file")
	}
	if !strings.Contains(err.Error(), "write it as raw SQL") {
		t.Errorf("the refusal does not say what to do instead: %v", err)
	}
}

func TestParse_rejectsAnUnknownOperation(t *testing.T) {
	_, err := migrate.Parse([]byte(`{"format":1,"id":"0001_x","atomic":true,"operations":[{"op":"teleport_table","args":{}}]}`))
	if err == nil || !strings.Contains(err.Error(), "unknown operation") {
		t.Errorf("err = %v", err)
	}
}

func TestParse_rejectsAFutureFormat(t *testing.T) {
	_, err := migrate.Parse([]byte(`{"format":99,"id":"0001_x","atomic":true,"operations":[]}`))
	var unsupported *migrate.ErrUnsupportedFormat
	if !errors.As(err, &unsupported) {
		t.Fatalf("err = %v, want an unsupported-format error", err)
	}
}

func TestStore_writesAndLoads(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "migrations")
	store := migrate.NewStore(dir)

	if set, err := store.Load(); err != nil || set.Len() != 0 {
		t.Fatalf("an empty project loaded %v, %v", set, err)
	}

	first := &migrate.Migration{ID: "0001_initial", Atomic: true, Operations: []migrate.Operation{
		migrate.CreateTable{Table: schema.Table{Schema: "public", Name: "users", Columns: []schema.Column{
			{Name: "id", Type: schema.Type{Name: "int8"}, Identity: schema.IdentityByDefault},
		}, PrimaryKey: &schema.PrimaryKey{Name: "users_pkey", Columns: []string{"id"}}}},
	}}
	path, err := store.Write(first)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if filepath.Base(path) != "0001_initial.json" {
		t.Errorf("wrote %s", path)
	}

	// A migration is never rewritten in place.
	if _, err := store.Write(first); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("rewriting a migration: %v", err)
	}

	second := &migrate.Migration{ID: "0002_email", DependsOn: []string{"0001_initial"}, Atomic: true,
		Operations: []migrate.Operation{
			migrate.AddColumn{Schema: "public", Table: "users", Column: schema.Column{Name: "email", Type: schema.Type{Name: "text"}}},
		}}
	if _, err := store.Write(second); err != nil {
		t.Fatalf("Write: %v", err)
	}

	set, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("loaded %d migrations", set.Len())
	}
	state, err := set.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	users, ok := state.Table("public", "users")
	if !ok || len(users.Columns) != 2 {
		t.Errorf("the reconstructed state is %+v", state.Tables)
	}
	if next := migrate.NextID(set, "add status"); next != "0003_add_status" {
		t.Errorf("NextID = %q", next)
	}
}

// A file whose name and ID disagree is refused, because the two would name one
// migration differently on two machines.
func TestStore_refusesAMisnamedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "0001_initial.json"),
		[]byte(`{"format":1,"id":"0002_other","atomic":true,"operations":[{"op":"drop_table","args":{"Schema":"public","Name":"t"}}]}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	_, err := migrate.NewStore(dir).Load()
	if err == nil || !strings.Contains(err.Error(), "a migration's file name is its ID") {
		t.Errorf("err = %v", err)
	}
}

func TestNextID(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{"add user status", "0001_add_user_status"},
		{"Add  User--Status!!", "0001_add_user_status"},
		{"../../etc/passwd", "0001_etc_passwd"},
		{"", "0001_auto"},
		{"!!!", "0001_auto"},
	} {
		if got := migrate.NextID(nil, tt.name); got != tt.want {
			t.Errorf("NextID(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
