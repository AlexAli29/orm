package migrate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Summaries and warnings.
//
// A migration is reviewed by a person before it runs, and what that person
// needs is not the operation list in declaration order — it is what happens to
// each object, and what about it is dangerous. So a summary groups by object
// and a warning carries a stable code, because a code is what a team writes
// down in a runbook and what CI greps for.

// Change is one operation, rendered for a summary.
type Change struct {
	// Group is the object the change belongs to, such as a table name. Changes
	// with the same group are printed together, in the order they first appear.
	Group string
	// Marker is +, - or ~ for an addition, a removal and an alteration.
	Marker string
	// Lines is the change itself: the first line, and any continuation lines
	// that carry detail too wide to fit on it.
	Lines []string
}

// Summarize groups operations by the object they change.
func Summarize(ops []Operation) []Change {
	out := make([]Change, 0, len(ops))
	for _, op := range ops {
		out = append(out, summarizeOp(op))
	}
	return out
}

// RenderSummary writes a grouped summary, indented by prefix.
func RenderSummary(ops []Operation, prefix string) string {
	var b strings.Builder
	var group string
	first := true
	for _, c := range Summarize(ops) {
		if first || c.Group != group {
			if !first {
				b.WriteString("\n")
			}
			if c.Group != "" {
				fmt.Fprintf(&b, "%s%s\n", prefix, c.Group)
			}
			group, first = c.Group, false
		}
		indent := prefix
		if c.Group != "" {
			indent += "  "
		}
		for i, line := range c.Lines {
			if i == 0 {
				fmt.Fprintf(&b, "%s%s %s\n", indent, c.Marker, line)
				continue
			}
			fmt.Fprintf(&b, "%s    %s\n", indent, line)
		}
	}
	return b.String()
}

// summarizeOp renders one operation.
//
// The switch is over the same set the checksum and the artifact cover; an
// operation with no case still produces something readable rather than nothing,
// because a summary that silently omitted an operation would be worse than one
// that described it plainly.
func summarizeOp(op Operation) Change {
	switch o := op.(type) {
	case CreateTable:
		return Change{Group: schema.ShortName(o.Table.Schema, o.Table.Name), Marker: "+", Lines: createTableLines(o.Table)}
	case DropTable:
		return Change{Group: schema.ShortName(o.Schema, o.Name), Marker: "-", Lines: []string{"drop table"}}
	case RenameTable:
		return Change{Group: schema.ShortName(o.Schema, o.From), Marker: "~", Lines: []string{"rename table -> " + o.To}}
	case AddColumn:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "+", Lines: []string{schema.FormatColumn(o.Column)}}
	case DropColumn:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "-", Lines: []string{"drop column " + o.Name}}
	case RenameColumn:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "~", Lines: []string{"rename " + o.From + " -> " + o.To}}
	case AlterColumn:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "~", Lines: []string{alterColumnSummary(o)}}
	case AddPrimaryKey:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "+",
			Lines: []string{schema.FormatPrimaryKey(o.Key)}}
	case DropPrimaryKey:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "-", Lines: []string{"primary key " + o.Name}}
	case AddUnique:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "+", Lines: schema.FormatUnique(o.Unique)}
	case DropUnique:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "-", Lines: []string{"unique " + o.Name}}
	case AddForeignKey:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "+", Lines: schema.FormatForeignKey(o.ForeignKey)}
	case DropForeignKey:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "-", Lines: []string{"foreign key " + o.Name}}
	case AddCheck:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "+", Lines: schema.FormatCheck(o.Check)}
	case DropCheck:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "-", Lines: []string{"check " + o.Name}}
	case ValidateConstraint:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "~", Lines: []string{"validate constraint " + o.Name}}
	case CreateIndex:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "+", Lines: schema.FormatIndex(o.Index)}
	case DropIndex:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "-", Lines: []string{"index " + o.Name}}
	case RenameIndex:
		return Change{Group: schema.ShortName(o.Schema, o.Table), Marker: "~", Lines: []string{"rename index " + o.From + " -> " + o.To}}
	case CreateEnum:
		return Change{Group: "enum " + o.Enum.Qualified(), Marker: "+",
			Lines: []string{"create (" + strings.Join(o.Enum.Labels, ", ") + ")"}}
	case DropEnum:
		return Change{Group: "enum " + o.Schema + "." + o.Name, Marker: "-", Lines: []string{"drop"}}
	case AddEnumValue:
		line := "label " + quoteLabel(o.Value)
		switch {
		case o.Before != "":
			line += " before " + quoteLabel(o.Before)
		case o.After != "":
			line += " after " + quoteLabel(o.After)
		}
		return Change{Group: "enum " + o.Schema + "." + o.Name, Marker: "+", Lines: []string{line}}
	case RenameEnumValue:
		return Change{Group: "enum " + o.Schema + "." + o.Name, Marker: "~",
			Lines: []string{"rename label " + quoteLabel(o.From) + " -> " + quoteLabel(o.To)}}
	case RenameEnum:
		return Change{Group: "enum " + o.Schema + "." + o.From, Marker: "~", Lines: []string{"rename -> " + o.To}}
	case CreateExtension:
		return Change{Group: "extensions", Marker: "+", Lines: []string{o.Extension.Name}}
	case RawSQL:
		desc := o.Description
		if desc == "" {
			desc = "raw SQL"
		}
		return Change{Group: "raw", Marker: "~", Lines: append([]string{desc}, sqlLines(o.Up)...)}
	case StateOnly:
		inner := summarizeOp(o.Op)
		inner.Lines = append(slices.Clone(inner.Lines), "(migration state only; no SQL runs)")
		return inner
	case RunFunc:
		return Change{Group: "data", Marker: "~", Lines: []string{"run " + o.Name}}
	default:
		return Change{Marker: "~", Lines: []string{op.Describe()}}
	}
}

