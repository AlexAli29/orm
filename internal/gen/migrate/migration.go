package migrate

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// The migration artifact.
//
// A migration is identified by its ID and by nothing else — not by a filename,
// not by its position in a directory listing, not by when it was written. That
// matters because the same migration has to mean the same thing on a laptop, in
// CI and on a server, and because an artifact whose identity depends on where it
// lives is one that changes meaning when somebody moves a file.

// FormatVersion is the version of the artifact format this build understands.
//
// It is stored with every migration and checked before anything is planned. A
// migration written by a future version of this tool may use operations or
// semantics that do not exist here, and running it under today's interpretation
// would apply something other than what its author reviewed.
const FormatVersion = 1

// Migration is one reviewable unit of schema change.
type Migration struct {
	// ID is the migration's stable identity, such as "0004_add_user_status".
	ID string
	// DependsOn names the migrations that must be applied first. A migration
	// with no dependencies is a root.
	DependsOn []string
	// Operations are applied in order. The order is part of the artifact: two
	// migrations with the same operations in a different order are different
	// migrations.
	Operations []Operation
	// Atomic asks for the whole migration to run in one transaction, which is
	// the default. It is set to false for a migration containing an operation
	// PostgreSQL refuses inside a transaction block.
	Atomic bool
	// Format is the artifact format the migration was written against. Zero
	// means the current one, so a hand-written migration need not say.
	Format int
}

// format resolves the artifact version, treating zero as the current one.
func (m *Migration) format() int {
	if m.Format == 0 {
		return FormatVersion
	}
	return m.Format
}

// Validate checks everything about a migration that can be checked without
// looking at any other one.
func (m *Migration) Validate() error {
	switch {
	case m == nil:
		return errors.New("migration is nil")
	case strings.TrimSpace(m.ID) == "":
		return errors.New("migration has no ID")
	case len(m.Operations) == 0:
		return fmt.Errorf("migration %s has no operations", m.ID)
	}
	if v := m.format(); v > FormatVersion {
		return &ErrUnsupportedFormat{ID: m.ID, Format: v, Supported: FormatVersion}
	}
	if slices.Contains(m.DependsOn, m.ID) {
		return fmt.Errorf("migration %s depends on itself", m.ID)
	}
	for i, op := range m.Operations {
		if op == nil {
			return fmt.Errorf("migration %s has a nil operation at index %d", m.ID, i)
		}
	}
	return m.checkAtomicity()
}

// checkAtomicity refuses a migration whose mode PostgreSQL will not accept.
//
// It is checked while planning rather than left to the server, because
// discovering it halfway through means discovering it after earlier operations
// have already run — in a transaction PostgreSQL is about to abort, or worse,
// outside one.
func (m *Migration) checkAtomicity() error {
	if !m.Atomic {
		return nil
	}
	for _, op := range m.Operations {
		if !op.Transactional() {
			return &ErrAtomicity{ID: m.ID, Operation: op.Describe()}
		}
	}
	return nil
}

