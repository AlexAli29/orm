package diag

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"

	"github.com/AlexAli29/orm/internal/gen/model"
)

// Severity is how much a finding matters.
type Severity uint8

// Severities, ordered so that a threshold comparison is a plain >=. The zero
// value is deliberately not a severity, so that a finding built without one can
// take its default from the code register instead of silently becoming a
// warning.
const (
	severityUnset Severity = iota
	// SeverityWarning is a disagreement the author may have meant.
	SeverityWarning
	// SeverityError is a disagreement that makes the mapping wrong.
	SeverityError
)

// String returns the severity's name, which is also its JSON encoding.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarning:
		return "warning"
	default:
		return "unset"
	}
}

// ParseSeverity parses a --fail-on threshold.
func ParseSeverity(s string) (Severity, error) {
	switch s {
	case "warning":
		return SeverityWarning, nil
	case "error":
		return SeverityError, nil
	default:
		return 0, fmt.Errorf("invalid severity %q, want warning or error", s)
	}
}

// Tier is the reconciliation stage a finding comes from. It exists so that
// consumers can group findings the way the checks are layered, and so that a
// reader can tell an entity-level problem from a column-level one at a glance.
type Tier string

// Reconciliation tiers.
const (
	TierEntity   Tier = "entity"
	TierColumn   Tier = "column"
	TierEnum     Tier = "enum"
	TierRelation Tier = "relation"
	TierTag      Tier = "tag"
)

// Code identifies a finding. Codes are public API: E004 means one thing for as
// long as the project exists. They are never renumbered and never reused, so
// retiring a check leaves a permanent hole in the sequence.
type Code string

// The frozen finding codes.
const (
	E001 Code = "E001" // Go field has no matching column
	E002 Code = "E002" // unmapped NOT NULL column with no default or generated value
	W003 Code = "W003" // PostgreSQL column exists but is unmapped
	E004 Code = "E004" // nullable DB column cannot be represented by the Go field
	W005 Code = "W005" // pointer Go field mapped to a NOT NULL column
	E006 Code = "E006" // incompatible Go and PostgreSQL types
	E007 Code = "E007" // enum labels differ
	E008 Code = "E008" // relation has no candidate foreign key
	E009 Code = "E009" // relation foreign key is ambiguous
	E010 Code = "E010" // remote-FK to-one relation is not guaranteed unique
	E011 Code = "E011" // table has no primary key
	E012 Code = "E012" // unsupported PostgreSQL type
	E013 Code = "E013" // type has no configured Go mapping
	E014 Code = "E014" // generated identifier collision
	W015 Code = "W015" // timestamp without time zone
	E016 Code = "E016" // table does not exist
	E017 Code = "E017" // multiple Go entities map to one PostgreSQL table
	E018 Code = "E018" // multiple Go fields map to one PostgreSQL column
	E019 Code = "E019" // many-cardinality relation has a local foreign key
	E020 Code = "E020" // relation target is not a mapped entity
	E021 Code = "E021" // invalid or unknown ORM tag
	E022 Code = "E022" // view entity unsupported
	E023 Code = "E023" // primary key column has no mapped Go field
	E024 Code = "E024" // relation crosses a package boundary
	E025 Code = "E025" // a managed view was declared with no definition
	E026 Code = "E026" // one relation name was declared as two different kinds
	E027 Code = "E027" // a view dependency names no declared relation
	E028 Code = "E028" // a view depends on itself
	E029 Code = "E029" // view dependencies form a cycle
	E030 Code = "E030" // the relation exists but is of another kind
	W031 Code = "W031" // the definition applied to this database is unknown
	E032 Code = "E032" // the definition in the database is not the one applied
	E033 Code = "E033" // the relation reads something no declaration mentions
	E034 Code = "E034" // a declared dependency is not one the relation has
	W035 Code = "W035" // the relation carries metadata this schema cannot express
	E036 Code = "E036" // a materialized view's indexes differ from the declaration
)

