package migrate

import (
	"fmt"
	"slices"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Constraint, index and enum operations.

// AddPrimaryKey adds a primary key to an existing table.
type AddPrimaryKey struct {
	Schema string
	Table  string
	Key    schema.PrimaryKey
}

func (o AddPrimaryKey) Describe() string {
	return fmt.Sprintf("add primary key %s on %s.%s", o.Key.Name, o.Schema, o.Table)
}

// Safety reports locking: PostgreSQL builds the backing index under a lock that
// blocks writes, and rejects the whole operation if any key column holds NULL.
func (o AddPrimaryKey) Safety() Safety      { return Locking }
func (o AddPrimaryKey) Transactional() bool { return true }

func (o AddPrimaryKey) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	if s.Tables[i].PrimaryKey != nil {
		return fmt.Errorf("table %s.%s already has a primary key", o.Schema, o.Table)
	}
	pk := o.Key
	pk.Columns = slices.Clone(pk.Columns)
	s.Tables[i].PrimaryKey = &pk
	return nil
}

func (o AddPrimaryKey) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " ADD " + primaryKeyClause(o.Key)}, nil
}

func (o AddPrimaryKey) Reverse(*schema.Schema) (Operation, error) {
	return DropPrimaryKey{Schema: o.Schema, Table: o.Table, Name: o.Key.Name}, nil
}

// DropPrimaryKey removes a primary key constraint.
type DropPrimaryKey struct {
	Schema string
	Table  string
	Name   string
}

func (o DropPrimaryKey) Describe() string {
	return fmt.Sprintf("drop primary key %s on %s.%s", o.Name, o.Schema, o.Table)
}
func (o DropPrimaryKey) Safety() Safety      { return Destructive }
func (o DropPrimaryKey) Transactional() bool { return true }

func (o DropPrimaryKey) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	s.Tables[i].PrimaryKey = nil
	return nil
}

func (o DropPrimaryKey) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " DROP CONSTRAINT " + ident(o.Name)}, nil
}

func (o DropPrimaryKey) Reverse(before *schema.Schema) (Operation, error) {
	t, ok := tableIn(before, o.Schema, o.Table)
	if !ok || t.PrimaryKey == nil {
		return irreversible("drop primary key "+o.Name, "the key's columns are not known")
	}
	return AddPrimaryKey{Schema: o.Schema, Table: o.Table, Key: *t.PrimaryKey}, nil
}

// AddUnique adds a unique constraint or a unique index.
//
// Which one is decided by the object itself: a UNIQUE constraint goes through
// ALTER TABLE and can be a foreign key's target, a bare unique index goes
// through CREATE INDEX and can be partial. They are different objects and stay
// different here.
type AddUnique struct {
	Schema string
	Table  string
	Unique schema.Unique
}

func (o AddUnique) Describe() string {
	kind := "unique index"
	if o.Unique.Constraint {
		kind = "unique constraint"
	}
	return fmt.Sprintf("add %s %s on %s.%s", kind, o.Unique.Name, o.Schema, o.Table)
}

func (o AddUnique) Safety() Safety      { return Locking }
func (o AddUnique) Transactional() bool { return true }

func (o AddUnique) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	u := o.Unique
	u.Columns = slices.Clone(u.Columns)
	s.Tables[i].Uniques = append(s.Tables[i].Uniques, u)
	s.Tables[i].Normalize()
	return nil
}

func (o AddUnique) SQL() ([]string, error) {
	if o.Unique.Constraint {
		return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " ADD " + uniqueClause(o.Unique)}, nil
	}
	return []string{uniqueIndexSQL(schema.Table{Schema: o.Schema, Name: o.Table}, o.Unique)}, nil
}

func (o AddUnique) Reverse(*schema.Schema) (Operation, error) {
	return DropUnique{Schema: o.Schema, Table: o.Table, Name: o.Unique.Name, Constraint: o.Unique.Constraint}, nil
}

// DropUnique removes a unique constraint or unique index.
type DropUnique struct {
	Schema     string
	Table      string
	Name       string
	Constraint bool
}

func (o DropUnique) Describe() string {
	return fmt.Sprintf("drop unique %s on %s.%s", o.Name, o.Schema, o.Table)
}
func (o DropUnique) Safety() Safety      { return Destructive }
func (o DropUnique) Transactional() bool { return true }

func (o DropUnique) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.Uniques, func(u schema.Unique) bool { return u.Name == o.Name })
	if idx < 0 {
		return fmt.Errorf("unique %s is not on %s.%s in the migration state", o.Name, o.Schema, o.Table)
	}
	t.Uniques = slices.Delete(t.Uniques, idx, idx+1)
	return nil
}

