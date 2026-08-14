package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Version is the only orm.yaml schema version v1 understands.
const Version = 1

// DefaultMigrationsDir is where migrations live unless the configuration says
// otherwise.
const DefaultMigrationsDir = "migrations"

// Strictness selects what a soft finding does.
type Strictness string

// Strictness levels. Off suppresses the finding entirely; the other two set its
// severity.
const (
	Off   Strictness = "off"
	Warn  Strictness = "warn"
	Error Strictness = "error"
)

var strictnessValues = []Strictness{Off, Warn, Error}

func parseStrictness(field, s string) (Strictness, error) {
	if slices.Contains(strictnessValues, Strictness(s)) {
		return Strictness(s), nil
	}
	return "", fmt.Errorf("strict.%s: invalid value %q, want off, warn or error", field, s)
}

// Config is a validated orm.yaml.
type Config struct {
	Version    int
	Schema     Schema
	Migrations Migrations
	Packages   []Package
	Types      map[string]TypeMapping
	Strict     Strict

	// Root is the directory the configuration file lives in. Every relative
	// path in the configuration resolves against it, and every source position
	// in a report is reported relative to it.
	Root string
	// Path is the configuration file the values came from.
	Path string
}

// Mode says which side of the project owns the schema.
type Mode uint8

const (
	// ModeDatabase is the default: PostgreSQL is authoritative and something
	// else — another migration tool, or hand-written SQL — maintains it. The
	// entities are checked against it and nothing here proposes to change it.
	ModeDatabase Mode = iota
	// ModeManaged means the Go declarations describe the desired schema and
	// migrations carry it there. PostgreSQL is still where the schema lives;
	// what changes is where the intent comes from.
	ModeManaged
)

func (m Mode) String() string {
	if m == ModeManaged {
		return "managed"
	}
	return "database"
}

// ParseMode reads a configured mode. An empty value is the default, so an
// existing configuration keeps meaning what it did.
func ParseMode(s string) (Mode, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "database":
		return ModeDatabase, nil
	case "managed":
		return ModeManaged, nil
	default:
		return 0, fmt.Errorf("unknown mode %q; write database or managed", s)
	}
}

// Schema says where the PostgreSQL schema comes from.
type Schema struct {
	// Mode says which side owns the schema.
	//
	// It is never inferred. A project with migration files is not thereby a
	// managed project, and a project without them is not thereby a
	// database-first one — the two workflows have different sources of truth,
	// and guessing which one an author meant is how a tool ends up proposing to
	// drop a table somebody's other migration system created.
	Mode Mode
	// DSN connects to a live database that already has the schema.
	DSN string
	// File is a DDL file to apply to a throwaway database, resolved relative to
	// Config.Root.
	File string
	// AdminDSN connects to a server with permission to create and drop that
	// throwaway database. It is required with File and ignored with DSN.
	AdminDSN string
	// SearchPath lists the schemas to introspect, in resolution order.
	SearchPath []string
}

// FromFile reports whether the schema is built from a DDL file.
func (s Schema) FromFile() bool { return s.File != "" }

// Migrations says where the migration artifacts live.
//
// The directory is configuration rather than a convention because a project
// with more than one service in it has more than one history, and a tool that
// assumed ./migrations would quietly plan the wrong one.
type Migrations struct {
	// Dir is the migrations directory, resolved relative to Config.Root.
	Dir string
}

// Package is one Go package holding entity structs.
type Package struct {
	// Path is the package directory, resolved relative to Config.Root.
	Path string
	// Output names the directory generated code would be written to: "same"
	// means the entity package's own directory. M1 generates nothing, but the
	// value still participates in reconciliation because two entities whose
	// generated identifiers would collide in one output directory are a
	// finding.
	Output string
}

// TypeMapping is a configured Go representation for a PostgreSQL type.
type TypeMapping struct {
	// Import is the package path of the Go type, empty for a builtin.
	Import string
	// Name is the type name within Import.
	Name string
	// Qualified is Import + "." + Name, which is what a resolved GoType reports
	// as its Named.
	Qualified string
	// Codec names the runtime codec that will encode the type. M1 records it
	// and does not use it.
	Codec string
}

