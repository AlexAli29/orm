package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Table and column operations.

// CreateTable creates a table with everything declared on it.
//
// The constraints declared inline are the ones PostgreSQL accepts in CREATE
// TABLE; indexes follow as their own statements, because an index is a separate
// object even when a constraint owns one.
type CreateTable struct {
	Table schema.Table
}

func (o CreateTable) Describe() string { return "create table " + o.Table.Qualified() }
func (o CreateTable) Safety() Safety   { return Safe }
func (o CreateTable) Transactional() bool {
	for _, i := range o.Table.Indexes {
		if i.Concurrently {
			return false
		}
	}
	return true
}

func (o CreateTable) Apply(s *schema.Schema) error {
	if _, ok := s.Table(o.Table.Schema, o.Table.Name); ok {
		return fmt.Errorf("table %s already exists in the migration state", o.Table.Qualified())
	}
	s.Tables = append(s.Tables, o.Table.Clone())
	s.Normalize()
	return nil
}

func (o CreateTable) SQL() ([]string, error) { return createTableSQL(o.Table) }

func (o CreateTable) Reverse(*schema.Schema) (Operation, error) {
	return DropTable{Schema: o.Table.Schema, Name: o.Table.Name}, nil
}

// DropTable removes a table and everything on it.
type DropTable struct {
	Schema string
	Name   string
}

func (o DropTable) Describe() string    { return "drop table " + o.Schema + "." + o.Name }
func (o DropTable) Safety() Safety      { return Destructive }
func (o DropTable) Transactional() bool { return true }

func (o DropTable) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Name)
	if err != nil {
		return err
	}
	s.Tables = slices.Delete(s.Tables, i, i+1)
	return nil
}

func (o DropTable) SQL() ([]string, error) {
	return []string{"DROP TABLE " + qualified(o.Schema, o.Name)}, nil
}

// Reverse recreates the table from the state as it was before the drop, which
// is the only place its shape still exists.
func (o DropTable) Reverse(before *schema.Schema) (Operation, error) {
	if before == nil {
		return irreversible("drop table "+o.Schema+"."+o.Name, "the table's definition is not known")
	}
	t, ok := before.Table(o.Schema, o.Name)
	if !ok {
		return irreversible("drop table "+o.Schema+"."+o.Name, "the table was not in the state before the drop")
	}
	// The table comes back empty. That is what reversing a drop can offer, and
	// saying so is better than implying the rows return with it.
	return CreateTable{Table: t.Clone()}, nil
}

// RenameTable renames a table, keeping its contents.
type RenameTable struct {
	Schema string
	From   string
	To     string
}

func (o RenameTable) Describe() string {
	return fmt.Sprintf("rename table %s.%s to %s", o.Schema, o.From, o.To)
}
func (o RenameTable) Safety() Safety      { return Safe }
func (o RenameTable) Transactional() bool { return true }

func (o RenameTable) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.From)
	if err != nil {
		return err
	}
	s.Tables[i].Name = o.To
	// A foreign key pointing at the old name now points at nothing, so every
	// reference moves with it.
	for ti := range s.Tables {
		for fi := range s.Tables[ti].ForeignKeys {
			fk := &s.Tables[ti].ForeignKeys[fi]
			if fk.RefSchema == o.Schema && fk.RefTable == o.From {
				fk.RefTable = o.To
			}
		}
	}
	s.Normalize()
	return nil
}

func (o RenameTable) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.From) + " RENAME TO " + ident(o.To)}, nil
}

func (o RenameTable) Reverse(*schema.Schema) (Operation, error) {
	return RenameTable{Schema: o.Schema, From: o.To, To: o.From}, nil
}

// AddColumn adds a column to an existing table.
type AddColumn struct {
	Schema string
	Table  string
	Column schema.Column
}

func (o AddColumn) Describe() string {
	return fmt.Sprintf("add column %s.%s.%s %s", o.Schema, o.Table, o.Column.Name, o.Column.Type)
}

// Safety reports what adding this column asks of the table.
//
// A NOT NULL column with no default cannot be added to a table that already has
// rows: PostgreSQL has nothing to put in them. Adding one with a default is a
// metadata-only operation on PostgreSQL 11 and later, which is why the two
// cases are classified differently rather than both being called risky.
func (o AddColumn) Safety() Safety {
	if !o.Column.Nullable && o.Column.Default.Empty() && o.Column.Identity == schema.NotIdentity && o.Column.Generated.Empty() {
		return RequiresData
	}
	return Safe
}

func (o AddColumn) Transactional() bool { return true }

