package diag

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// RenderGitHub writes the report as GitHub Actions workflow commands, which the
// runner turns into annotations on the pull request.
//
// The format is one line per finding:
//
//	::error file=internal/domain/user.go,line=14,col=2,title=E004::message
//
// Findings with no source position are emitted without file properties, so they
// appear in the job log rather than being dropped.
func RenderGitHub(w io.Writer, r *Report) error {
	bw := bufio.NewWriter(w)
	for _, f := range r.Findings() {
		var props []string
		if !f.Pos.IsZero() {
			props = append(props, "file="+escapeProperty(f.Pos.File))
			if f.Pos.Line > 0 {
				props = append(props, fmt.Sprintf("line=%d", f.Pos.Line))
			}
			if f.Pos.Col > 0 {
				props = append(props, fmt.Sprintf("col=%d", f.Pos.Col))
			}
		}
		props = append(props, "title="+escapeProperty(string(f.Code)+" "+f.Code.Title()))

		fmt.Fprintf(bw, "::%s %s::%s\n", f.Severity, strings.Join(props, ","), escapeData(annotationBody(f)))
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("writing the GitHub report: %w", err)
	}
	return nil
}

// annotationBody is the message plus the reason and fix, which is all the
// context an annotation can carry.
func annotationBody(f Finding) string {
	var b strings.Builder
	b.WriteString(f.Message)
	if f.Reason != "" {
		b.WriteString("\n\n")
		b.WriteString(f.Reason)
	}
	if f.Fix != "" {
		b.WriteString("\n\nFix: ")
		b.WriteString(f.Fix)
	}
	return b.String()
}

// escapeData escapes the characters a workflow command message may not contain.
var dataEscaper = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")

func escapeData(s string) string { return dataEscaper.Replace(s) }

// escapeProperty additionally escapes the two characters that separate
// properties from each other and from their values.
var propertyEscaper = strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")

func escapeProperty(s string) string { return propertyEscaper.Replace(s) }
