package diag

import (
	"encoding/json"
	"fmt"
	"io"
)

// JSONVersion is the version of the JSON report shape. The field names below
// are public API and change only with this number.
const JSONVersion = 1

// jsonReport is the wire shape of a report.
type jsonReport struct {
	Version  int           `json:"version"`
	Summary  jsonSummary   `json:"summary"`
	Findings []jsonFinding `json:"findings"`
}

type jsonSummary struct {
	Findings int `json:"findings"`
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
}

type jsonFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Tier     string `json:"tier"`
	Message  string `json:"message"`
	Reason   string `json:"reason,omitempty"`
	Fix      string `json:"fix,omitempty"`

	Entity string `json:"entity,omitempty"`
	Field  string `json:"field,omitempty"`
	GoType string `json:"go_type,omitempty"`

	Table  string `json:"table,omitempty"`
	Column string `json:"column,omitempty"`
	PGType string `json:"pg_type,omitempty"`

	Relation   string `json:"relation,omitempty"`
	Constraint string `json:"constraint,omitempty"`

	Position *jsonPosition `json:"position,omitempty"`
}

type jsonPosition struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// RenderJSON writes the report as JSON.
//
// The document carries no timestamp and no absolute path, so two runs over the
// same inputs produce identical bytes and a report can be committed or diffed.
func RenderJSON(w io.Writer, r *Report) error {
	findings := r.Findings()
	doc := jsonReport{
		Version: JSONVersion,
		Summary: jsonSummary{
			Findings: len(findings),
			Errors:   r.Count(SeverityError),
			Warnings: r.Count(SeverityWarning),
		},
		Findings: make([]jsonFinding, 0, len(findings)),
	}
	for _, f := range findings {
		jf := jsonFinding{
			Code:       string(f.Code),
			Severity:   f.Severity.String(),
			Tier:       string(f.Tier()),
			Message:    f.Message,
			Reason:     f.Reason,
			Fix:        f.Fix,
			Entity:     f.Entity,
			Field:      f.Field,
			GoType:     f.GoType,
			Table:      f.Table,
			Column:     f.Column,
			PGType:     f.PGType,
			Relation:   f.Relation,
			Constraint: f.Constraint,
		}
		if !f.Pos.IsZero() {
			jf.Position = &jsonPosition{File: f.Pos.File, Line: f.Pos.Line, Column: f.Pos.Col}
		}
		doc.Findings = append(doc.Findings, jf)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("writing the JSON report: %w", err)
	}
	return nil
}