func (o AddColumn) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	if _, ok := s.Tables[i].Column(o.Column.Name); ok {
		return fmt.Errorf("column %s.%s.%s already exists in the migration state", o.Schema, o.Table, o.Column.Name)
	}
	s.Tables[i].Columns = append(s.Tables[i].Columns, o.Column)
	return nil
}

func (o AddColumn) SQL() ([]string, error) {
	def, err := columnDefinition(o.Column)
	if err != nil {
		return nil, err
	}
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " ADD COLUMN " + def}, nil
}

func (o AddColumn) Reverse(*schema.Schema) (Operation, error) {
	return DropColumn{Schema: o.Schema, Table: o.Table, Name: o.Column.Name}, nil
}

// DropColumn removes a column and its data.
type DropColumn struct {
	Schema string
	Table  string
	Name   string
}

func (o DropColumn) Describe() string {
	return fmt.Sprintf("drop column %s.%s.%s", o.Schema, o.Table, o.Name)
}
func (o DropColumn) Safety() Safety      { return Destructive }
func (o DropColumn) Transactional() bool { return true }

func (o DropColumn) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.Columns, func(c schema.Column) bool { return c.Name == o.Name })
	if idx < 0 {
		return fmt.Errorf("column %s.%s.%s is not in the migration state", o.Schema, o.Table, o.Name)
	}
	t.Columns = slices.Delete(t.Columns, idx, idx+1)
	return nil
}

func (o DropColumn) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " DROP COLUMN " + ident(o.Name)}, nil
}

func (o DropColumn) Reverse(before *schema.Schema) (Operation, error) {
	if before == nil {
		return irreversible("drop column "+o.Name, "the column's definition is not known")
	}
	t, ok := before.Table(o.Schema, o.Table)
	if !ok {
		return irreversible("drop column "+o.Name, "the table was not in the state before the drop")
	}
	c, ok := t.Column(o.Name)
	if !ok {
		return irreversible("drop column "+o.Name, "the column was not in the state before the drop")
	}
	return AddColumn{Schema: o.Schema, Table: o.Table, Column: c}, nil
}

// RenameColumn renames a column, keeping its data.
//
// This is the operation rename detection exists to produce. The alternative —
// a drop and an add — is valid SQL that silently discards the column's
// contents, which is why guessing between them is never acceptable.
type RenameColumn struct {
	Schema string
	Table  string
	From   string
	To     string
}

func (o RenameColumn) Describe() string {
	return fmt.Sprintf("rename column %s.%s.%s to %s", o.Schema, o.Table, o.From, o.To)
}
func (o RenameColumn) Safety() Safety      { return Safe }
func (o RenameColumn) Transactional() bool { return true }

func (o RenameColumn) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.Columns, func(c schema.Column) bool { return c.Name == o.From })
	if idx < 0 {
		return fmt.Errorf("column %s.%s.%s is not in the migration state", o.Schema, o.Table, o.From)
	}
	t.Columns[idx].Name = o.To
	// Everything naming the column follows it, or the state would describe
	// constraints over a column that no longer exists.
	renameIn := func(cols []string) {
		for i := range cols {
			if cols[i] == o.From {
				cols[i] = o.To
			}
		}
	}
	if t.PrimaryKey != nil {
		renameIn(t.PrimaryKey.Columns)
	}
	for ui := range t.Uniques {
		renameIn(t.Uniques[ui].Columns)
	}
	for fi := range t.ForeignKeys {
		renameIn(t.ForeignKeys[fi].Columns)
	}
	for ii := range t.Indexes {
		for ci := range t.Indexes[ii].Columns {
			if t.Indexes[ii].Columns[ci].Name == o.From {
				t.Indexes[ii].Columns[ci].Name = o.To
			}
		}
		renameIn(t.Indexes[ii].Include)
	}
	for ti := range s.Tables {
		for fi := range s.Tables[ti].ForeignKeys {
			fk := &s.Tables[ti].ForeignKeys[fi]
			if fk.RefSchema == o.Schema && fk.RefTable == o.Table {
				renameIn(fk.RefColumns)
			}
		}
	}
	return nil
}

func (o RenameColumn) SQL() ([]string, error) {
	return []string{
		"ALTER TABLE " + qualified(o.Schema, o.Table) + " RENAME COLUMN " + ident(o.From) + " TO " + ident(o.To),
	}, nil
}

func (o RenameColumn) Reverse(*schema.Schema) (Operation, error) {
	return RenameColumn{Schema: o.Schema, Table: o.Table, From: o.To, To: o.From}, nil
}