// Strict holds the configurable severities.
type Strict struct {
	UnmappedColumns    Strictness
	TimestampWithoutTZ Strictness
}

// yamlFile mirrors the on-disk document. It is deliberately separate from
// Config so that "absent" and "empty" stay distinguishable during validation.
type yamlFile struct {
	Version    *int           `yaml:"version"`
	Schema     yamlSchema     `yaml:"schema"`
	Migrations yamlMigrations `yaml:"migrations"`
	Packages   []yamlPackage  `yaml:"packages"`
	Types      yaml.Node      `yaml:"types"`
	Strict     yamlStrict     `yaml:"strict"`
}

type yamlMigrations struct {
	Dir string `yaml:"dir"`
}

type yamlSchema struct {
	Mode       string   `yaml:"mode"`
	DSN        string   `yaml:"dsn"`
	File       string   `yaml:"file"`
	AdminDSN   string   `yaml:"admin_dsn"`
	SearchPath []string `yaml:"search_path"`
}

type yamlPackage struct {
	Path   string `yaml:"path"`
	Output string `yaml:"output"`
}

type yamlType struct {
	Go    string `yaml:"go"`
	Codec string `yaml:"codec"`
}

type yamlStrict struct {
	UnmappedColumns    string `yaml:"unmapped_columns"`
	TimestampWithoutTZ string `yaml:"timestamp_without_tz"`
}

// Load reads, expands and validates the configuration at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading configuration: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving configuration path: %w", err)
	}
	cfg, err := parse(data, abs, os.LookupEnv)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return cfg, nil
}

// Lookup resolves an environment variable, reporting whether it is set. It is
// the shape of os.LookupEnv, which is what Load supplies.
type Lookup func(string) (string, bool)