func (o DropUnique) SQL() ([]string, error) {
	if o.Constraint {
		return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " DROP CONSTRAINT " + ident(o.Name)}, nil
	}
	return []string{"DROP INDEX " + qualified(o.Schema, o.Name)}, nil
}

func (o DropUnique) Reverse(before *schema.Schema) (Operation, error) {
	t, ok := tableIn(before, o.Schema, o.Table)
	if !ok {
		return irreversible("drop unique "+o.Name, "the table is not known")
	}
	idx := slices.IndexFunc(t.Uniques, func(u schema.Unique) bool { return u.Name == o.Name })
	if idx < 0 {
		return irreversible("drop unique "+o.Name, "the object's definition is not known")
	}
	return AddUnique{Schema: o.Schema, Table: o.Table, Unique: t.Uniques[idx]}, nil
}

// AddForeignKey adds a foreign key constraint.
//
// Adding one validates every existing row, which locks both tables. NOT VALID
// is how that is avoided: the constraint applies to new rows immediately and
// the existing ones are checked later by [ValidateConstraint], under a much
// weaker lock.
type AddForeignKey struct {
	Schema     string
	Table      string
	ForeignKey schema.ForeignKey
}

func (o AddForeignKey) Describe() string {
	s := fmt.Sprintf("add foreign key %s on %s.%s -> %s.%s",
		o.ForeignKey.Name, o.Schema, o.Table, o.ForeignKey.RefSchema, o.ForeignKey.RefTable)
	if o.ForeignKey.NotValid {
		s += " (not valid)"
	}
	return s
}

func (o AddForeignKey) Safety() Safety {
	if o.ForeignKey.NotValid {
		return Safe
	}
	return Locking
}

func (o AddForeignKey) Transactional() bool { return true }

func (o AddForeignKey) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	f := o.ForeignKey
	f.Columns = slices.Clone(f.Columns)
	f.RefColumns = slices.Clone(f.RefColumns)
	s.Tables[i].ForeignKeys = append(s.Tables[i].ForeignKeys, f)
	s.Tables[i].Normalize()
	return nil
}

func (o AddForeignKey) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " ADD " + foreignKeyClause(o.ForeignKey)}, nil
}

func (o AddForeignKey) Reverse(*schema.Schema) (Operation, error) {
	return DropForeignKey{Schema: o.Schema, Table: o.Table, Name: o.ForeignKey.Name}, nil
}

// DropForeignKey removes a foreign key constraint.
type DropForeignKey struct {
	Schema string
	Table  string
	Name   string
}

func (o DropForeignKey) Describe() string {
	return fmt.Sprintf("drop foreign key %s on %s.%s", o.Name, o.Schema, o.Table)
}
func (o DropForeignKey) Safety() Safety      { return Destructive }
func (o DropForeignKey) Transactional() bool { return true }

func (o DropForeignKey) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == o.Name })
	if idx < 0 {
		return fmt.Errorf("foreign key %s is not on %s.%s in the migration state", o.Name, o.Schema, o.Table)
	}
	t.ForeignKeys = slices.Delete(t.ForeignKeys, idx, idx+1)
	return nil
}

func (o DropForeignKey) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " DROP CONSTRAINT " + ident(o.Name)}, nil
}

func (o DropForeignKey) Reverse(before *schema.Schema) (Operation, error) {
	t, ok := tableIn(before, o.Schema, o.Table)
	if !ok {
		return irreversible("drop foreign key "+o.Name, "the table is not known")
	}
	idx := slices.IndexFunc(t.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == o.Name })
	if idx < 0 {
		return irreversible("drop foreign key "+o.Name, "the constraint's definition is not known")
	}
	return AddForeignKey{Schema: o.Schema, Table: o.Table, ForeignKey: t.ForeignKeys[idx]}, nil
}

// AddCheck adds a CHECK constraint.
type AddCheck struct {
	Schema string
	Table  string
	Check  schema.Check
}

func (o AddCheck) Describe() string {
	s := fmt.Sprintf("add check %s on %s.%s", o.Check.Name, o.Schema, o.Table)
	if o.Check.NotValid {
		s += " (not valid)"
	}
	return s
}

func (o AddCheck) Safety() Safety {
	if o.Check.NotValid {
		return Safe
	}
	return Locking
}

func (o AddCheck) Transactional() bool { return true }

func (o AddCheck) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	s.Tables[i].Checks = append(s.Tables[i].Checks, o.Check)
	s.Tables[i].Normalize()
	return nil
}

func (o AddCheck) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " ADD " + checkClause(o.Check)}, nil
}

