package diag_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/AlexAli29/orm/gen/diag"
	"github.com/AlexAli29/orm/internal/gen/model"
)

func sample() *diag.Report {
	r := &diag.Report{}
	// Deliberately added out of order, to prove the renderers sort.
	r.Add(diag.Finding{
		Code:    diag.W003,
		Message: "column public.users.legacy is not mapped",
		Reason:  "no Go field maps to it",
		Fix:     "map it or drop the column",
		Entity:  "domain.User",
		Table:   "public.users",
		Column:  "legacy",
		PGType:  "text",
	})
	r.Add(diag.Finding{
		Code:    diag.E004,
		Message: "nullable column public.users.nickname cannot be represented by string",
		Reason:  "a plain string has no value for SQL NULL",
		Fix:     "use *string",
		Entity:  "domain.User",
		Field:   "Nickname",
		GoType:  "string",
		Table:   "public.users",
		Column:  "nickname",
		PGType:  "text",
		Pos:     model.Position{File: "internal/domain/user.go", Line: 14, Col: 2},
	})
	r.Add(diag.Finding{
		Code:       diag.E010,
		Message:    "has-one Profile is not backed by a unique constraint",
		Entity:     "domain.User",
		Field:      "Profile",
		Relation:   "Profile",
		Constraint: "profiles_user_id_fkey",
		Pos:        model.Position{File: "internal/domain/user.go", Line: 20, Col: 2},
	})
	return r
}

func TestReport_severityDefaultsFromTheRegister(t *testing.T) {
	r := sample()
	got := map[diag.Code]diag.Severity{}
	for _, f := range r.Findings() {
		got[f.Code] = f.Severity
	}
	if got[diag.W003] != diag.SeverityWarning {
		t.Errorf("W003 severity = %v, want warning", got[diag.W003])
	}
	if got[diag.E004] != diag.SeverityError {
		t.Errorf("E004 severity = %v, want error", got[diag.E004])
	}
}

func TestReport_explicitSeverityWins(t *testing.T) {
	r := &diag.Report{}
	r.Add(diag.Finding{Code: diag.W003, Severity: diag.SeverityError, Message: "raised by configuration"})
	if got := r.Findings()[0].Severity; got != diag.SeverityError {
		t.Errorf("severity = %v, want the explicitly set error", got)
	}
}

func TestReport_counts(t *testing.T) {
	r := sample()
	if got := r.Count(diag.SeverityError); got != 2 {
		t.Errorf("errors = %d, want 2", got)
	}
	if got := r.Count(diag.SeverityWarning); got != 1 {
		t.Errorf("warnings = %d, want 1", got)
	}
	if got := r.Len(); got != 3 {
		t.Errorf("Len = %d, want 3", got)
	}
}

func TestReport_failedThreshold(t *testing.T) {
	warnOnly := &diag.Report{}
	warnOnly.Add(diag.Finding{Code: diag.W003, Message: "m"})

	if warnOnly.Failed(diag.SeverityError) {
		t.Error("a warning-only report failed at the error threshold")
	}
	if !warnOnly.Failed(diag.SeverityWarning) {
		t.Error("a warning-only report passed at the warning threshold")
	}

	clean := &diag.Report{}
	if clean.Failed(diag.SeverityWarning) {
		t.Error("an empty report failed")
	}
}

func TestReport_sortIsTotalAndStable(t *testing.T) {
	first := renderAll(t, sample())
	for i := range 3 {
		if got := renderAll(t, sample()); got != first {
			t.Fatalf("render %d differed from the first", i+2)
		}
	}
}

func renderAll(t *testing.T, r *diag.Report) string {
	t.Helper()
	var b bytes.Buffer
	if err := diag.RenderText(&b, r); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	if err := diag.RenderJSON(&b, r); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if err := diag.RenderGitHub(&b, r); err != nil {
		t.Fatalf("RenderGitHub: %v", err)
	}
	return b.String()
}