// parse is Load without the file system, so tests can supply an environment.
func parse(data []byte, absPath string, getenv Lookup) (*Config, error) {
	var raw yamlFile
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}

	types, err := decodeTypes(&raw.Types)
	if err != nil {
		return nil, err
	}

	exp := expander{getenv: getenv}
	cfg := &Config{
		Root:  filepath.Dir(absPath),
		Path:  absPath,
		Types: make(map[string]TypeMapping, len(types)),
	}

	if raw.Version == nil {
		return nil, errors.New("version: required")
	}
	cfg.Version = *raw.Version

	mode, err := ParseMode(exp.string("schema.mode", raw.Schema.Mode))
	if err != nil {
		return nil, fmt.Errorf("schema.mode: %w", err)
	}
	cfg.Schema = Schema{
		Mode:       mode,
		DSN:        exp.string("schema.dsn", raw.Schema.DSN),
		File:       exp.string("schema.file", raw.Schema.File),
		AdminDSN:   exp.string("schema.admin_dsn", raw.Schema.AdminDSN),
		SearchPath: make([]string, 0, len(raw.Schema.SearchPath)),
	}
	for i, s := range raw.Schema.SearchPath {
		cfg.Schema.SearchPath = append(cfg.Schema.SearchPath, exp.string(fmt.Sprintf("schema.search_path[%d]", i), s))
	}
	cfg.Migrations = Migrations{Dir: exp.string("migrations.dir", raw.Migrations.Dir)}

	for i, p := range raw.Packages {
		cfg.Packages = append(cfg.Packages, Package{
			Path:   exp.string(fmt.Sprintf("packages[%d].path", i), p.Path),
			Output: exp.string(fmt.Sprintf("packages[%d].output", i), p.Output),
		})
	}

	for _, key := range sortedKeys(types) {
		t := types[key]
		codec := exp.string(fmt.Sprintf("types.%s.codec", key), t.Codec)
		gostr := exp.string(fmt.Sprintf("types.%s.go", key), t.Go)
		imp, name, err := splitGoType(gostr)
		if err != nil && exp.err == nil {
			exp.err = fmt.Errorf("types.%s.go: %w", key, err)
		}
		cfg.Types[key] = TypeMapping{
			Import:    imp,
			Name:      name,
			Qualified: qualify(imp, name),
			Codec:     codec,
		}
	}

	cfg.Strict = Strict{UnmappedColumns: Warn, TimestampWithoutTZ: Warn}
	if s := exp.string("strict.unmapped_columns", raw.Strict.UnmappedColumns); s != "" {
		v, err := parseStrictness("unmapped_columns", s)
		if err != nil {
			return nil, err
		}
		cfg.Strict.UnmappedColumns = v
	}
	if s := exp.string("strict.timestamp_without_tz", raw.Strict.TimestampWithoutTZ); s != "" {
		v, err := parseStrictness("timestamp_without_tz", s)
		if err != nil {
			return nil, err
		}
		cfg.Strict.TimestampWithoutTZ = v
	}

	if exp.err != nil {
		return nil, exp.err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// decodeTypes decodes the types section, which yaml.Node defers so that a
// missing section stays distinguishable from an empty one.
func decodeTypes(n *yaml.Node) (map[string]yamlType, error) {
	if n == nil || n.Kind == 0 || n.Tag == "!!null" {
		return nil, nil
	}
	var types map[string]yamlType
	if err := n.Decode(&types); err != nil {
		return nil, fmt.Errorf("types: %w", err)
	}
	return types, nil
}

func (c *Config) validate() error {
	if c.Version != Version {
		return fmt.Errorf("version: unsupported version %d, want %d", c.Version, Version)
	}

	switch {
	case c.Schema.DSN == "" && c.Schema.File == "":
		return errors.New("schema: one of dsn or file is required")
	case c.Schema.DSN != "" && c.Schema.File != "":
		return errors.New("schema: dsn and file are mutually exclusive")
	case c.Schema.File != "" && c.Schema.AdminDSN == "":
		return errors.New("schema.admin_dsn: required when schema.file is set, because the DDL is applied to a throwaway database")
	}
	if len(c.Schema.SearchPath) == 0 {
		c.Schema.SearchPath = []string{"public"}
	}
	if slices.Contains(c.Schema.SearchPath, "") {
		return errors.New("schema.search_path: entries must not be empty")
	}

	if c.Migrations.Dir == "" {
		c.Migrations.Dir = DefaultMigrationsDir
	}

	if len(c.Packages) == 0 {
		return errors.New("packages: at least one package is required")
	}
	seen := make(map[string]bool, len(c.Packages))
	for i := range c.Packages {
		p := &c.Packages[i]
		if p.Path == "" {
			return fmt.Errorf("packages[%d].path: required", i)
		}
		if p.Output == "" {
			p.Output = "same"
		}
		if seen[p.Path] {
			return fmt.Errorf("packages[%d].path: %q listed more than once", i, p.Path)
		}
		seen[p.Path] = true
	}

	for _, key := range sortedKeys(c.Types) {
		t := c.Types[key]
		if t.Name == "" {
			return fmt.Errorf("types.%s.go: required", key)
		}
	}
	return nil
}

// Dir returns the absolute directory of package i.
func (c *Config) Dir(p Package) string {
	if filepath.IsAbs(p.Path) {
		return filepath.Clean(p.Path)
	}
	return filepath.Join(c.Root, p.Path)
}

// OutputDir returns the absolute directory generated code for p would go to.
func (c *Config) OutputDir(p Package) string {
	if p.Output == "" || p.Output == "same" {
		return c.Dir(p)
	}
	if filepath.IsAbs(p.Output) {
		return filepath.Clean(p.Output)
	}
	return filepath.Join(c.Root, p.Output)
}

// SchemaFile returns the absolute path of the DDL file, or "".
func (c *Config) SchemaFile() string {
	if c.Schema.File == "" {
		return ""
	}
	if filepath.IsAbs(c.Schema.File) {
		return filepath.Clean(c.Schema.File)
	}
	return filepath.Join(c.Root, c.Schema.File)
}

// MigrationsDir returns the absolute directory the migration artifacts live in.
func (c *Config) MigrationsDir() string {
	dir := c.Migrations.Dir
	if dir == "" {
		dir = DefaultMigrationsDir
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(c.Root, dir)
}

// Rel makes an absolute path relative to the configuration root so that reports
// never carry machine-specific paths. Paths outside the root keep their base
// name only.
func (c *Config) Rel(path string) string {
	if path == "" {
		return ""
	}
	rel, err := filepath.Rel(c.Root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.Base(path)
	}
	return filepath.ToSlash(rel)
}

// expander substitutes ${NAME} references and records the first failure so that
// a whole document can be walked before reporting.
type expander struct {
	getenv func(string) (string, bool)
	err    error
}

func (e *expander) string(field, s string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	out, err := expand(s, e.getenv)
	if err != nil && e.err == nil {
		e.err = fmt.Errorf("%s: %w", field, err)
	}
	return out
}

// expand replaces ${NAME} with the environment value, failing on an unset
// variable. $$ is a literal dollar sign. A bare $ or $NAME is rejected, because
// silently treating it as a literal is exactly how an unexpanded DSN reaches
// PostgreSQL.
func expand(s string, getenv func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		if c != '$' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '$' {
			b.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(s) || s[i+1] != '{' {
			return "", fmt.Errorf("malformed reference at offset %d: want ${NAME} or $$ for a literal dollar", i)
		}
		end := strings.IndexByte(s[i:], '}')
		if end < 0 {
			return "", fmt.Errorf("unterminated ${ at offset %d", i)
		}
		name := s[i+2 : i+end]
		if name == "" {
			return "", fmt.Errorf("empty variable name at offset %d", i)
		}
		v, ok := getenv(name)
		if !ok {
			return "", fmt.Errorf("environment variable %s is not set", name)
		}
		b.WriteString(v)
		i += end + 1
	}
	return b.String(), nil
}

// splitGoType splits "github.com/shopspring/decimal.Decimal" into its import
// path and type name, and accepts a bare builtin such as "string".
func splitGoType(s string) (imp, name string, err error) {
	if s == "" {
		return "", "", errors.New("required")
	}
	slash := strings.LastIndexByte(s, '/')
	dot := strings.IndexByte(s[slash+1:], '.')
	if dot < 0 {
		if slash >= 0 {
			return "", "", fmt.Errorf("%q has no type name; write path/to/pkg.TypeName", s)
		}
		return "", s, nil
	}
	dot += slash + 1
	imp, name = s[:dot], s[dot+1:]
	if imp == "" || name == "" {
		return "", "", fmt.Errorf("%q is not a qualified Go type name", s)
	}
	if strings.Contains(name, ".") {
		return "", "", fmt.Errorf("%q has more than one type qualifier", s)
	}
	return imp, name, nil
}

func qualify(imp, name string) string {
	if imp == "" {
		return name
	}
	return imp + "." + name
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// Template is the starting configuration written by `orm init`.
const Template = `version: 1

schema:
  # database: PostgreSQL owns the schema and something else maintains it.
  # managed:  the Go declarations own it and orm migrations carry it there.
  mode: database
  # A live database that already carries the schema.
  dsn: ${DATABASE_URL}
  # Or build one from DDL instead, which needs an administrative connection:
  # file: db/schema.sql
  # admin_dsn: ${ADMIN_DATABASE_URL}
  search_path:
    - public

packages:
  - path: ./internal/domain
    output: same

# Types PostgreSQL has but Go does not agree on. numeric and uuid have no
# default mapping on purpose: silently choosing float64 for numeric loses money.
types:
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid

strict:
  unmapped_columns: warn
  timestamp_without_tz: warn
`

// ManagedTemplate is the starting configuration for a project whose Go
// declarations own the schema.
//
// It is a separate template rather than a commented-out section of the other
// one, because the mode is a decision about who owns the schema and a project
// should have made it before the first command runs.
const ManagedTemplate = `version: 1

schema:
  # The Go declarations describe the schema; migrations carry it to PostgreSQL.
  mode: managed
  dsn: ${DATABASE_URL}
  search_path:
    - public

migrations:
  dir: migrations

packages:
  - path: ./internal/domain
    output: same

# Types PostgreSQL has but Go does not agree on. numeric and uuid have no
# default mapping on purpose: silently choosing float64 for numeric loses money.
types:
  numeric:
    go: github.com/shopspring/decimal.Decimal
    codec: decimal
  uuid:
    go: github.com/google/uuid.UUID
    codec: uuid

strict:
  unmapped_columns: warn
  timestamp_without_tz: warn
`