func createTableLines(t schema.Table) []string {
	return append([]string{"create table"}, schema.FormatTable(t)...)
}

func alterColumnSummary(o AlterColumn) string {
	// Describe already says exactly what changed; the table name is the group
	// here, so only the column and the changes belong on the line.
	return strings.TrimPrefix(o.Describe(), fmt.Sprintf("alter column %s.%s.", o.Schema, o.Table))
}

func sqlLines(sql string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(sql), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func quoteLabel(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Warning is an advisory finding about a migration.
//
// Warnings are never errors. Safety classification is conservative by design —
// it describes what an operation does to a table, not what that means for a
// particular deployment — and a tool that refused to run a migration because
// dropping a column is destructive would be a tool nobody could use to drop a
// column.
type Warning struct {
	// Code is stable. It is what a runbook cites and what CI matches on, so it
	// means the same thing for as long as the code exists.
	Code string
	// Message says what the risk is, in one line.
	Message string
	// Operation is the operation that raised it, empty for a warning about the
	// migration as a whole.
	Operation string
}

func (w Warning) String() string {
	if w.Operation == "" {
		return w.Code + " " + w.Message
	}
	return w.Code + " " + w.Message + ": " + w.Operation
}

// The warning codes. They are public API.
const (
	// WIndexNotConcurrent is a plain CREATE INDEX.
	WIndexNotConcurrent = "W201"
	// WDropColumn discards a column's data.
	WDropColumn = "W202"
	// WDropTable discards a table.
	WDropTable = "W203"
	// WSetNotNull rejects the change unless every row already has a value.
	WSetNotNull = "W204"
	// WTypeRewrite may rewrite the whole table.
	WTypeRewrite = "W205"
	// WDropConstraint discards what a constraint or index guaranteed.
	WDropConstraint = "W206"
	// WNonAtomic is a migration that cannot run in a transaction.
	WNonAtomic = "W207"
	// WNotNullNoDefault is a new NOT NULL column with nothing to put in
	// existing rows.
	WNotNullNoDefault = "W208"
	// WValidatesRows is a constraint checked against every existing row.
	WValidatesRows = "W209"
	// WIrreversible is an operation with no computable inverse.
	WIrreversible = "W210"
	// WRawSQL is SQL the engine executes without modelling.
	WRawSQL = "W211"

	// WSpatialReinterpret warns about a spatial type change PostgreSQL accepts
	// silently and that changes what every stored coordinate means.
	WSpatialReinterpret = "W212"
)

// Warnings reports what is worth knowing before a set of operations runs.
//
// The order is the order of the operations, so a warning can be read next to
// the change that raised it. Duplicates are kept: two dropped columns are two
// warnings, because a reader counting them is counting real risks.
func Warnings(ops []Operation) []Warning {
	var out []Warning
	for _, op := range ops {
		out = append(out, warningsFor(op)...)
	}
	return out
}

// PlanWarnings reports the warnings for a whole plan, including the ones about
// how a migration runs rather than what it does.
func PlanWarnings(p Plan) []Warning {
	var out []Warning
	for _, s := range p.Steps {
		if !s.Atomic {
			out = append(out, Warning{
				Code:      WNonAtomic,
				Message:   "runs outside a transaction: an operation that fails part way through leaves the ones before it applied",
				Operation: s.Migration.ID,
			})
		}
		out = append(out, Warnings(s.Operations)...)
	}
	return out
}

func warningsFor(op Operation) []Warning {
	warn := func(code, message string) []Warning {
		return []Warning{{Code: code, Message: message, Operation: op.Describe()}}
	}
	switch o := op.(type) {
	case CreateIndex:
		if !o.Index.Concurrently {
			return warn(WIndexNotConcurrent, "CREATE INDEX without CONCURRENTLY blocks writes to the table until the build finishes")
		}
	case CreateTable:
		var out []Warning
		for _, i := range o.Table.Indexes {
			if !i.Concurrently {
				// An index built with the table it belongs to blocks nothing:
				// the table is empty and nothing else can see it yet.
				continue
			}
			out = append(out, Warning{Code: WIndexNotConcurrent,
				Message: "an index on a table being created cannot be built concurrently", Operation: op.Describe()})
		}
		return out
	case DropColumn:
		return warn(WDropColumn, "dropping a column discards its data, and reversing the migration cannot bring it back")
	case DropTable:
		return warn(WDropTable, "dropping a table discards the table and everything in it")
	case DropIndex, DropUnique, DropCheck, DropForeignKey, DropPrimaryKey:
		return warn(WDropConstraint, "dropping a constraint or index discards what it guaranteed about existing rows")
	case DropEnum:
		return warn(WIrreversible, "dropping a type cannot be reversed without knowing what its labels were")
	case AddColumn:
		if o.Safety() == RequiresData {
			return warn(WNotNullNoDefault,
				"a NOT NULL column with no default cannot be added to a table that already has rows; give it a default, or add it nullable and backfill first")
		}
	case AlterColumn:
		var out []Warning
		if o.From.Nullable && !o.To.Nullable {
			out = append(out, Warning{Code: WSetNotNull,
				Message:   "SET NOT NULL scans every row and fails if any of them is NULL; backfill first",
				Operation: op.Describe()})
		}
		if o.From.Type.Canonical() != o.To.Type.Canonical() {
			out = append(out, Warning{Code: WTypeRewrite,
				Message:   "changing a column's type may rewrite the whole table under an exclusive lock",
				Operation: op.Describe()})
		}
		if msg := spatialReinterpretation(o.From.Type, o.To.Type); msg != "" {
			out = append(out, Warning{Code: WSpatialReinterpret, Message: msg, Operation: op.Describe()})
		}
		return out
	case AddForeignKey:
		if !o.ForeignKey.NotValid {
			return warn(WValidatesRows,
				"the constraint is checked against every existing row while the table is locked; add it NOT VALID and VALIDATE CONSTRAINT afterwards to avoid that")
		}
	case AddCheck:
		if !o.Check.NotValid {
			return warn(WValidatesRows,
				"the constraint is checked against every existing row while the table is locked; add it NOT VALID and VALIDATE CONSTRAINT afterwards to avoid that")
		}
	case RawSQL:
		return warn(WRawSQL,
			"raw SQL runs as written, and the migration state changes only as much as the operations around it say")
	case unsupportedEnumChange:
		return warn(WIrreversible, "PostgreSQL cannot remove an enum label")
	}
	return nil
}

// RenderWarnings writes a warning block, or nothing when there is none.
func RenderWarnings(ws []Warning, prefix string) string {
	if len(ws) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%sWarnings\n", prefix)
	for _, w := range ws {
		fmt.Fprintf(&b, "%s  %s %s\n", prefix, w.Code, w.Message)
		if w.Operation != "" {
			fmt.Fprintf(&b, "%s      %s\n", prefix, w.Operation)
		}
	}
	return b.String()
}

// spatialReinterpretation reports a spatial type change that PostgreSQL will
// accept without complaint and that changes what the stored numbers mean.
//
// Most dangerous spatial changes stop themselves. Changing the SRID fails,
// because PostGIS checks every row against the new type modifier; changing the
// shape fails, with a hint asking for a USING clause. The engine needs to say
// nothing extra about either — a migration that will not run is a migration
// somebody will look at.
//
// Casting geometry to geography is the exception, and it is one-directional.
// PostGIS defines that cast as an assignment cast, so the ALTER succeeds, no row
// is rejected, and not one coordinate changes. What changes is the surface they
// are on: the same pair of numbers that was a point in a plane is now a point on
// a spheroid, and every distance, length and area in the application silently
// changes units and answers.
//
// The other direction needs an explicit USING and so stops itself. It is still
// warned about, because a migration whose USING somebody supplied is a migration
// somebody should have thought about — but the message says which of the two it
// is, since "PostgreSQL will not catch this" is only true one way round.
func spatialReinterpretation(from, to schema.Type) string {
	a, aok := spatialFamily(from)
	b, bok := spatialFamily(to)
	if !aok || !bok || a == b {
		return ""
	}
	if a == "geometry" {
		return "casting geometry to geography keeps every coordinate and changes what they mean:" +
			" measurements over the column become metres on the spheroid instead of units in the plane." +
			" PostgreSQL accepts this without rejecting a row, so nothing else will catch it —" +
			" transform the coordinates first if that is what was meant"
	}
	return "casting geography to geometry keeps every coordinate and changes what they mean:" +
		" measurements over the column become units in the coordinate system instead of metres." +
		" PostgreSQL refuses this without an explicit USING clause, so the migration will not run" +
		" until somebody writes down what the conversion should be"
}

// spatialFamily reports which PostGIS storage family a type is, ignoring the
// modifier: geometry(Point,4326) and geometry are both geometry.
func spatialFamily(t schema.Type) (string, bool) {
	name := t.Canonical().Name
	if i := strings.IndexByte(name, '('); i >= 0 {
		name = name[:i]
	}
	switch name {
	case "geometry", "geography":
		return name, true
	default:
		return "", false
	}
}