// codeInfo is the frozen description of a code.
type codeInfo struct {
	Severity Severity
	Tier     Tier
	Title    string
}

// registry is the finding code register. Adding a row is a public API change;
// changing one is a breaking change.
var registry = map[Code]codeInfo{
	E001: {SeverityError, TierColumn, "Go field has no matching column"},
	E002: {SeverityError, TierColumn, "unmapped NOT NULL column with no default or generated value"},
	W003: {SeverityWarning, TierColumn, "PostgreSQL column exists but is unmapped"},
	E004: {SeverityError, TierColumn, "nullable PostgreSQL column cannot be represented by the Go field"},
	W005: {SeverityWarning, TierColumn, "pointer Go field mapped to a NOT NULL column"},
	E006: {SeverityError, TierColumn, "incompatible Go and PostgreSQL types"},
	E007: {SeverityError, TierEnum, "enum labels differ"},
	E008: {SeverityError, TierRelation, "relation has no candidate foreign key"},
	E009: {SeverityError, TierRelation, "relation foreign key is ambiguous"},
	E010: {SeverityError, TierRelation, "remote-FK to-one relation is not guaranteed unique"},
	E011: {SeverityError, TierEntity, "table has no primary key"},
	E012: {SeverityError, TierColumn, "unsupported PostgreSQL type"},
	E013: {SeverityError, TierColumn, "type has no configured Go mapping"},
	E014: {SeverityError, TierEntity, "generated identifier collision"},
	W015: {SeverityWarning, TierColumn, "timestamp without time zone"},
	E016: {SeverityError, TierEntity, "table does not exist"},
	E017: {SeverityError, TierEntity, "multiple Go entities map to one PostgreSQL table"},
	E018: {SeverityError, TierColumn, "multiple Go fields map to one PostgreSQL column"},
	E019: {SeverityError, TierRelation, "many-cardinality relation has a local foreign key"},
	E020: {SeverityError, TierRelation, "relation target is not a mapped entity"},
	E021: {SeverityError, TierTag, "invalid or unknown ORM tag"},
	E022: {SeverityError, TierEntity, "view entity unsupported"},
	E023: {SeverityError, TierEntity, "primary key column has no mapped Go field"},
	E024: {SeverityError, TierRelation, "relation target is in another package"},
	E025: {SeverityError, TierEntity, "managed view has no definition"},
	E026: {SeverityError, TierEntity, "one relation name is declared as two different kinds"},
	E027: {SeverityError, TierEntity, "view dependency does not name a declared relation"},
	E028: {SeverityError, TierEntity, "view depends on itself"},
	E029: {SeverityError, TierEntity, "view dependencies form a cycle"},
	E030: {SeverityError, TierEntity, "relation exists with a different kind"},
	W031: {SeverityWarning, TierEntity, "definition provenance unknown"},
	E032: {SeverityError, TierEntity, "view definition drift"},
	E033: {SeverityError, TierEntity, "undeclared relation dependency"},
	E034: {SeverityError, TierEntity, "declared dependency the relation does not have"},
	W035: {SeverityWarning, TierEntity, "unrepresented relation metadata"},
	E036: {SeverityError, TierEntity, "materialized view index drift"},
}

// Codes returns every registered code in register order, which is numeric: the
// severity prefix is part of the name, not part of the ordering, so W003 sits
// between E002 and E004 exactly as it does in the specification.
func Codes() []Code {
	out := make([]Code, 0, len(registry))
	for c := range registry {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Code) int {
		return cmp.Or(cmp.Compare(a.Num(), b.Num()), cmp.Compare(a, b))
	})
	return out
}

// Num returns the numeric part of the code, or -1 when it has none.
func (c Code) Num() int {
	if len(c) < 2 {
		return -1
	}
	n, err := strconv.Atoi(string(c[1:]))
	if err != nil {
		return -1
	}
	return n
}

// Severity returns the code's default severity.
func (c Code) Severity() Severity { return registry[c].Severity }

// Tier returns the reconciliation stage the code belongs to.
func (c Code) Tier() Tier { return registry[c].Tier }

