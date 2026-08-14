package migrate

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Enum, extension and escape-hatch operations.

// CreateEnum creates an enum type.
type CreateEnum struct {
	Enum schema.Enum
}

func (o CreateEnum) Describe() string {
	return fmt.Sprintf("create enum %s (%s)", o.Enum.Qualified(), strings.Join(o.Enum.Labels, ", "))
}
func (o CreateEnum) Safety() Safety      { return Safe }
func (o CreateEnum) Transactional() bool { return true }

func (o CreateEnum) Apply(s *schema.Schema) error {
	if _, ok := s.Enum(o.Enum.Schema, o.Enum.Name); ok {
		return fmt.Errorf("enum %s already exists in the migration state", o.Enum.Qualified())
	}
	e := o.Enum
	e.Labels = slices.Clone(e.Labels)
	s.Enums = append(s.Enums, e)
	s.Normalize()
	return nil
}

func (o CreateEnum) SQL() ([]string, error) {
	if len(o.Enum.Labels) == 0 {
		return nil, fmt.Errorf("enum %s has no labels", o.Enum.Qualified())
	}
	labels := make([]string, 0, len(o.Enum.Labels))
	for _, l := range o.Enum.Labels {
		labels = append(labels, literal(l))
	}
	return []string{
		"CREATE TYPE " + qualified(o.Enum.Schema, o.Enum.Name) + " AS ENUM (" + strings.Join(labels, ", ") + ")",
	}, nil
}

func (o CreateEnum) Reverse(*schema.Schema) (Operation, error) {
	return DropEnum{Schema: o.Enum.Schema, Name: o.Enum.Name}, nil
}

// DropEnum removes an enum type.
type DropEnum struct {
	Schema string
	Name   string
}

func (o DropEnum) Describe() string    { return "drop enum " + o.Schema + "." + o.Name }
func (o DropEnum) Safety() Safety      { return Destructive }
func (o DropEnum) Transactional() bool { return true }

func (o DropEnum) Apply(s *schema.Schema) error {
	i, err := enumOf(s, o.Schema, o.Name)
	if err != nil {
		return err
	}
	s.Enums = slices.Delete(s.Enums, i, i+1)
	return nil
}

func (o DropEnum) SQL() ([]string, error) {
	return []string{"DROP TYPE " + qualified(o.Schema, o.Name)}, nil
}

func (o DropEnum) Reverse(before *schema.Schema) (Operation, error) {
	if before == nil {
		return irreversible("drop enum "+o.Schema+"."+o.Name, "the type's labels are not known")
	}
	e, ok := before.Enum(o.Schema, o.Name)
	if !ok {
		return irreversible("drop enum "+o.Schema+"."+o.Name, "the type was not in the state before the drop")
	}
	return CreateEnum{Enum: e}, nil
}

// AddEnumValue adds a label to an existing enum.
//
// Where it goes matters: PostgreSQL sorts an enum by declaration order, so a
// label added at the end sorts last unless BEFORE or AFTER says otherwise.
type AddEnumValue struct {
	Schema string
	Name   string
	Value  string
	// Before and After place the new label. At most one may be set; neither
	// means the end.
	Before string
	After  string
}

func (o AddEnumValue) Describe() string {
	s := fmt.Sprintf("add value %q to enum %s.%s", o.Value, o.Schema, o.Name)
	switch {
	case o.Before != "":
		s += " before " + o.Before
	case o.After != "":
		s += " after " + o.After
	}
	return s
}

func (o AddEnumValue) Safety() Safety { return Safe }

// Transactional is true on PostgreSQL 12 and later, which is the floor this
// project supports; earlier servers refused ALTER TYPE ... ADD VALUE inside a
// transaction block.
func (o AddEnumValue) Transactional() bool { return true }

func (o AddEnumValue) Apply(s *schema.Schema) error {
	i, err := enumOf(s, o.Schema, o.Name)
	if err != nil {
		return err
	}
	e := &s.Enums[i]
	if slices.Contains(e.Labels, o.Value) {
		return fmt.Errorf("enum %s.%s already has the label %q", o.Schema, o.Name, o.Value)
	}
	switch {
	case o.Before != "":
		at := slices.Index(e.Labels, o.Before)
		if at < 0 {
			return fmt.Errorf("enum %s.%s has no label %q to insert before", o.Schema, o.Name, o.Before)
		}
		e.Labels = slices.Insert(e.Labels, at, o.Value)
	case o.After != "":
		at := slices.Index(e.Labels, o.After)
		if at < 0 {
			return fmt.Errorf("enum %s.%s has no label %q to insert after", o.Schema, o.Name, o.After)
		}
		e.Labels = slices.Insert(e.Labels, at+1, o.Value)
	default:
		e.Labels = append(e.Labels, o.Value)
	}
	return nil
}

