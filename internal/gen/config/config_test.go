package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(pairs ...string) Lookup {
	m := make(map[string]string, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const minimal = `version: 1
schema:
  dsn: postgres://localhost/app
packages:
  - path: ./internal/domain
`

func load(t *testing.T, doc string, lookup Lookup) (*Config, error) {
	t.Helper()
	if lookup == nil {
		lookup = env()
	}
	return parse([]byte(doc), filepath.Join("/proj", "orm.yaml"), lookup)
}

func TestParse_minimalDefaults(t *testing.T) {
	cfg, err := load(t, minimal, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Schema.SearchPath; len(got) != 1 || got[0] != "public" {
		t.Errorf("search_path = %v, want [public]", got)
	}
	if got := cfg.Packages[0].Output; got != "same" {
		t.Errorf("output = %q, want %q", got, "same")
	}
	if cfg.Strict.UnmappedColumns != Warn || cfg.Strict.TimestampWithoutTZ != Warn {
		t.Errorf("strict = %+v, want both warn", cfg.Strict)
	}
	if cfg.Root != "/proj" {
		t.Errorf("Root = %q, want /proj", cfg.Root)
	}
}

func TestParse_fullDocument(t *testing.T) {
	const doc = `version: 1
schema:
  dsn: ${DATABASE_URL}
  search_path:
    - app
    - public
packages:
  - path: ./internal/domain
    output: same
  - path: ./internal/billing
    output: ./internal/gen
types:
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
  uuid:
    go: github.com/google/uuid.UUID
strict:
  unmapped_columns: error
  timestamp_without_tz: off
`
	cfg, err := load(t, doc, env("DATABASE_URL", "postgres://u@h/db"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Schema.DSN != "postgres://u@h/db" {
		t.Errorf("dsn = %q, want the expanded value", cfg.Schema.DSN)
	}
	if got := strings.Join(cfg.Schema.SearchPath, ","); got != "app,public" {
		t.Errorf("search_path = %q, want app,public", got)
	}
	num := cfg.Types["numeric"]
	if num.Import != "github.com/shopspring/decimal" || num.Name != "Decimal" {
		t.Errorf("numeric = %+v, want shopspring/decimal.Decimal", num)
	}
	if num.Qualified != "github.com/shopspring/decimal.Decimal" {
		t.Errorf("numeric.Qualified = %q", num.Qualified)
	}
	if num.Codec != "decimal" {
		t.Errorf("numeric.Codec = %q, want decimal", num.Codec)
	}
	if cfg.Strict.UnmappedColumns != Error {
		t.Errorf("unmapped_columns = %q, want error", cfg.Strict.UnmappedColumns)
	}
	if cfg.Strict.TimestampWithoutTZ != Off {
		t.Errorf("timestamp_without_tz = %q, want off", cfg.Strict.TimestampWithoutTZ)
	}
	if got := cfg.OutputDir(cfg.Packages[1]); got != filepath.Clean("/proj/internal/gen") {
		t.Errorf("OutputDir = %q", got)
	}
	if got := cfg.OutputDir(cfg.Packages[0]); got != filepath.Clean("/proj/internal/domain") {
		t.Errorf("OutputDir(same) = %q", got)
	}
}

func TestParse_errors(t *testing.T) {
	tests := []struct {
		name   string
		doc    string
		lookup Lookup
		want   string
	}{
		{
			name: "missing version",
			doc:  "schema:\n  dsn: x\npackages:\n  - path: ./d\n",
			want: "version: required",
		},
		{
			name: "wrong version",
			doc:  "version: 2\nschema:\n  dsn: x\npackages:\n  - path: ./d\n",
			want: "unsupported version 2",
		},
		{
			name: "no schema source",
			doc:  "version: 1\npackages:\n  - path: ./d\n",
			want: "one of dsn or file is required",
		},
		{
			name: "both schema sources",
			doc:  "version: 1\nschema:\n  dsn: x\n  file: db/schema.sql\n  admin_dsn: y\npackages:\n  - path: ./d\n",
			want: "dsn and file are mutually exclusive",
		},
		{
			name: "file without admin dsn",
			doc:  "version: 1\nschema:\n  file: db/schema.sql\npackages:\n  - path: ./d\n",
			want: "schema.admin_dsn: required",
		},
		{
			name: "no packages",
			doc:  "version: 1\nschema:\n  dsn: x\n",
			want: "at least one package is required",
		},
		{
			name: "duplicate package",
			doc:  "version: 1\nschema:\n  dsn: x\npackages:\n  - path: ./d\n  - path: ./d\n",
			want: `"./d" listed more than once`,
		},
		{
			name: "unknown field",
			doc:  "version: 1\nschema:\n  dsn: x\n  nope: 1\npackages:\n  - path: ./d\n",
			want: "field nope not found",
		},
		{
			name: "bad strictness",
			doc:  "version: 1\nschema:\n  dsn: x\npackages:\n  - path: ./d\nstrict:\n  unmapped_columns: loud\n",
			want: `invalid value "loud"`,
		},
		{
			name: "unqualified configured type",
			doc:  "version: 1\nschema:\n  dsn: x\npackages:\n  - path: ./d\ntypes:\n  numeric:\n    go: github.com/shopspring/decimal\n",
			want: "has no type name",
		},
		{
			name: "empty configured type",
			doc:  "version: 1\nschema:\n  dsn: x\npackages:\n  - path: ./d\ntypes:\n  numeric:\n    codec: decimal\n",
			want: "types.numeric.go: required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(t, tt.doc, tt.lookup)
			if err == nil {
				t.Fatalf("parse succeeded, want error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParse_unsetEnvironmentVariableIsAnError(t *testing.T) {
	const doc = "version: 1\nschema:\n  dsn: ${DATABASE_URL}\npackages:\n  - path: ./d\n"
	_, err := load(t, doc, env())
	if err == nil {
		t.Fatal("parse succeeded with an unset variable, want error")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is not set") {
		t.Errorf("error = %v, want it to name the unset variable", err)
	}
}

func TestExpand(t *testing.T) {
	lookup := env("A", "1", "EMPTY", "")
	tests := []struct {
		in      string
		want    string
		wantErr string
	}{
		{in: "plain", want: "plain"},
		{in: "${A}", want: "1"},
		{in: "x${A}y${EMPTY}z", want: "x1yz"},
		{in: "$$HOME", want: "$HOME"},
		{in: "$HOME", wantErr: "malformed reference"},
		{in: "${A", wantErr: "unterminated"},
		{in: "${}", wantErr: "empty variable name"},
		{in: "${MISSING}", wantErr: "not set"},
		{in: "trailing$", wantErr: "malformed reference"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := expand(tt.in, lookup)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expand(%q) error = %v, want it to contain %q", tt.in, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expand(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("expand(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSplitGoType(t *testing.T) {
	tests := []struct {
		in       string
		wantImp  string
		wantName string
		wantErr  bool
	}{
		{in: "github.com/shopspring/decimal.Decimal", wantImp: "github.com/shopspring/decimal", wantName: "Decimal"},
		{in: "time.Time", wantImp: "time", wantName: "Time"},
		{in: "string", wantName: "string"},
		{in: "github.com/x/y", wantErr: true},
		{in: "a.b.c", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			imp, name, err := splitGoType(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("splitGoType(%q) succeeded, want error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitGoType(%q): %v", tt.in, err)
			}
			if imp != tt.wantImp || name != tt.wantName {
				t.Errorf("splitGoType(%q) = (%q, %q), want (%q, %q)", tt.in, imp, name, tt.wantImp, tt.wantName)
			}
		})
	}
}

func TestTemplate_isValid(t *testing.T) {
	cfg, err := load(t, Template, env("DATABASE_URL", "postgres://localhost/app"))
	if err != nil {
		t.Fatalf("the orm init template does not load: %v", err)
	}
	if len(cfg.Types) != 2 {
		t.Errorf("template configured %d types, want 2", len(cfg.Types))
	}
}

func TestLoad_fromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orm.yaml")
	if err := os.WriteFile(path, []byte(minimal), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Root is the configuration's own directory, because every relative path
	// in the document and every position in a report resolves against it.
	if cfg.Root != dir {
		t.Errorf("Root = %q, want %q", cfg.Root, dir)
	}
	if got := cfg.Dir(cfg.Packages[0]); got != filepath.Join(dir, "internal", "domain") {
		t.Errorf("Dir = %q", got)
	}
}

func TestLoad_missingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
	if !strings.Contains(err.Error(), "reading configuration") {
		t.Errorf("error = %v, want it to name the failing step", err)
	}
}

func TestSchemaFile_resolvesAgainstTheRoot(t *testing.T) {
	const doc = `version: 1
schema:
  file: db/schema.sql
  admin_dsn: postgres://localhost/postgres
packages:
  - path: ./domain
`
	cfg, err := load(t, doc, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.Schema.FromFile() {
		t.Error("FromFile() = false for a file-backed schema")
	}
	if got := cfg.SchemaFile(); got != filepath.Join("/proj", "db", "schema.sql") {
		t.Errorf("SchemaFile() = %q", got)
	}

	dsnCfg, err := load(t, minimal, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if dsnCfg.Schema.FromFile() {
		t.Error("FromFile() = true for a DSN-backed schema")
	}
	if got := dsnCfg.SchemaFile(); got != "" {
		t.Errorf("SchemaFile() = %q for a DSN-backed schema, want empty", got)
	}
}

func TestAbsolutePathsAreLeftAlone(t *testing.T) {
	const doc = `version: 1
schema:
  file: /abs/db/schema.sql
  admin_dsn: postgres://localhost/postgres
packages:
  - path: /abs/domain
    output: /abs/gen
`
	cfg, err := load(t, doc, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.SchemaFile(); got != "/abs/db/schema.sql" {
		t.Errorf("SchemaFile() = %q", got)
	}
	if got := cfg.Dir(cfg.Packages[0]); got != "/abs/domain" {
		t.Errorf("Dir = %q", got)
	}
	if got := cfg.OutputDir(cfg.Packages[0]); got != "/abs/gen" {
		t.Errorf("OutputDir = %q", got)
	}
}

func TestRel_neverEscapesTheRoot(t *testing.T) {
	cfg := &Config{Root: "/proj"}
	if got := cfg.Rel("/proj/internal/domain/user.go"); got != "internal/domain/user.go" {
		t.Errorf("Rel = %q", got)
	}
	if got := cfg.Rel("/elsewhere/user.go"); got != "user.go" {
		t.Errorf("Rel outside the root = %q, want the base name only", got)
	}
	if got := cfg.Rel(""); got != "" {
		t.Errorf("Rel(\"\") = %q", got)
	}
}