// Title returns the code's frozen one-line description.
func (c Code) Title() string { return registry[c].Title }

// Known reports whether the code is registered.
func (c Code) Known() bool {
	_, ok := registry[c]
	return ok
}

// Finding is one disagreement between the Go structs and the schema.
//
// The fields beyond Code and Message exist because "E004" on its own tells an
// author nothing. A finding names what it is about on both sides — the entity
// and field, the table and column, both types — and then says why it is a
// problem and what to do about it.
type Finding struct {
	// Code identifies the kind of finding and never changes meaning. It is what
	// a project suppresses, alerts on and gates CI against, which is why the
	// register is part of the public contract.
	Code Code
	// Severity is how much it matters. It is fixed by the code rather than
	// decided per finding, so two occurrences of one code are never reported at
	// different severities.
	Severity Severity
	// Message is the one-line summary.
	Message string
	// Reason explains why the two sides disagree.
	Reason string
	// Fix is the concrete change that would resolve it.
	Fix string

	// The Go side of the disagreement: the entity, the field on it, and the
	// field's type as written. Empty when the finding is not about one field.
	Entity string
	Field  string
	GoType string

	// The PostgreSQL side: the table, the column, and the column's type as the
	// catalog reports it. Empty when the finding is not about one column.
	Table  string
	Column string
	PGType string

	// Relation is the relation field's name, for relation findings.
	Relation string
	// Constraint is the PostgreSQL constraint or index the finding is about.
	Constraint string

	// Pos is where in the Go source the finding should be reported, so an
	// editor and a CI annotation can point at it.
	Pos model.Position
}

// Tier returns the finding's tier, from its code.
func (f Finding) Tier() Tier { return f.Code.Tier() }

// Report is the outcome of one reconciliation.
type Report struct {
	findings []Finding
}

// Add appends a finding, defaulting its severity from the code register when
// the caller did not set one explicitly. Configurable checks set it themselves.
func (r *Report) Add(f Finding) {
	if f.Severity == severityUnset {
		f.Severity = f.Code.Severity()
	}
	r.findings = append(r.findings, f)
}

// Findings returns the findings in report order.
func (r *Report) Findings() []Finding {
	r.sort()
	return r.findings
}

// Len returns the number of findings.
func (r *Report) Len() int { return len(r.findings) }

// Count returns the number of findings at a severity.
func (r *Report) Count(s Severity) int {
	n := 0
	for _, f := range r.findings {
		if f.Severity == s {
			n++
		}
	}
	return n
}

// Failed reports whether any finding is at or above the threshold.
func (r *Report) Failed(threshold Severity) bool {
	for _, f := range r.findings {
		if f.Severity >= threshold {
			return true
		}
	}
	return false
}

// sort puts the findings in a total order over stable keys. Nothing in the key
// depends on the order the checks happened to run in, so the same inputs always
// produce the same bytes.
//
// Within an entity, located findings come before schema-only ones: a reader
// works through the source file and then through what the schema has that the
// source does not.
func (r *Report) sort() {
	slices.SortStableFunc(r.findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Entity, b.Entity),
			cmp.Compare(locatedRank(a), locatedRank(b)),
			cmp.Compare(a.Pos.File, b.Pos.File),
			cmp.Compare(a.Pos.Line, b.Pos.Line),
			cmp.Compare(a.Pos.Col, b.Pos.Col),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Field, b.Field),
			cmp.Compare(a.Table, b.Table),
			cmp.Compare(a.Column, b.Column),
			cmp.Compare(a.Relation, b.Relation),
			cmp.Compare(a.Constraint, b.Constraint),
			cmp.Compare(a.Message, b.Message),
			cmp.Compare(a.Reason, b.Reason),
		)
	})
}

// locatedRank sorts findings that point at a source position ahead of findings
// that only point at a schema object.
func locatedRank(f Finding) int {
	if f.Pos.IsZero() {
		return 1
	}
	return 0
}