func (o AddEnumValue) SQL() ([]string, error) {
	stmt := "ALTER TYPE " + qualified(o.Schema, o.Name) + " ADD VALUE " + literal(o.Value)
	switch {
	case o.Before != "":
		stmt += " BEFORE " + literal(o.Before)
	case o.After != "":
		stmt += " AFTER " + literal(o.After)
	}
	return []string{stmt}, nil
}

// Reverse is refused. PostgreSQL has no ALTER TYPE ... DROP VALUE, and the only
// way to remove a label is to rebuild the type — which cannot be done while any
// row still holds the value, and which this engine will not do behind a
// rollback.
func (o AddEnumValue) Reverse(*schema.Schema) (Operation, error) {
	return irreversible(fmt.Sprintf("add value %q to enum %s.%s", o.Value, o.Schema, o.Name),
		"PostgreSQL cannot remove an enum label; rebuilding the type would need every row using it to be rewritten first")
}

// RenameEnumValue renames a label in place, which PostgreSQL does support.
type RenameEnumValue struct {
	Schema string
	Name   string
	From   string
	To     string
}

func (o RenameEnumValue) Describe() string {
	return fmt.Sprintf("rename enum value %s.%s %q to %q", o.Schema, o.Name, o.From, o.To)
}
func (o RenameEnumValue) Safety() Safety      { return Safe }
func (o RenameEnumValue) Transactional() bool { return true }

func (o RenameEnumValue) Apply(s *schema.Schema) error {
	i, err := enumOf(s, o.Schema, o.Name)
	if err != nil {
		return err
	}
	at := slices.Index(s.Enums[i].Labels, o.From)
	if at < 0 {
		return fmt.Errorf("enum %s.%s has no label %q", o.Schema, o.Name, o.From)
	}
	s.Enums[i].Labels[at] = o.To
	return nil
}

func (o RenameEnumValue) SQL() ([]string, error) {
	return []string{
		"ALTER TYPE " + qualified(o.Schema, o.Name) + " RENAME VALUE " + literal(o.From) + " TO " + literal(o.To),
	}, nil
}

func (o RenameEnumValue) Reverse(*schema.Schema) (Operation, error) {
	return RenameEnumValue{Schema: o.Schema, Name: o.Name, From: o.To, To: o.From}, nil
}

// RenameEnum renames an enum type.
type RenameEnum struct {
	Schema string
	From   string
	To     string
}

func (o RenameEnum) Describe() string {
	return fmt.Sprintf("rename enum %s.%s to %s", o.Schema, o.From, o.To)
}
func (o RenameEnum) Safety() Safety      { return Safe }
func (o RenameEnum) Transactional() bool { return true }

func (o RenameEnum) Apply(s *schema.Schema) error {
	i, err := enumOf(s, o.Schema, o.From)
	if err != nil {
		return err
	}
	s.Enums[i].Name = o.To
	// Every column of the old type now names a type that does not exist.
	for ti := range s.Tables {
		for ci := range s.Tables[ti].Columns {
			t := &s.Tables[ti].Columns[ci].Type
			if t.Schema == o.Schema && t.Name == o.From {
				t.Name = o.To
			}
		}
	}
	s.Normalize()
	return nil
}

func (o RenameEnum) SQL() ([]string, error) {
	return []string{"ALTER TYPE " + qualified(o.Schema, o.From) + " RENAME TO " + ident(o.To)}, nil
}

func (o RenameEnum) Reverse(*schema.Schema) (Operation, error) {
	return RenameEnum{Schema: o.Schema, From: o.To, To: o.From}, nil
}

// CreateExtension installs a PostgreSQL extension.
//
// It is always explicit. A schema that uses citext does not cause the extension
// to be installed as a side effect, because installing one is a privileged act
// with effects outside the schema that asked for it.
type CreateExtension struct {
	Extension schema.Extension
}

func (o CreateExtension) Describe() string    { return "create extension " + o.Extension.Name }
func (o CreateExtension) Safety() Safety      { return Safe }
func (o CreateExtension) Transactional() bool { return true }

func (o CreateExtension) Apply(s *schema.Schema) error {
	for _, e := range s.Extensions {
		if e.Name == o.Extension.Name {
			return nil
		}
	}
	s.Extensions = append(s.Extensions, o.Extension)
	s.Normalize()
	return nil
}

func (o CreateExtension) SQL() ([]string, error) {
	stmt := "CREATE EXTENSION IF NOT EXISTS " + ident(o.Extension.Name)
	if o.Extension.Schema != "" {
		stmt += " SCHEMA " + ident(o.Extension.Schema)
	}
	return []string{stmt}, nil
}

