package diag

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// RenderText writes the report for a human reader.
//
// Every finding names the location, the severity, the code, and then both sides
// of the disagreement, because a bare code is not a diagnostic. The layout is
// the familiar file:line:col: severity: message so that editors and greps that
// already understand compiler output understand this too.
func RenderText(w io.Writer, r *Report) error {
	bw := bufio.NewWriter(w)
	findings := r.Findings()

	for _, f := range findings {
		fmt.Fprintf(bw, "%s: %s: %s: %s\n", location(f), f.Severity, f.Code, f.Message)
		for _, d := range details(f) {
			// A detail may run to several lines — a suggested YAML fragment,
			// say. Continuation lines are indented under the value so the
			// labels stay a readable column.
			for i, line := range strings.Split(d.value, "\n") {
				if i == 0 {
					fmt.Fprintf(bw, "    %-12s %s\n", d.label+":", line)
					continue
				}
				fmt.Fprintf(bw, "    %-12s %s\n", "", line)
			}
		}
		bw.WriteByte('\n')
	}

	switch {
	case len(findings) == 0:
		fmt.Fprintln(bw, "reconciliation clean: the entities and the schema agree")
	default:
		fmt.Fprintf(bw, "%s: %s, %s\n",
			plural(len(findings), "finding", "findings"),
			plural(r.Count(SeverityError), "error", "errors"),
			plural(r.Count(SeverityWarning), "warning", "warnings"))
	}

	if err := bw.Flush(); err != nil {
		return fmt.Errorf("writing the text report: %w", err)
	}
	return nil
}

// location is the most specific place the finding can point at: a source
// position when one exists, otherwise the schema object it is about.
func location(f Finding) string {
	if !f.Pos.IsZero() {
		return f.Pos.String()
	}
	switch {
	case f.Table != "" && f.Column != "":
		return f.Table + "." + f.Column
	case f.Table != "":
		return f.Table
	case f.Entity != "":
		return f.Entity
	default:
		return "<no location>"
	}
}

type detail struct{ label, value string }

func details(f Finding) []detail {
	var ds []detail
	add := func(label, value string) {
		if value != "" {
			ds = append(ds, detail{label, value})
		}
	}
	switch {
	case f.Field != "" && f.Entity != "":
		add("Go", f.Entity+"."+f.Field+describeType(f.GoType))
	case f.Entity != "":
		add("Go", f.Entity+describeType(f.GoType))
	}
	switch {
	case f.Column != "" && f.Table != "":
		add("PostgreSQL", f.Table+"."+f.Column+describeType(f.PGType))
	case f.Table != "":
		add("PostgreSQL", f.Table+describeType(f.PGType))
	}
	add("relation", f.Relation)
	add("constraint", f.Constraint)
	add("reason", f.Reason)
	add("fix", f.Fix)
	return ds
}

func describeType(t string) string {
	if t == "" {
		return ""
	}
	return " " + t
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
