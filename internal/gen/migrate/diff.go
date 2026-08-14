package migrate

import (
	"fmt"
	"slices"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The semantic diff.
//
// It compares two canonical states and produces the operations that turn the
// first into the second. It compares the states themselves and never the SQL
// they would render: two identical schemas can render differently — a default
// PostgreSQL normalised, a constraint order that means nothing — and a diff
// over text would report changes nobody made and miss ones they did.
//
// Order matters in the output. Things are created before the things that depend
// on them and dropped in the opposite order, so a plan can be applied in the
// sequence it is printed.

// Rename is a rename the caller confirmed.
//
// Nothing here guesses. A column that disappeared and one that appeared may be
// a rename or may be a drop and an add, and the difference is whether the data
// survives — so the engine reports candidates and something outside it decides.
type Rename struct {
	Schema string
	// Table is empty for a table rename, and names the table for a column one.
	Table string
	From  string
	To    string
}

// RenameCandidate is a pair the diff cannot tell apart from a drop and an add.
type RenameCandidate struct {
	Schema string
	// Table is empty when the candidate is a table rather than a column.
	Table string
	From  string
	To    string
	// Reason says what made them look alike, for a prompt to show.
	Reason string
}

func (c RenameCandidate) String() string {
	if c.Table == "" {
		return fmt.Sprintf("table %s.%s -> %s.%s", c.Schema, c.From, c.Schema, c.To)
	}
	return fmt.Sprintf("%s.%s.%s -> %s", c.Schema, c.Table, c.From, c.To)
}

// Diff is the result of comparing two states.
type Diff struct {
	// Operations turn the first state into the second.
	Operations []Operation
	// Candidates are renames the diff could not decide, which it treated as a
	// drop and an add. A caller that can ask should ask and diff again.
	Candidates []RenameCandidate
}

// Empty reports whether the states already agree.
func (d Diff) Empty() bool { return len(d.Operations) == 0 }

// Options control how a diff is produced.
type Options struct {
	// Renames are the ones already confirmed. A confirmed rename is applied
	// before anything else looks at the objects involved, so the rest of the
	// diff sees them as the same object under the new name.
	Renames []Rename
	// Views carries what planning views needs beyond the migration state: the
	// live database and the definitions this project recorded there. It is the
	// zero value for an offline plan, and the planner reports what it cannot
	// check rather than assuming it is fine.
	Views ViewPlanInput
}

// Compute produces the operations that turn from into to.
func Compute(from, to *schema.Schema, opts Options) (Diff, error) {
	// The confirmed renames are applied to a copy of the old state first. After
	// that the two states name the same objects the same way, and every
	// remaining difference is a real change rather than a rename in disguise.
	state := from.Clone()
	var d Diff
	for _, r := range opts.Renames {
		op, err := renameOperation(state, r)
		if err != nil {
			return Diff{}, err
		}
		if err := op.Apply(state); err != nil {
			return Diff{}, fmt.Errorf("applying the confirmed rename %s: %w", r.From, err)
		}
		d.Operations = append(d.Operations, op)
	}

	state.Normalize()
	desired := to.Clone()
	desired.Normalize()

	// Views and materialized views are not planned yet.
	//
	// The refusal is here, before anything else runs, and it is total: no
	// partial migration is written, and nothing downstream sees a view. The
	// alternative — planning the tables and quietly ignoring the views — would
	// produce a migration that looked complete, applied cleanly, and left the
	// schema missing every relation the project declared. A command that
	// refuses is a command somebody fixes; one that half-succeeds is one they
	// find out about later.
	//
	// It also cannot reinterpret a view as a table: the desired schema keeps
	// them in separate slices of separate types, so there is no path by which a
	// stored query reaches diffTables at all.
	diffExtensions(state, desired, &d)
	diffEnums(state, desired, &d)
	if err := diffTables(state, desired, opts.Views, &d); err != nil {
		return Diff{}, err
	}
	// Stored queries last, so that a table they read is created before them.
	// Views and materialized views are planned in one pass because they can
	// depend on each other in either direction.
	if err := planStoredRelations(state, desired, opts.Views, &d); err != nil {
		return Diff{}, err
	}
	return d, nil
}

// renameOperation turns a confirmed rename into its operation.
func renameOperation(state *schema.Schema, r Rename) (Operation, error) {
	if r.Table == "" {
		if _, ok := state.Table(r.Schema, r.From); !ok {
			return nil, fmt.Errorf("cannot rename %s.%s: no such table in the migration state", r.Schema, r.From)
		}
		return RenameTable{Schema: r.Schema, From: r.From, To: r.To}, nil
	}
	t, ok := state.Table(r.Schema, r.Table)
	if !ok {
		return nil, fmt.Errorf("cannot rename %s.%s.%s: no such table in the migration state", r.Schema, r.Table, r.From)
	}
	if _, ok := t.Column(r.From); !ok {
		return nil, fmt.Errorf("cannot rename %s.%s.%s: no such column in the migration state", r.Schema, r.Table, r.From)
	}
	return RenameColumn{Schema: r.Schema, Table: r.Table, From: r.From, To: r.To}, nil
}

func diffExtensions(from, to *schema.Schema, d *Diff) {
	for _, want := range to.Extensions {
		if !slices.ContainsFunc(from.Extensions, func(e schema.Extension) bool { return e.Name == want.Name }) {
			d.Operations = append(d.Operations, CreateExtension{Extension: want})
		}
	}
	// An extension nobody declares any more is left alone. Dropping one takes
	// every object depending on it, which is never what removing a line from a
	// declaration was asking for.
}

func diffEnums(from, to *schema.Schema, d *Diff) {
	for _, want := range to.Enums {
		have, ok := from.Enum(want.Schema, want.Name)
		if !ok {
			d.Operations = append(d.Operations, CreateEnum{Enum: want})
			continue
		}
		diffEnumLabels(have, want, d)
	}
	for _, have := range from.Enums {
		if _, ok := to.Enum(have.Schema, have.Name); !ok {
			d.Operations = append(d.Operations, DropEnum{Schema: have.Schema, Name: have.Name})
		}
	}
}

// diffEnumLabels adds the labels a type gained and refuses the changes
// PostgreSQL cannot make.
//
// Adding a label is one statement. Removing one is not: PostgreSQL has no way
// to drop a label, and rebuilding the type would need every row that holds the
// value rewritten first — a data migration whose correct form depends entirely
// on what the application means by the value being removed. Generating one
// automatically would be guessing about data.
func diffEnumLabels(have, want schema.Enum, d *Diff) {
	for _, label := range have.Labels {
		if slices.Contains(want.Labels, label) {
			continue
		}
		d.Operations = append(d.Operations, unsupportedEnumChange{
			Enum:  have,
			Label: label,
		})
		return
	}
	// Each new label is placed where the desired order says it goes, so the
	// type sorts the way it was declared rather than the way it grew.
	for i, label := range want.Labels {
		if slices.Contains(have.Labels, label) {
			continue
		}
		op := AddEnumValue{Schema: want.Schema, Name: want.Name, Value: label}
		if i > 0 {
			op.After = want.Labels[i-1]
		} else if len(want.Labels) > 1 {
			op.Before = want.Labels[1]
		}
		d.Operations = append(d.Operations, op)
	}
}

func diffTables(from, to *schema.Schema, in ViewPlanInput, d *Diff) error {
	// Created tables come first so that a foreign key added below has something
	// to point at — and among themselves in an order where each one's
	// references already exist.
	var created []schema.Table
	for _, want := range to.Tables {
		if _, ok := from.Table(want.Schema, want.Name); !ok {
			// PostgreSQL has one namespace for relations, so a table cannot be
			// created where a view or a materialized view already stands. The
			// planner refuses rather than writing CREATE TABLE and letting the
			// server discover it: a migration that can never apply is still a
			// file somebody committed, reviewed and shipped.
			//
			// This is the table half of what refuseOccupiedName does for
			// stored queries. It fires only when a relation of another kind
			// holds the name, so a project with no views plans exactly as it
			// always did.
			if kind, occupied := from.Relation(want.Schema, want.Name); occupied && kind != schema.KindTable {
				return fmt.Errorf("cannot create table %s.%s: the migration state already has a "+
					"%s of that name. PostgreSQL has one namespace for relations, so creating "+
					"this table means removing that one — which is a decision a migration should "+
					"state rather than infer", want.Schema, want.Name, kind)
			}
			// The database may hold a relation this project never created — a
			// view somebody built by hand, or a materialized view a previous
			// declaration owned. The migration state cannot see either.
			if in.Actual != nil {
				// Only another kind is refused. A table of this name that the
				// migration state does not know about is a situation the table
				// planner has always handled its own way, and changing that
				// would move behaviour this milestone froze.
				if kind, occupied := in.Actual.Relation(want.Schema, want.Name); occupied && kind != schema.KindTable {
					return fmt.Errorf("cannot create table %s.%s: the database already has a %s of "+
						"that name. Dropping it is not something this planner will decide on its "+
						"own; write the migration that says what happens to it",
						want.Schema, want.Name, kind)
				}
			}
			created = append(created, want)
		}
	}
	for _, op := range createTableOps(created) {
		d.Operations = append(d.Operations, op)
	}
	for _, want := range to.Tables {
		have, ok := from.Table(want.Schema, want.Name)
		if !ok {
			continue
		}
		if err := diffTable(have, want, d); err != nil {
			return err
		}
	}
	// Dropped tables come last, after anything referring to them has gone.
	for _, have := range from.Tables {
		if _, ok := to.Table(have.Schema, have.Name); !ok {
			d.Candidates = append(d.Candidates, tableRenameCandidates(have, from, to)...)
			d.Operations = append(d.Operations, DropTable{Schema: have.Schema, Name: have.Name})
		}
	}
	return nil
}

// createTableOps orders new tables so that each one's foreign keys have
// something to point at by the time it is created.
//
// A table referencing another that does not exist yet is a statement PostgreSQL
// refuses, so the order is not cosmetic. When two new tables reference each
// other there is no order that works, and the cycle is broken the way it has to
// be: the tables are created without the constraints that close it, and those
// constraints are added afterwards.
func createTableOps(tables []schema.Table) []Operation {
	// References are only a constraint on ordering when they point at another
	// table being created in the same breath. A reference to a table that
	// already exists is satisfied whatever the order.
	pending := make(map[string]bool, len(tables))
	for _, t := range tables {
		pending[t.Qualified()] = true
	}
	deps := make(map[string][]string, len(tables))
	for _, t := range tables {
		for _, fk := range t.ForeignKeys {
			ref := fk.RefSchema + "." + fk.RefTable
			// A table referencing itself needs no ordering: the constraint is
			// created with the table.
			if ref != t.Qualified() && pending[ref] {
				deps[t.Qualified()] = append(deps[t.Qualified()], ref)
			}
		}
	}

	byName := make(map[string]schema.Table, len(tables))
	names := make([]string, 0, len(tables))
	for _, t := range tables {
		byName[t.Qualified()] = t
		names = append(names, t.Qualified())
	}
	slices.Sort(names)

	var (
		ops      []Operation
		emitted  = make(map[string]bool, len(tables))
		deferred []schema.ForeignKey
		deferTbl []schema.Table
	)
	// Repeatedly emit whatever is ready, in name order so the result is the
	// same on every run.
	for len(emitted) < len(names) {
		progress := false
		for _, name := range names {
			if emitted[name] {
				continue
			}
			ready := true
			for _, dep := range deps[name] {
				if !emitted[dep] {
					ready = false
					break
				}
			}
			if !ready {
				continue
			}
			ops = append(ops, CreateTable{Table: byName[name]})
			emitted[name] = true
			progress = true
		}
		if progress {
			continue
		}
		// Everything left is in a cycle. The one with the first name gives up
		// the references that close it; those come back as their own operations
		// once every table exists.
		for _, name := range names {
			if emitted[name] {
				continue
			}
			t := byName[name]
			var kept []schema.ForeignKey
			for _, fk := range t.ForeignKeys {
				ref := fk.RefSchema + "." + fk.RefTable
				if ref != name && pending[ref] && !emitted[ref] {
					deferred = append(deferred, fk)
					deferTbl = append(deferTbl, t)
					continue
				}
				kept = append(kept, fk)
			}
			t.ForeignKeys = kept
			byName[name] = t
			delete(deps, name)
			break
		}
	}
	for i, fk := range deferred {
		ops = append(ops, AddForeignKey{
			Schema: deferTbl[i].Schema, Table: deferTbl[i].Name, ForeignKey: fk,
		})
	}
	return ops
}

// tableRenameCandidates reports tables that appeared and might be this one
// under a new name.
func tableRenameCandidates(dropped schema.Table, from, to *schema.Schema) []RenameCandidate {
	var out []RenameCandidate
	for _, added := range to.Tables {
		if _, existed := from.Table(added.Schema, added.Name); existed {
			continue
		}
		if added.Schema != dropped.Schema || !sameColumnShape(dropped, added) {
			continue
		}
		out = append(out, RenameCandidate{
			Schema: dropped.Schema,
			From:   dropped.Name,
			To:     added.Name,
			Reason: "the same columns, in the same order, with the same types",
		})
	}
	return out
}

// sameColumnShape reports whether two tables have identical column lists, which
// is the only evidence a rename can be inferred from without asking.
func sameColumnShape(a, b schema.Table) bool {
	if len(a.Columns) != len(b.Columns) || len(a.Columns) == 0 {
		return false
	}
	for i := range a.Columns {
		if !sameColumn(a.Columns[i], b.Columns[i]) {
			return false
		}
	}
	return true
}

// sameColumn compares two columns as PostgreSQL would understand them.
//
// Expressions are compared through the same normalisation the drift check uses:
// the catalog does not keep the text somebody typed, and a diff that compared
// raw strings would propose changing 'active' into 'active'::account_state
// forever.
func sameColumn(a, b schema.Column) bool {
	return a.Name == b.Name && a.Type.Canonical() == b.Type.Canonical() && a.Nullable == b.Nullable &&
		schema.SameExpr(a.Default, b.Default) && a.Identity == b.Identity &&
		schema.SameExpr(a.Generated, b.Generated)
}

func diffTable(have, want schema.Table, d *Diff) error {
	diffColumns(have, want, d)
	diffPrimaryKey(have, want, d)
	diffUniques(have, want, d)
	diffChecks(have, want, d)
	diffForeignKeys(have, want, d)
	diffIndexes(have, want, d)
	return nil
}

func diffColumns(have, want schema.Table, d *Diff) {
	for _, w := range want.Columns {
		h, ok := have.Column(w.Name)
		if !ok {
			d.Operations = append(d.Operations, AddColumn{Schema: want.Schema, Table: want.Name, Column: w})
			continue
		}
		if sameColumn(h, w) && h.Collation == w.Collation {
			continue
		}
		d.Operations = append(d.Operations, AlterColumn{
			Schema: want.Schema, Table: want.Name, Name: w.Name, From: h, To: w,
		})
	}
	for _, h := range have.Columns {
		if _, ok := want.Column(h.Name); ok {
			continue
		}
		d.Candidates = append(d.Candidates, columnRenameCandidates(have, want, h)...)
		d.Operations = append(d.Operations, DropColumn{Schema: have.Schema, Table: have.Name, Name: h.Name})
	}
}

// columnRenameCandidates reports the added columns that look like this dropped
// one under a new name.
//
// "Look like" is everything but the name: same type, same nullability, same
// default. That is the strongest evidence available without asking, and it is
// still only evidence — two columns of type text are indistinguishable from
// each other, which is exactly why this returns candidates rather than
// operations.
func columnRenameCandidates(have, want schema.Table, dropped schema.Column) []RenameCandidate {
	var out []RenameCandidate
	for _, added := range want.Columns {
		if _, existed := have.Column(added.Name); existed {
			continue
		}
		if added.Type != dropped.Type || added.Nullable != dropped.Nullable ||
			!schema.SameExpr(added.Default, dropped.Default) || added.Identity != dropped.Identity {
			continue
		}
		out = append(out, RenameCandidate{
			Schema: have.Schema,
			Table:  have.Name,
			From:   dropped.Name,
			To:     added.Name,
			Reason: fmt.Sprintf("both are %s, %s, with the same default", added.Type, nullability(added)),
		})
	}
	return out
}

func nullability(c schema.Column) string {
	if c.Nullable {
		return "nullable"
	}
	return "NOT NULL"
}

func diffPrimaryKey(have, want schema.Table, d *Diff) {
	switch {
	case have.PrimaryKey == nil && want.PrimaryKey == nil:
		return
	case have.PrimaryKey == nil:
		d.Operations = append(d.Operations, AddPrimaryKey{Schema: want.Schema, Table: want.Name, Key: *want.PrimaryKey})
	case want.PrimaryKey == nil:
		d.Operations = append(d.Operations, DropPrimaryKey{Schema: have.Schema, Table: have.Name, Name: have.PrimaryKey.Name})
	case !samePrimaryKey(*have.PrimaryKey, *want.PrimaryKey):
		// A primary key is replaced rather than altered: PostgreSQL has no
		// statement that changes one in place, and the two halves are separate
		// operations with separate risks.
		d.Operations = append(d.Operations,
			DropPrimaryKey{Schema: have.Schema, Table: have.Name, Name: have.PrimaryKey.Name},
			AddPrimaryKey{Schema: want.Schema, Table: want.Name, Key: *want.PrimaryKey})
	}
}

func samePrimaryKey(a, b schema.PrimaryKey) bool {
	// The name is compared too: a constraint renamed is a constraint whose name
	// something else may refer to.
	return a.Name == b.Name && slices.Equal(a.Columns, b.Columns)
}

func diffUniques(have, want schema.Table, d *Diff) {
	for _, w := range want.Uniques {
		i := slices.IndexFunc(have.Uniques, func(u schema.Unique) bool { return u.Name == w.Name })
		if i < 0 {
			d.Operations = append(d.Operations, AddUnique{Schema: want.Schema, Table: want.Name, Unique: w})
			continue
		}
		if sameUnique(have.Uniques[i], w) {
			continue
		}
		d.Operations = append(d.Operations,
			DropUnique{Schema: have.Schema, Table: have.Name, Name: w.Name, Constraint: have.Uniques[i].Constraint},
			AddUnique{Schema: want.Schema, Table: want.Name, Unique: w})
	}
	for _, h := range have.Uniques {
		if !slices.ContainsFunc(want.Uniques, func(u schema.Unique) bool { return u.Name == h.Name }) {
			d.Operations = append(d.Operations,
				DropUnique{Schema: have.Schema, Table: have.Name, Name: h.Name, Constraint: h.Constraint})
		}
	}
}

// sameUnique asks the one rule that decides when two unique objects are the same
// object.
//
// This package used to answer it itself, with a copy that agreed with the schema
// comparison and that nothing kept agreeing — the same shape as the duplicated
// index comparison, in the same two packages, for the object next to it.
//
// It stays a named function rather than being inlined at its call site so that
// "what counts as the same unique here" has one answer to point at, and so a
// future caller reaches for this rather than writing a third comparison.
func sameUnique(a, b schema.Unique) bool { return schema.SameUnique(a, b) }

func diffChecks(have, want schema.Table, d *Diff) {
	for _, w := range want.Checks {
		i := slices.IndexFunc(have.Checks, func(c schema.Check) bool { return c.Name == w.Name })
		if i < 0 {
			d.Operations = append(d.Operations, AddCheck{Schema: want.Schema, Table: want.Name, Check: w})
			continue
		}
		h := have.Checks[i]
		same := schema.SameExpr(h.Expression, w.Expression)
		switch {
		case same && h.NotValid == w.NotValid:
			continue
		// A constraint that was NOT VALID and is now expected to be valid is
		// validated rather than rebuilt, which is the whole point of the staged
		// pattern.
		case same && h.NotValid && !w.NotValid:
			d.Operations = append(d.Operations, ValidateConstraint{Schema: want.Schema, Table: want.Name, Name: w.Name})
		default:
			d.Operations = append(d.Operations,
				DropCheck{Schema: have.Schema, Table: have.Name, Name: w.Name},
				AddCheck{Schema: want.Schema, Table: want.Name, Check: w})
		}
	}
	for _, h := range have.Checks {
		if !slices.ContainsFunc(want.Checks, func(c schema.Check) bool { return c.Name == h.Name }) {
			d.Operations = append(d.Operations, DropCheck{Schema: have.Schema, Table: have.Name, Name: h.Name})
		}
	}
}

func diffForeignKeys(have, want schema.Table, d *Diff) {
	for _, w := range want.ForeignKeys {
		i := slices.IndexFunc(have.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == w.Name })
		if i < 0 {
			d.Operations = append(d.Operations, AddForeignKey{Schema: want.Schema, Table: want.Name, ForeignKey: w})
			continue
		}
		h := have.ForeignKeys[i]
		switch {
		case sameForeignKey(h, w):
			continue
		case sameForeignKeyShape(h, w) && h.NotValid && !w.NotValid:
			d.Operations = append(d.Operations, ValidateConstraint{Schema: want.Schema, Table: want.Name, Name: w.Name})
		default:
			d.Operations = append(d.Operations,
				DropForeignKey{Schema: have.Schema, Table: have.Name, Name: w.Name},
				AddForeignKey{Schema: want.Schema, Table: want.Name, ForeignKey: w})
		}
	}
	for _, h := range have.ForeignKeys {
		if !slices.ContainsFunc(want.ForeignKeys, func(f schema.ForeignKey) bool { return f.Name == h.Name }) {
			d.Operations = append(d.Operations, DropForeignKey{Schema: have.Schema, Table: have.Name, Name: h.Name})
		}
	}
}