// Reverse is refused. Dropping an extension takes everything that depends on it
// with it, which is not a rollback anybody asked for.
func (o CreateExtension) Reverse(*schema.Schema) (Operation, error) {
	return irreversible("create extension "+o.Extension.Name,
		"dropping an extension also drops every object that depends on it")
}

// RawSQL runs SQL the engine does not model.
//
// It changes nothing in the migration state, which is deliberate: this engine
// does not parse SQL, and a state it guessed at would be wrong in exactly the
// cases where the escape hatch was needed. If the SQL does change the schema,
// pair it with [StateOnly] so that the state stays true.
type RawSQL struct {
	Up string
	// Down is the SQL that undoes Up. Without it the operation is irreversible,
	// which is the honest answer rather than a no-op that claims otherwise.
	Down string
	// Atomic reports whether the SQL may run inside a transaction.
	Atomic bool
	// Description is what a plan prints. Without one the SQL's first line is
	// used, which is usually enough to recognise it.
	Description string
}

func (o RawSQL) Describe() string {
	if o.Description != "" {
		return "sql: " + o.Description
	}
	line, _, _ := strings.Cut(strings.TrimSpace(o.Up), "\n")
	if len(line) > 60 {
		line = line[:57] + "..."
	}
	return "sql: " + line
}

// Safety reports destructive, because the engine cannot read the SQL and the
// safe assumption about something it cannot read is the cautious one.
func (o RawSQL) Safety() Safety             { return Destructive }
func (o RawSQL) Transactional() bool        { return o.Atomic }
func (o RawSQL) Apply(*schema.Schema) error { return nil }

func (o RawSQL) SQL() ([]string, error) {
	if strings.TrimSpace(o.Up) == "" {
		return nil, fmt.Errorf("a raw SQL operation has no statement")
	}
	return []string{o.Up}, nil
}

func (o RawSQL) Reverse(*schema.Schema) (Operation, error) {
	if strings.TrimSpace(o.Down) == "" {
		return irreversible("raw SQL", "no reverse statement was given")
	}
	return RawSQL{Up: o.Down, Atomic: o.Atomic, Description: "reverse of " + o.Describe()}, nil
}

// StateOnly changes the migration state without running any SQL.
//
// It is the other half of [RawSQL]: when SQL the engine cannot model does
// change the schema, this is how the state is told what happened. Together they
// are how a hand-written migration keeps the state honest without the engine
// having to parse anything.
type StateOnly struct {
	Op Operation
}

func (o StateOnly) Describe() string {
	return "state only: " + o.Op.Describe()
}
func (o StateOnly) Safety() Safety               { return Safe }
func (o StateOnly) Transactional() bool          { return true }
func (o StateOnly) Apply(s *schema.Schema) error { return o.Op.Apply(s) }
func (o StateOnly) SQL() ([]string, error)       { return nil, nil }

func (o StateOnly) Reverse(before *schema.Schema) (Operation, error) {
	rev, err := o.Op.Reverse(before)
	if err != nil {
		return nil, err
	}
	return StateOnly{Op: rev}, nil
}

// RunFunc is a data migration written in Go.
//
// It receives a transaction and runs whatever SQL it needs. It deliberately
// does not receive generated repositories: a migration written today has to
// keep working after the entity it was written against is renamed, moved or
// deleted, and a migration that referred to today's generated API would stop
// compiling the moment somebody changed it. Raw SQL against the schema as it
// was at that point in history is the thing that stays valid.
type RunFunc struct {
	Name string
	Up   func(ctx context.Context, ex SQLRunner) error
	Down func(ctx context.Context, ex SQLRunner) error
}

func (o RunFunc) Describe() string           { return "run: " + o.Name }
func (o RunFunc) Safety() Safety             { return Destructive }
func (o RunFunc) Transactional() bool        { return true }
func (o RunFunc) Apply(*schema.Schema) error { return nil }

// SQL returns nothing: the operation runs Go rather than a statement, and the
// executor calls Up directly.
func (o RunFunc) SQL() ([]string, error) { return nil, nil }

func (o RunFunc) Reverse(*schema.Schema) (Operation, error) {
	if o.Down == nil {
		return irreversible("run "+o.Name, "no reverse function was given")
	}
	return RunFunc{Name: "reverse of " + o.Name, Up: o.Down}, nil
}

// literal renders a string as a PostgreSQL literal.
//
// Enum labels are schema identifiers in everything but syntax: they come from a
// declaration a developer wrote, not from a request. Quoting them properly is
// still the only acceptable way to put them in a statement.
func literal(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