func (o AddCheck) Reverse(*schema.Schema) (Operation, error) {
	return DropCheck{Schema: o.Schema, Table: o.Table, Name: o.Check.Name}, nil
}

// DropCheck removes a CHECK constraint.
type DropCheck struct {
	Schema string
	Table  string
	Name   string
}

func (o DropCheck) Describe() string {
	return fmt.Sprintf("drop check %s on %s.%s", o.Name, o.Schema, o.Table)
}
func (o DropCheck) Safety() Safety      { return Destructive }
func (o DropCheck) Transactional() bool { return true }

func (o DropCheck) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.Checks, func(c schema.Check) bool { return c.Name == o.Name })
	if idx < 0 {
		return fmt.Errorf("check %s is not on %s.%s in the migration state", o.Name, o.Schema, o.Table)
	}
	t.Checks = slices.Delete(t.Checks, idx, idx+1)
	return nil
}

func (o DropCheck) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " DROP CONSTRAINT " + ident(o.Name)}, nil
}

func (o DropCheck) Reverse(before *schema.Schema) (Operation, error) {
	t, ok := tableIn(before, o.Schema, o.Table)
	if !ok {
		return irreversible("drop check "+o.Name, "the table is not known")
	}
	idx := slices.IndexFunc(t.Checks, func(c schema.Check) bool { return c.Name == o.Name })
	if idx < 0 {
		return irreversible("drop check "+o.Name, "the constraint's definition is not known")
	}
	return AddCheck{Schema: o.Schema, Table: o.Table, Check: t.Checks[idx]}, nil
}

// ValidateConstraint checks the rows a NOT VALID constraint skipped.
//
// It is the second half of PostgreSQL's staged pattern: add the constraint
// without scanning the table, then validate under a lock that does not block
// reads or writes.
type ValidateConstraint struct {
	Schema string
	Table  string
	Name   string
}

func (o ValidateConstraint) Describe() string {
	return fmt.Sprintf("validate constraint %s on %s.%s", o.Name, o.Schema, o.Table)
}
func (o ValidateConstraint) Safety() Safety      { return Safe }
func (o ValidateConstraint) Transactional() bool { return true }

// Apply clears the NOT VALID flag wherever the named constraint sits.
func (o ValidateConstraint) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	for ci := range t.Checks {
		if t.Checks[ci].Name == o.Name {
			t.Checks[ci].NotValid = false
			return nil
		}
	}
	for fi := range t.ForeignKeys {
		if t.ForeignKeys[fi].Name == o.Name {
			t.ForeignKeys[fi].NotValid = false
			return nil
		}
	}
	return fmt.Errorf("constraint %s is not on %s.%s in the migration state", o.Name, o.Schema, o.Table)
}

func (o ValidateConstraint) SQL() ([]string, error) {
	return []string{"ALTER TABLE " + qualified(o.Schema, o.Table) + " VALIDATE CONSTRAINT " + ident(o.Name)}, nil
}

// Reverse is refused. PostgreSQL has no statement that marks a validated
// constraint invalid again, and pretending otherwise would produce a migration
// that fails halfway through a rollback.
func (o ValidateConstraint) Reverse(*schema.Schema) (Operation, error) {
	return irreversible("validate constraint "+o.Name,
		"PostgreSQL cannot mark a validated constraint NOT VALID again")
}

// CreateIndex creates an index.
type CreateIndex struct {
	Schema string
	Table  string
	Index  schema.Index
}

func (o CreateIndex) Describe() string {
	s := fmt.Sprintf("create index %s on %s.%s", o.Index.Name, o.Schema, o.Table)
	if o.Index.Concurrently {
		s += " concurrently"
	}
	return s
}

// Safety reports locking for an ordinary index, which blocks writes to the
// table while it is built, and safe for a concurrent one, which does not.
func (o CreateIndex) Safety() Safety {
	if o.Index.Concurrently {
		return Safe
	}
	return Locking
}

// Transactional reports false for a concurrent index, which PostgreSQL refuses
// to run inside a transaction block. That answer decides how the whole
// migration executes.
func (o CreateIndex) Transactional() bool { return !o.Index.Concurrently }

func (o CreateIndex) Apply(s *schema.Schema) error {
	h, err := indexHolderOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	// An index already in the state is refused rather than added again.
	//
	// The planner never emits one: it creates an index only when the state does
	// not have it. What does produce one is an artifact somebody edited or a
	// merge that kept both sides, and replaying that quietly left the same index
	// in the state twice — which the planner cannot repair, because both loops
	// there match by name and a duplicate looks like agreement. The project then
	// never converges, and nothing says why.
	//
	// The two operations either side of this one already refuse the
	// corresponding impossibility: creating a relation that exists, dropping an
	// index that does not. This one was the gap.
	if slices.ContainsFunc(*h.indexes, func(x schema.Index) bool { return x.Name == o.Index.Name }) {
		return fmt.Errorf("index %s is already on %s.%s in the migration state",
			o.Index.Name, o.Schema, o.Table)
	}
	idx := o.Index
	idx.Columns = slices.Clone(idx.Columns)
	idx.Include = slices.Clone(idx.Include)
	*h.indexes = append(*h.indexes, idx)
	h.normalize()
	return nil
}