func TestRenderText(t *testing.T) {
	var b bytes.Buffer
	if err := diag.RenderText(&b, sample()); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	const want = `internal/domain/user.go:14:2: error: E004: nullable column public.users.nickname cannot be represented by string
    Go:          domain.User.Nickname string
    PostgreSQL:  public.users.nickname text
    reason:      a plain string has no value for SQL NULL
    fix:         use *string

internal/domain/user.go:20:2: error: E010: has-one Profile is not backed by a unique constraint
    Go:          domain.User.Profile
    relation:    Profile
    constraint:  profiles_user_id_fkey

public.users.legacy: warning: W003: column public.users.legacy is not mapped
    Go:          domain.User
    PostgreSQL:  public.users.legacy text
    reason:      no Go field maps to it
    fix:         map it or drop the column

3 findings: 2 errors, 1 warning
`
	if got := b.String(); got != want {
		t.Errorf("text report:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderText_locationFallsBackToTheSchemaObject(t *testing.T) {
	// Not every finding has a source position: an unmapped column is a fact
	// about the schema, and pointing at the entity declaration is the closest
	// the source gets. The renderer degrades through table.column, table,
	// entity, and only then admits it has nothing.
	tests := []struct {
		name string
		f    diag.Finding
		want string
	}{
		{
			name: "position wins",
			f:    diag.Finding{Code: diag.E001, Message: "m", Entity: "d.U", Table: "public.users", Column: "c", Pos: model.Position{File: "a.go", Line: 1, Col: 2}},
			want: "a.go:1:2",
		},
		{
			name: "table and column",
			f:    diag.Finding{Code: diag.W003, Message: "m", Entity: "d.U", Table: "public.users", Column: "c"},
			want: "public.users.c",
		},
		{
			name: "table only",
			f:    diag.Finding{Code: diag.E011, Message: "m", Entity: "d.U", Table: "public.users"},
			want: "public.users",
		},
		{
			name: "entity only",
			f:    diag.Finding{Code: diag.E014, Message: "m", Entity: "d.U"},
			want: "d.U",
		},
		{
			name: "nothing at all",
			f:    diag.Finding{Code: diag.E014, Message: "m"},
			want: "<no location>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &diag.Report{}
			r.Add(tt.f)
			var b bytes.Buffer
			if err := diag.RenderText(&b, r); err != nil {
				t.Fatalf("RenderText: %v", err)
			}
			if got, _, _ := strings.Cut(b.String(), ": "); got != tt.want {
				t.Errorf("location = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderText_multiLineDetailStaysInItsColumn(t *testing.T) {
	r := &diag.Report{}
	r.Add(diag.Finding{
		Code:    diag.E013,
		Message: "m",
		Fix:     "add this to orm.yaml:\ntypes:\n  numeric:\n    go: pkg.Decimal",
		Pos:     model.Position{File: "a.go", Line: 1, Col: 1},
	})
	var b bytes.Buffer
	if err := diag.RenderText(&b, r); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	const want = `a.go:1:1: error: E013: m
    fix:         add this to orm.yaml:
                 types:
                   numeric:
                     go: pkg.Decimal

1 finding: 1 error, 0 warnings
`
	if got := b.String(); got != want {
		t.Errorf("text report:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderGitHub_multiLineBodyIsEscapedToOneLine(t *testing.T) {
	// A workflow command is line-oriented, so a fix spanning several lines has
	// to survive as %0A rather than breaking the annotation in half.
	r := &diag.Report{}
	r.Add(diag.Finding{
		Code:    diag.E013,
		Message: "m",
		Fix:     "line one\nline two",
		Pos:     model.Position{File: "a.go", Line: 1, Col: 1},
	})
	var b bytes.Buffer
	if err := diag.RenderGitHub(&b, r); err != nil {
		t.Fatalf("RenderGitHub: %v", err)
	}
	if n := strings.Count(strings.TrimSuffix(b.String(), "\n"), "\n"); n != 0 {
		t.Errorf("the annotation spans %d extra lines:\n%s", n, b.String())
	}
	if !strings.Contains(b.String(), "line one%0Aline two") {
		t.Errorf("annotation = %q", b.String())
	}
}

func TestRenderText_clean(t *testing.T) {
	var b bytes.Buffer
	if err := diag.RenderText(&b, &diag.Report{}); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	const want = "reconciliation clean: the entities and the schema agree\n"
	if got := b.String(); got != want {
		t.Errorf("clean report = %q, want %q", got, want)
	}
}

func TestRenderJSON(t *testing.T) {
	var b bytes.Buffer
	if err := diag.RenderJSON(&b, sample()); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}

	var doc struct {
		Version int `json:"version"`
		Summary struct {
			Findings int `json:"findings"`
			Errors   int `json:"errors"`
			Warnings int `json:"warnings"`
		} `json:"summary"`
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("the JSON report does not parse: %v\n%s", err, b.String())
	}
	if doc.Version != diag.JSONVersion {
		t.Errorf("version = %d, want %d", doc.Version, diag.JSONVersion)
	}
	if doc.Summary.Findings != 3 || doc.Summary.Errors != 2 || doc.Summary.Warnings != 1 {
		t.Errorf("summary = %+v", doc.Summary)
	}

	first := doc.Findings[0]
	for key, want := range map[string]any{
		"code":     "E004",
		"severity": "error",
		"tier":     "column",
		"entity":   "domain.User",
		"field":    "Nickname",
		"go_type":  "string",
		"table":    "public.users",
		"column":   "nickname",
		"pg_type":  "text",
	} {
		if first[key] != want {
			t.Errorf("findings[0].%s = %v, want %v", key, first[key], want)
		}
	}
	pos, ok := first["position"].(map[string]any)
	if !ok {
		t.Fatalf("findings[0].position = %v, want an object", first["position"])
	}
	if pos["file"] != "internal/domain/user.go" || pos["line"] != 14.0 || pos["column"] != 2.0 {
		t.Errorf("position = %v", pos)
	}

	// A finding without a position omits it entirely rather than emitting a
	// zero one that a consumer would have to special-case.
	last := doc.Findings[2]
	if _, ok := last["position"]; ok {
		t.Errorf("a positionless finding carries %v", last["position"])
	}
	if _, ok := last["field"]; ok {
		t.Errorf("an empty field was serialised: %v", last["field"])
	}
}

func TestRenderJSON_noAbsolutePathsOrTimestamps(t *testing.T) {
	var b bytes.Buffer
	if err := diag.RenderJSON(&b, sample()); err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	out := b.String()
	for _, bad := range []string{`"/`, "timestamp", "generated_at", "time"} {
		if strings.Contains(out, bad) {
			t.Errorf("the JSON report contains %q, which would break reproducibility", bad)
		}
	}
}

func TestRenderGitHub(t *testing.T) {
	var b bytes.Buffer
	if err := diag.RenderGitHub(&b, sample()); err != nil {
		t.Fatalf("RenderGitHub: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d annotations, want 3:\n%s", len(lines), b.String())
	}

	const wantFirst = "::error file=internal/domain/user.go,line=14,col=2," +
		"title=E004 nullable PostgreSQL column cannot be represented by the Go field" +
		"::nullable column public.users.nickname cannot be represented by string" +
		"%0A%0Aa plain string has no value for SQL NULL%0A%0AFix: use *string"
	if lines[0] != wantFirst {
		t.Errorf("annotation:\n%s\nwant:\n%s", lines[0], wantFirst)
	}

	if !strings.HasPrefix(lines[2], "::warning title=W003 ") {
		t.Errorf("a positionless finding rendered as %q, want no file properties", lines[2])
	}
}

func TestRenderGitHub_escaping(t *testing.T) {
	r := &diag.Report{}
	r.Add(diag.Finding{
		Code:    diag.E001,
		Message: "a message with, a comma: a colon and 100% of a newline\nhere",
		Pos:     model.Position{File: "a,b:c.go", Line: 1, Col: 1},
	})
	var b bytes.Buffer
	if err := diag.RenderGitHub(&b, r); err != nil {
		t.Fatalf("RenderGitHub: %v", err)
	}
	got := b.String()
	if !strings.Contains(got, "file=a%2Cb%3Ac.go") {
		t.Errorf("property separators were not escaped: %s", got)
	}
	if !strings.Contains(got, "100%25 of a newline%0Ahere") {
		t.Errorf("message data was not escaped: %s", got)
	}
}

func TestCodes_registerIsCompleteAndFrozen(t *testing.T) {
	want := []diag.Code{
		diag.E001, diag.E002, diag.W003, diag.E004, diag.W005, diag.E006,
		diag.E007, diag.E008, diag.E009, diag.E010, diag.E011, diag.E012,
		diag.E013, diag.E014, diag.W015, diag.E016, diag.E017, diag.E018,
		diag.E019, diag.E020, diag.E021, diag.E022, diag.E023, diag.E024,
		diag.E025, diag.E026, diag.E027, diag.E028, diag.E029,
		diag.E030, diag.W031, diag.E032,
		diag.E033, diag.E034, diag.W035, diag.E036,
	}
	got := diag.Codes()
	if len(got) != len(want) {
		t.Fatalf("the register holds %d codes, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("code %d = %s, want %s", i, got[i], want[i])
		}
		if got[i].Title() == "" {
			t.Errorf("%s has no title", got[i])
		}
		if !got[i].Known() {
			t.Errorf("%s is not registered", got[i])
		}
	}
	// The W prefix is not decorative: it names the codes whose default
	// severity is a warning.
	for _, c := range got {
		wantWarning := strings.HasPrefix(string(c), "W")
		if isWarning := c.Severity() == diag.SeverityWarning; isWarning != wantWarning {
			t.Errorf("%s severity = %v, which disagrees with its prefix", c, c.Severity())
		}
	}
}

func TestParseSeverity(t *testing.T) {
	for in, want := range map[string]diag.Severity{
		"warning": diag.SeverityWarning,
		"error":   diag.SeverityError,
	} {
		got, err := diag.ParseSeverity(in)
		if err != nil {
			t.Errorf("ParseSeverity(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSeverity(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := diag.ParseSeverity("info"); err == nil {
		t.Error("ParseSeverity(\"info\") succeeded, want an error")
	}
}