// Checksum is the migration's semantic fingerprint.
//
// It covers the format, the ID, the dependencies, the mode and every operation
// with its arguments, in order. It covers nothing about where the migration
// lives, when it was written, or how it was formatted — so reformatting a file
// or moving it does not make an applied migration look modified, and changing
// what it does always does.
func (m *Migration) Checksum() (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "orm-migration/%d\n", m.format())
	fmt.Fprintf(h, "id %s\n", m.ID)
	// Dependencies are sorted: naming the same two migrations in the other
	// order is the same statement about ordering.
	deps := slices.Clone(m.DependsOn)
	slices.Sort(deps)
	fmt.Fprintf(h, "depends %s\n", strings.Join(deps, ","))
	fmt.Fprintf(h, "atomic %t\n", m.Atomic)
	for i, op := range m.Operations {
		fmt.Fprintf(h, "op %d ", i)
		if err := fingerprintOp(h, op); err != nil {
			return "", fmt.Errorf("checksumming migration %s: %w", m.ID, err)
		}
		fmt.Fprint(h, "\n")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fingerprintMaterializedView writes a materialized view's semantic content.
//
// It covers the creation policy, because WITH DATA and WITH NO DATA are
// different statements that leave the database in different states, and a
// checksum that ignored the difference would call them one migration.
//
// It does not cover whether the view is currently populated. That is runtime
// state: it changes every time somebody refreshes, and a checksum that moved
// with it would report every applied migration as modified.
func fingerprintMaterializedView(w io.Writer, m schema.MaterializedView) error {
	fmt.Fprintf(w, "%s.%s definition=%s with_data=%t", m.Schema, m.Name, m.Definition.Identity(), m.WithData)
	for _, c := range m.Columns {
		fmt.Fprintf(w, " col=%s:%s", c.Name, c.Type)
	}
	for _, ix := range m.Indexes {
		fmt.Fprintf(w, " index=%s", ix.Name)
	}
	for _, d := range m.DependsOn {
		fmt.Fprintf(w, " depends=%s", d.Qualified())
	}
	return nil
}

// fingerprintView writes a view's semantic content.
//
// The definition goes in as its portable source identity rather than as its
// text, for the same reason the lock holds that: a checksum has to be the same
// on every machine and against every server, and it must move when the meaning
// moves. Reformatting moves the identity, which is the documented contract.
func fingerprintView(w io.Writer, v schema.View) error {
	fmt.Fprintf(w, "%s.%s definition=%s", v.Schema, v.Name, v.Definition.Identity())
	for _, c := range v.Columns {
		fmt.Fprintf(w, " col=%s:%s", c.Name, c.Type)
	}
	deps := make([]string, 0, len(v.DependsOn))
	for _, d := range v.DependsOn {
		deps = append(deps, d.Qualified())
	}
	slices.Sort(deps)
	for _, d := range deps {
		fmt.Fprintf(w, " depends=%s", d)
	}
	return nil
}

// fingerprintOp writes one operation's semantic content.
//
// The switch is exhaustive by construction: an operation type nobody added a
// case for produces an error rather than a checksum that ignores it, so a new
// operation cannot silently hash the same as an old one.
func fingerprintOp(w io.Writer, op Operation) error {
	switch o := op.(type) {
	case CreateView:
		fmt.Fprint(w, "create-view ")
		return fingerprintView(w, o.View)
	case ReplaceView:
		fmt.Fprint(w, "replace-view ")
		return fingerprintView(w, o.View)
	case DropView:
		fmt.Fprintf(w, "drop-view %s.%s", o.Schema, o.Name)
	case CreateMaterializedView:
		fmt.Fprint(w, "create-materialized-view ")
		return fingerprintMaterializedView(w, o.View)
	case DropMaterializedView:
		fmt.Fprintf(w, "drop-materialized-view %s.%s", o.Schema, o.Name)
	case CreateTable:
		fmt.Fprint(w, "create-table ")
		return fingerprintTable(w, o.Table)
	case DropTable:
		fmt.Fprintf(w, "drop-table %s.%s", o.Schema, o.Name)
	case RenameTable:
		fmt.Fprintf(w, "rename-table %s.%s->%s", o.Schema, o.From, o.To)
	case AddColumn:
		fmt.Fprintf(w, "add-column %s.%s ", o.Schema, o.Table)
		fingerprintColumn(w, o.Column)
	case DropColumn:
		fmt.Fprintf(w, "drop-column %s.%s.%s", o.Schema, o.Table, o.Name)
	case RenameColumn:
		fmt.Fprintf(w, "rename-column %s.%s.%s->%s", o.Schema, o.Table, o.From, o.To)
	case AlterColumn:
		fmt.Fprintf(w, "alter-column %s.%s.%s from=", o.Schema, o.Table, o.Name)
		fingerprintColumn(w, o.From)
		fmt.Fprint(w, " to=")
		fingerprintColumn(w, o.To)
		fmt.Fprintf(w, " using=%s", o.Using)
	case AddPrimaryKey:
		fmt.Fprintf(w, "add-pk %s.%s %s(%s)", o.Schema, o.Table, o.Key.Name, strings.Join(o.Key.Columns, ","))
	case DropPrimaryKey:
		fmt.Fprintf(w, "drop-pk %s.%s %s", o.Schema, o.Table, o.Name)
	case AddUnique:
		fmt.Fprintf(w, "add-unique %s.%s ", o.Schema, o.Table)
		fingerprintUnique(w, o.Unique)
	case DropUnique:
		fmt.Fprintf(w, "drop-unique %s.%s %s constraint=%t", o.Schema, o.Table, o.Name, o.Constraint)
	case AddForeignKey:
		fmt.Fprintf(w, "add-fk %s.%s ", o.Schema, o.Table)
		fingerprintForeignKey(w, o.ForeignKey)
	case DropForeignKey:
		fmt.Fprintf(w, "drop-fk %s.%s %s", o.Schema, o.Table, o.Name)
	case AddCheck:
		fmt.Fprintf(w, "add-check %s.%s %s (%s) notvalid=%t", o.Schema, o.Table, o.Check.Name, o.Check.Expression, o.Check.NotValid)
	case DropCheck:
		fmt.Fprintf(w, "drop-check %s.%s %s", o.Schema, o.Table, o.Name)
	case ValidateConstraint:
		fmt.Fprintf(w, "validate %s.%s %s", o.Schema, o.Table, o.Name)
	case CreateIndex:
		fmt.Fprintf(w, "create-index %s.%s ", o.Schema, o.Table)
		fingerprintIndex(w, o.Index)
	case DropIndex:
		fmt.Fprintf(w, "drop-index %s.%s %s concurrently=%t", o.Schema, o.Table, o.Name, o.Concurrently)
	case RenameIndex:
		fmt.Fprintf(w, "rename-index %s.%s %s->%s", o.Schema, o.Table, o.From, o.To)
	case CreateEnum:
		fmt.Fprintf(w, "create-enum %s (%s)", o.Enum.Qualified(), strings.Join(o.Enum.Labels, ","))
	case DropEnum:
		fmt.Fprintf(w, "drop-enum %s.%s", o.Schema, o.Name)
	case AddEnumValue:
		fmt.Fprintf(w, "add-enum-value %s.%s %q before=%s after=%s", o.Schema, o.Name, o.Value, o.Before, o.After)
	case RenameEnumValue:
		fmt.Fprintf(w, "rename-enum-value %s.%s %q->%q", o.Schema, o.Name, o.From, o.To)
	case RenameEnum:
		fmt.Fprintf(w, "rename-enum %s.%s->%s", o.Schema, o.From, o.To)
	case CreateExtension:
		fmt.Fprintf(w, "create-extension %s schema=%s", o.Extension.Name, o.Extension.Schema)
	case RawSQL:
		// The SQL itself is the artifact here: there is nothing structured to
		// hash, so changing a character changes the migration.
		fmt.Fprintf(w, "raw up=%q down=%q atomic=%t", o.Up, o.Down, o.Atomic)
	case StateOnly:
		fmt.Fprint(w, "state-only ")
		return fingerprintOp(w, o.Op)
	case RunFunc:
		// A Go function has no stable representation to hash — two builds of
		// the same source produce different addresses, and its behaviour is not
		// inspectable. The name is what identifies it, which is why it is
		// required.
		if strings.TrimSpace(o.Name) == "" {
			return errors.New("a RunFunc operation has no name, and a function cannot be checksummed without one")
		}
		fmt.Fprintf(w, "run %s reversible=%t", o.Name, o.Down != nil)
	case unsupportedEnumChange:
		fmt.Fprintf(w, "unsupported-enum-change %s %q", o.Enum.Qualified(), o.Label)
	default:
		return fmt.Errorf("operation %T has no checksum representation", op)
	}
	return nil
}

func fingerprintTable(w io.Writer, t schema.Table) error {
	fmt.Fprintf(w, "%s.%s cols=[", t.Schema, t.Name)
	for i, c := range t.Columns {
		if i > 0 {
			fmt.Fprint(w, " ")
		}
		fingerprintColumn(w, c)
	}
	fmt.Fprint(w, "]")
	if t.PrimaryKey != nil {
		fmt.Fprintf(w, " pk=%s(%s)", t.PrimaryKey.Name, strings.Join(t.PrimaryKey.Columns, ","))
	}
	// The collections are sorted by name so that the same table assembled in a
	// different order fingerprints the same. Their internal column orders are
	// not: those carry meaning.
	for _, u := range sortedByName(t.Uniques, func(u schema.Unique) string { return u.Name }) {
		fmt.Fprint(w, " ")
		fingerprintUnique(w, u)
	}
	for _, f := range sortedByName(t.ForeignKeys, func(f schema.ForeignKey) string { return f.Name }) {
		fmt.Fprint(w, " ")
		fingerprintForeignKey(w, f)
	}
	for _, c := range sortedByName(t.Checks, func(c schema.Check) string { return c.Name }) {
		fmt.Fprintf(w, " check=%s(%s)notvalid=%t", c.Name, c.Expression, c.NotValid)
	}
	for _, i := range sortedByName(t.Indexes, func(i schema.Index) string { return i.Name }) {
		fmt.Fprint(w, " ")
		fingerprintIndex(w, i)
	}
	return nil
}

func fingerprintColumn(w io.Writer, c schema.Column) {
	fmt.Fprintf(w, "%s:%s null=%t default=%s identity=%d generated=%s collate=%s",
		c.Name, c.Type, c.Nullable, c.Default, c.Identity, c.Generated, c.Collation)
}

func fingerprintUnique(w io.Writer, u schema.Unique) {
	fmt.Fprintf(w, "unique=%s(%s)constraint=%t where=%s nnd=%t",
		u.Name, strings.Join(u.Columns, ","), u.Constraint, u.Where, u.NullsNotDistinct)
}

func fingerprintForeignKey(w io.Writer, f schema.ForeignKey) {
	fmt.Fprintf(w, "fk=%s(%s)->%s.%s(%s) del=%s upd=%s deferrable=%t deferred=%t notvalid=%t",
		f.Name, strings.Join(f.Columns, ","), f.RefSchema, f.RefTable, strings.Join(f.RefColumns, ","),
		normalizeAction(f.OnDelete), normalizeAction(f.OnUpdate), f.Deferrable, f.InitiallyDeferred, f.NotValid)
}

func fingerprintIndex(w io.Writer, i schema.Index) {
	fmt.Fprintf(w, "index=%s unique=%t method=%s where=%s include=[%s] keys=[",
		i.Name, i.Unique, schema.IndexMethod(i.Method), i.Where, strings.Join(i.Include, ","))
	for n, c := range i.Columns {
		if n > 0 {
			fmt.Fprint(w, " ")
		}
		fmt.Fprintf(w, "%s%s dir=%d nulls=%d opclass=%s", c.Name, c.Expression, c.Direction, c.Nulls, c.OpClass)
	}
	// Whether the index was built concurrently is how it was created, not what
	// it is — but it is part of the migration's meaning, because it decides
	// whether the migration can run in a transaction.
	fmt.Fprintf(w, "] concurrently=%t", i.Concurrently)
}

// sortedByName returns a copy ordered by a name, for fingerprinting collections
// whose order carries no meaning.
func sortedByName[T any](items []T, name func(T) string) []T {
	out := slices.Clone(items)
	slices.SortFunc(out, func(a, b T) int { return cmp.Compare(name(a), name(b)) })
	return out
}

// ErrUnsupportedFormat reports a migration written against a newer artifact
// format than this build understands.
type ErrUnsupportedFormat struct {
	ID        string
	Format    int
	Supported int
}

func (e *ErrUnsupportedFormat) Error() string {
	return fmt.Sprintf("migration %s uses artifact format %d, and this build understands up to %d;"+
		" upgrade the tool rather than running it under an older interpretation", e.ID, e.Format, e.Supported)
}

// ErrAtomicity reports a migration marked atomic that contains an operation
// PostgreSQL refuses inside a transaction block.
type ErrAtomicity struct {
	ID        string
	Operation string
}

func (e *ErrAtomicity) Error() string {
	return fmt.Sprintf("migration %s is atomic but contains %q, which PostgreSQL cannot run inside a transaction block;"+
		" mark the migration non-atomic", e.ID, e.Operation)
}

// ErrMigrationModified reports an applied migration whose artifact changed.
//
// History is a record of what ran. A migration edited after it was applied
// describes something that never happened on any database that already has it,
// and applying the edit to a fresh database would produce a different schema
// from the one production is running.
type ErrMigrationModified struct {
	ID      string
	Applied string
	Current string
}

func (e *ErrMigrationModified) Error() string {
	return fmt.Sprintf("migration %s was modified after it was applied\n\n    applied:  %s\n    current:  %s\n\n"+
		"history cannot be rewritten; write a new migration instead", e.ID, short(e.Applied), short(e.Current))
}

func short(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// ErrGraph reports a migration set that does not form a usable history.
type ErrGraph struct {
	Reason string
	// IDs are the migrations involved, in a deterministic order.
	IDs []string
}

func (e *ErrGraph) Error() string {
	if len(e.IDs) == 0 {
		return "migration graph: " + e.Reason
	}
	return "migration graph: " + e.Reason + ": " + strings.Join(e.IDs, ", ")
}

// ErrUnknownTarget reports a target that names no migration.
type ErrUnknownTarget struct{ Target string }

func (e *ErrUnknownTarget) Error() string {
	return fmt.Sprintf("no migration named %q", e.Target)
}

// ErrHistory reports applied history that does not match the migrations on
// disk — a migration recorded as applied that no longer exists.
type ErrHistory struct {
	Reason string
	IDs    []string
}

func (e *ErrHistory) Error() string {
	if len(e.IDs) == 0 {
		return "migration history: " + e.Reason
	}
	return "migration history: " + e.Reason + ": " + strings.Join(e.IDs, ", ")
}