func sameForeignKey(a, b schema.ForeignKey) bool {
	return sameForeignKeyShape(a, b) && a.NotValid == b.NotValid
}

func sameForeignKeyShape(a, b schema.ForeignKey) bool {
	return slices.Equal(a.Columns, b.Columns) && slices.Equal(a.RefColumns, b.RefColumns) &&
		a.RefSchema == b.RefSchema && a.RefTable == b.RefTable &&
		normalizeAction(a.OnDelete) == normalizeAction(b.OnDelete) &&
		normalizeAction(a.OnUpdate) == normalizeAction(b.OnUpdate) &&
		a.Deferrable == b.Deferrable && a.InitiallyDeferred == b.InitiallyDeferred
}

// normalizeAction treats an unset action as NO ACTION, which is what PostgreSQL
// stores for one. Without this, a constraint declared without an action and the
// same constraint read back from the catalog would differ forever.
func normalizeAction(a schema.Action) schema.Action {
	if a == "" {
		return schema.NoAction
	}
	return a
}

func diffIndexes(have, want schema.Table, d *Diff) {
	for _, w := range want.Indexes {
		i := slices.IndexFunc(have.Indexes, func(x schema.Index) bool { return x.Name == w.Name })
		if i < 0 {
			d.Operations = append(d.Operations, CreateIndex{Schema: want.Schema, Table: want.Name, Index: w})
			continue
		}
		if sameIndex(have.Indexes[i], w) {
			continue
		}
		// An index cannot be altered in place. Dropping first keeps the name
		// free for the replacement.
		d.Operations = append(d.Operations,
			DropIndex{Schema: have.Schema, Table: have.Name, Name: w.Name, Concurrently: w.Concurrently},
			CreateIndex{Schema: want.Schema, Table: want.Name, Index: w})
	}
	for _, h := range have.Indexes {
		if !slices.ContainsFunc(want.Indexes, func(x schema.Index) bool { return x.Name == h.Name }) {
			d.Operations = append(d.Operations,
				DropIndex{Schema: have.Schema, Table: have.Name, Name: h.Name, Concurrently: h.Concurrently})
		}
	}
}