func (o CreateIndex) SQL() ([]string, error) {
	stmt, err := createIndexSQL(o.Schema, o.Table, o.Index)
	if err != nil {
		return nil, err
	}
	return []string{stmt}, nil
}

func (o CreateIndex) Reverse(*schema.Schema) (Operation, error) {
	return DropIndex{Schema: o.Schema, Table: o.Table, Name: o.Index.Name, Concurrently: o.Index.Concurrently}, nil
}

// DropIndex removes an index.
type DropIndex struct {
	Schema string
	Table  string
	Name   string
	// Concurrently asks for DROP INDEX CONCURRENTLY, which like its
	// counterpart cannot run inside a transaction.
	Concurrently bool
}

func (o DropIndex) Describe() string {
	return fmt.Sprintf("drop index %s on %s.%s", o.Name, o.Schema, o.Table)
}

// Safety reports what dropping the index costs, which is a lock rather than
// data: an index holds nothing of its own, and rebuilding it is a matter of
// time. The lock is the same one building it takes, so the two are classified
// the same way — DROP INDEX takes ACCESS EXCLUSIVE on the table and
// CONCURRENTLY does not.
func (o DropIndex) Safety() Safety {
	if o.Concurrently {
		return Safe
	}
	return Locking
}

func (o DropIndex) Transactional() bool { return !o.Concurrently }

func (o DropIndex) Apply(s *schema.Schema) error {
	h, err := indexHolderOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	idx := slices.IndexFunc(*h.indexes, func(x schema.Index) bool { return x.Name == o.Name })
	if idx < 0 {
		return fmt.Errorf("index %s is not on %s.%s in the migration state", o.Name, o.Schema, o.Table)
	}
	*h.indexes = slices.Delete(*h.indexes, idx, idx+1)
	return nil
}

func (o DropIndex) SQL() ([]string, error) {
	stmt := "DROP INDEX "
	if o.Concurrently {
		stmt += "CONCURRENTLY "
	}
	return []string{stmt + qualified(o.Schema, o.Name)}, nil
}

func (o DropIndex) Reverse(before *schema.Schema) (Operation, error) {
	have, ok := indexesIn(before, o.Schema, o.Table)
	if !ok {
		return irreversible("drop index "+o.Name, "the relation is not known")
	}
	idx := slices.IndexFunc(have, func(x schema.Index) bool { return x.Name == o.Name })
	if idx < 0 {
		return irreversible("drop index "+o.Name, "the index's definition is not known")
	}
	return CreateIndex{Schema: o.Schema, Table: o.Table, Index: have[idx]}, nil
}

// RenameIndex renames an index.
type RenameIndex struct {
	Schema string
	Table  string
	From   string
	To     string
}

func (o RenameIndex) Describe() string {
	return fmt.Sprintf("rename index %s.%s to %s", o.Schema, o.From, o.To)
}
func (o RenameIndex) Safety() Safety      { return Safe }
func (o RenameIndex) Transactional() bool { return true }

func (o RenameIndex) Apply(s *schema.Schema) error {
	i, err := tableOf(s, o.Schema, o.Table)
	if err != nil {
		return err
	}
	t := &s.Tables[i]
	idx := slices.IndexFunc(t.Indexes, func(x schema.Index) bool { return x.Name == o.From })
	if idx < 0 {
		return fmt.Errorf("index %s is not on %s.%s in the migration state", o.From, o.Schema, o.Table)
	}
	t.Indexes[idx].Name = o.To
	t.Normalize()
	return nil
}

func (o RenameIndex) SQL() ([]string, error) {
	return []string{"ALTER INDEX " + qualified(o.Schema, o.From) + " RENAME TO " + ident(o.To)}, nil
}

func (o RenameIndex) Reverse(*schema.Schema) (Operation, error) {
	return RenameIndex{Schema: o.Schema, Table: o.Table, From: o.To, To: o.From}, nil
}

// tableIn is the read-only lookup the reverse operations use.
func tableIn(s *schema.Schema, schemaName, name string) (schema.Table, bool) {
	if s == nil {
		return schema.Table{}, false
	}
	return s.Table(schemaName, name)
}