// AlterColumn changes a column's type, nullability, default or identity.
//
// It carries both the old and the new state so that it can be reversed and so
// that its SQL can be decomposed: PostgreSQL changes each property with its own
// ALTER TABLE clause, and a single operation that emitted them all
// unconditionally would issue statements for properties nobody changed.
type AlterColumn struct {
	Schema string
	Table  string
	Name   string
	From   schema.Column
	To     schema.Column
	// Using is the USING expression a type change needs when PostgreSQL cannot
	// cast implicitly. Nothing invents one: a cast the engine guessed is a data
	// transformation nobody reviewed.
	Using schema.Expr
}

func (o AlterColumn) Describe() string {
	var changes []string
	if o.From.Type.Canonical() != o.To.Type.Canonical() {
		changes = append(changes, fmt.Sprintf("type %s -> %s", o.From.Type, o.To.Type))
	}
	if o.From.Nullable != o.To.Nullable {
		if o.To.Nullable {
			changes = append(changes, "drop not null")
		} else {
			changes = append(changes, "set not null")
		}
	}
	if o.From.Default != o.To.Default {
		switch {
		case o.To.Default.Empty():
			changes = append(changes, "drop default")
		default:
			changes = append(changes, "default "+string(o.To.Default))
		}
	}
	if o.From.Identity != o.To.Identity {
		changes = append(changes, "identity "+o.To.Identity.String())
	}
	return fmt.Sprintf("alter column %s.%s.%s: %s", o.Schema, o.Table, o.Name, strings.Join(changes, ", "))
}

// Safety reports the worst of what this alteration does.
func (o AlterColumn) Safety() Safety {
	switch {
	// Making a column reject NULL scans every row, and fails if any of them is
	// NULL — which the engine cannot know from here.
	case o.From.Nullable && !o.To.Nullable:
		return RequiresData
	// A type change may rewrite the table, and may fail on values the new type
	// cannot hold.
	case o.From.Type.Canonical() != o.To.Type.Canonical():
		return Locking
	default:
		return Safe
	}
}

func (o AlterColumn) Transactional() bool { return true }

func (o AlterColumn) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.Columns, func(c schema.Column) bool { return c.Name == o.Name })
	if idx < 0 {
		return fmt.Errorf("column %s.%s.%s is not in the migration state", o.Schema, o.Table, o.Name)
	}
	to := o.To
	to.Name = o.Name
	t.Columns[idx] = to
	return nil
}

// SQL emits one clause per property that actually changed.
func (o AlterColumn) SQL() ([]string, error) {
	table := qualified(o.Schema, o.Table)
	col := ident(o.Name)
	prefix := "ALTER TABLE " + table + " ALTER COLUMN " + col + " "

	var out []string
	if o.From.Type.Canonical() != o.To.Type.Canonical() || (o.From.Collation != o.To.Collation && o.To.Collation != "") {
		stmt := prefix + "TYPE " + o.To.Type.String()
		if o.To.Collation != "" {
			stmt += " COLLATE " + ident(o.To.Collation)
		}
		if !o.Using.Empty() {
			stmt += " USING " + string(o.Using)
		}
		out = append(out, stmt)
	}
	if o.From.Default != o.To.Default {
		if o.To.Default.Empty() {
			out = append(out, prefix+"DROP DEFAULT")
		} else {
			out = append(out, prefix+"SET DEFAULT "+string(o.To.Default))
		}
	}
	if o.From.Nullable != o.To.Nullable {
		if o.To.Nullable {
			out = append(out, prefix+"DROP NOT NULL")
		} else {
			out = append(out, prefix+"SET NOT NULL")
		}
	}
	if o.From.Identity != o.To.Identity {
		switch o.To.Identity {
		case schema.NotIdentity:
			out = append(out, prefix+"DROP IDENTITY")
		case schema.IdentityAlways:
			out = append(out, prefix+"ADD GENERATED ALWAYS AS IDENTITY")
		case schema.IdentityByDefault:
			out = append(out, prefix+"ADD GENERATED BY DEFAULT AS IDENTITY")
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("alter column %s.%s.%s changes nothing", o.Schema, o.Table, o.Name)
	}
	return out, nil
}

// Reverse swaps the two states. A type change back may need its own USING
// expression, which nobody can supply from here — so the reverse is offered
// without one and PostgreSQL decides whether it casts.
func (o AlterColumn) Reverse(*schema.Schema) (Operation, error) {
	return AlterColumn{
		Schema: o.Schema, Table: o.Table, Name: o.Name,
		From: o.To, To: o.From,
	}, nil
}