// sameIndex asks the one rule that decides when two indexes are the same index.
//
// This package used to answer it itself, with a copy that agreed with the
// schema comparison and that nothing kept agreeing. Removing an axis from
// either left the other's tests green, so a project would either stop being
// offered the migration for an index it declared or stop being told about one
// somebody changed by hand — one half at a time, with nothing red in between.
//
// It stays as a named function rather than being inlined at the two call sites
// so that "what counts as the same index here" has one answer to point at, and
// so that a future caller reaches for this rather than writing a third
// comparison.
func sameIndex(a, b schema.Index) bool { return schema.SameIndex(a, b) }

// unsupportedEnumChange is the operation a removed enum label produces.
//
// It exists so that the diff can report the problem in the place a caller is
// already looking, rather than returning an error that loses everything else
// the diff found. It refuses to render SQL, so a migration containing one
// cannot be applied by accident.
type unsupportedEnumChange struct {
	Enum  schema.Enum
	Label string
}

func (o unsupportedEnumChange) Describe() string {
	return fmt.Sprintf("remove label %q from enum %s — not supported", o.Label, o.Enum.Qualified())
}
func (o unsupportedEnumChange) Safety() Safety      { return Destructive }
func (o unsupportedEnumChange) Transactional() bool { return true }

// Apply refuses rather than doing nothing.
//
// A no-op here would be an operation that claims to change the state and does
// not, so reconstructing a history containing one would produce a schema no
// migration describes — silently, which is the worst way for a migration state
// to be wrong. The operation exists to carry a refusal into a summary, and this
// is that refusal in the one other place it could be missed.
func (o unsupportedEnumChange) Apply(*schema.Schema) error {
	_, err := o.SQL()
	return err
}

func (o unsupportedEnumChange) SQL() ([]string, error) {
	return nil, fmt.Errorf(
		"enum %s no longer declares the label %q, and PostgreSQL cannot remove one:"+
			" the type would have to be rebuilt, which needs every row still holding %q to be given another value first."+
			" Write that migration by hand, with the update your application's data actually needs",
		o.Enum.Qualified(), o.Label, o.Label)
}

func (o unsupportedEnumChange) Reverse(*schema.Schema) (Operation, error) {
	return irreversible(o.Describe(), "the change cannot be made in the first place")
}
