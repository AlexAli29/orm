package lock

import (
	"fmt"
	"slices"
	"strings"

	"github.com/AlexAli29/orm/internal/gen/schema"
)

// Views in the lock.
//
// The lock answers one question: what did the project commit to? It does not
// answer what any server made of it, and the difference is the reason this file
// is careful about what it writes.
//
//	orm.lock            what the project declares — portable, committed
//	orm check           what a database holds — per server, never committed
//	orm_schema_views    what one server made of what was applied
//
// A deparsed definition is not portable: PostgreSQL 16 stopped qualifying
// columns it does not need to, so one unchanged view reads differently on 15
// and 16. Writing that here would make an identical project produce different
// bytes depending on which server somebody happened to point at, and the churn
// would arrive as a diff nobody can explain. What goes in instead is the
// portable fingerprint of the declaration itself, which never touches a server.
//
// Nothing here writes an OID, a server version, a population state or a path.

// writeViews renders the view half of a desired schema.
//
// It is additive: a project with no views writes nothing, so a lock file from
// before views existed reads and re-renders identically. That is what keeps old
// projects from reporting stale generated code because the tool learned a new
// feature they do not use.
func writeViews(b *strings.Builder, s *schema.Schema) {
	if s == nil {
		return
	}
	views := slices.Clone(s.Views)
	schema.SortViews(views)
	for _, v := range views {
		fmt.Fprintf(b, "view %s\n", v.Qualified())
		writeDefinition(b, v.Definition)
		writeRelColumns(b, v.Columns)
		writeDeps(b, v.DependsOn)
		for _, o := range v.Options {
			fmt.Fprintf(b, "  option %s=%s\n", o.Name, o.Value)
		}
	}

	mats := slices.Clone(s.MaterializedViews)
	schema.SortMaterializedViews(mats)
	for _, m := range mats {
		fmt.Fprintf(b, "materialized-view %s\n", m.Qualified())
		writeDefinition(b, m.Definition)
		writeRelColumns(b, m.Columns)
		writeDeps(b, m.DependsOn)
		// Creation policy, which changes what CREATE does. Not the current
		// population, which changes every time anybody refreshes and is state
		// the server owns.
		if m.WithData {
			b.WriteString("  create with-data\n")
		} else {
			b.WriteString("  create with-no-data\n")
		}
		indexes := slices.Clone(m.Indexes)
		slices.SortFunc(indexes, func(a, c schema.Index) int { return strings.Compare(a.Name, c.Name) })
		for _, ix := range indexes {
			writeIndex(b, ix)
		}
	}
}

// writeDefinition writes the portable identity of a definition, and the mode
// that produced it.
//
// The mode is here so that a typed definition can arrive later without the
// format changing shape: it will supply a SourceIdentity through the same
// field, and the mode is what says which kind of declaration it came from.
// Adding it now costs one line and avoids a format break for a feature already
// planned.
func writeDefinition(b *strings.Builder, d schema.Definition) {
	mode := "raw"
	if d.SQL == "" {
		mode = "none"
	}
	fmt.Fprintf(b, "  definition %s %s\n", mode, d.Identity())
}

func writeRelColumns(b *strings.Builder, cols []schema.Column) {
	// Column order is the relation's output order, which is observable result
	// shape. It is never sorted.
	for _, c := range cols {
		null := "not-null"
		if c.Nullable {
			null = "null"
		}
		fmt.Fprintf(b, "  column %s %s %s\n", c.Name, c.Type, null)
	}
}

func writeDeps(b *strings.Builder, refs []schema.RelationRef) {
	sorted := slices.Clone(refs)
	schema.SortRefs(sorted)
	// Sorted, so that reordering //orm:depends-on directives without changing
	// the set leaves the lock alone: the order somebody wrote them in is not
	// part of what the project committed to.
	for _, d := range sorted {
		fmt.Fprintf(b, "  depends-on %s\n", d.Qualified())
	}
}

func writeIndex(b *strings.Builder, ix schema.Index) {
	fmt.Fprintf(b, "  index %s", ix.Name)
	if ix.Unique {
		b.WriteString(" unique")
	}
	if ix.Method != "" {
		fmt.Fprintf(b, " using %s", ix.Method)
	}
	for _, c := range ix.Columns {
		if c.Expression != "" {
			fmt.Fprintf(b, " (%s)", c.Expression)
			continue
		}
		fmt.Fprintf(b, " %s", c.Name)
		if c.OpClass != "" {
			fmt.Fprintf(b, ":%s", c.OpClass)
		}
		if c.Direction != 0 {
			fmt.Fprintf(b, " %v", c.Direction)
		}
		if c.Nulls != 0 {
			fmt.Fprintf(b, " nulls-%v", c.Nulls)
		}
	}
	if len(ix.Include) > 0 {
		fmt.Fprintf(b, " include %s", strings.Join(ix.Include, ","))
	}
	if ix.Where != "" {
		fmt.Fprintf(b, " where %s", ix.Where)
	}
	b.WriteString("\n")
}

// FingerprintSchema reduces a desired schema's views to a portable digest
// fragment, for a caller that has a schema rather than a mapping.
//
// It exists so that the view half of the lock can be tested and reasoned about
// on its own: given one desired schema, this is the text, and everything in it
// is something the project declared.
func FingerprintSchema(s *schema.Schema) string {
	var b strings.Builder
	writeViews(&b, s)
	return b.String()
}
